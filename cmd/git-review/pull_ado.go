package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
	adoreview "github.com/hdweiss/git-issue/internal/bridge/ado/review"
	"github.com/hdweiss/git-issue/internal/cli"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/origins"
	"github.com/hdweiss/git-issue/internal/remote"
	"github.com/hdweiss/git-issue/internal/review"
)

// pullADO imports pull requests from Azure DevOps.
//
// The shape mirrors pullGitHub, which mirrors the git path: fetch, union into
// the reviews ref, write any check runs to their own ref, then report what
// moved. What differs is only where the events come from, and that a pull
// request is per-repository — so the target has to name one.
func pullADO(s *entity.Store, spec remote.Spec, opts pullOptions) error {
	if spec.ScopeSet {
		return fmt.Errorf("ado: a review pull has no '#' scope; an area path is a work-item concept")
	}
	target, err := adoapi.ParseTarget(spec.URL)
	if err != nil {
		return fmt.Errorf("%s: %w", spec.Target, err)
	}
	if target.Repo == "" {
		return fmt.Errorf("%s names no repository; a review pull needs the /_git/<repo> URL, not a collection/project slug", spec.Target)
	}

	token, source, cred := adoapi.Token(s.Repo, adoHost(target), opts.token)
	client := adoapi.New(token)

	// Azure DevOps compares project names case-insensitively, so two spellings
	// of one project would be two paths in the ledger and two watermarks. This
	// is also the first request of the run, which makes it where a bad
	// credential is reported rather than several calls later. Reviews have no
	// area scope, so there is nothing else to resolve.
	name, err := client.Project(target)
	if err != nil {
		return importErrorADOReview(target, source, err)
	}
	target.Project = name

	filter := adoapi.PullFilter{All: opts.all, Since: opts.since, Limit: opts.limit}
	scope := "open pull requests"
	if opts.all {
		scope = "all pull requests"
	}

	state, err := s.SyncState()
	if err != nil {
		return err
	}
	// A date the caller asked for wins and does not disturb the watermark:
	// --since is a question, not a checkpoint. --full asks for everything and
	// does move it, because it genuinely read everything.
	if !opts.sinceGiven && !opts.full {
		filter.Since = state.Since(adoReviewSyncKey(target, opts.all))
	}

	fmt.Fprintf(os.Stderr, "Importing %s from %s/%s%s\n", scope, target, target.Repo, sinceNote(filter.Since))
	prog := cli.Progress{W: os.Stderr, Noun: "pull requests"}
	pulls, err := client.FetchPulls(target, filter, prog.Update)
	prog.Done()
	if err != nil {
		return importErrorADOReview(target, source, err)
	}
	if cred != nil {
		s.Repo.CredentialApprove(*cred)
	}

	led := origins.NewStore(s.Repo)
	ledger, err := led.Load(target.String())
	if err != nil {
		return err
	}
	for _, w := range ledger.Warnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}

	// Filed first, imported second, the same two passes the other bridges make:
	// a `closes` link names a local entity, so it can only be written once the
	// ledger holds the work item it points at. The second pass costs no
	// requests. Only the review mappings are filed in the first pass — recording
	// a comment there would make the second import skip it as one this clone
	// posted.
	imported := make([]adoreview.Entity, len(pulls))
	ids := make([]string, len(pulls))
	for i, pr := range pulls {
		e, err := adoreview.Import(s.Format(), target, pr, ledger)
		if err != nil {
			return fmt.Errorf("importing pull request %d: %w", pr.ID, err)
		}
		id := e.ID
		if claimed, ok := ledger.Entity(e.Origin); ok {
			id = claimed
		}
		imported[i], ids[i] = e, id
		recordReviewADO(ledger, id, e)
	}

	incoming := make([]entity.Incoming, 0, len(pulls))
	var runs []review.CommitCheck
	for i, pr := range pulls {
		e := imported[i]
		if e.Unresolved > 0 {
			again, err := adoreview.Import(s.Format(), target, pr, ledger)
			if err != nil {
				return fmt.Errorf("importing pull request %d: %w", pr.ID, err)
			}
			e = again
		}
		incoming = append(incoming, entity.Incoming{ID: ids[i], Actions: e.Actions})
		recordADO(ledger, ids[i], e)
		runs = append(runs, e.Checks...)
	}

	pull, err := s.Apply(target.String(), incoming)
	if err != nil {
		return err
	}

	if n, err := writeChecks(s, runs); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record check runs: %s\n", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "Recorded %s on %s\n",
			cli.Plural(n, "check run"), cli.Plural(commitsOf(runs), "commit"))
	}

	if err := led.Save(ledger, ledgerMessage(target.String(), len(pulls))); err != nil {
		return err
	}
	if !opts.sinceGiven {
		if err := state.Record(adoReviewWatermark(pulls), adoReviewSyncKeys(target, opts.all)...); err != nil {
			return err
		}
	}
	if err := syncReport(s).Report(pull, fmt.Sprintf("From %s/%s", target, target.Repo), uniqueIDs(s)); err != nil {
		return err
	}
	cli.Repack(s.Repo, pull.Commits, os.Stderr)
	return nil
}

// recordReviewADO files where the review itself lives, and nothing about its
// comments — which is what the first of the two import passes may safely say.
func recordReviewADO(ledger *origins.Ledger, id string, e adoreview.Entity) {
	ledger.Add(origins.KindReview, id, e.Origin)
	if e.URL != "" {
		ledger.Add(origins.KindURL, id, e.URL)
	}
}

// recordADO files everything an import established about where an entity lives
// upstream. Adding a mapping that is already there changes nothing.
func recordADO(ledger *origins.Ledger, id string, e adoreview.Entity) {
	recordReviewADO(ledger, id, e)
	for _, c := range e.Comments {
		ledger.Add(origins.KindComment, id, c.EventID, c.Upstream)
	}
	for _, t := range e.Threads {
		ledger.Add(origins.KindThread, id, t.EventID, t.Upstream)
	}
}

// adoHost is the host a credential is looked up under, which is the API's host
// rather than the project's full path.
func adoHost(t adoapi.Target) string {
	host := strings.TrimPrefix(strings.TrimPrefix(t.Base, "https://"), "http://")
	if slash := strings.Index(host, "/"); slash >= 0 {
		host = host[:slash]
	}
	return host
}

// adoReviewWatermark is the newest activity time among the pull requests just
// imported.
func adoReviewWatermark(pulls []adoapi.PullRequest) time.Time {
	var newest time.Time
	for _, pr := range pulls {
		if pr.UpdatedAt.After(newest) {
			newest = pr.UpdatedAt
		}
	}
	return newest
}

// adoReviewSyncKey names one repository at one scope. The scope is part of the
// key because an open-only run has read every active pull request up to its
// watermark and says nothing about the rest.
const adoReviewSyncScheme = "ado-reviews:"

func adoReviewSyncKey(t adoapi.Target, all bool) string {
	scope := "open"
	if all {
		scope = "all"
	}
	return adoReviewSyncScheme + t.String() + "/" + t.Repo + ":" + scope
}

// adoReviewSyncKeys are the watermarks one run may advance. An --all run has
// necessarily seen every active pull request too, so it advances both.
func adoReviewSyncKeys(t adoapi.Target, all bool) []string {
	if all {
		return []string{adoReviewSyncKey(t, true), adoReviewSyncKey(t, false)}
	}
	return []string{adoReviewSyncKey(t, false)}
}

// importErrorADOReview says which credential was used, so that a failure on a
// repository somebody cannot see reads as an authorization problem rather than a
// typo.
func importErrorADOReview(target adoapi.Target, source adoapi.TokenSource, err error) error {
	var e *adoapi.Error
	if !errors.As(err, &e) {
		return err
	}
	switch {
	case source == adoapi.FromNowhere:
		return fmt.Errorf("%s: %w\n       no Azure DevOps token found; set AZURE_DEVOPS_EXT_PAT, or run: az login", target, e)
	case e.Unauthorized():
		return fmt.Errorf("%s: %w\n       using the token from %s", target, e, source)
	default:
		return fmt.Errorf("%s: %w", target, e)
	}
}

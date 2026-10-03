package main

import (
	"fmt"
	"os"
	"time"

	giteaapi "github.com/hdweiss/git-issue/internal/bridge/gitea/api"
	giteaissue "github.com/hdweiss/git-issue/internal/bridge/gitea/issue"
	"github.com/hdweiss/git-issue/internal/cli"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/origins"
	"github.com/hdweiss/git-issue/internal/remote"
)

// pullGitea imports issues from a Gitea (or Forgejo) API.
//
// The shape mirrors the GitHub path deliberately, which mirrors the git one:
// fetch, union into the notes ref, then report what moved in the columns `list`
// uses. What differs is only where the events come from.
func pullGitea(s *entity.Store, spec remote.Spec, opts pullOptions) error {
	target, err := giteaapi.ParseTarget(spec.URL)
	if err != nil {
		return fmt.Errorf("%s: %w", spec.Target, err)
	}

	token, source, cred := giteaapi.Token(s.Repo, target.Host, opts.token)
	client := giteaapi.New(target.API(), token)

	filter := giteaapi.Filter{Open: !opts.all, Since: opts.since, Limit: opts.limit}
	scope := "open issues"
	if opts.all {
		scope = "all issues"
	}

	state, err := s.SyncState()
	if err != nil {
		return err
	}
	if !opts.sinceGiven && !opts.full {
		filter.Since = state.Since(giteaSyncKey(target, opts.all))
	}

	fmt.Fprintf(os.Stderr, "Importing %s from %s%s\n", scope, target, sinceNote(filter.Since))
	prog := cli.Progress{W: os.Stderr, Noun: "issues"}
	issues, err := client.Fetch(target, filter, prog.Update)
	prog.Done()
	if err != nil {
		return importErrorGitea(target, source, err)
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

	// Filed first, imported second — a link can only be written once the ledger
	// holds the issue it names, and a batch routinely contains both ends. See
	// the same two passes in pull_github.go.
	imported := make([]giteaissue.Entity, len(issues))
	ids := make([]string, len(issues))
	for i, iss := range issues {
		e, err := giteaissue.Import(s.Format(), target, iss, ledger)
		if err != nil {
			return fmt.Errorf("importing %s: %w", iss.HTMLURL, err)
		}
		id := e.ID
		if claimed, ok := ledger.Entity(e.Origin); ok {
			id = claimed
		}
		imported[i], ids[i] = e, id
		recordGiteaIssue(ledger, id, e)
	}

	incoming := make([]entity.Incoming, 0, len(issues))
	for i, iss := range issues {
		e := imported[i]
		if e.Unresolved > 0 {
			again, err := giteaissue.Import(s.Format(), target, iss, ledger)
			if err != nil {
				return fmt.Errorf("importing %s: %w", iss.HTMLURL, err)
			}
			e = again
		}
		incoming = append(incoming, entity.Incoming{ID: ids[i], Actions: e.Actions})
		recordGitea(ledger, ids[i], e)
	}

	pull, err := s.Apply(target.String(), incoming)
	if err != nil {
		return err
	}

	if err := led.Save(ledger, ledgerMessage(target.String(), len(issues))); err != nil {
		return err
	}
	if !opts.sinceGiven {
		if err := state.Record(giteaWatermark(issues), giteaSyncKeys(target, opts.all)...); err != nil {
			return err
		}
	}
	if err := report(pull, fmt.Sprintf("From %s", target), uniqueIDs(s)); err != nil {
		return err
	}
	cli.Repack(s.Repo, pull.Commits, os.Stderr)
	return nil
}

func recordGitea(ledger *origins.Ledger, id string, e giteaissue.Entity) {
	recordGiteaIssue(ledger, id, e)
	for _, c := range e.Comments {
		ledger.Add(origins.KindComment, id, c.EventID, c.Upstream)
	}
}

func recordGiteaIssue(ledger *origins.Ledger, id string, e giteaissue.Entity) {
	ledger.Add(origins.KindIssue, id, e.Origin)
	if e.URL != "" {
		ledger.Add(origins.KindURL, id, e.URL)
	}
}

// giteaWatermark is the newest updatedAt among the issues just imported. Issues
// arrive ordered by updatedAt ascending, so a run cut short by --limit has read
// a prefix and the next run asks for exactly the rest.
func giteaWatermark(issues []giteaapi.Issue) time.Time {
	var newest time.Time
	for _, iss := range issues {
		if iss.UpdatedAt.After(newest) {
			newest = iss.UpdatedAt
		}
	}
	return newest
}

func giteaSyncKey(t giteaapi.Target, all bool) string {
	scope := "open"
	if all {
		scope = "all"
	}
	return "gitea:" + t.String() + ":" + scope
}

func giteaSyncKeys(t giteaapi.Target, all bool) []string {
	if all {
		return []string{giteaSyncKey(t, true), giteaSyncKey(t, false)}
	}
	return []string{giteaSyncKey(t, false)}
}

// importErrorGitea says which credential was used, so a 404 on a private
// repository reads as an authorization problem rather than a typo.
func importErrorGitea(target giteaapi.Target, source giteaapi.TokenSource, err error) error {
	e, ok := err.(*giteaapi.Error)
	if !ok {
		return err
	}
	switch {
	case source == giteaapi.FromNowhere:
		return fmt.Errorf("%s: %w\n       no Gitea token found; set GITEA_TOKEN, or store one with git credential", target, e)
	case e.Unauthorized():
		return fmt.Errorf("%s: %w\n       using the token from %s", target, e, source)
	default:
		return fmt.Errorf("%s: %w", target, e)
	}
}

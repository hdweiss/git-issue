package main

import (
	"fmt"
	"os"
	"time"

	ghapi "github.com/hdweiss/git-issue/internal/bridge/github/api"
	ghissue "github.com/hdweiss/git-issue/internal/bridge/github/issue"
	"github.com/hdweiss/git-issue/internal/cli"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/origins"
	"github.com/hdweiss/git-issue/internal/remote"
)

// pullGitHub imports issues from GitHub's API.
//
// The shape mirrors the git path deliberately: fetch, union into the notes
// ref, then report what moved in the columns `list` uses. What differs is only
// where the events come from — which is the point of the bridge boundary.
func pullGitHub(s *entity.Store, spec remote.Spec, opts pullOptions) error {
	target, err := ghapi.ParseTarget(spec.URL)
	if err != nil {
		return fmt.Errorf("%s: %w", spec.Target, err)
	}

	token, source, cred := ghapi.Token(s.Repo, target.Host, opts.token)
	client := ghapi.New(target.Endpoint(), token)

	// Open issues only unless asked otherwise. The consequence is worth
	// knowing: an issue closed upstream since the last import is not in the
	// result set, so its local copy keeps saying open until an --all run — a
	// filter cannot report the absence of what it filtered out.
	filter := ghapi.Filter{States: []string{ghapi.StateOpen}, Since: opts.since, Limit: opts.limit}
	scope := "open issues"
	if opts.all {
		filter.States, scope = nil, "all issues"
	}

	state, err := s.SyncState()
	if err != nil {
		return err
	}
	// A date the caller asked for wins, and does not disturb the watermark:
	// --since is a question, not a checkpoint. --full asks for everything, and
	// does move the watermark, because it genuinely read everything.
	if !opts.sinceGiven && !opts.full {
		filter.Since = state.Since(syncKey(target, opts.all))
	}

	fmt.Fprintf(os.Stderr, "Importing %s from %s%s\n", scope, target, sinceNote(filter.Since))
	prog := cli.Progress{W: os.Stderr, Noun: "issues"}
	issues, err := client.Fetch(target, filter, prog.Update)
	prog.Done()
	if err != nil {
		return importError(target, source, err)
	}
	// The credential worked, so let git's helper record it — that is the other
	// half of the contract `git credential fill` starts, and the only place
	// this program has any say in a token being stored.
	if cred != nil {
		s.Repo.CredentialApprove(*cred)
	}

	// The ledger answers two questions the blob cannot: which local entity an
	// upstream issue is already filed as, and which upstream comments are
	// already here as entries this repository posted. Without the first, an
	// issue created by a push comes back as a second entity; without the second,
	// a comment created by a push comes back as a second comment.
	led := origins.NewStore(s.Repo)
	ledger, err := led.Load(target.String())
	if err != nil {
		return err
	}
	for _, w := range ledger.Warnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}

	// A batch is filed first and imported second.
	//
	// A relation names a local entity, so a link can only be written once the
	// ledger holds the issue it points at — and for a target further down the
	// same page, it does not yet. The first pass establishes where every issue
	// in the batch is filed; the second imports the ones that had a link they
	// could not write, which costs no requests because the responses are
	// already here. Without it a link between two issues that arrived together
	// would wait for a later run, and with a `--since` watermark that run may
	// never re-read either of them.
	//
	// Only the issue mappings are filed in the first pass. Recording a comment
	// there would make the second import treat it as one this clone posted, and
	// skip it.
	imported := make([]ghissue.Entity, len(issues))
	ids := make([]string, len(issues))
	for i, gh := range issues {
		e, err := ghissue.Import(s.Format(), gh, ledger)
		if err != nil {
			return fmt.Errorf("importing %s: %w", gh.URL, err)
		}
		// An entity the ledger already names is filed under the id it has here,
		// not under the one the import derived. The blob then carries two create
		// events, and the fold keeps the first by (c, id) — a locally created
		// issue's create carries c = 1 against a bridged one's Unix timestamp,
		// so the note key and the folded create agree deterministically.
		id := e.ID
		if claimed, ok := ledger.Entity(e.Origin); ok {
			id = claimed
		}
		imported[i], ids[i] = e, id
		recordIssue(ledger, id, e)
	}

	incoming := make([]entity.Incoming, 0, len(issues))
	for i, gh := range issues {
		e := imported[i]
		if e.Unresolved > 0 {
			again, err := ghissue.Import(s.Format(), gh, ledger)
			if err != nil {
				return fmt.Errorf("importing %s: %w", gh.URL, err)
			}
			e = again
		}
		incoming = append(incoming, entity.Incoming{ID: ids[i], Actions: e.Actions})
		record(ledger, ids[i], e)
	}

	pull, err := s.Apply(target.String(), incoming)
	if err != nil {
		return err
	}

	// After the events are safely on the ref, for the same reason the watermark
	// waits: a mapping recorded for a write that failed would tell the next run
	// that an issue is upstream when it is not.
	if err := led.Save(ledger, ledgerMessage(target.String(), len(issues))); err != nil {
		return err
	}

	// Only after the events are safely on the ref. A watermark advanced before
	// the write would skip, on the next run, exactly the issues the failed
	// write lost.
	if !opts.sinceGiven {
		if err := state.Record(watermark(issues), syncKeys(target, opts.all)...); err != nil {
			return err
		}
	}
	if err := report(pull, fmt.Sprintf("From %s", target), uniqueIDs(s)); err != nil {
		return err
	}
	cli.Repack(s.Repo, pull.Commits, os.Stderr)
	return nil
}

// record files everything an import established about where an entity lives
// upstream. Adding a mapping that is already there changes nothing, so a
// re-import leaves the ledger — and its ref — exactly where it was.
func record(ledger *origins.Ledger, id string, e ghissue.Entity) {
	recordIssue(ledger, id, e)
	for _, c := range e.Comments {
		ledger.Add(origins.KindComment, id, c.EventID, c.Upstream)
	}
}

// recordIssue files where the issue itself lives, and nothing about its
// comments — which is what the first of the two import passes may safely say.
func recordIssue(ledger *origins.Ledger, id string, e ghissue.Entity) {
	ledger.Add(origins.KindIssue, id, e.Origin)
	if e.URL != "" {
		ledger.Add(origins.KindURL, id, e.URL)
	}
}

// ledgerMessage is what a ledger commit says. It goes on refs/git-issue/origins
// and never on the notes ref, so `git issue log` stays a history of the tracker
// rather than of its bookkeeping.
func ledgerMessage(tracker string, n int) string {
	return fmt.Sprintf("Record %s from %s\n\nWhere each one lives upstream, so a later import converges on the\nissue already here instead of filing a second one.\n\nTracker: %s\n",
		plural(n, "issue"), tracker, tracker)
}

// watermark is the newest updatedAt among the issues just imported.
//
// Safe to advance to even when --limit truncated the run. Issues arrive
// ordered by updatedAt ascending, so a truncated result is a prefix: every
// issue left unread has an updatedAt at or after this one, and the next run
// asks for exactly them. `since` is inclusive, so the boundary issue is re-read
// rather than lost, and re-reading an entity adds nothing.
func watermark(issues []ghapi.Issue) time.Time {
	var newest time.Time
	for _, gh := range issues {
		if gh.UpdatedAt.After(newest) {
			newest = gh.UpdatedAt
		}
	}
	return newest
}

// syncKey names one repository at one scope. The scope is part of the key
// because the two windows are not the same window: an open-only run has read
// every *open* issue up to its watermark and says nothing about closed ones.
func syncKey(t ghapi.Target, all bool) string {
	scope := "open"
	if all {
		scope = "all"
	}
	return "github:" + t.String() + ":" + scope
}

// syncKeys are the watermarks one run may advance.
//
// An --all run has necessarily seen every open issue too, so it advances both.
// The reverse is not true, which is why an open-only run advances only its own
// — otherwise a later --all would start from a point it had never reached and
// silently skip every issue closed before it.
func syncKeys(t ghapi.Target, all bool) []string {
	if all {
		return []string{syncKey(t, true), syncKey(t, false)}
	}
	return []string{syncKey(t, false)}
}

func sinceNote(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	return ", updated since " + since.Local().Format("2006-01-02 15:04")
}

// importError says which credential was used, so that a 404 on a private
// repository reads as an authorization problem rather than a typo.
//
// GitHub answers "you may not see this" with 404 rather than 403 for private
// repositories, so the token in play is the single most useful thing to name.
func importError(target ghapi.Target, source ghapi.TokenSource, err error) error {
	e, ok := err.(*ghapi.Error)
	if !ok {
		return err
	}
	switch {
	case source == ghapi.FromNowhere:
		return fmt.Errorf("%s: %w\n       no GitHub token found; set GITHUB_TOKEN, or run: gh auth login", target, e)
	case e.Unauthorized() || len(e.Messages) > 0:
		return fmt.Errorf("%s: %w\n       using the token from %s", target, e, source)
	default:
		return fmt.Errorf("%s: %w", target, e)
	}
}

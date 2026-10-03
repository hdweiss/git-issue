package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	ghapi "github.com/hdweiss/git-issue/internal/bridge/github/api"
	ghreview "github.com/hdweiss/git-issue/internal/bridge/github/review"
	"github.com/hdweiss/git-issue/internal/cli"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/origins"
	"github.com/hdweiss/git-issue/internal/remote"
	"github.com/hdweiss/git-issue/internal/render"
	"github.com/hdweiss/git-issue/internal/review"
)

const pullUsage = `usage: git review pull [--all] [--full] [--since <date>] [--limit <n>]
                       [--token <token>] [[git:|github:|ado:]<remote>]

       (no remote)          the current branch's own remote: always its notes
                             ref, and its pull requests too if it is GitHub or
                             Azure DevOps
       origin, git:origin   another copy of the notes refs
       github:origin        GitHub's API, via the bridge
       github:owner/name    a repository this clone has no remote for
       ado:origin           Azure DevOps' API, via the bridge
       ado:<clone-url>      a repository this clone has no remote for; the URL
                             must name the repo (.../_git/<repo>)

       --all                closed and merged pull requests too
       --full               re-read everything, ignoring the watermark
       --since <date>       only what changed at or after this date
       --limit <n>          stop after this many
       --token <token>      the GitHub or Azure DevOps token to use

An import reads open pull requests by default. Most repositories carry far more
merged ones than live ones, and a first sync that read a decade of them would be
the slowest thing you ever did with this. --all lifts the filter, and asking for
closed ones always includes the merged ones — a filter that omitted them would
omit the majority of what closed, which is never what anybody means.

Each scope keeps its own watermark, so --all resumes from where the last --all
finished rather than from where the last open-only pull did.

A bridge import also writes the head commit's check runs to
refs/notes/checks/runs, keyed by commit. They are not part of any review's blob:
a check is a fact about a commit.`

type pullOptions struct {
	all        bool
	full       bool
	since      time.Time
	sinceGiven bool
	limit      int
	token      string
}

func cmdPull(args []string) error {
	opts, target, err := parsePullFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(pullUsage)
		return nil
	}
	if err != nil {
		return err
	}

	s, err := open()
	if err != nil {
		return err
	}
	spec := remote.Parse(s.Repo, target)

	switch spec.Scheme {
	case remote.SchemeGitHub:
		return pullGitHub(s, spec, opts)
	case remote.SchemeADO:
		return pullADO(s, spec, opts)
	default:
		// A bare target is plain git, and a bare *invocation* also asks the
		// bridge when the remote it resolved to turns out to be a forge — the
		// same rule git-issue follows, and the reason an unqualified pull can
		// never be the one that prompts for a token unless the remote is one.
		return pullDefault(s, spec, target, opts)
	}
}

// pullDefault is the git leg, plus the bridge leg when the remote resolved to a
// forge and nobody named a scheme.
//
// A forge remote is rarely pushed a notes ref at all — that is what the bridge
// exists for — so finding none there is not a failure on its own. It stops being
// swallowed once there is no bridge leg left to redeem the pull, or once the git
// leg failed on something else entirely: no such remote, a network problem, an
// auth problem.
func pullDefault(s *entity.Store, spec remote.Spec, target string, opts pullOptions) error {
	// At most one bridge can be the remote's, since no URL is both.
	scheme := ""
	if target == "" {
		switch {
		case spec.LooksLikeGitHub():
			scheme = remote.SchemeGitHub
		case spec.LooksLikeADO():
			scheme = remote.SchemeADO
		}
	}

	err := pullGit(s, spec)
	empty := errors.Is(err, entity.ErrNoRemoteRef)
	if err != nil && (scheme == "" || !empty) {
		// Dressed here rather than in pullGit, so that the sentinel is still
		// recognisable above and only the error somebody will actually read
		// gets rewritten.
		return pullGitError(spec, s.FullRef(), err)
	}
	if scheme == "" {
		return nil
	}
	if !empty {
		// The git leg printed something worth reading; separate it from the
		// bridge leg's own report rather than running the two together.
		fmt.Fprintln(os.Stderr)
	}
	if scheme == remote.SchemeADO {
		return pullADO(s, spec, opts)
	}
	return pullGitHub(s, spec, opts)
}

// pullGit fetches the remote's copy of both refs and unions them into the local
// ones.
//
// The checks ref travels with the reviews. It is not core state — a clone
// without it folds complete reviews — but it is useful to share, and fetching
// it costs one refspec.
func pullGit(s *entity.Store, spec remote.Spec) error {
	if !spec.IsGitRemote() {
		return fmt.Errorf("no remote named '%s'; git review pull needs a configured remote", spec.Target)
	}

	// The ledger first, for the reason it is pushed first: reviews that arrive
	// without their mappings will be re-created upstream by this clone's next
	// bridge push, while mappings for reviews not yet here are simply missed.
	ledgerErr := origins.NewStore(s.Repo).Pull(spec.Name)

	pull, err := s.PullGit(spec.Name)
	if err != nil {
		return err
	}
	if ledgerErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not fetch the origin ledger: %s\n", ledgerErr)
	}

	// A checks ref the remote does not have is not a failure: it is optional
	// state, and a repository that records none is the ordinary case.
	if _, err := openChecks(s.Repo).PullGit(spec.Name); err != nil && !errors.Is(err, entity.ErrNoRemoteRef) {
		fmt.Fprintf(os.Stderr, "warning: could not fetch the checks ref: %s\n", err)
	}

	return syncReport(s).Report(pull, "From "+spec.URL, uniqueIDs(s))
}

// pullGitError explains a git leg that came back empty, and points at the bridge
// when the remote is a forge.
//
// A forge remote almost never carries a notes ref — that is what the bridge is
// for — so somebody who reached for a plain `pull origin` on one needs to be
// told the reviews are not in git refs at all, rather than shown a bare "remote
// has no copy of this ref" and left to work out which ref and why.
func pullGitError(spec remote.Spec, ref string, err error) error {
	hint := ""
	if spec.LooksLikeGitHub() {
		hint = fmt.Sprintf("\n       pull requests on GitHub are read through the bridge: git review pull github:%s", spec.Target)
	}
	if spec.LooksLikeADO() {
		hint = fmt.Sprintf("\n       pull requests on Azure DevOps are read through the bridge: git review pull ado:%s", spec.Target)
	}
	if errors.Is(err, entity.ErrNoRemoteRef) {
		return fmt.Errorf("%s has no %s; nothing has been pushed there yet%s", spec.Target, ref, hint)
	}
	return fmt.Errorf("%s%s", err, hint)
}

// syncReport is how this command reports a pull or a push.
func syncReport(s *entity.Store) cli.Sync {
	runs := loadAllChecks(s)
	return cli.Sync{
		Noun: "review",
		Row: func(id string, st entity.State) render.Row {
			return review.Row(id, st, review.Summarize(runs[review.Head(st)]))
		},
	}
}

// pullGitHub imports pull requests from GitHub's API.
func pullGitHub(s *entity.Store, spec remote.Spec, opts pullOptions) error {
	target, err := ghapi.ParseTarget(spec.URL)
	if err != nil {
		return fmt.Errorf("%s: %w", spec.Target, err)
	}

	token, source, cred := ghapi.Token(s.Repo, target.Host, opts.token)
	client := ghapi.New(target.Endpoint(), token)

	filter := ghapi.Filter{States: []string{ghapi.StateOpen}, Since: opts.since, Limit: opts.limit}
	scope := "open pull requests"
	if opts.all {
		filter.States, scope = nil, "all pull requests"
	}

	state, err := s.SyncState()
	if err != nil {
		return err
	}
	// A date the caller asked for wins and does not disturb the watermark:
	// --since is a question, not a checkpoint. --full asks for everything and
	// does move it, because it genuinely read everything.
	if !opts.sinceGiven && !opts.full {
		filter.Since = state.Since(syncKey(target, opts.all))
	}

	fmt.Fprintf(os.Stderr, "Importing %s from %s%s\n", scope, target, sinceNote(filter.Since))
	prog := cli.Progress{W: os.Stderr, Noun: "pull requests"}
	// A repository whose pages GitHub will not finish is read in smaller ones,
	// which is slower and worth saying rather than leaving as an unexplained
	// pause.
	client.Notice = func(s string) { prog.Note("note: " + s) }
	pulls, err := client.FetchPulls(target, filter, prog.Update)
	prog.Done()
	if err != nil {
		return importError(target, source, err)
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

	// A batch is filed first and imported second, the same two passes the issue
	// bridge makes and for the same reason: a `closes` link names a local
	// entity, so it can only be written once the ledger holds what it points at.
	// The second pass costs no requests, because the responses are already here.
	//
	// Only the review mappings are filed in the first pass. Recording a comment
	// there would make the second import treat it as one this clone posted, and
	// skip it.
	imported := make([]ghreview.Entity, len(pulls))
	ids := make([]string, len(pulls))
	for i, pr := range pulls {
		e, err := ghreview.Import(s.Format(), pr, ledger)
		if err != nil {
			return fmt.Errorf("importing %s: %w", pr.URL, err)
		}
		// An entity the ledger already names is filed under the id it has here,
		// not under the one the import derived.
		id := e.ID
		if claimed, ok := ledger.Entity(e.Origin); ok {
			id = claimed
		}
		imported[i], ids[i] = e, id
		recordReview(ledger, id, e)
	}

	incoming := make([]entity.Incoming, 0, len(pulls))
	var checks []ghreview.Check
	for i, pr := range pulls {
		e := imported[i]
		if e.Unresolved > 0 {
			again, err := ghreview.Import(s.Format(), pr, ledger)
			if err != nil {
				return fmt.Errorf("importing %s: %w", pr.URL, err)
			}
			e = again
		}
		incoming = append(incoming, entity.Incoming{ID: ids[i], Actions: e.Actions})
		record(ledger, ids[i], e)
		checks = append(checks, e.Checks...)
	}

	pull, err := s.Apply(target.String(), incoming)
	if err != nil {
		return err
	}

	// The checks go to their own ref, in their own commits. A failure there is
	// reported and swallowed: the reviews are already safely written, and
	// checks are re-fetchable state that nothing else depends on.
	if n, err := writeChecks(s, checks); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record check runs: %s\n", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "Recorded %s on %s\n",
			cli.Plural(n, "check run"), cli.Plural(commitsOf(checks), "commit"))
	}

	// After the events are safely on the ref: a mapping recorded for a write
	// that failed would tell the next run that a review is upstream when it is
	// not.
	if err := led.Save(ledger, ledgerMessage(target.String(), len(pulls))); err != nil {
		return err
	}
	if !opts.sinceGiven {
		if err := state.Record(watermark(pulls), syncKeys(target, opts.all)...); err != nil {
			return err
		}
	}
	if err := syncReport(s).Report(pull, fmt.Sprintf("From %s", target), uniqueIDs(s)); err != nil {
		return err
	}
	cli.Repack(s.Repo, pull.Commits, os.Stderr)
	return nil
}

// writeChecks records every imported run on the checks ref, grouped by the
// commit it belongs to, and reports how many it wrote.
//
// Grouped because the ref is keyed by commit: one write per commit rather than
// one per run means a commit's whole rollup lands in one action, which is what
// it was.
func writeChecks(s *entity.Store, checks []review.CommitCheck) (int, error) {
	if len(checks) == 0 {
		return 0, nil
	}
	store := openChecks(s.Repo)

	byCommit := map[string][]review.Check{}
	at := map[string]time.Time{}
	who := map[string]string{}
	var order []string
	for _, c := range checks {
		if _, seen := byCommit[c.Commit]; !seen {
			order = append(order, c.Commit)
		}
		byCommit[c.Commit] = append(byCommit[c.Commit], c.Run)
		if c.At.After(at[c.Commit]) {
			at[c.Commit] = c.At
		}
		who[c.Commit] = c.Author
	}

	n := 0
	for _, commit := range order {
		wrote, err := review.WriteChecks(store, who[commit], at[commit].Unix(), commit, byCommit[commit])
		if err != nil {
			return n, err
		}
		if wrote {
			n += len(byCommit[commit])
		}
	}
	return n, nil
}

func commitsOf(checks []review.CommitCheck) int {
	seen := map[string]bool{}
	for _, c := range checks {
		seen[c.Commit] = true
	}
	return len(seen)
}

// record files everything an import established about where an entity lives
// upstream. Adding a mapping that is already there changes nothing, so a
// re-import leaves the ledger — and its ref — exactly where it was.
func record(ledger *origins.Ledger, id string, e ghreview.Entity) {
	recordReview(ledger, id, e)
	for _, c := range e.Comments {
		ledger.Add(origins.KindComment, id, c.EventID, c.Upstream)
	}
	for _, t := range e.Threads {
		ledger.Add(origins.KindThread, id, t.EventID, t.Upstream)
	}
}

// recordReview files where the review itself lives, and nothing about its
// comments — which is what the first of the two import passes may safely say.
func recordReview(ledger *origins.Ledger, id string, e ghreview.Entity) {
	ledger.Add(origins.KindReview, id, e.Origin)
	if e.URL != "" {
		ledger.Add(origins.KindURL, id, e.URL)
	}
}

func ledgerMessage(tracker string, n int) string {
	return fmt.Sprintf("Record %s from %s\n\nWhere each one lives upstream, so a later import converges on the\nreview already here instead of filing a second one.\n\nTracker: %s\n",
		cli.Plural(n, "review"), tracker, tracker)
}

func watermark(pulls []ghapi.PullRequest) time.Time {
	var newest time.Time
	for _, pr := range pulls {
		if pr.UpdatedAt.After(newest) {
			newest = pr.UpdatedAt
		}
	}
	return newest
}

// syncKey names one repository at one scope. The scope is part of the key
// because the two windows are not the same window: an open-only run has read
// every *open* pull request up to its watermark and says nothing about the
// rest.
// syncScheme prefixes every watermark this command keeps, which is what makes
// the reviews' share of a shared file addressable: `destroy` forgets these and
// leaves the issue bridge's own alone.
const syncScheme = "github-reviews:"

func syncKey(t ghapi.Target, all bool) string {
	scope := "open"
	if all {
		scope = "all"
	}
	return syncScheme + t.String() + ":" + scope
}

// syncKeys are the watermarks one run may advance. An --all run has necessarily
// seen every open pull request too, so it advances both; the reverse is not
// true.
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
func importError(target ghapi.Target, source ghapi.TokenSource, err error) error {
	e, ok := err.(*ghapi.Error)
	if !ok {
		return err
	}
	switch {
	case source == ghapi.FromNowhere:
		return fmt.Errorf("%s: %w\n       no GitHub token found; set GITHUB_TOKEN, or run: gh auth login", target, e)
	// By the time this reaches here the import has already retried and asked for
	// one pull request at a time, so there is nothing left for it to shrink.
	// What is left is asking for less of the repository, or trying later.
	case e.TooHeavy():
		return fmt.Errorf("%s: %w\n       even one pull request a page was too much; try again, or narrow the import with --since or --limit", target, e)
	case e.Unauthorized() || len(e.Messages) > 0:
		return fmt.Errorf("%s: %w\n       using the token from %s", target, e, source)
	default:
		return fmt.Errorf("%s: %w", target, e)
	}
}

func parsePullFlags(args []string) (pullOptions, string, error) {
	var (
		opts  pullOptions
		since string
	)
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.all, "all", false, "closed and merged pull requests too")
	fs.BoolVar(&opts.full, "full", false, "re-read everything, ignoring the watermark")
	fs.StringVar(&since, "since", "", "only what changed at or after this date")
	fs.IntVar(&opts.limit, "limit", 0, "stop after this many")
	fs.StringVar(&opts.token, "token", "", "the GitHub token to use")

	rest, err := parsePermuted(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, "", err
		}
		return opts, "", fmt.Errorf("%s\n%s", err, pullUsage)
	}
	if len(rest) > 1 {
		return opts, "", fmt.Errorf("pull takes one remote, got '%s' as well\n%s", rest[1], pullUsage)
	}

	if since != "" {
		at, err := parseDate(since)
		if err != nil {
			return opts, "", err
		}
		opts.since, opts.sinceGiven = at, true
	}
	if opts.full && opts.sinceGiven {
		return opts, "", fmt.Errorf("--full re-reads everything and --since asks for a window; use one")
	}

	target := ""
	if len(rest) == 1 {
		target = rest[0]
	}
	return opts, target, nil
}

func parseDate(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if at, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return at, nil
		}
	}
	return time.Time{}, fmt.Errorf("could not read '%s' as a date; try 2006-01-02", s)
}

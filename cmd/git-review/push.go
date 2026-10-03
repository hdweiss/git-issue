package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	ghapi "github.com/hdweiss/git-issue/internal/bridge/github/api"
	ghreview "github.com/hdweiss/git-issue/internal/bridge/github/review"
	"github.com/hdweiss/git-issue/internal/cli"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/origins"
	"github.com/hdweiss/git-issue/internal/remote"
	"github.com/hdweiss/git-issue/internal/render"
	"github.com/hdweiss/git-issue/internal/review"
)

const pushUsage = `usage: git review push [--dry-run] [-y] [--refresh] [--token <token>]
                       [[git:|github:|ado:]<remote>] [<id>…]

       (no remote)          the current branch's own remote: its refs, and its
                             pull requests too if it is GitHub
       origin, git:origin   send the notes refs and the origin ledger to another clone
       github:origin        write local changes back through the GitHub bridge
       github:owner/name    a repository this clone has no remote for — a fork
       ado:origin           write local changes back through the Azure DevOps bridge
       ado:<repo url>       an Azure DevOps repository named by its /_git/<repo> URL

       <id>…                push only these reviews
       --dry-run            print what would be pushed and stop
   -y, --yes                do not ask before writing to a bridge
       --refresh            re-import what was pushed, to see it as upstream now has it
       --token <token>      the forge token to use

The two legs write to different places. 'push github:origin' writes to the forge
and records what it created in the local origin ledger; 'push origin' is what
shares the notes refs and that ledger with other clones.

A bridge push asks before it writes anything, and prints the plan first. Nothing
is forced: a remote holding reviews this clone has not seen is refused, and the
fix is 'git review pull' then push again — both refs are grow-only sets of lines,
so merging never discards anyone's write.

The head branch is never pushed as a side effect. A review this clone opened can
only be filed upstream once its branch is there, which is an ordinary
'git push <remote> <branch>' and stays the caller's to run.`

// cmdPush writes reviews outward.
//
// The target grammar is pull's, deliberately: the same word names the same place
// in both directions. What differs is that this one writes, which is why it
// confirms and pull does not.
func cmdPush(args []string) error {
	opts, rest, err := parsePushFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(pushUsage)
		return nil
	}
	if err != nil {
		return err
	}

	// A target is a word that names a remote or a scheme; anything else is a
	// review id. Only the first positional can be a target, so an id in that
	// position is told apart by not resolving as one.
	target := ""
	if len(rest) > 0 && looksLikeTarget(rest[0]) {
		target, rest = rest[0], rest[1:]
	}
	opts.ids = rest

	s, err := open()
	if err != nil {
		return err
	}
	spec := remote.Parse(s.Repo, target)

	switch spec.Scheme {
	case remote.SchemeGitHub:
		return pushGitHub(s, spec, opts)
	case remote.SchemeADO:
		return pushADO(s, spec, opts)
	default:
		if err := pushGit(s, spec, opts); err != nil {
			// A forge remote is rarely sent a notes ref at all — that is what the
			// bridge is for — so a refusal there is not fatal while there is still
			// a bridge leg to redeem the push.
			if target != "" || !spec.LooksLikeGitHub() {
				return err
			}
			fmt.Fprintf(os.Stderr, "git-review: %s\n", err)
		}
		if target == "" && spec.LooksLikeGitHub() {
			fmt.Println()
			bridged := spec
			bridged.Scheme = remote.SchemeGitHub
			return pushGitHub(s, bridged, opts)
		}
		return nil
	}
}

type pushOptions struct {
	dryRun  bool
	yes     bool
	refresh bool
	token   string
	ids     []string
}

func parsePushFlags(args []string) (pushOptions, []string, error) {
	var opts pushOptions
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.dryRun, "dry-run", false, "print what would be pushed and stop")
	fs.BoolVar(&opts.yes, "yes", false, "do not ask before writing to a bridge")
	fs.BoolVar(&opts.yes, "y", false, "do not ask before writing to a bridge")
	fs.BoolVar(&opts.refresh, "refresh", false, "re-import what was pushed")
	fs.StringVar(&opts.token, "token", "", "the GitHub token to use")

	rest, err := parsePermuted(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, nil, err
		}
		return opts, nil, fmt.Errorf("%s\n%s", err, pushUsage)
	}
	return opts, rest, nil
}

// looksLikeTarget distinguishes `push origin` from `push 4f2a1c9`.
//
// A scheme is decisive. Otherwise the word is a target only if it is not a
// plausible object-name prefix, so an id can never be mistaken for a remote that
// does not exist — and a remote actually named in hex is still recognised,
// because it resolves.
func looksLikeTarget(arg string) bool {
	if scheme, _, ok := strings.Cut(arg, ":"); ok {
		for _, known := range remote.Schemes {
			if scheme == known {
				return true
			}
		}
	}
	return !looksLikeID(arg)
}

// pushGit sends the tracker's refs to another clone: the origin ledger, the
// reviews, and the checks that go with them.
func pushGit(s *entity.Store, spec remote.Spec, opts pushOptions) error {
	// An id narrows which reviews a bridge writes. A git push sends a ref,
	// whole; accepting the argument silently would imply it had narrowed
	// something.
	if len(opts.ids) > 0 {
		return fmt.Errorf("naming reviews applies to a bridge push; a git push sends %s entire", s.FullRef())
	}
	if !spec.IsGitRemote() {
		return fmt.Errorf("no remote named '%s'; git review push needs a configured remote", spec.Target)
	}
	if opts.dryRun {
		fmt.Printf("Would push %s and %s to %s\n", s.FullRef(), origins.Ref, spec.Name)
		return nil
	}

	// The ledger goes first, and its failure is fatal rather than a warning: a
	// clone that receives reviews without their mappings will re-create every
	// one of them upstream on its next bridge push, and nothing undoes that.
	//
	// A repository that has never talked to a bridge has no ledger at all, which
	// is the ordinary state of one that only reads local reviews — so there is
	// nothing to send rather than something to fail over.
	led := origins.NewStore(s.Repo)
	if s.Repo.RefSHA(led.Ref) != "" {
		// Merged before it is sent, because the ledger is a union of lines and a
		// push that had not seen the remote's would be refused as a
		// non-fast-forward with nothing actually in conflict.
		if err := led.Pull(spec.Name); err != nil {
			return fmt.Errorf("merging the origin ledger from %s: %w", spec.Name, err)
		}
		if err := s.Repo.Push(spec.Name, led.Refspec()); err != nil {
			return fmt.Errorf("pushing the origin ledger to %s: %w", spec.Name, err)
		}
	}

	push, err := s.PushGit(spec.Name)
	if err != nil {
		if errors.Is(err, entity.ErrNonFastForward) {
			return fmt.Errorf("%s has reviews this clone has not seen; run 'git review pull %s' first", spec.Name, spec.Name)
		}
		// A forge remote will usually refuse the notes ref outright, and the
		// useful thing to say is that reviews go there through the bridge
		// rather than through a ref push.
		if spec.LooksLikeGitHub() {
			return fmt.Errorf("%s\n       pull requests on GitHub are written through the bridge: git review push github:%s", err, spec.Target)
		}
		return err
	}
	// The checks ref goes too, when there is one. Optional state, so a failure
	// is a warning: the reviews are what the push was for.
	if _, err := openChecks(s.Repo).PushGit(spec.Name); err != nil && !errors.Is(err, entity.ErrNoRemoteRef) {
		fmt.Fprintf(os.Stderr, "warning: could not push the checks ref: %s\n", err)
	}
	return syncReport(s).Report(push, "To "+spec.URL, uniqueIDs(s))
}

// pushGitHub writes local changes back through the GitHub bridge. It reads the
// tracker and builds the pusher; runBridgePush does the rest.
func pushGitHub(s *entity.Store, spec remote.Spec, opts pushOptions) error {
	target, err := ghapi.ParseTarget(spec.URL)
	if err != nil {
		return fmt.Errorf("%s: %w", spec.Target, err)
	}
	tracker := target.String()

	led, ledger, journal, err := recoverLedger(s, tracker)
	if err != nil {
		return err
	}

	candidates, err := collect(s, ledger, opts.ids)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		fmt.Println("Everything up-to-date")
		return nil
	}

	token, _, _ := ghapi.Token(s.Repo, target.Host, opts.token)
	pusher := ghreview.NewPusher(ghapi.New(target.Endpoint(), token), target, s.Format(), s.Vocab, ledger)

	return runBridgePush(s, tracker, led, ledger, journal, candidates, pusher, opts, func() error {
		return pullGitHub(s, spec, pullOptions{all: true, full: true, token: opts.token})
	})
}

// reviewPusher is the platform-specific half of a bridge push: everything else
// — the plan report, the confirmation, the journalling — is runBridgePush's.
type reviewPusher interface {
	Plan(candidates []review.Candidate) (review.Plan, error)
	Apply(c review.Candidate, d review.Delta) (review.Result, error)
}

// recoverLedger loads the origin ledger for one tracker and commits any mappings
// an interrupted run left in the journal — before anything else, or this run
// would create upstream duplicates of everything the last one already filed.
func recoverLedger(s *entity.Store, tracker string) (*origins.Store, *origins.Ledger, *origins.Journal, error) {
	led := origins.NewStore(s.Repo)
	ledger, err := led.Load(tracker)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, w := range ledger.Warnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}

	journal, err := led.OpenJournal(tracker)
	if err != nil {
		return nil, nil, nil, err
	}
	if !journal.Empty() {
		fmt.Fprintf(os.Stderr, "Recovering %s from an interrupted push\n", cli.Plural(len(journal.Lines()), "mapping"))
		journal.Into(ledger)
		if err := led.Save(ledger, recoveredMessage(tracker, len(journal.Lines()))); err != nil {
			return nil, nil, nil, err
		}
		journal.Done()
	}
	return led, ledger, journal, nil
}

// runBridgePush is the write-back order of operations, shared by both bridges.
// refresh re-imports what was pushed and is the caller's because only it knows
// the pull flags.
func runBridgePush(s *entity.Store, tracker string, led *origins.Store, ledger *origins.Ledger, journal *origins.Journal, candidates []review.Candidate, pusher reviewPusher, opts pushOptions, refresh func() error) error {
	fmt.Fprintf(os.Stderr, "Reading %s from %s\n", cli.Plural(len(candidates), "review"), tracker)
	plan, err := pusher.Plan(candidates)
	if err != nil {
		return err
	}

	if err := reportPlan(s, tracker, plan); err != nil {
		return err
	}
	if len(plan.Deltas) == 0 || opts.dryRun {
		return conflictExit(plan)
	}
	if !opts.yes {
		ok, err := confirm(fmt.Sprintf("\nPush %s to %s?", cli.Plural(len(plan.Deltas), "review"), tracker))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Nothing was pushed.")
			return nil
		}
	}

	pushed, applyErr := applyDeltas(pusher, candidates, plan, ledger, journal)

	// The ledger is written whatever happened. A mapping earned and then dropped
	// costs a duplicate pull request that nothing undoes, so it is committed
	// before any error is reported.
	if !journal.Empty() {
		if err := led.Save(ledger, pushedMessage(tracker, pushed)); err != nil {
			return err
		}
		journal.Done()
	}
	if applyErr != nil {
		return applyErr
	}

	fmt.Printf("%s pushed to %s\n", cli.Plural(pushed, "review"), tracker)
	if s.Repo.RefSHA(led.Ref) != "" {
		fmt.Printf("Run 'git review push %s' to share the origin ledger with other clones.\n", remote.Default)
	}
	if opts.refresh {
		return refresh()
	}
	return conflictExit(plan)
}

// collect gathers the reviews worth reading upstream state for.
//
// The prefilter is what keeps a push from fetching the whole tracker: a review
// whose every event came from this bridge has nothing local to say. Every
// locally written event takes its author from user.email, including one that
// arrived from another clone over the git leg, so this never skips a review that
// had something to push.
func collect(s *entity.Store, ledger *origins.Ledger, ids []string) ([]review.Candidate, error) {
	if len(ids) > 0 {
		var out []review.Candidate
		for _, want := range ids {
			id, st, err := s.Find(want, review.Type)
			if err != nil {
				return nil, err
			}
			upstream, _ := ledger.Upstream(id)
			out = append(out, review.Candidate{ID: id, Events: st.Events, State: st, Upstream: upstream})
		}
		return out, nil
	}

	notes, err := s.Notes()
	if err != nil {
		return nil, err
	}
	var out []review.Candidate
	err = s.Each(notes, func(id string, st entity.State) error {
		upstream, mapped := ledger.Upstream(id)
		if mapped && !hasLocalWrite(st) {
			return nil
		}
		out = append(out, review.Candidate{ID: id, Events: st.Events, State: st, Upstream: upstream})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Newest first, matching every other listing, so the plan lines up with the
	// `list` somebody just ran.
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// hasLocalWrite reports whether any event came from somewhere other than a
// bridge — a genuine local write, which is what a push has something to say
// about. An author with no scheme prefix is a user.email.
func hasLocalWrite(st entity.State) bool {
	for _, ev := range st.Events {
		if !strings.HasPrefix(ev.A, "github:") && !strings.HasPrefix(ev.A, "ado:") {
			return true
		}
	}
	return false
}

// applyDeltas sends each delta, journalling what it earns as it goes.
//
// One review at a time, and the journal is flushed to disk after every one. That
// is what makes seeding a fork resumable: interrupt it, run it again, and only
// the reviews with no mapping are filed.
func applyDeltas(pusher reviewPusher, candidates []review.Candidate, plan review.Plan, ledger *origins.Ledger, journal *origins.Journal) (int, error) {
	byID := map[string]review.Candidate{}
	for _, c := range candidates {
		byID[c.ID] = c
	}

	pushed := 0
	for _, d := range plan.Deltas {
		result, err := pusher.Apply(byID[d.ID], d)
		// Record before reporting: Apply returns whatever it managed to create
		// even when it then failed, and an unrecorded creation is the one loss
		// that cannot be undone.
		if jerr := journalResult(journal, ledger, result); jerr != nil {
			return pushed, jerr
		}
		if err != nil {
			return pushed, fmt.Errorf("%s: %w", short(d.ID), err)
		}
		pushed++
	}
	return pushed, nil
}

func journalResult(journal *origins.Journal, ledger *origins.Ledger, r review.Result) error {
	record := func(kind string, fields ...string) error {
		if err := journal.Append(kind, fields...); err != nil {
			return err
		}
		ledger.Add(kind, fields...)
		return nil
	}

	if r.Upstream != "" {
		if err := record(origins.KindReview, r.ID, r.Upstream); err != nil {
			return err
		}
	}
	if r.URL != "" {
		if err := record(origins.KindURL, r.ID, r.URL); err != nil {
			return err
		}
	}
	for _, set := range []struct {
		kind    string
		origins []review.CommentOrigin
	}{
		{origins.KindComment, r.Comments},
		{origins.KindThread, r.Threads},
		{origins.KindVerdict, r.Verdicts},
	} {
		for _, o := range set.origins {
			if err := record(set.kind, r.ID, o.EventID, o.Upstream); err != nil {
				return err
			}
		}
	}
	return nil
}

// reportPlan prints what the push intends to do, in a listing's own columns.
func reportPlan(s *entity.Store, tracker string, plan review.Plan) error {
	if len(plan.Deltas) == 0 && len(plan.Conflicts) == 0 && len(plan.Skipped) == 0 {
		fmt.Println("Everything up-to-date")
		return nil
	}

	fmt.Println("To " + tracker)

	conflicted := map[string][]string{}
	for _, c := range plan.Conflicts {
		conflicted[c.ID] = append(conflicted[c.ID], c.Field)
	}

	runs := loadAllChecks(s)
	row := func(id string) (render.Row, error) {
		_, st, err := s.Find(id, review.Type)
		if err != nil {
			return render.Row{}, err
		}
		return review.Row(id, st, review.Summarize(runs[review.Head(st)])), nil
	}

	var rows []render.Change
	notes := map[string]string{}
	for _, d := range plan.Deltas {
		status := byte(entity.Updated)
		note := strings.Join(d.Fields(), ", ")
		if d.Create {
			status, note = byte(entity.Added), "new pull request"
		}
		r, err := row(d.ID)
		if err != nil {
			return err
		}
		rows = append(rows, render.Change{Status: status, Row: r})
		notes[d.ID] = note
	}
	for id, fields := range conflicted {
		r, err := row(id)
		if err != nil {
			return err
		}
		rows = append(rows, render.Change{Status: '!', Row: r})
		notes[id] = strings.Join(fields, ", ") + " changed on both sides"
	}

	render.WriteChanges(os.Stdout, rows, uniqueIDs(s))
	for _, r := range rows {
		if note := notes[r.Row.ID]; note != "" {
			fmt.Printf("   %s  %s\n", short(r.Row.ID), note)
		}
	}
	for _, skip := range plan.Skipped {
		if skip.Field != "" {
			fmt.Printf("   %s  %s not pushed: %s\n", short(skip.ID), skip.Field, skip.Reason)
		} else {
			fmt.Printf("   %s  skipped: %s\n", short(skip.ID), skip.Reason)
		}
	}

	summary := []string{cli.Plural(len(plan.Deltas), "review") + " to push"}
	if n := len(conflicted); n > 0 {
		summary = append(summary, fmt.Sprintf("%d conflicted", n))
	}
	fmt.Println(strings.Join(summary, ", "))

	if len(conflicted) > 0 {
		fmt.Println("hint: pull, look at what the other side did, then edit:")
		fmt.Printf("hint:   git review pull github:%s\n", tracker)
	}
	return nil
}

// conflictExit makes a partly-refused push visible to a script. Everything that
// could be pushed was; the non-zero status is what says something was not.
func conflictExit(plan review.Plan) error {
	if len(plan.Conflicts) == 0 {
		return nil
	}
	return exitCode(1)
}

// confirm asks before an outward-facing write.
//
// Off a terminal there is nobody to ask, so an unattended run must say --yes
// rather than be assumed to mean it. A push creates things upstream that nothing
// here can take back.
func confirm(prompt string) (bool, error) {
	if !gitx.IsTerminal(os.Stdin) {
		return false, fmt.Errorf("not a terminal; re-run with --yes to push without asking")
	}
	fmt.Printf("%s [y/N] ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func recoveredMessage(tracker string, n int) string {
	return fmt.Sprintf("Recover %s for %s\n\nMappings earned by a push that did not finish recording them.\n\nTracker: %s\n",
		cli.Plural(n, "mapping"), tracker, tracker)
}

func pushedMessage(tracker string, n int) string {
	return fmt.Sprintf("Link %s to %s\n\nWhere each one now lives upstream, so a later import converges on the\nreview already here instead of filing a second one.\n\nTracker: %s\n",
		cli.Plural(n, "review"), tracker, tracker)
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

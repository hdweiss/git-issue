package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/cli"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/remote"
	"github.com/hdweiss/git-issue/internal/render"
	"github.com/hdweiss/git-issue/internal/review"
)

const statusUsage = `usage: git review status <id> [--approvals <n>] [--ignore-checks]
                         [--allow-unresolved] [--json]

Says what is standing between this review and a merge: checks that failed or are
still running, threads nobody has resolved, changes somebody requested,
approvals still wanted. Exits 0 when nothing is, and 1 when something is, so a
script can branch on it.

This is a report, not a gate. Nothing in a grow-only set can prevent a merge —
a verdict is a statement and a check is a machine's opinion — so what this says
is what branch protection would probably say, computed from what this clone
holds.

       --approvals <n>       how many non-stale approvals are wanted (default 0)
       --ignore-checks       do not read the checks ref at all
       --allow-unresolved    open threads do not count as blockers
       --json                machine-readable, for an agent's loop

--approvals defaults to none, because requiring one would report every review
nobody has been asked to read yet as blocked. A stale approval does not count
and is named as the reason when it is the thing missing.

Checks live on their own ref, keyed by commit, and are not core state: a clone
that has never fetched it reads no checks and therefore reports none failing.
That is the correct reading of what it holds, not a claim that the build passed.`

const checksUsage = `usage: git review checks [<id>] [--commit <sha>] [--json]
       git review checks [<id>] [--commit <sha>] --set <name>=<conclusion> [--url <url>]

The check runs recorded against a commit: a build, a quality gate, a scan.

       <id>                       the review whose head to read or write
       --commit <sha>             a commit directly, with no review involved
       --set <name>=<conclusion>  record a result; repeatable
       --url <url>                where to read about the run just set
       --json                     machine-readable

They live on refs/notes/checks/runs, keyed by commit sha rather than by review,
because a check result is a fact about a commit. It is true whichever review
contains that commit, it is shared by every branch that contains it, and it
outlives the review. That is also what makes rebases behave with no logic here:
an old commit keeps its old results, and a rewritten one is a different key with
none yet — nothing has to detect a force-push.

That keying is why --commit works with no review at all: 'git review checks
--commit HEAD --set build=fail' is a build script recording what it found, and
nothing about it needs a review to exist. 'git log --notes=checks/runs' then
shows the results against the commits themselves.

pass, fail, pending, skipped and cancelled are the conclusions this build
defines; the vocabulary is open and anything else is carried through as written.

A re-run supersedes the previous result of the same name, retracting it where
this clone can see it. A commit with no checks prints nothing and exits 0.`

const diffUsage = `usage: git review diff <id> [<git diff flag>…]

The change this review proposes, computed from its base and its revision.

Nothing stores a diff. The commits are already in the object store, so the diff
is git's own, run over base…revision — which is also why a diff here can never
go stale or disagree with the code.

Every git diff flag passes through: --stat, --name-only, -w, and so on.

It needs both commits in this repository. A review whose head has not been
fetched — a fork's, usually — says so; 'git review checkout <id>' fetches it.`

const checkoutUsage = `usage: git review checkout [<id> [<comment>]] [--branch <name>] [--detach]

Fetches the review's head and checks it out. With no id it checks out this
repository's default branch, which is the way back out of a review.

       --branch <name>    check out under this local name
       --detach           check out the commit itself, leaving no branch

A second id names a comment on that review and opens the file it is anchored to,
with the cursor on the line it sits on: the branch and the place to start
reading, in one gesture. A reply takes you to its thread's anchor, since that is
the place the conversation is about. The line is the one the anchor records, so
an anchor the head has moved past says so rather than being quietly relocated.

The editor is git's own — GIT_EDITOR, core.editor, VISUAL, EDITOR — asked for
the line with '+<line> <file>', the convention vi established and vim, nvim,
emacs, nano, kakoune and micro all still take. The few that spell it their own
way (VS Code, Sublime, Kate, Helix, Zed, the JetBrains editors) are recognised
by name. For anything else, or for a wrapper of your own:

       git config review.openCommand 'code -g %f:%l'

where %f is the path and %l the line.

The head is remote-qualified, but the qualifier only names a remote in the clone
that wrote it: a review imported from a forge spells it with the head
repository's owner, and an outside contributor's fork is exactly the repository
nobody here has a remote for. Three routes are tried in turn — a remote by that
name, a remote whose URL is that owner's repository, and the forge's own
refs/pull/<n>/head on the remote the review was imported from, which reaches a
fork's branch without adding the fork as a remote.`

func cmdStatus(args []string) error {
	var (
		req    review.Requirements
		asJSON bool
		fs     = flag.NewFlagSet("status", flag.ContinueOnError)
	)
	fs.SetOutput(io.Discard)
	fs.IntVar(&req.MinApprovals, "approvals", 0, "how many non-stale approvals are wanted")
	fs.BoolVar(&req.IgnoreChecks, "ignore-checks", false, "do not read the checks ref")
	fs.BoolVar(&req.AllowUnresolved, "allow-unresolved", false, "open threads do not block")
	fs.BoolVar(&asJSON, "json", false, "machine-readable output")

	targets, err := parseTargets(fs, "status", args, statusUsage, 1)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(statusUsage)
		return nil
	}
	if err != nil {
		return err
	}

	s, err := open()
	if err != nil {
		return err
	}
	id, st, err := s.Find(targets[0], review.Type)
	if err != nil {
		return err
	}

	var checks []review.Check
	if !req.IgnoreChecks {
		checks = checksFor(s.Repo, st)
	}
	report := review.StatusOf(id, st, checks, review.GitRepo{Repo: s.Repo}, req)

	if asJSON {
		if err := writeStatusJSON(os.Stdout, report); err != nil {
			return err
		}
	} else {
		writeStatus(os.Stdout, report)
	}

	// Exit 1 when something is in the way, so a shell can branch on it without
	// reading the output. Not an error message: the command did its job.
	if !report.Ready() {
		return exitCode(1)
	}
	return nil
}

func writeStatus(w io.Writer, r review.Report) {
	fmt.Fprintf(w, "review %s\n", render.Abbrev(r.ID))
	fmt.Fprintf(w, "    %s\n\n", r.Title)

	if branches := strings.TrimSpace(r.Base + " ← " + r.Head); branches != "←" {
		fmt.Fprintf(w, "  %-12s %s\n", "Branches", branches)
	}
	if r.Revision != "" {
		fmt.Fprintf(w, "  %-12s %s\n", "Revision", render.Abbrev(r.Revision))
	}
	fmt.Fprintf(w, "  %-12s %s\n", "Status", r.Status)
	if s := r.Summary.Plain(); s != "" {
		fmt.Fprintf(w, "  %-12s %s\n", "Checks", s)
	}
	fmt.Fprintf(w, "  %-12s %d approving, %d requesting changes\n", "Verdicts", len(r.Approvals), len(r.Changes))
	fmt.Fprintf(w, "  %-12s %d of %d resolved\n", "Threads", len(r.Resolvable)-len(r.Unresolved), len(r.Resolvable))

	if r.Ready() {
		fmt.Fprintf(w, "\nNothing is blocking this review.\n")
		return
	}

	fmt.Fprintf(w, "\nBlocking:\n")
	for _, b := range r.Blockers {
		fmt.Fprintf(w, "  - %s\n", b.Summary)
		for _, d := range b.Detail {
			fmt.Fprintf(w, "      %s\n", d)
		}
	}
}

// statusJSON is the shape an agent reads. Spelled out as its own type rather
// than marshalling review.Report directly, so the wire format is a decision
// made here and does not drift every time a field is added to the report.
type statusJSON struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Draft    bool   `json:"draft"`
	Base     string `json:"base,omitempty"`
	Head     string `json:"head,omitempty"`
	Revision string `json:"revision,omitempty"`
	Ready    bool   `json:"ready"`

	Checks []checkJSON `json:"checks"`
	// Verdicts are each person's current position, stale ones included and
	// marked. Dropping a stale verdict would tell an agent that nobody has an
	// opinion when somebody does.
	Verdicts []verdictJSON `json:"verdicts"`
	Threads  []threadJSON  `json:"threads"`
	Blockers []blockerJSON `json:"blockers"`
}

type checkJSON struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url,omitempty"`
}

type verdictJSON struct {
	Author   string `json:"author"`
	Value    string `json:"value"`
	Revision string `json:"revision,omitempty"`
	Stale    bool   `json:"stale"`
}

type threadJSON struct {
	ID       string `json:"id"`
	Author   string `json:"author"`
	Body     string `json:"body"`
	Resolved bool   `json:"resolved"`
	Replies  int    `json:"replies"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	EndLine  int    `json:"end_line,omitempty"`
	Revision string `json:"revision,omitempty"`
	// Currency is current, outdated, detached or unknown — the four distinct
	// answers, not a boolean, because outdated and detached call for different
	// things and reachability alone cannot tell them apart.
	Currency string `json:"currency,omitempty"`
}

type blockerJSON struct {
	Kind    string   `json:"kind"`
	Summary string   `json:"summary"`
	Detail  []string `json:"detail,omitempty"`
}

func writeStatusJSON(w io.Writer, r review.Report) error {
	out := statusJSON{
		ID:       r.ID,
		Title:    r.Title,
		Status:   r.Status,
		Draft:    r.Draft,
		Base:     r.Base,
		Head:     r.Head,
		Revision: r.Revision,
		Ready:    r.Ready(),
		Checks:   []checkJSON{},
		Verdicts: []verdictJSON{},
		Threads:  []threadJSON{},
		Blockers: []blockerJSON{},
	}
	for _, c := range r.Checks {
		out.Checks = append(out.Checks, checkJSON{Name: c.Name, Conclusion: c.Conclusion, URL: c.URL})
	}
	for _, v := range append(append([]review.Verdict{}, r.Approvals...), append(r.Changes, r.Stale...)...) {
		out.Verdicts = append(out.Verdicts, verdictJSON{
			Author: v.Author, Value: v.Value, Revision: v.Revision, Stale: v.Stale,
		})
	}
	for _, t := range r.Threads {
		j := threadJSON{
			ID:       t.Root.ID(),
			Author:   t.Root.Event.A,
			Body:     t.Root.Body.Display(),
			Resolved: t.Resolved,
			Replies:  t.Replies,
		}
		if t.Anchored {
			j.Path, j.Line, j.EndLine = t.Anchor.Path, t.Anchor.First, t.Anchor.Last
			j.Revision, j.Currency = t.Anchor.Revision, t.Currency.String()
		}
		out.Threads = append(out.Threads, j)
	}
	for _, b := range r.Blockers {
		out.Blockers = append(out.Blockers, blockerJSON{Kind: string(b.Kind), Summary: b.Summary, Detail: b.Detail})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func cmdChecks(args []string) error {
	var (
		asJSON bool
		commit string
		url    string
		set    listFlag
	)
	fs := flag.NewFlagSet("checks", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&asJSON, "json", false, "machine-readable output")
	fs.StringVar(&commit, "commit", "", "a commit to read or write directly")
	fs.StringVar(&url, "url", "", "where to read about the run being set")
	fs.Var(&set, "set", "record a result: <name>=<conclusion>; repeatable")

	rest, err := parsePermuted(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(checksUsage)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s\n%s", err, checksUsage)
	}
	if len(rest) > 1 {
		return fmt.Errorf("checks takes one review, got '%s' as well\n%s", rest[1], checksUsage)
	}
	if len(rest) == 0 && commit == "" {
		return fmt.Errorf("%s", checksUsage)
	}

	s, err := open()
	if err != nil {
		return err
	}

	// The commit is what a check is keyed by, so it is resolved before anything
	// else: a review is only ever a way of naming one.
	target := ""
	if len(rest) == 1 {
		_, st, err := s.Find(rest[0], review.Type)
		if err != nil {
			return err
		}
		target = review.Head(st)
		if target == "" && commit == "" {
			return fmt.Errorf("this review records no revision, so there is no commit to key checks by")
		}
	}
	if commit != "" {
		// Resolved through git, so 'HEAD' and a branch name work where a build
		// script is standing.
		resolved := s.Repo.RefSHA(commit)
		if resolved == "" {
			return fmt.Errorf("could not resolve '%s' to a commit", commit)
		}
		if target != "" && target != resolved {
			fmt.Fprintf(os.Stderr, "warning: --commit names %s, which is not this review's revision %s\n",
				render.Abbrev(resolved), render.Abbrev(target))
		}
		target = resolved
	}

	if len(set.values) > 0 {
		return recordChecks(s, target, set.values, url)
	}

	checks, err := review.LoadChecks(openChecks(s.Repo), target)
	if err != nil {
		return err
	}

	if asJSON {
		out := []checkJSON{}
		for _, c := range checks {
			out = append(out, checkJSON{Name: c.Name, Conclusion: c.Conclusion, URL: c.URL})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(checks) == 0 {
		return nil
	}
	p := cli.StartPager(s)
	defer p.Finish()
	fmt.Fprintf(p, "checks on %s\n\n", render.Abbrev(target))
	width := 0
	for _, c := range checks {
		width = max(width, len(c.Conclusion))
	}
	for _, c := range checks {
		line := fmt.Sprintf("  %-*s  %s", width, c.Conclusion, c.Name)
		if c.URL != "" {
			line += "  " + c.URL
		}
		fmt.Fprintln(p, line)
	}
	return nil
}

// recordChecks writes results against one commit.
//
// The url applies to every --set in the same run, which is what a build script
// wants: one run of one pipeline reports several gates and has one page.
func recordChecks(s *entity.Store, commit string, set []string, url string) error {
	who, err := author(s)
	if err != nil {
		return err
	}

	checks := make([]review.Check, 0, len(set))
	for _, arg := range set {
		name, conclusion, ok := strings.Cut(arg, "=")
		name, conclusion = strings.TrimSpace(name), strings.TrimSpace(conclusion)
		if !ok || name == "" || conclusion == "" {
			return fmt.Errorf("--set is spelled <name>=<conclusion>, as in 'build=pass'")
		}
		if strings.ContainsAny(name, " \t") || strings.ContainsAny(conclusion, " \t") {
			return fmt.Errorf("a check's name and conclusion are single words; got '%s'", arg)
		}
		checks = append(checks, review.Check{Name: name, Conclusion: conclusion, URL: url})
	}

	wrote, err := review.WriteChecks(openChecks(s.Repo), who, time.Now().Unix(), commit, checks)
	if err != nil {
		return err
	}
	if !wrote {
		fmt.Println("nothing changed")
		return nil
	}
	for _, c := range checks {
		fmt.Printf("%s  %s %s\n", render.Abbrev(commit), c.Name, c.Conclusion)
	}
	return nil
}

func cmdDiff(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", diffUsage)
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Println(diffUsage)
		return nil
	}

	s, err := open()
	if err != nil {
		return err
	}
	_, st, err := s.Find(args[0], review.Type)
	if err != nil {
		return err
	}

	base, head := review.Base(st), review.Head(st)
	if head == "" {
		return fmt.Errorf("this review records no revision, so there is nothing to diff")
	}
	if !s.Repo.HasCommit(head) {
		return fmt.Errorf("commit %s is not in this repository; 'git review checkout %s' fetches it", render.Abbrev(head), render.Abbrev(args[0]))
	}

	// base…head, git's own three-dot form: what the head added since it left
	// the base, rather than every difference between two branches that have
	// both moved. That is the change under review.
	spec := head
	if base != "" {
		spec = base + "..." + head
	}
	return s.Repo.Diff(append([]string{spec}, args[1:]...), os.Stdout, os.Stderr)
}

func cmdCheckout(args []string) error {
	var branch string
	var detach bool
	fs := flag.NewFlagSet("checkout", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&branch, "branch", "", "check out under this local name")
	fs.BoolVar(&detach, "detach", false, "check out the commit itself")

	// Parsed here rather than through parseTargets, which requires a target:
	// this is the one command whose id is optional.
	targets, err := parsePermuted(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		// Printf rather than Println: the usage names %f and %l, and Println
		// with a format directive in it is a vet error.
		fmt.Printf("%s\n", checkoutUsage)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s\n%s", err, checkoutUsage)
	}
	if len(targets) > 2 {
		return fmt.Errorf("checkout takes a review and at most one comment, got '%s' as well\n%s", targets[2], checkoutUsage)
	}

	s, err := open()
	if err != nil {
		return err
	}

	// No review is a request to leave one. A review is somewhere you go and
	// then come back from, and the branch to come back to is the same default
	// `add` proposes a new review against.
	if len(targets) == 0 || targets[0] == "" {
		if branch != "" || detach {
			return fmt.Errorf("--branch and --detach describe a review's head, and no review was named\n%s", checkoutUsage)
		}
		return checkoutDefault(s.Repo)
	}

	id, st, err := s.Find(targets[0], review.Type)
	if err != nil {
		return err
	}

	head, revision := review.HeadRef(st), review.Head(st)
	if head == "" && revision == "" {
		return fmt.Errorf("this review names no head branch and records no revision; there is nothing to check out")
	}

	// A revision this clone already holds needs no network at all: the review
	// proposes one commit and it is here, so a second checkout of the same
	// review works offline and in silence.
	src := fetchHead(s, id, head, !s.Repo.HasCommit(revision))

	// The recorded revision is what the review proposes, so it is what gets
	// checked out; the branch is the fallback for a review that records no sha.
	target := revision
	if target == "" {
		target = src.Ref
	}
	if !s.Repo.HasCommit(target) {
		return unreachableHead(head, revision)
	}

	// Without --branch or --detach, a local branch named after the head's own
	// branch: that is what somebody picking a review up to work on it wants,
	// and it is what makes a later push land where the review expects.
	where := src.Branch
	if where == "" {
		where = "review-" + render.Abbrev(id)
	}
	switch {
	case branch != "":
		where = branch
		err = s.Repo.CheckoutBranch(branch, target)
	case detach:
		where = "detached HEAD"
		err = s.Repo.Checkout(target)
	default:
		err = s.Repo.CheckoutBranch(where, target)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s at %s\n", where, render.Abbrev(s.Repo.RefSHA("HEAD")))

	// A second id is a request to be put where that comment is: the checkout
	// was to work on it, and the file and line are what the thread is about.
	if len(targets) == 2 {
		return openComment(s, st, targets[1])
	}
	return nil
}

// openComment opens the file a comment is anchored to, at the line it sits on.
//
// The working tree is at the review's head by now, and the anchor's line counts
// against the commit it was written on, so the two can disagree — that is what
// an outdated anchor is. The line is still where the reader is sent, because it
// is the only number anybody recorded, and the disagreement is reported rather
// than silently corrected: relocating an anchor is editing what somebody said
// (docs/reviews.md).
func openComment(s *entity.Store, st entity.State, target string) error {
	c, err := st.FindComment(target)
	if err != nil {
		return err
	}

	// A reply inherits its thread's anchor, so a reply id takes the reader to
	// the same place the conversation is about.
	root := threadRoot(st, c)
	anchor, ok := review.AnchorOf(st, root.ID())
	if !ok {
		return fmt.Errorf("comment %s is not anchored to a place in the code, so there is nothing to open", render.Abbrev(c.ID()))
	}

	top := s.Repo.Toplevel()
	if top == "" {
		return fmt.Errorf("this repository has no working tree to open %s in", anchor.Path)
	}
	path := filepath.Join(top, anchor.Path)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s is not in the working tree at this revision, so there is nothing to open", anchor.Path)
	}

	if label := anchor.Currency(review.GitRepo{Repo: s.Repo}, review.Head(st)).Label(); label != "" {
		fmt.Fprintf(os.Stderr, "warning: this anchor is %s — line %d is where it sat on %s\n",
			label, anchor.First, render.Abbrev(anchor.Revision))
	}
	fmt.Fprintf(os.Stderr, "Opening %s\n", anchor.Where())
	return s.Repo.EditFileAt(path, anchor.First, s.Repo.ConfigDefault(openCommandKey))
}

// openCommandKey configures how an editor is told to open a file at a line, for
// an editor gitx does not recognise or a wrapper of somebody's own: a command
// line with %f for the path and %l for the line.
const openCommandKey = "review.openCommand"

// threadRoot is the entry a thread hangs from: the one that carries the anchor
// and the resolution. A parent that is not in the state — a blob that arrived
// truncated — ends the walk where it stands rather than looping.
func threadRoot(st entity.State, c entity.Comment) entity.Comment {
	byID := make(map[string]entity.Comment, len(st.Thread))
	for _, e := range st.Thread {
		byID[e.ID()] = e
	}
	for c.Parent() != "" {
		parent, ok := byID[c.Parent()]
		if !ok {
			break
		}
		c = parent
	}
	return c
}

// checkoutDefault checks out the branch reviews are proposed against: the
// remote's own default where it published one, and whichever of the usual names
// this repository holds where it did not. The same answer `add` fills a base in
// with, so the two agree on what "the branch this work targets" means.
func checkoutDefault(repo *gitx.Repo) error {
	rem := repo.BranchRemote()
	if rem == "" {
		rem = remote.Default
	}
	name := repo.DefaultBranch(rem)
	if name == "" {
		return fmt.Errorf("this repository names no default branch: %s published none, and there is no main, master or trunk here", rem)
	}
	if err := repo.Checkout(name); err != nil {
		return err
	}
	fmt.Printf("%s at %s\n", name, render.Abbrev(repo.RefSHA("HEAD")))
	return nil
}

// unreachableHead explains a head this clone could not reach, naming the thing
// that is missing rather than leaving git's own error to say it.
func unreachableHead(head, revision string) error {
	what := "the head branch"
	if revision != "" {
		what = "commit " + render.Abbrev(revision)
	}
	if head == "" {
		return fmt.Errorf("%s is not in this repository, and the review names no head branch to fetch it from", what)
	}
	return fmt.Errorf("%s is not in this repository: no remote here carries %s, and no pull-request head ref reached it", what, head)
}

// headSource is where this clone can reach a review's head: a ref that resolves
// here, and the branch's own name, for the local branch a checkout creates.
type headSource struct {
	Ref    string
	Branch string
}

// fetchHead brings a review's head within reach of this clone and says where it
// landed.
//
// The head is remote-qualified, but the qualifier only names a git remote in the
// clone that wrote it. A review imported from a forge spells it with the head
// repository's *owner* — `contributor/fix-area-walk` — and an outside
// contributor's fork is exactly the repository nobody has a remote for. So three
// routes are tried, cheapest first:
//
//   - a remote by that name, which is what a locally written head means and what
//     a clone that added the fork under its owner's name has;
//   - a remote whose URL is that owner's repository, whatever it is called here.
//     That is also the same-repository case, where the owner is the origin's own
//     and no fork is involved at all;
//   - the forge's copy of the pull request head, refs/pull/<n>/head on the
//     repository the review was imported from. It is published for a fork's pull
//     request exactly as for a branch's, which makes it the one route that needs
//     no second remote and no second credential.
//
// Nothing here is fatal. A route that fails says so and the next is tried, and a
// checkout can still succeed on a copy this clone already holds — which is the
// right outcome for a head branch deleted after its merge. With fetch false
// nothing is asked of the network at all, and only the naming below is done.
func fetchHead(s *entity.Store, id, head string, fetch bool) headSource {
	repo := s.Repo
	qualifier, branch, qualified := strings.Cut(head, "/")

	// A qualifier is only one if something here recognises it. Where nothing
	// does, the whole head is the branch name: `feature/walk` is one branch, not
	// a `walk` on a remote called `feature`.
	rem := ""
	if qualified {
		rem = remoteFor(repo, qualifier)
	}
	src := headSource{Branch: head}
	if rem != "" {
		src.Branch = branch
	}

	if fetch && rem != "" {
		ref := "refs/remotes/" + rem + "/" + branch
		if err := repo.Fetch(rem, "refs/heads/"+branch+":"+ref); err == nil {
			src.Ref = ref
			return src
		} else {
			fmt.Fprintf(os.Stderr, "warning: could not fetch %s from %s: %s\n", branch, rem, err)
		}
	}

	if fetch {
		if ref := fetchPullHead(s, id); ref != "" {
			src.Ref = ref
			// A pull request's head is `<owner>/<branch>`, so reaching it this
			// way settles what the qualifier was.
			if qualified {
				src.Branch = branch
			}
			return src
		}
	}

	// Nothing was fetched: whatever this clone already holds under that name is
	// still the head, and a stale copy checks out fine.
	for _, ref := range []string{"refs/remotes/" + head, "refs/heads/" + head} {
		if head != "" && repo.RefSHA(ref) != "" {
			src.Ref = ref
			break
		}
	}
	return src
}

// remoteFor is the remote a head's qualifier names: one called that, or one
// whose URL belongs to that owner.
func remoteFor(repo *gitx.Repo, qualifier string) string {
	if qualifier == "" {
		return ""
	}
	if repo.RemoteURL(qualifier) != "" {
		return qualifier
	}
	for _, name := range repo.Remotes() {
		if owner := repoOwner(repo.RemoteURL(name)); owner != "" && strings.EqualFold(owner, qualifier) {
			return name
		}
	}
	return ""
}

// fetchPullHead fetches the forge's own copy of the pull request's head, and
// reports the ref it landed at.
//
// GitHub publishes every pull request's head at refs/pull/<n>/head on the
// repository the request was opened against, fork or not. That reaches an
// outside contributor's branch over the remote and the credential that already
// work, with no fork remote to add and nothing to clean up afterwards.
//
// The number comes from the URL the origin ledger recorded at import: a review
// that came from no forge has none, and this does nothing.
func fetchPullHead(s *entity.Store, id string) string {
	url, err := reviewWebURL(s, id)
	if err != nil || url == "" {
		return ""
	}
	repo, number, ok := cutPullURL(url)
	if !ok {
		return ""
	}
	rem := remoteForRepo(s.Repo, repo)
	if rem == "" {
		return ""
	}
	ref := "refs/remotes/" + rem + "/pull/" + number
	if err := s.Repo.Fetch(rem, "refs/pull/"+number+"/head:"+ref); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not fetch pull request %s from %s: %s\n", number, rem, err)
		return ""
	}
	return ref
}

// cutPullURL splits a pull request's web URL into the repository it lives on and
// its number: `https://host/owner/name/pull/41`, with anything after the number
// — /files, /commits — dropped.
func cutPullURL(url string) (repo, number string, ok bool) {
	repo, rest, ok := strings.Cut(url, "/pull/")
	if !ok {
		return "", "", false
	}
	number, _, _ = strings.Cut(rest, "/")
	if number == "" {
		return "", "", false
	}
	for _, c := range number {
		if c < '0' || c > '9' {
			return "", "", false
		}
	}
	return repo, number, true
}

// remoteForRepo is the remote that carries the repository at url: the one whose
// own URL points at it, or — a web URL and a clone URL rarely agree character
// for character, and an enterprise install may not agree on the host at all —
// the remote a plain `git pull` here would use.
func remoteForRepo(repo *gitx.Repo, url string) string {
	if want := repoPath(url); want != "" {
		for _, name := range repo.Remotes() {
			if strings.EqualFold(repoPath(repo.RemoteURL(name)), want) {
				return name
			}
		}
	}
	rem := repo.BranchRemote()
	if rem == "" {
		rem = remote.Default
	}
	if repo.RemoteURL(rem) == "" {
		return ""
	}
	return rem
}

// repoPath is the `owner/name` a repository URL ends in — the part two URLs for
// the same repository agree on whatever their scheme, their host spelling or
// their .git suffix. Anything shorter than two segments is no answer at all,
// which is how a path that is not a forge URL declines to match one.
func repoPath(url string) string {
	s := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(url), "/"), ".git")
	// scp-style (git@host:owner/name) puts the path after a colon; every other
	// form has one too, in its scheme.
	if _, after, ok := strings.Cut(s, ":"); ok {
		s = after
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}

// repoOwner is who a repository URL belongs to, which is what a forge-written
// head qualifies its branch with.
func repoOwner(url string) string {
	owner, _, _ := strings.Cut(repoPath(url), "/")
	return owner
}

const logUsage = `usage: git review log [<id>] [<git log flag>…]

The tracker's own history over refs/notes/reviews/open, one commit per action,
with the author and date of whoever did it.

Every git log flag passes through: --oneline, -p, --author, --since.

Two limits worth knowing. The log does not converge where the blobs do: two
clones that import the same review independently produce identical blobs and two
sets of commits, so a merged log lists every action twice. And commit order is
write order rather than history — a resumed import writes events older than ones
already on the ref — so read it with --author-date-order.`

func cmdLog(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println(logUsage)
		return nil
	}

	s, err := open()
	if err != nil {
		return err
	}

	// A leading id limits the log to one review, by the path its blob sits at.
	// Everything after it is git's.
	var flags []string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, _, err := s.Find(args[0], review.Type)
		if err != nil {
			return err
		}
		paths, err := s.Repo.NotePaths(s.FullRef())
		if err != nil {
			return err
		}
		path, ok := paths[id]
		if !ok {
			return fmt.Errorf("no history for %s on %s", render.Abbrev(id), s.FullRef())
		}
		flags = append(append([]string{}, args[1:]...), "--", path)
	} else {
		flags = args
	}

	return s.Repo.Log(append([]string{s.FullRef()}, flags...), os.Stdout, os.Stderr)
}

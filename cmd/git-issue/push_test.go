package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hdweiss/git-issue/internal/issue"
)

// The whole git-mode push: send the notes ref, report what the remote gained in
// a listing's columns, and leave the tracking ref where the remote now is.
func TestPushGit(t *testing.T) {
	bin := build(t)
	alice, bob := clonePair(t)

	first := strings.TrimSpace(gitIssue(t, bin, alice, "add", "-t", "First issue", "-d", "body").stdout)
	second := strings.TrimSpace(gitIssue(t, bin, alice, "add", "-t", "Second issue").stdout)

	got := gitIssue(t, bin, alice, "push", "origin")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{
		"* [new reference]   refs/notes/issues/open -> refs/notes/issues/open",
		" A " + first,
		" A " + second,
		"2 issues changed, 2 added(+)",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("push output missing %q\n%s", want, got.stdout)
		}
	}

	// The other clone can read what was sent, which is the only proof that
	// matters.
	if out := gitIssue(t, bin, bob, "pull", "origin"); out.code != 0 {
		t.Fatalf("pull after push: %s", out.stderr)
	}
	if listed := gitIssue(t, bin, bob).stdout; !strings.Contains(listed, first) {
		t.Errorf("pushed issue is not listed on the other side\n%s", listed)
	}

	// A second push has nothing to say. Without the tracking-ref update it
	// would re-report the whole tracker every time.
	again := gitIssue(t, bin, alice, "push", "origin")
	if again.code != 0 {
		t.Fatalf("second push: %s", again.stderr)
	}
	if !strings.Contains(again.stdout, "Everything up-to-date") {
		t.Errorf("second push said:\n%s", again.stdout)
	}
	if strings.Contains(again.stdout, "To ") {
		t.Errorf("an up-to-date push printed a header:\n%s", again.stdout)
	}
}

// A remote holding issues this clone has not seen is refused, and the refusal
// says what to do about it. Both refs here are grow-only unions, so pull then
// push always works and a force is never the answer.
func TestPushGitRefusesNonFastForward(t *testing.T) {
	bin := build(t)
	alice, bob := clonePair(t)

	gitIssue(t, bin, alice, "add", "-t", "Alice's issue")
	if out := gitIssue(t, bin, alice, "push", "origin"); out.code != 0 {
		t.Fatalf("alice's first push: %s", out.stderr)
	}

	// Bob starts from what Alice pushed, then both sides write independently.
	gitIssue(t, bin, bob, "pull", "origin")
	gitIssue(t, bin, bob, "add", "-t", "Bob's issue")
	if out := gitIssue(t, bin, bob, "push", "origin"); out.code != 0 {
		t.Fatalf("bob's push: %s", out.stderr)
	}
	gitIssue(t, bin, alice, "add", "-t", "Alice's second issue")

	got := gitIssue(t, bin, alice, "push", "origin")
	if got.code == 0 {
		t.Fatalf("a diverged push succeeded:\n%s", got.stdout)
	}
	for _, want := range []string{
		"holds issues this clone has not seen",
		"git issue pull origin",
	} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("refusal missing %q:\n%s", want, got.stderr)
		}
	}

	// The documented remedy, and it terminates.
	if out := gitIssue(t, bin, alice, "pull", "origin"); out.code != 0 {
		t.Fatalf("pull after refusal: %s", out.stderr)
	}
	if out := gitIssue(t, bin, alice, "push", "origin"); out.code != 0 {
		t.Fatalf("push after pull: %s", out.stderr)
	}
	// Nothing was lost on either side, which is what the refusal protects.
	gitIssue(t, bin, bob, "pull", "origin")
	listed := gitIssue(t, bin, bob).stdout
	for _, want := range []string{"Alice's issue", "Bob's issue", "Alice's second issue"} {
		if !strings.Contains(listed, want) {
			t.Errorf("%q did not survive the merge:\n%s", want, listed)
		}
	}
}

// --dry-run says what would go and sends nothing.
func TestPushGitDryRun(t *testing.T) {
	bin := build(t)
	alice, bob := clonePair(t)

	gitIssue(t, bin, alice, "add", "-t", "Not sent yet")
	got := gitIssue(t, bin, alice, "push", "--dry-run", "origin")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "1 issues would be pushed") {
		t.Errorf("dry run said:\n%s", got.stdout)
	}

	// Nothing reached the remote, so there is still no notes ref to pull.
	gitIssue(t, bin, bob, "pull", "origin")
	if listed := gitIssue(t, bin, bob).stdout; strings.Contains(listed, "Not sent yet") {
		t.Errorf("a dry run actually pushed:\n%s", listed)
	}
}

// A git push sends a ref whole. Naming issues is a bridge's vocabulary, and
// accepting it silently would imply it had narrowed something.
func TestPushGitRejectsIDs(t *testing.T) {
	bin := build(t)
	alice, _ := clonePair(t)
	id := strings.TrimSpace(gitIssue(t, bin, alice, "add", "-t", "An issue").stdout)

	got := gitIssue(t, bin, alice, "push", "origin", id)
	if got.code == 0 {
		t.Fatalf("naming an issue on a git push succeeded:\n%s", got.stdout)
	}
	if !strings.Contains(got.stderr, "applies to a bridge push") {
		t.Errorf("error was:\n%s", got.stderr)
	}
}

// push is in the command list and has its own help, like every other command.
func TestPushHelp(t *testing.T) {
	bin := build(t)
	alice, _ := clonePair(t)

	if out := gitIssue(t, bin, alice, "-h").stdout; !strings.Contains(out, "push") {
		t.Errorf("push missing from the command list:\n%s", out)
	}
	got := gitIssue(t, bin, alice, "push", "--help")
	if got.code != 0 {
		t.Fatalf("push --help exited %d", got.code)
	}
	for _, want := range []string{"--dry-run", "github:origin", "origin ledger"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("push --help missing %q:\n%s", want, got.stdout)
		}
	}
}

// The origin ledger travels with the notes ref, so a clone that pulls gets the
// mappings too. Without them its first bridge push re-creates every issue
// upstream, and nothing undoes that.
func TestPushCarriesTheLedger(t *testing.T) {
	bin := build(t)
	alice, bob := clonePair(t)

	id := strings.TrimSpace(gitIssue(t, bin, alice, "add", "-t", "Linked upstream").stdout)
	// Stand in for what a bridge push would record.
	writeLedger(t, alice, "github.com/acme/git-issue", "issue "+id+" I_kwDOAbCdEf\n")

	if out := gitIssue(t, bin, alice, "push", "origin"); out.code != 0 {
		t.Fatalf("push: %s", out.stderr)
	}
	if out := gitIssue(t, bin, bob, "pull", "origin"); out.code != 0 {
		t.Fatalf("pull: %s", out.stderr)
	}

	got := gitIn(t, bob, "cat-file", "blob", "refs/git-issue/origins:github.com/acme/git-issue")
	if !strings.Contains(got, id) {
		t.Errorf("the ledger did not travel:\n%s", got)
	}
}

// writeLedger puts one tracker's mappings on the ledger ref, the way a bridge
// push does.
//
// Through a scratch index rather than mktree, because a tracker name is a
// nested path — "github.com/acme/git-issue" is three tree levels — and
// update-index builds those where mktree wants one level at a time.
func writeLedger(t *testing.T, dir, tracker, body string) {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + index}

	blob := strings.TrimSpace(gitEnv(t, dir, env, body, "hash-object", "-w", "--stdin"))
	gitEnv(t, dir, env, "", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+tracker)
	tree := strings.TrimSpace(gitEnv(t, dir, env, "", "write-tree"))
	commit := strings.TrimSpace(gitEnv(t, dir, env, "record\n", "commit-tree", tree))
	gitIn(t, dir, "update-ref", "refs/git-issue/origins", commit)
}

// gitEnv runs git with extra environment and optional stdin.
func gitEnv(t *testing.T, dir string, env []string, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// pushServer is a GitHub that accepts writes, for the properties only an
// end-to-end run can show: that a bridge push leaves the notes ref alone, and
// that what it earns lands on the ledger in one commit.
type pushServer struct {
	*httptest.Server
	created int
}

func newPushServer(t *testing.T) *pushServer {
	t.Helper()
	gh := &pushServer{}
	gh.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		json.Unmarshal(raw, &req)

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.Query, "createIssue"):
			gh.created++
			fmt.Fprintf(w, `{"data":{"createIssue":{"issue":{"id":"I_new%d","url":"https://github.com/hdweiss/git-issue/issues/%d"}}}}`, gh.created, gh.created)
		case strings.Contains(req.Query, "addComment"):
			fmt.Fprintf(w, `{"data":{"addComment":{"commentEdge":{"node":{"id":"IC_new%d"}}}}}`, gh.created)
		case strings.Contains(req.Query, "{ id } }"):
			io.WriteString(w, `{"data":{"repository":{"id":"R_repo"}}}`)
		case strings.Contains(req.Query, "nodes { id name }"):
			io.WriteString(w, `{"data":{"repository":{"labels":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"LA_bug","name":"bug"},{"id":"LA_design","name":"design"},{"id":"LA_stale","name":"stale"}]}}}}`)
		case strings.Contains(req.Query, "milestones(first:"):
			io.WriteString(w, `{"data":{"repository":{"milestones":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"MI_v1","title":"v1.0"}]}}}}`)
		case strings.Contains(req.Query, "user(login:"):
			io.WriteString(w, `{"data":{"u0":{"id":"U_a","login":"hdweiss@gmail.com"},"u1":{"id":"U_b","login":"rev@example.com"}}}`)
		default:
			io.WriteString(w, `{"data":{"nodes":[]}}`)
		}
	}))
	t.Cleanup(gh.Close)
	return gh
}

func (g *pushServer) target() string { return "github:" + g.URL + "/hdweiss/git-issue" }

// The property §3 of the design owes: a bridge push never writes to the notes
// ref, so `git issue log` stays a history of the tracker rather than of its own
// bookkeeping. Everything it produces goes to the ledger, in one commit.
func TestPushBridgeLeavesTheNotesRefAlone(t *testing.T) {
	bin := build(t)
	repo := fixtureRepo(t)
	gh := newPushServer(t)
	env := []string{"GITHUB_TOKEN=test-token"}

	before := strings.TrimSpace(gitIn(t, repo, "rev-parse", "refs/notes/issues/open"))
	got := gitIssueEnv(t, bin, repo, env, "push", "--yes", gh.target())
	if got.code != 0 {
		t.Fatalf("exit %d: %s\n%s", got.code, got.stderr, got.stdout)
	}
	if gh.created == 0 {
		t.Fatal("nothing was created upstream")
	}

	after := strings.TrimSpace(gitIn(t, repo, "rev-parse", "refs/notes/issues/open"))
	if after != before {
		t.Errorf("a push moved the notes ref %s -> %s", before, after)
	}

	// The mappings are on the ledger, and they took exactly one commit.
	commits := strings.TrimSpace(gitIn(t, repo, "rev-list", "--count", "refs/git-issue/origins"))
	if commits != "1" {
		t.Errorf("the ledger gained %s commits, want 1", commits)
	}
	ledger := gitIn(t, repo, "cat-file", "blob", "refs/git-issue/origins:"+strings.TrimPrefix(gh.URL, "http://")+"/hdweiss/git-issue")
	if strings.Count(ledger, "issue ") != gh.created {
		t.Errorf("ledger records %d issue lines, want %d:\n%s",
			strings.Count(ledger, "issue "), gh.created, ledger)
	}

	// And the journal is gone, because its lines are committed.
	if _, err := os.Stat(filepath.Join(repo, ".git", "git-issue", "origins-journal")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(repo, ".git", "git-issue", "origins-journal"))
		if len(entries) > 0 {
			t.Errorf("journal survived a clean push: %v", entries)
		}
	}
}

// A second push has nothing to create: every issue now has a mapping, and the
// prefilter plus the diff agree there is nothing to send.
func TestPushBridgeIsIdempotent(t *testing.T) {
	bin := build(t)
	repo := fixtureRepo(t)
	gh := newPushServer(t)
	env := []string{"GITHUB_TOKEN=test-token"}

	if got := gitIssueEnv(t, bin, repo, env, "push", "--yes", gh.target()); got.code != 0 {
		t.Fatalf("first push: %s", got.stderr)
	}
	firstCreated := gh.created
	ledgerBefore := strings.TrimSpace(gitIn(t, repo, "rev-parse", "refs/git-issue/origins"))

	got := gitIssueEnv(t, bin, repo, env, "push", "--yes", gh.target())
	if got.code != 0 {
		t.Fatalf("second push: %s", got.stderr)
	}
	if gh.created != firstCreated {
		t.Errorf("a second push created %d more issues upstream", gh.created-firstCreated)
	}
	if now := strings.TrimSpace(gitIn(t, repo, "rev-parse", "refs/git-issue/origins")); now != ledgerBefore {
		t.Errorf("a no-op push moved the ledger %s -> %s", ledgerBefore, now)
	}
}

// Off a terminal there is nobody to ask, so a push without --yes refuses rather
// than assuming consent. It creates nothing on the way to saying so.
func TestPushBridgeRefusesWithoutConfirmation(t *testing.T) {
	bin := build(t)
	repo := fixtureRepo(t)
	gh := newPushServer(t)

	got := gitIssueEnv(t, bin, repo, []string{"GITHUB_TOKEN=test-token"}, "push", gh.target())
	if got.code == 0 {
		t.Fatalf("an unconfirmed push succeeded:\n%s", got.stdout)
	}
	if !strings.Contains(got.stderr, "--yes") {
		t.Errorf("refusal does not mention --yes:\n%s", got.stderr)
	}
	if gh.created != 0 {
		t.Errorf("an unconfirmed push created %d issues", gh.created)
	}
}

// --dry-run prints the plan and writes nothing, anywhere.
func TestPushBridgeDryRun(t *testing.T) {
	bin := build(t)
	repo := fixtureRepo(t)
	gh := newPushServer(t)

	got := gitIssueEnv(t, bin, repo, []string{"GITHUB_TOKEN=test-token"}, "push", "--dry-run", gh.target())
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	// The creates are listed as " A " rows and tallied, the way `git push`
	// reports a ref it is about to grow — not as a page of "new issue" notes.
	if !strings.Contains(got.stdout, " A ") || !strings.Contains(got.stdout, "issues to push") {
		t.Errorf("dry run did not describe the creates:\n%s", got.stdout)
	}
	if strings.Contains(got.stdout, "new issue") {
		t.Errorf("dry run still prints a per-issue \"new issue\" note:\n%s", got.stdout)
	}
	if gh.created != 0 {
		t.Errorf("a dry run created %d issues", gh.created)
	}
	if refs := gitIn(t, repo, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, "origins") {
		t.Errorf("a dry run wrote the ledger:\n%s", refs)
	}
}

// A dry run on the git leg must not merge the remote's ledger into the local
// one. Fetching a tracking ref is a cache update and fair game; unioning
// somebody else's mappings into the live ledger is a write, and a command that
// says it is doing nothing must do nothing.
func TestPushGitDryRunDoesNotMergeTheLedger(t *testing.T) {
	bin := build(t)
	alice, bob := clonePair(t)

	id := strings.TrimSpace(gitIssue(t, bin, alice, "add", "-t", "Shared").stdout)
	writeLedger(t, alice, "github.com/acme/git-issue", "issue "+id+" I_alice\n")
	if out := gitIssue(t, bin, alice, "push", "origin"); out.code != 0 {
		t.Fatalf("alice's push: %s", out.stderr)
	}

	// Bob is level on the notes ref, so only the ledger can diverge — which is
	// what this is about.
	if out := gitIssue(t, bin, bob, "pull", "origin"); out.code != 0 {
		t.Fatalf("bob's pull: %s", out.stderr)
	}
	// A ledger of his own, on its own history, as a second bridge push would
	// leave it.
	writeLedger(t, bob, "github.com/acme/git-issue", "issue 0123456789ab I_bob\n")
	before := strings.TrimSpace(gitIn(t, bob, "rev-parse", "refs/git-issue/origins"))

	if out := gitIssue(t, bin, bob, "push", "--dry-run", "origin"); out.code != 0 {
		t.Fatalf("dry run: %s", out.stderr)
	}
	if now := strings.TrimSpace(gitIn(t, bob, "rev-parse", "refs/git-issue/origins")); now != before {
		t.Errorf("a dry run moved the ledger %s -> %s", before, now)
	}

	// A real push does merge, and then both mappings are there.
	if out := gitIssue(t, bin, bob, "push", "origin"); out.code != 0 {
		t.Fatalf("push: %s", out.stderr)
	}
	got := gitIn(t, bob, "cat-file", "blob", "refs/git-issue/origins:github.com/acme/git-issue")
	for _, want := range []string{"I_alice", "I_bob"} {
		if !strings.Contains(got, want) {
			t.Errorf("ledger missing %s after the merge:\n%s", want, got)
		}
	}
}

// A sync that moves the ref without moving any issue says so, rather than
// listing an issue as updated.
//
// This is what every pull after a bridge push used to look like: pushing a
// title makes GitHub record a rename, the next import writes that faithfully as
// its own event, and the blob grows by a line that says what it already said.
// The ref lines stay — the ref really did move — but claiming an issue changed
// sends someone looking for a difference that is not there.
func TestSyncReportsNoVisibleChange(t *testing.T) {
	bin := build(t)
	alice, bob := clonePair(t)

	id := strings.TrimSpace(gitIssue(t, bin, alice, "add", "-t", "Shared", "-l", "bug").stdout)
	if out := gitIssue(t, bin, alice, "push", "origin"); out.code != 0 {
		t.Fatalf("alice's push: %s", out.stderr)
	}
	if out := gitIssue(t, bin, bob, "pull", "origin"); out.code != 0 {
		t.Fatalf("bob's pull: %s", out.stderr)
	}

	// A second add of a label the issue already carries: a real event, a real
	// blob change, and nothing a reader sees — the shape of a bridge echo.
	echo := `{"a":"github:someone","c":9,"n":"cccccccccccccccc","op":"label.add","ts":1756200000,"v":1,"val":"bug"}`
	appendEvent(t, alice, id, echo)
	if out := gitIssue(t, bin, alice, "push", "origin"); out.code != 0 {
		t.Fatalf("push of the echo: %s", out.stderr)
	}

	got := gitIssue(t, bin, bob, "pull", "origin")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "No issue changed.") {
		t.Errorf("an echo was not reported as a no-op:\n%s", got.stdout)
	}
	if strings.Contains(got.stdout, " M ") || strings.Contains(got.stdout, "updated") {
		t.Errorf("an echo was listed as an update:\n%s", got.stdout)
	}
	// The ref did move, and the report still says so.
	if !strings.Contains(got.stdout, "refs/notes/issues/open") {
		t.Errorf("the ref-update line went missing:\n%s", got.stdout)
	}
	// And the label is still there once, not twice.
	if listed := gitIssue(t, bin, bob).stdout; strings.Count(listed, "bug") != 1 {
		t.Errorf("the duplicate add is visible in the listing:\n%s", listed)
	}
}

// appendEvent adds a raw event line to an entity's blob, standing in for what
// another writer would have appended.
func appendEvent(t *testing.T, dir, id, line string) {
	t.Helper()
	cmd := exec.Command("git", "notes", "--ref=issues/open", "append", "-F", "-", fullID(t, dir, id))
	cmd.Dir, cmd.Stdin = dir, strings.NewReader(line+"\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("append: %v\n%s", err, out)
	}
}

// A push applies each delta after the ones it links to, so a whole tree can be
// seeded in one run: the epic's create earns the mapping that the issues filed
// under it name.
//
// Order is otherwise preserved, and a cycle is not a reason to lose a delta.
func TestDeltasApplyAfterWhatTheyLinkTo(t *testing.T) {
	rel := func(target string) []issue.Relation {
		return []issue.Relation{{Kind: issue.KindParent, Target: target}}
	}
	order := func(deltas []issue.Delta) []string {
		var ids []string
		for _, d := range inLinkOrder(deltas) {
			ids = append(ids, d.ID)
		}
		return ids
	}

	for _, tc := range []struct {
		name   string
		in     []issue.Delta
		want   []string
		sorted bool
	}{{
		name: "a child before its parent is moved after it",
		in: []issue.Delta{
			{ID: "child", RelationsAdded: rel("epic")},
			{ID: "epic"},
		},
		want: []string{"epic", "child"},
	}, {
		name: "a chain is ordered root first",
		in: []issue.Delta{
			{ID: "leaf", RelationsAdded: rel("mid")},
			{ID: "mid", RelationsAdded: rel("root")},
			{ID: "root"},
		},
		want: []string{"root", "mid", "leaf"},
	}, {
		name: "a link to something not in this push orders nothing",
		in: []issue.Delta{
			{ID: "b", RelationsAdded: rel("elsewhere")},
			{ID: "a"},
		},
		want: []string{"b", "a"},
	}, {
		name: "a cycle keeps every delta",
		in: []issue.Delta{
			{ID: "a", RelationsAdded: rel("b")},
			{ID: "b", RelationsAdded: rel("a")},
		},
		want: []string{"b", "a"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := order(tc.in)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("order = %v, want %v", got, tc.want)
			}
		})
	}
}

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// build compiles the command once per test binary and returns its path.
func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "git-review")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

type result struct {
	stdout, stderr string
	code           int
}

func gitReview(t *testing.T, bin, dir string, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	// No editor: a command that would open one has nothing to open, which is
	// the deterministic case to assert on. The config files go with them, since
	// core.editor in whatever ~/.gitconfig the host has would count as a choice.
	cmd.Env = append(os.Environ(),
		"TZ=Europe/Berlin",
		"GIT_EDITOR=", "VISUAL=", "EDITOR=",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)

	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()

	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

// mustRun runs a command and fails the test if it did not succeed.
func mustRun(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	r := gitReview(t, bin, dir, args...)
	if r.code != 0 {
		t.Fatalf("git review %s: exit %d\n%s%s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return strings.TrimSpace(r.stdout)
}

// repo builds a repository with two commits on a feature branch, which is what
// `add` reads its base, head and revision defaults from.
func repo(t *testing.T) (dir, first, second string) {
	t.Helper()
	dir = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q", "-b", "main")
	git("config", "user.email", "dev@example.com")
	git("config", "user.name", "Dev")
	write("main.go", "package main\n\nfunc main() {}\n")
	git("add", "-A")
	git("commit", "-qm", "initial")

	git("checkout", "-qb", "feature/walk")
	write("main.go", "package main\n\nfunc main() {}\n\nfunc walk() {\n\tprintln(\"walk\")\n}\n")
	git("commit", "-qam", "add walk")
	first = git("rev-parse", "HEAD")

	write("main.go", "package main\n\nfunc main() {}\n\nfunc walk() error {\n\treturn nil\n}\n")
	git("commit", "-qam", "return an error")
	second = git("rev-parse", "HEAD")
	return dir, first, second
}

// A review opened with nothing but a title takes its branches from where the
// caller is standing, which is the whole point of the defaults.
func TestAddDefaultsBranchesFromTheBranchYouAreOn(t *testing.T) {
	bin := build(t)
	dir, _, head := repo(t)

	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk", "-d", "Body.")
	if id == "" {
		t.Fatal("add printed no id")
	}

	out := mustRun(t, bin, dir, "show", id)
	for _, want := range []string{
		"Branches: main ← feature/walk",
		"Revision: " + head[:12],
		"Status:   open",
		"Fix the walk",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show is missing %q:\n%s", want, out)
		}
	}
}

// A comment anchored to a line records the revision it was written against, and
// `show` says where the thread sits — on the entry itself, since the line is
// what the comment is about, and with the commit those line numbers count
// against.
func TestAnchoredCommentRecordsTheRevision(t *testing.T) {
	bin := build(t)
	dir, _, head := repo(t)
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")

	entry := mustRun(t, bin, dir, "comment", id, "--on", "main.go:5-6", "-m", "Return an error.")
	if entry == "" {
		t.Fatal("comment printed no id")
	}

	out := mustRun(t, bin, dir, "show", id)
	for _, want := range []string{
		"File:     main.go:5-6",
		"Revision: " + head[:12],
		"Return an error.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show is missing %q:\n%s", want, out)
		}
	}

	// The same placement when the thread is asked for on its own, which is the
	// view somebody answering one comment is looking at.
	alone := mustRun(t, bin, dir, "show", id, entry)
	for _, want := range []string{"File:     main.go:5-6", "Revision: " + head[:12]} {
		if !strings.Contains(alone, want) {
			t.Errorf("the thread on its own is missing %q:\n%s", want, alone)
		}
	}
}

// An unanchored remark is a remark about the review, not about a line, and
// nothing is invented for it.
func TestUnanchoredCommentCarriesNoPlacement(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")
	mustRun(t, bin, dir, "comment", id, "-m", "Looks reasonable to me.")

	out := mustRun(t, bin, dir, "show", id)
	if strings.Contains(out, "File:") {
		t.Errorf("a comment on no line should carry no file:\n%s", out)
	}
}

// A reply inherits its thread's anchor, so --on on one is refused rather than
// producing a thread whose entries disagree about what they discuss.
func TestReplyRefusesItsOwnAnchor(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")
	entry := mustRun(t, bin, dir, "comment", id, "--on", "main.go:5", "-m", "Here.")

	r := gitReview(t, bin, dir, "comment", id, entry, "--on", "main.go:9", "-m", "No.")
	if r.code == 0 {
		t.Fatal("a reply carrying its own anchor should be refused")
	}
	if !strings.Contains(r.stderr, "inherits") {
		t.Errorf("stderr does not explain why:\n%s", r.stderr)
	}
}

// close with two positionals resolves a thread; with one it closes the review.
// The same slot edit and remove use for a comment, so the three read alike.
func TestCloseResolvesAThreadAndClosesAReview(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")
	entry := mustRun(t, bin, dir, "comment", id, "--on", "main.go:5", "-m", "Here.")

	out := mustRun(t, bin, dir, "close", id, entry, "-m", "Fixed.")
	if !strings.Contains(out, "resolved") {
		t.Errorf("close <id> <comment> should resolve: %q", out)
	}
	if !strings.Contains(mustRun(t, bin, dir, "show", id), "resolved") {
		t.Error("show should mark the thread resolved")
	}

	if out := mustRun(t, bin, dir, "close", id); !strings.Contains(out, "status") {
		t.Errorf("close <id> should close the review: %q", out)
	}
	if !strings.Contains(mustRun(t, bin, dir, "show", id), "Status:   closed") {
		t.Error("the review should be closed")
	}

	// Reopening the thread is the same shape said the other way.
	if out := mustRun(t, bin, dir, "reopen", id, entry); !strings.Contains(out, "unresolved") {
		t.Errorf("reopen <id> <comment> should unresolve: %q", out)
	}
}

// A verdict carries the revision it was cast against, so a later --sync makes
// it stale — reported, never dropped, never re-pointed.
func TestVerdictGoesStaleAfterSync(t *testing.T) {
	bin := build(t)
	dir, first, second := repo(t)

	// Open the review against the older commit, so --sync has somewhere to go.
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk", "--revision", first)
	mustRun(t, bin, dir, "approve", id, "-m", "Looks right.")

	out := mustRun(t, bin, dir, "show", id)
	if !strings.Contains(out, "approve  dev@example.com") || strings.Contains(out, "stale") {
		t.Errorf("the fresh approval should not be stale:\n%s", out)
	}

	mustRun(t, bin, dir, "edit", id, "--sync")
	out = mustRun(t, bin, dir, "show", id)
	if !strings.Contains(out, "stale, on "+first[:12]) {
		t.Errorf("the approval should be stale against %s:\n%s", first[:12], out)
	}
	if !strings.Contains(out, "Revision: "+second[:12]) {
		t.Errorf("the head should have moved to %s:\n%s", second[:12], out)
	}
}

// Anchors go outdated and detached for different reasons, and the two are
// reported as the different things they are.
func TestAnchorGoesOutdatedWhenTheFileMovesOn(t *testing.T) {
	bin := build(t)
	dir, first, _ := repo(t)
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk", "--revision", first)
	mustRun(t, bin, dir, "comment", id, "--on", "main.go:5", "-m", "Return an error.")

	if strings.Contains(mustRun(t, bin, dir, "show", id), "outdated") {
		t.Error("a fresh anchor is not outdated")
	}

	// The second commit rewrites main.go, so the anchor's commit is still an
	// ancestor and the file under it is not what it was.
	mustRun(t, bin, dir, "edit", id, "--sync")
	if !strings.Contains(mustRun(t, bin, dir, "show", id), "outdated") {
		t.Error("the anchor should read outdated once the file changed")
	}
}

// status exits 1 when something is in the way, so a script can branch on it.
func TestStatusExitsOneWhenBlocked(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")

	if r := gitReview(t, bin, dir, "status", id); r.code != 0 {
		t.Fatalf("a fresh review has nothing blocking it: exit %d\n%s", r.code, r.stdout)
	}

	mustRun(t, bin, dir, "comment", id, "--on", "main.go:5", "-m", "Fix this.")
	r := gitReview(t, bin, dir, "status", id)
	if r.code != 1 {
		t.Errorf("an open thread should block: exit %d\n%s", r.code, r.stdout)
	}
	if !strings.Contains(r.stdout, "unresolved") {
		t.Errorf("status should say what is blocking:\n%s", r.stdout)
	}
}

// A top-level remark is discussion, not a question awaiting resolution.
// Counting it would make every review anyone spoke on permanently unready.
func TestUnanchoredCommentDoesNotBlock(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")
	mustRun(t, bin, dir, "comment", id, "-m", "Nice work.")

	if r := gitReview(t, bin, dir, "status", id); r.code != 0 {
		t.Errorf("a loose comment must not block: exit %d\n%s", r.code, r.stdout)
	}
}

// Checks are keyed by commit, so recording one needs no review at all.
func TestChecksAreKeyedByCommit(t *testing.T) {
	bin := build(t)
	dir, _, head := repo(t)

	mustRun(t, bin, dir, "checks", "--commit", "HEAD", "--set", "build=pass", "--url", "https://ci/1")
	out := mustRun(t, bin, dir, "checks", "--commit", head)
	if !strings.Contains(out, "pass  build") || !strings.Contains(out, "https://ci/1") {
		t.Errorf("checks did not come back:\n%s", out)
	}

	// A review whose head is that commit reads the same runs, because the key
	// is the commit rather than the review.
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")
	if !strings.Contains(mustRun(t, bin, dir, "checks", id), "pass  build") {
		t.Error("a review should read its head commit's checks")
	}

	// A re-run supersedes the previous result of the same name.
	mustRun(t, bin, dir, "checks", id, "--set", "build=fail")
	out = mustRun(t, bin, dir, "checks", id)
	if strings.Contains(out, "pass  build") || !strings.Contains(out, "fail  build") {
		t.Errorf("the re-run should supersede:\n%s", out)
	}

	r := gitReview(t, bin, dir, "status", id)
	if r.code != 1 || !strings.Contains(r.stdout, "failing") {
		t.Errorf("a failing check should block: exit %d\n%s", r.code, r.stdout)
	}
}

// A clone with no checks ref reports no checks failing rather than failing to
// report: core state may never depend on that ref.
func TestNoChecksRefIsNotAFailure(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")

	if out := mustRun(t, bin, dir, "checks", id); out != "" {
		t.Errorf("no checks should print nothing, got %q", out)
	}
	if r := gitReview(t, bin, dir, "status", id); r.code != 0 {
		t.Errorf("no checks ref must not block: exit %d\n%s", r.code, r.stdout)
	}
	if out := mustRun(t, bin, dir, "list", "--checks", "none", "--no-tree"); !strings.Contains(out, id) {
		t.Errorf("--checks none should list it:\n%s", out)
	}
}

func TestStatusJSON(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")
	mustRun(t, bin, dir, "comment", id, "--on", "main.go:5-6", "-m", "Fix this.")
	mustRun(t, bin, dir, "checks", id, "--set", "build=fail")

	r := gitReview(t, bin, dir, "status", id, "--json")
	if r.code != 1 {
		t.Fatalf("expected exit 1, got %d\n%s", r.code, r.stderr)
	}

	var got struct {
		Ready   bool `json:"ready"`
		Checks  []struct{ Name, Conclusion string }
		Threads []struct {
			Path     string
			Line     int
			EndLine  int `json:"end_line"`
			Currency string
			Resolved bool
		}
		Blockers []struct{ Kind, Summary string }
	}
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("bad json: %v\n%s", err, r.stdout)
	}
	if got.Ready {
		t.Error("ready should be false")
	}
	if len(got.Checks) != 1 || got.Checks[0].Conclusion != "fail" {
		t.Errorf("checks = %+v", got.Checks)
	}
	if len(got.Threads) != 1 || got.Threads[0].Path != "main.go" || got.Threads[0].Line != 5 || got.Threads[0].EndLine != 6 {
		t.Errorf("threads = %+v", got.Threads)
	}
	if got.Threads[0].Currency != "current" {
		t.Errorf("currency = %q, want current", got.Threads[0].Currency)
	}
	kinds := map[string]bool{}
	for _, b := range got.Blockers {
		kinds[b.Kind] = true
	}
	for _, want := range []string{"checks", "unresolved"} {
		if !kinds[want] {
			t.Errorf("missing blocker %q in %+v", want, got.Blockers)
		}
	}
}

// A piped listing keeps words rather than glyphs, so it stays greppable.
func TestPipedListingIsGreppable(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	mustRun(t, bin, dir, "add", "-t", "Under review")
	mustRun(t, bin, dir, "add", "-t", "A draft", "--draft")

	out := mustRun(t, bin, dir, "list", "--no-tree")
	if strings.ContainsAny(out, "🔍📝🔀🚫✓✗👍✋") {
		t.Errorf("piped output should carry no glyphs:\n%s", out)
	}
	if !strings.Contains(out, "draft") {
		t.Errorf("the lifecycle word should be there:\n%s", out)
	}
}

func TestFilters(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	open := mustRun(t, bin, dir, "add", "-t", "Under review")
	draft := mustRun(t, bin, dir, "add", "-t", "A draft", "--draft")
	closed := mustRun(t, bin, dir, "add", "-t", "Abandoned")
	mustRun(t, bin, dir, "close", closed)

	has := func(args []string, want string, in bool) {
		t.Helper()
		out := mustRun(t, bin, dir, append([]string{"list", "--no-tree"}, args...)...)
		if strings.Contains(out, want) != in {
			t.Errorf("list %v: want %s present=%v\n%s", args, want, in, out)
		}
	}
	has([]string{"--draft"}, draft, true)
	has([]string{"--draft"}, open, false)
	has([]string{"--no-draft"}, draft, false)
	// closed and merged are different endings and a filter must not merge them.
	has([]string{"--state", "closed"}, closed, true)
	has([]string{"--state", "closed"}, open, false)
	has([]string{"--state", "merged"}, closed, false)
	has([]string{"--state", "open"}, open, true)
	has([]string{"--state", "open"}, closed, false)
	has([]string{"--base", "main"}, open, true)
	has([]string{"--base", "nope"}, open, false)
}

// merged is a claim about the code, so a local writer may not assert it.
func TestEditRefusesMerged(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")

	r := gitReview(t, bin, dir, "edit", id, "--status", "merged")
	if r.code == 0 {
		t.Fatal("writing merged locally should be refused")
	}
	if !strings.Contains(r.stderr, "reached the base") {
		t.Errorf("stderr should say why:\n%s", r.stderr)
	}
}

// --as takes its target as a flag value, so the second positional always means
// a comment.
func TestCloseAsSupersededWritesTheLink(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	old := mustRun(t, bin, dir, "add", "-t", "First attempt")
	replacement := mustRun(t, bin, dir, "add", "-t", "Second attempt")

	mustRun(t, bin, dir, "close", old, "--as", "superseded:"+replacement)
	out := mustRun(t, bin, dir, "show", old)
	if !strings.Contains(out, "Superseded by") || !strings.Contains(out, replacement) {
		t.Errorf("the link should be there:\n%s", out)
	}
	if !strings.Contains(out, "superseded") {
		t.Errorf("the reason should be there:\n%s", out)
	}
	// The other end reads the inverse, derived and never stored.
	if !strings.Contains(mustRun(t, bin, dir, "show", replacement), "Supersedes") {
		t.Error("the far end should derive Supersedes")
	}

	// A reason that names something must be given one.
	if r := gitReview(t, bin, dir, "close", old, "--as", "superseded"); r.code == 0 {
		t.Error("--as superseded with no target should be refused")
	}
	// And one that names nothing must not be.
	if r := gitReview(t, bin, dir, "close", old, "--as", "not-planned:"+replacement); r.code == 0 {
		t.Error("--as not-planned with a target should be refused")
	}
}

// closes is a link and nothing more: nothing here reaches the issue.
func TestClosesIsRecordedAndNothingElse(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	target := "4b0755a3e7697bfdf17e42e9f4b307c161ea2a40"

	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk", "--closes", target)
	out := mustRun(t, bin, dir, "show", id)
	if !strings.Contains(out, "Closes:") || !strings.Contains(out, target[:12]) {
		t.Errorf("the link should be there:\n%s", out)
	}

	// The issue ref is never written to.
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/notes/issues/open")
	cmd.Dir = dir
	if err := cmd.Run(); err == nil {
		t.Error("git-review must never write to the issue refs")
	}
}

func TestDiffIsComputedNeverStored(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")

	out := mustRun(t, bin, dir, "diff", id, "--stat")
	if !strings.Contains(out, "main.go") {
		t.Errorf("diff should cover the change:\n%s", out)
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	for _, args := range [][]string{
		{"-h"}, {"help"},
		{"add", "-h"}, {"edit", "-h"}, {"comment", "-h"}, {"close", "-h"},
		{"reopen", "-h"}, {"approve", "-h"}, {"request-changes", "-h"},
		{"status", "-h"}, {"checks", "-h"}, {"diff", "-h"}, {"checkout", "-h"},
		{"list", "-h"}, {"show", "-h"}, {"remove", "-h"}, {"log", "-h"},
	} {
		r := gitReview(t, bin, dir, args...)
		if r.code != 0 {
			t.Errorf("%v: exit %d\n%s", args, r.code, r.stderr)
		}
		if r.stdout == "" {
			t.Errorf("%v printed no help", args)
		}
	}
}

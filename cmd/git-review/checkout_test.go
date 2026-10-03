package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo builds a repository at path with one commit on branch, and returns
// the commit. The path's own shape matters to the caller: a checkout finds a
// fork by the owner its URL ends in, and a directory is a URL here.
func gitRepo(t *testing.T, path, branch string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	gitOut(t, path, "init", "-q", "-b", branch)
	gitOut(t, path, "config", "user.email", "them@example.com")
	gitOut(t, path, "config", "user.name", "Them")
	if err := os.WriteFile(filepath.Join(path, "main.go"), []byte("package main\n\nfunc main() {}\n\nfunc walk() error {\n\treturn nil\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, path, "add", "-A")
	gitOut(t, path, "commit", "-qm", "their work")
	return gitOut(t, path, "rev-parse", "HEAD")
}

// fakeEditor writes a script that records the arguments it was given, and
// returns the script and the file it writes them to. It stands in for an editor
// the way a real one is invoked: through the shell, as git runs core.editor.
func fakeEditor(t *testing.T) (script, log string) {
	t.Helper()
	dir := t.TempDir()
	script, log = filepath.Join(dir, "editor"), filepath.Join(dir, "args")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + log + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, log
}

// gitReviewEnv is gitReview with extra environment, for the cases that are about
// what the environment says — which editor to open, above all.
func gitReviewEnv(t *testing.T, bin, dir string, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(),
		"TZ=Europe/Berlin", "GIT_EDITOR=", "VISUAL=", "EDITOR=",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	), env...)

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

// Checking out a review to answer one comment is one gesture: the branch, and
// the file open at the line the comment sits on.
func TestCheckoutOpensTheCommentsLine(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	editor, log := fakeEditor(t)

	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")
	entry := mustRun(t, bin, dir, "comment", id, "--on", "main.go:5-6", "-m", "Return an error.")

	r := gitReviewEnv(t, bin, dir, []string{"GIT_EDITOR=" + editor}, "checkout", id, entry)
	if r.code != 0 {
		t.Fatalf("checkout with a comment: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, "Opening main.go:5-6") {
		t.Errorf("checkout did not say what it opened:\n%s", r.stderr)
	}

	args, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the editor was never run: %v", err)
	}
	got := strings.Fields(string(args))
	if len(got) != 2 || got[0] != "+5" || filepath.Base(got[1]) != "main.go" {
		t.Errorf("the editor was not pointed at the line: %q", got)
	}
	// The path is absolute, so it does not depend on where the editor is run.
	if !filepath.IsAbs(got[1]) {
		t.Errorf("the path should be absolute: %q", got[1])
	}
}

// A reply has no anchor of its own; it inherits the thread's, so naming one
// takes the reader where the conversation is.
func TestCheckoutOpensAReplyAtItsThreadsAnchor(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	editor, log := fakeEditor(t)

	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")
	root := mustRun(t, bin, dir, "comment", id, "--on", "main.go:5", "-m", "Return an error.")
	reply := mustRun(t, bin, dir, "comment", id, root, "-m", "Agreed.")

	r := gitReviewEnv(t, bin, dir, []string{"GIT_EDITOR=" + editor}, "checkout", id, reply)
	if r.code != 0 {
		t.Fatalf("checkout with a reply: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	args, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the editor was never run: %v", err)
	}
	if got := strings.Fields(string(args)); len(got) == 0 || got[0] != "+5" {
		t.Errorf("the reply did not inherit its thread's line: %q", got)
	}
}

// review.openCommand is the escape hatch for an editor this build cannot point
// at a line by name. The placeholders are positional parameters, not pasted
// text, so a path with a space in it survives.
func TestCheckoutOpenCommandOverridesTheConvention(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	editor, log := fakeEditor(t)

	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")
	entry := mustRun(t, bin, dir, "comment", id, "--on", "main.go:5-6", "-m", "Return an error.")
	gitOut(t, dir, "config", "review.openCommand", editor+" --goto %f:%l")

	r := gitReviewEnv(t, bin, dir, []string{"GIT_EDITOR=" + editor}, "checkout", id, entry)
	if r.code != 0 {
		t.Fatalf("checkout with a comment: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	args, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the editor was never run: %v", err)
	}
	got := strings.Fields(string(args))
	if len(got) != 2 || got[0] != "--goto" || !strings.HasSuffix(got[1], "main.go:5") {
		t.Errorf("the configured command was not used: %q", got)
	}
}

// A comment on no line has nowhere to open, and says so rather than opening
// something arbitrary. The checkout itself still happened.
func TestCheckoutSaysWhenACommentHasNoAnchor(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	editor, _ := fakeEditor(t)

	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")
	entry := mustRun(t, bin, dir, "comment", id, "-m", "Looks reasonable to me.")

	r := gitReviewEnv(t, bin, dir, []string{"GIT_EDITOR=" + editor}, "checkout", id, entry)
	if r.code == 0 {
		t.Fatalf("an unanchored comment has nothing to open:\n%s", r.stdout)
	}
	if !strings.Contains(r.stderr, "not anchored") {
		t.Errorf("the message should say why:\n%s", r.stderr)
	}
	if !strings.Contains(r.stdout, "feature/walk at ") {
		t.Errorf("the checkout itself should still have happened:\n%s", r.stdout)
	}
}

// A review is somewhere you go and then leave, so checkout with nothing to check
// out is the way back to the branch the reviews are proposed against.
func TestCheckoutWithNoReviewReturnsToTheDefaultBranch(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)

	out := mustRun(t, bin, dir, "checkout")
	if !strings.HasPrefix(out, "main at ") {
		t.Errorf("checkout did not report the branch it moved to:\n%s", out)
	}
	if got := gitOut(t, dir, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Errorf("HEAD is on %s, not the default branch", got)
	}
}

// --branch and --detach say what to do with a review's head, so asking for them
// with no review is a mistake rather than a request about the default branch.
func TestCheckoutRefusesHeadFlagsWithNoReview(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)

	r := gitReview(t, bin, dir, "checkout", "--detach")
	if r.code == 0 {
		t.Fatalf("--detach with no review should be refused:\n%s", r.stdout)
	}
	if !strings.Contains(r.stderr, "no review was named") {
		t.Errorf("the refusal should say what is missing:\n%s", r.stderr)
	}
}

// The head's qualifier is the fork's owner, and an owner is not a remote name.
// The fork is configured here under another name entirely, and is found by what
// its URL says it is.
func TestCheckoutFindsTheForkByItsURL(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)

	fork := filepath.Join(t.TempDir(), "contributor", "git-issue")
	sha := gitRepo(t, fork, "fix-area-walk")
	gitOut(t, dir, "remote", "add", "upstream-fork", fork)

	id := mustRun(t, bin, dir, "add", "-t", "Fix the area walk",
		"--head", "contributor/fix-area-walk", "--revision", sha)

	out := mustRun(t, bin, dir, "checkout", id)
	if !strings.Contains(out, "fix-area-walk at "+sha[:12]) {
		t.Errorf("checkout did not report the head it moved to:\n%s", out)
	}
	if got := gitOut(t, dir, "symbolic-ref", "--short", "HEAD"); got != "fix-area-walk" {
		t.Errorf("HEAD is on %s, not the head branch", got)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != sha {
		t.Errorf("HEAD is at %s, not the review's revision %s", got, sha)
	}
	// Fetched into the remote's own namespace, which is where a later --sync
	// and a later diff look for it.
	if !refExists(t, dir, "refs/remotes/upstream-fork/fix-area-walk") {
		t.Error("the fork's branch should have been fetched into its remote")
	}

	// The commit is here now, so checking the same review out again asks the
	// network for nothing — which is what makes it work offline.
	gitOut(t, dir, "checkout", "-q", "main")
	gitOut(t, dir, "remote", "remove", "upstream-fork")
	r := gitReview(t, bin, dir, "checkout", id)
	if r.code != 0 || r.stderr != "" {
		t.Errorf("a second checkout should need no remote: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
}

// Nothing here has a remote for the fork at all. The pull request's head is
// published on the repository it was opened against, which is the remote that
// already works, so that is where it is fetched from.
func TestCheckoutFetchesAForksHeadThroughThePullRef(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	pullFixture(t, bin, dir)

	// The upstream repository as a forge serves it: the fork's branch is not on
	// it, and the pull request's head is, at refs/pull/<n>/head.
	upstream := filepath.Join(t.TempDir(), "git-issue")
	sha := gitRepo(t, upstream, "main")
	gitOut(t, upstream, "update-ref", "refs/pull/41/head", sha)
	gitOut(t, dir, "remote", "add", "origin", upstream)

	id := ""
	for _, line := range strings.Split(mustRun(t, bin, dir, "list", "--no-tree"), "\n") {
		if strings.Contains(line, "Fix the area subtree walk") {
			id = strings.Fields(line)[0]
		}
	}
	if id == "" {
		t.Fatal("the imported review is not in the listing")
	}
	// The fixture's revision is a sha no repository holds; the one the pull ref
	// resolves to is what this upstream proposes.
	mustRun(t, bin, dir, "edit", id, "--revision", sha)

	out := mustRun(t, bin, dir, "checkout", id)
	if !strings.Contains(out, "fix-area-walk at "+sha[:12]) {
		t.Errorf("checkout did not report the head it moved to:\n%s", out)
	}
	if got := gitOut(t, dir, "symbolic-ref", "--short", "HEAD"); got != "fix-area-walk" {
		t.Errorf("HEAD is on %s, not the head branch", got)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != sha {
		t.Errorf("HEAD is at %s, not the review's revision %s", got, sha)
	}
	if !refExists(t, dir, "refs/remotes/origin/pull/41") {
		t.Error("the pull request head should have been fetched")
	}
}

// A head nothing here can reach says so in terms of the review, rather than
// leaving git's own error about a bad revision to stand for it.
func TestCheckoutSaysWhenNothingCanReachTheHead(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)

	id := mustRun(t, bin, dir, "add", "-t", "Fix the area walk",
		"--head", "contributor/fix-area-walk",
		"--revision", "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d")

	r := gitReview(t, bin, dir, "checkout", id)
	if r.code == 0 {
		t.Fatalf("a head no remote carries cannot be checked out:\n%s", r.stdout)
	}
	for _, want := range []string{"is not in this repository", "contributor/fix-area-walk"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("the message is missing %q:\n%s", want, r.stderr)
		}
	}
}

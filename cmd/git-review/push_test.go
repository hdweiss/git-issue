package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// idOf is the id of the review whose title contains want.
func idOf(t *testing.T, bin, dir, want string) string {
	t.Helper()
	for _, line := range strings.Split(mustRun(t, bin, dir, "list", "--no-tree"), "\n") {
		if strings.Contains(line, want) {
			return strings.Fields(line)[0]
		}
	}
	t.Fatalf("no review titled %q is listed", want)
	return ""
}

// A dry run reads the tracker, prints what it would send, and writes nothing.
//
// The fake refuses every mutation, so "nothing was written" is enforced by the
// server rather than asserted after the fact: a --dry-run that wrote would fail
// here rather than pass quietly.
func TestPushDryRunReportsThePlan(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	srv := fakeGitHub(t)
	pullFrom(t, bin, dir, srv)

	id := idOf(t, bin, dir, "Fix the area subtree walk")
	mustRun(t, bin, dir, "edit", id, "-t", "Fix the area subtree walk, properly")

	r := gitReview(t, bin, dir, "push", "--dry-run", "--token", "test-token",
		"github:"+srv.URL+"/hdweiss/git-issue")
	if r.code != 0 {
		t.Fatalf("dry run: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "title") || !strings.Contains(r.stdout, "1 review to push") {
		t.Errorf("the plan should name the changed field:\n%s%s", r.stdout, r.stderr)
	}
	// The other imported review has nothing local to say, so the prefilter keeps
	// it out of the plan entirely.
	if strings.Contains(r.stdout, "Retire the old walker") {
		t.Errorf("a review with no local write should not be in the plan:\n%s", r.stdout)
	}
}

// A review that only echoes what the tracker said has nothing to send, and the
// prefilter says so without reading anything upstream.
func TestPushIsUpToDateAfterAnImport(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	srv := fakeGitHub(t)
	pullFrom(t, bin, dir, srv)

	r := gitReview(t, bin, dir, "push", "--token", "test-token",
		"github:"+srv.URL+"/hdweiss/git-issue")
	if r.code != 0 {
		t.Fatalf("push: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "Everything up-to-date") {
		t.Errorf("nothing local changed:\n%s%s", r.stdout, r.stderr)
	}
}

// Off a terminal there is nobody to ask, so a bridge push refuses rather than
// answering for the caller. It creates things upstream that nothing here undoes.
func TestPushAsksBeforeWriting(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	srv := fakeGitHub(t)
	pullFrom(t, bin, dir, srv)

	id := idOf(t, bin, dir, "Fix the area subtree walk")
	mustRun(t, bin, dir, "edit", id, "-t", "Renamed")

	r := gitReview(t, bin, dir, "push", "--token", "test-token",
		"github:"+srv.URL+"/hdweiss/git-issue")
	if r.code == 0 {
		t.Fatalf("a push with something to send should not proceed unasked:\n%s", r.stdout)
	}
	if !strings.Contains(r.stderr, "--yes") {
		t.Errorf("the refusal should name the way through:\n%s%s", r.stdout, r.stderr)
	}
}

// A review push to Azure DevOps needs a target that resolves to a repository —
// a pull request is per-repository, not per-project.
func TestPushADONeedsARepository(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)

	r := gitReview(t, bin, dir, "push", "--token", "test-token",
		"ado:https://dev.azure.com/org/proj")
	if r.code == 0 || !strings.Contains(r.stderr, "repository") {
		t.Errorf("a slug target should be refused:\n%s%s", r.stdout, r.stderr)
	}
}

// An area path is a work-item concept; a review push carries no '#' scope.
func TestPushADORejectsScope(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)

	r := gitReview(t, bin, dir, "push", "--token", "test-token",
		"ado:https://dev.azure.com/org/proj/_git/repo#Web")
	if r.code == 0 || !strings.Contains(r.stderr, "scope") {
		t.Errorf("a scoped target should be refused:\n%s%s", r.stdout, r.stderr)
	}
}

// An id narrows which reviews a bridge writes. A git push sends a ref whole, so
// accepting the argument silently would imply it had narrowed something.
func TestPushRefusesIDsOnTheGitLeg(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	bareRemote(t, dir)

	mustRun(t, bin, dir, "add", "-t", "A local read")
	id := idOf(t, bin, dir, "A local read")

	r := gitReview(t, bin, dir, "push", "origin", id)
	if r.code == 0 || !strings.Contains(r.stderr, "entire") {
		t.Errorf("expected the git leg to refuse an id:\n%s%s", r.stdout, r.stderr)
	}
}

// The git leg carries all three refs, and the ledger goes first — a clone that
// received reviews without their mappings would re-file every one of them
// upstream on its next bridge push.
func TestPushGitCarriesEveryRef(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	srv := fakeGitHub(t)
	pullFrom(t, bin, dir, srv)
	bare := bareRemote(t, dir)

	out := mustRun(t, bin, dir, "push", "origin")
	if !strings.Contains(out, "reviews/open") {
		t.Errorf("the reviews ref should have been sent:\n%s", out)
	}
	for _, ref := range []string{
		"refs/notes/reviews/open",
		"refs/notes/checks/runs",
		"refs/git-issue/origins",
	} {
		if !refExists(t, bare, ref) {
			t.Errorf("%s did not reach the remote", ref)
		}
	}
}

// bareRemote makes a bare repository and points 'origin' at it.
func bareRemote(t *testing.T, dir string) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "peer.git")
	run := func(wd string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = wd
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run(t.TempDir(), "init", "--bare", bare)
	run(dir, "remote", "add", "origin", bare)
	return bare
}

// Nothing is forced. Both refs are grow-only sets of lines, so pull-then-push
// always works and there is never a reason to reach for a force — which is why
// the refusal spells the remedy out.
func TestPushGitRefusesNonFastForward(t *testing.T) {
	bin := build(t)
	alice, _, _ := repo(t)
	bare := bareRemote(t, alice)
	bob := clone(t, bare)

	mustRun(t, bin, alice, "add", "-t", "Alice's read")
	mustRun(t, bin, alice, "push", "origin")

	mustRun(t, bin, bob, "pull", "origin")
	mustRun(t, bin, bob, "add", "-t", "Bob's read")
	mustRun(t, bin, bob, "push", "origin")

	mustRun(t, bin, alice, "add", "-t", "Alice's second read")
	got := gitReview(t, bin, alice, "push", "origin")
	if got.code == 0 {
		t.Fatalf("a diverged push succeeded:\n%s", got.stdout)
	}
	for _, want := range []string{"reviews this clone has not seen", "git review pull origin"} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("the refusal is missing %q:\n%s", want, got.stderr)
		}
	}

	// The documented remedy, and it terminates.
	mustRun(t, bin, alice, "pull", "origin")
	mustRun(t, bin, alice, "push", "origin")

	mustRun(t, bin, bob, "pull", "origin")
	listed := mustRun(t, bin, bob, "list", "--no-tree")
	for _, want := range []string{"Alice's read", "Bob's read", "Alice's second read"} {
		if !strings.Contains(listed, want) {
			t.Errorf("%q did not survive the merge:\n%s", want, listed)
		}
	}
}

func TestPushHelp(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)

	out := mustRun(t, bin, dir, "push", "--help")
	for _, want := range []string{"github:origin", "--dry-run", "--refresh"} {
		if !strings.Contains(out, want) {
			t.Errorf("the usage does not mention %q:\n%s", want, out)
		}
	}
}

// clone makes a second working copy of a bare repository.
func clone(t *testing.T, bare string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	run := func(wd string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = wd
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run(t.TempDir(), "clone", bare, dir)
	run(dir, "config", "user.email", "bob@example.com")
	run(dir, "config", "user.name", "Bob")
	return dir
}

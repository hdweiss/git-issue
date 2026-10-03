package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// syncFile is where the import watermarks live, which is what a destroy has to
// leave in the right state.
func syncFile(t *testing.T, dir string) string {
	t.Helper()
	// git answers with a path relative to where it ran unless the repository is
	// elsewhere, so it is resolved against the same directory.
	common := gitOut(t, dir, "rev-parse", "--git-common-dir")
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	return filepath.Join(common, "git-issue", "sync.json")
}

// destroy is the one command that deletes rather than appends: it removes the
// refs outright, refuses without --yes, and is kept out of the command list.
func TestDestroyNeedsYes(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	id := mustRun(t, bin, dir, "add", "-t", "Fix the walk")
	mustRun(t, bin, dir, "checks", id, "--set", "build=pass")

	if help := gitReview(t, bin, dir, "help"); strings.Contains(help.stdout, "destroy") {
		t.Errorf("destroy is listed in help:\n%s", help.stdout)
	}

	// Without --yes it names what it would remove and changes nothing.
	refused := gitReview(t, bin, dir, "destroy")
	if refused.code == 0 {
		t.Fatal("destroy without --yes was accepted")
	}
	for _, want := range []string{"--yes", "refs/notes/reviews/open", "refs/notes/checks/runs"} {
		if !strings.Contains(refused.stderr, want) {
			t.Errorf("the refusal is missing %q: %q", want, refused.stderr)
		}
	}
	if !refExists(t, dir, "refs/notes/reviews/open") {
		t.Fatal("destroy without --yes still deleted the reviews")
	}

	done := gitReview(t, bin, dir, "destroy", "--yes")
	if done.code != 0 {
		t.Fatalf("destroy --yes failed: %s", done.stderr)
	}
	for _, want := range []string{"destroyed refs/notes/reviews/open", "destroyed refs/notes/checks/runs"} {
		if !strings.Contains(done.stdout, want) {
			t.Errorf("destroy did not report %q:\n%s", want, done.stdout)
		}
	}
	if refExists(t, dir, "refs/notes/reviews/open") || refExists(t, dir, "refs/notes/checks/runs") {
		t.Error("a ref survived destroy")
	}

	// A second run has nothing to delete and says so, without failing.
	again := gitReview(t, bin, dir, "destroy", "--yes")
	if again.code != 0 || !strings.Contains(again.stdout, "nothing to destroy") {
		t.Errorf("repeat destroy: exit %d, stdout %q", again.code, again.stdout)
	}
}

// The watermark is the piece with teeth. Left behind, it tells the next pull
// that everything up to it is already imported, and the repository refills with
// a slice of the tracker instead of the tracker.
func TestDestroyClearsTheImportWatermark(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	pullFixture(t, bin, dir)

	before, err := os.ReadFile(syncFile(t, dir))
	if err != nil {
		t.Fatalf("the import recorded no watermark: %v", err)
	}
	if !strings.Contains(string(before), "github-reviews:") {
		t.Fatalf("no review watermark to clear:\n%s", before)
	}

	done := gitReview(t, bin, dir, "destroy", "--yes")
	if done.code != 0 {
		t.Fatalf("destroy --yes failed: %s", done.stderr)
	}
	if !strings.Contains(done.stdout, "watermark") {
		t.Errorf("destroy did not report clearing the watermark:\n%s", done.stdout)
	}
	if body, err := os.ReadFile(syncFile(t, dir)); err == nil && strings.Contains(string(body), "github-reviews:") {
		t.Errorf("the review watermark survived destroy:\n%s", body)
	}

	// And the mappings that said which pull request each review was: left
	// behind, the ones claiming comments would make the next import skip
	// comments this clone no longer holds.
	if refExists(t, dir, "refs/git-issue/origins") {
		tree := gitOut(t, dir, "ls-tree", "-r", "refs/git-issue/origins")
		if strings.Contains(tree, "hdweiss/git-issue") {
			blob := gitOut(t, dir, "cat-file", "-p", "refs/git-issue/origins:"+firstLine(gitOut(t, dir, "ls-tree", "-r", "--name-only", "refs/git-issue/origins")))
			if strings.Contains(blob, "review ") {
				t.Errorf("review mappings survived destroy:\n%s", blob)
			}
		}
	}
}

// A repository holding both types keeps the issues' share of the shared state.
// The two commands destroy their own entities, not each other's.
func TestDestroyLeavesTheIssueWatermarkAlone(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	pullFixture(t, bin, dir)

	// An issue bridge's watermark, written by hand: this package's binary is
	// git-review, and what matters is the key's scheme rather than who wrote it.
	path := syncFile(t, dir)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mixed := strings.Replace(string(body), `"sources": {`,
		`"sources": {`+"\n    "+`"github:github.com/hdweiss/git-issue:open": "2026-01-01T00:00:00Z",`, 1)
	if mixed == string(body) {
		t.Fatalf("could not add an issue watermark to:\n%s", body)
	}
	if err := os.WriteFile(path, []byte(mixed), 0o644); err != nil {
		t.Fatal(err)
	}

	if r := gitReview(t, bin, dir, "destroy", "--yes"); r.code != 0 {
		t.Fatalf("destroy --yes failed: %s", r.stderr)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the file went with the review watermarks: %v", err)
	}
	if !strings.Contains(string(after), `"github:github.com/hdweiss/git-issue:open"`) {
		t.Errorf("the issue watermark went too:\n%s", after)
	}
	if strings.Contains(string(after), "github-reviews:") {
		t.Errorf("a review watermark survived:\n%s", after)
	}
}

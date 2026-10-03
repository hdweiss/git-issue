package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// logRepo is an empty repository that writes as one known person, so the log's
// author lines are worth asserting on. The fixture repo is no use here: its
// notes were filed with `git notes add`, which is exactly the history this
// replaces.
func logRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	gitIn(t, t.TempDir(), "init", "-q", dir)
	gitIn(t, dir, "config", "user.email", "alice@example.com")
	gitIn(t, dir, "config", "user.name", "alice")
	return dir
}

// buffer is what a scripted editor writes back: the shape `git issue edit`
// hands out and reads in — the title on the first line, the description below.
func buffer(title, body string) string {
	if body == "" {
		return title + "\n"
	}
	return title + "\n\n" + body + "\n"
}

// A local write is a commit like any other now: authored by whoever wrote it,
// saying what they did. Before this it said "Notes added by 'git notes add'".
func TestLogDescribesLocalWrites(t *testing.T) {
	bin, dir := build(t), logRepo(t)

	add := gitIssue(t, bin, dir, "add", "-t", "Anchor blobs are pruned by gc",
		"-d", "Unreachable from every ref, so gc deletes it.", "--type", "bug", "-l", "storage")
	if add.code != 0 {
		t.Fatalf("add: exit %d: %s", add.code, add.stderr)
	}
	id := strings.TrimSpace(add.stdout)

	got := gitIssue(t, bin, dir, "log")
	if got.code != 0 {
		t.Fatalf("log: exit %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{
		`Create issue "Anchor blobs are pruned by gc"`,
		"Unreachable from every ref, so gc deletes it.",
		"Author: alice <alice@example.com>",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("log missing %q:\n%s", want, got.stdout)
		}
	}
	if strings.Contains(got.stdout, "git notes") {
		t.Errorf("a local write still carries git's default message:\n%s", got.stdout)
	}

	// The Issue trailer names the entity in full, which is the only join from
	// a commit back to the issue it wrote.
	trailer := strings.TrimSpace(gitIn(t, dir, "log", "-1",
		"--format=%(trailers:key=Issue,valueonly)", "refs/notes/issues/open"))
	// add prints the abbreviated id; the trailer carries it in full, because a
	// commit has to name the entity unambiguously.
	if !strings.HasPrefix(trailer, id) || len(trailer) <= len(id) {
		t.Errorf("Issue trailer is %q, want the full id behind %s", trailer, id)
	}
}

// An edit is its own commit, and one that changes several fields says so in
// the body rather than in a subject nobody would read.
func TestLogDescribesEdits(t *testing.T) {
	bin, dir := build(t), logRepo(t)

	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "First title", "-l", "merge").stdout)

	env, _ := fakeEditor(t, buffer("Second title", ""))
	if got := gitIssueEnv(t, bin, dir, []string{env}, "edit", id); got.code != 0 {
		t.Fatalf("edit: exit %d: %s", got.code, got.stderr)
	}

	oneline := gitIssue(t, bin, dir, "log", "--oneline").stdout
	for _, want := range []string{`Rename to "Second title"`, `Create issue "First title"`} {
		if !strings.Contains(oneline, want) {
			t.Errorf("log missing %q:\n%s", want, oneline)
		}
	}
	// The rename says what the issue used to be called, which is otherwise
	// only recoverable by folding the blob.
	if !strings.Contains(gitIssue(t, bin, dir, "log").stdout, `Previously: "First title"`) {
		t.Errorf("the rename does not name the old title")
	}
}

// `git issue log <id>` limits the log to one issue; everything else is git
// log's own.
func TestLogOneIssue(t *testing.T) {
	bin, dir := build(t), logRepo(t)

	first := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "First issue").stdout)
	gitIssue(t, bin, dir, "add", "-t", "Second issue")

	got := gitIssue(t, bin, dir, "log", first, "--oneline")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "First issue") {
		t.Errorf("log %s does not show its own issue:\n%s", first, got.stdout)
	}
	if strings.Contains(got.stdout, "Second issue") {
		t.Errorf("log %s shows another issue:\n%s", first, got.stdout)
	}

	// An id nobody holds is refused rather than silently logging everything.
	if got := gitIssue(t, bin, dir, "log", "deadbeef"); got.code == 0 {
		t.Errorf("log of an unknown id was accepted:\n%s", got.stdout)
	}
	if got := gitIssue(t, bin, dir, "log", "-h"); got.code != 0 || !strings.HasPrefix(got.stdout, "usage:") {
		t.Errorf("log -h: exit %d, stdout %q", got.code, got.stdout)
	}
}

// git re-shards a notes tree as it grows, which renames every note in it —
// `abcd…` becomes `ab/cd…`. Limiting the log to one spelling silently loses
// the history written under the other, so `log <id>` names every depth.
func TestLogSurvivesAReshard(t *testing.T) {
	bin, dir := build(t), logRepo(t)

	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Survives a reshard").stdout)
	before := notePath(t, dir, id)

	// Enough notes that git prefers a two-level fanout — measured: it switches
	// somewhere between 64 and 96 — and spread across every leading byte,
	// because git's heuristic looks at whether all sixteen top-level buckets
	// are populated rather than at the count alone. Filling them by adding real
	// issues would leave that to chance: 100 random ids miss a bucket about
	// once in forty runs, which is a flaky test rather than a rare one.
	//
	// These are `git notes` writes of git's own, which is also what re-shards:
	// nothing this program writes ever moves a note already placed.
	for i := 0; i < 256; i += 2 {
		filler := fmt.Sprintf("%02x%s", i, strings.Repeat("0", 38))
		gitIn(t, dir, "notes", "--ref=issues/open", "add", "-m", "filler", filler)
	}
	after := notePath(t, dir, id)
	if before == after {
		t.Fatalf("the tree did not re-shard: %s stayed at %s", id, before)
	}

	// One more write, which lands on the path the tree now uses.
	env, _ := fakeEditor(t, buffer("Survived a reshard", ""))
	if got := gitIssueEnv(t, bin, dir, []string{env}, "edit", id); got.code != 0 {
		t.Fatalf("edit: exit %d: %s", got.code, got.stderr)
	}

	got := gitIssue(t, bin, dir, "log", id, "--oneline")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{`Create issue "Survives a reshard"`, `Rename to "Survived a reshard"`} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("log lost %q across the re-shard (%s -> %s):\n%s", want, before, after, got.stdout)
		}
	}
}

// notePath is where an entity's blob currently sits in the notes tree, fanout
// separators and all.
func notePath(t *testing.T, dir, id string) string {
	t.Helper()
	for _, line := range strings.Split(gitIn(t, dir, "ls-tree", "-r", "--name-only",
		"refs/notes/issues/open"), "\n") {
		if strings.HasPrefix(strings.ReplaceAll(line, "/", ""), id) {
			return line
		}
	}
	t.Fatalf("no note for %s", id)
	return ""
}

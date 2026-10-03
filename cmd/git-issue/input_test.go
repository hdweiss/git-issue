package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A pipe with no -t is the whole buffer: first line the title, the rest the
// description — the same shape the editor uses, and git commit before it.
func TestAddReadsPipedBuffer(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)

	in := strings.NewReader("Piped title\n\nPiped body\n\n# a heading survives\n")
	added := gitIssueIO(t, bin, dir, noEditor, in, "add", "--type", "bug")
	if added.code != 0 {
		t.Fatalf("add failed: %s", added.stderr)
	}

	shown := gitIssue(t, bin, dir, strings.TrimSpace(added.stdout)).stdout
	for _, want := range []string{"Piped title", "Piped body", "# a heading survives"} {
		if !strings.Contains(shown, want) {
			t.Errorf("show is missing %q\n%s", want, shown)
		}
	}
	hasHeader(t, shown, "Type", "bug")
}

// With -t given, the request is already complete, so a pipe has to be asked
// for: `-F -` makes it the description alone, and its first line is not eaten
// as a title. Implicit stdin is not offered here — see readBuffer.
func TestAddPipeIsBodyWhenTitleGiven(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)

	in := strings.NewReader("First line of the body\nsecond line\n")
	added := gitIssueIO(t, bin, dir, noEditor, in, "add", "-t", "Explicit title", "-F", "-")
	if added.code != 0 {
		t.Fatalf("add failed: %s", added.stderr)
	}

	shown := gitIssue(t, bin, dir, strings.TrimSpace(added.stdout)).stdout
	if !strings.Contains(shown, "Explicit title") || !strings.Contains(shown, "First line of the body") {
		t.Errorf("title or body wrong\n%s", shown)
	}
}

// -F names a file to take the buffer from, so no cat is needed.
func TestAddReadsFile(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	path := filepath.Join(t.TempDir(), "issue.md")
	if err := os.WriteFile(path, []byte("From a file\n\nthe body\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	added := gitIssueEnv(t, bin, dir, noEditor, "add", "-F", path)
	if added.code != 0 {
		t.Fatalf("add -F failed: %s", added.stderr)
	}
	if shown := gitIssue(t, bin, dir, strings.TrimSpace(added.stdout)).stdout; !strings.Contains(shown, "From a file") || !strings.Contains(shown, "the body") {
		t.Errorf("the file was not read\n%s", shown)
	}
}

// -d and -F both setting the body is refused, not silently one or the other.
func TestAddRefusesTwoBodies(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	path := filepath.Join(t.TempDir(), "b.md")
	if err := os.WriteFile(path, []byte("T\n\nfile body\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := gitIssueEnv(t, bin, dir, noEditor, "add", "-t", "T", "-d", "flag body", "-F", path)
	if got.code == 0 || !strings.Contains(got.stderr, "both give the body") {
		t.Errorf("exit %d, stderr %q", got.code, got.stderr)
	}
}

// A piped buffer whose first line is blank has no title, and aborts the way an
// empty editor buffer does.
func TestAddPipedEmptyTitleAborts(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)

	in := strings.NewReader("\n\njust a body, no title\n")
	got := gitIssueIO(t, bin, dir, noEditor, in, "add")
	if got.code == 0 || !strings.Contains(got.stderr, "first line of the input is empty") {
		t.Errorf("exit %d, stderr %q", got.code, got.stderr)
	}
}

// Piping to edit replaces the buffer wholesale — first line included, so it can
// rename the issue, the same as retyping the editor's first line.
func TestEditReadsPipedBuffer(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Old title", "-d", "old body").stdout)

	in := strings.NewReader("New title\n\nnew body\n")
	got := gitIssueIO(t, bin, dir, noEditor, in, "edit", id)
	if got.code != 0 {
		t.Fatalf("edit failed: %s", got.stderr)
	}
	if !strings.Contains(got.stdout, "title, description changed") {
		t.Errorf("did not report both fields\n%s", got.stdout)
	}
	shown := gitIssue(t, bin, dir, id).stdout
	if !strings.Contains(shown, "New title") || !strings.Contains(shown, "new body") || strings.Contains(shown, "Old title") {
		t.Errorf("the edit did not land\n%s", shown)
	}
}

// With -t, a pipe on edit is the description only; the title stays as the flag
// set it and the first line of the pipe is not consumed. Naming a field makes
// the request complete, so the pipe is spelled `-F -` — see readBuffer.
func TestEditPipeIsBodyWhenTitleGiven(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Keep me").stdout)

	in := strings.NewReader("line one\nline two\n")
	if got := gitIssueIO(t, bin, dir, noEditor, in, "edit", id, "-t", "Renamed", "-F", "-"); got.code != 0 {
		t.Fatalf("edit failed: %s", got.stderr)
	}
	shown := gitIssue(t, bin, dir, id).stdout
	if !strings.Contains(shown, "Renamed") || !strings.Contains(shown, "line one") {
		t.Errorf("title or body wrong\n%s", shown)
	}
}

// -F - is the explicit spelling of the same thing.
func TestEditFileDashIsStdin(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title").stdout)

	in := strings.NewReader("Title\n\nfrom stdin via -F -\n")
	if got := gitIssueIO(t, bin, dir, noEditor, in, "edit", id, "-F", "-"); got.code != 0 {
		t.Fatalf("edit -F - failed: %s", got.stderr)
	}
	if shown := gitIssue(t, bin, dir, id).stdout; !strings.Contains(shown, "from stdin via -F -") {
		t.Errorf("stdin was not read\n%s", shown)
	}
}

// A pipe posts a comment without -m, and without an editor.
func TestCommentReadsPipe(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title").stdout)

	in := strings.NewReader("Confirmed on 2.42.\n")
	posted := gitIssueIO(t, bin, dir, noEditor, in, "comment", id)
	if posted.code != 0 {
		t.Fatalf("comment failed: %s", posted.stderr)
	}
	if shown := gitIssue(t, bin, dir, id).stdout; !strings.Contains(shown, "Confirmed on 2.42.") {
		t.Errorf("the piped comment did not land\n%s", shown)
	}
}

// -m and -F both giving the text is refused.
func TestCommentRefusesMessageAndFile(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title").stdout)

	got := gitIssueEnv(t, bin, dir, noEditor, "comment", id, "-m", "x", "-F", "-")
	if got.code == 0 || !strings.Contains(got.stderr, "both give the text") {
		t.Errorf("exit %d, stderr %q", got.code, got.stderr)
	}
}

// A comment is rewritten from a pipe too.
func TestEditCommentReadsPipe(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title").stdout)
	entry := strings.TrimSpace(gitIssue(t, bin, dir, "comment", id, "-m", "first draft").stdout)

	in := strings.NewReader("second draft\n")
	if got := gitIssueIO(t, bin, dir, noEditor, in, "edit", id, entry, "-F", "-"); got.code != 0 {
		t.Fatalf("edit comment failed: %s", got.stderr)
	}
	shown := gitIssue(t, bin, dir, id).stdout
	if !strings.Contains(shown, "second draft") || strings.Contains(shown, "first draft") {
		t.Errorf("the comment was not rewritten\n%s", shown)
	}
}

// runWithDeadPipe runs the binary with stdin held open on a pipe nobody ever
// writes to and nobody ever closes — a script, a git hook and a CI runner all
// hand one down. A command the flags already complete must finish anyway; one
// that reads it blocks until the deadline kills it.
func runWithDeadPipe(t *testing.T, bin, dir string, args ...string) result {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close() // held open for the whole run: the read never sees EOF

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "TZ="+goldenTZ), noEditor...)
	cmd.Stdin = r

	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%v blocked on stdin nobody is feeding", args)
	}

	code := 0
	var exit *exec.ExitError
	if err != nil {
		if !asExitError(err, &exit) {
			t.Fatalf("run %v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

// A request the flags already complete must never read stdin. Non-terminal
// stdin is not the same thing as a pipe somebody is feeding, and reading one
// nobody closes hangs the process for good — which is what `git issue edit <id>
// -l bug` used to do from a script or a hook.
func TestCompleteRequestDoesNotReadStdin(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title", "-d", "body").stdout)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"edit one field", []string{"edit", id, "-l", "bug"}},
		{"edit a scalar", []string{"edit", id, "--milestone", "v2"}},
		{"add with a title", []string{"add", "-t", "Filed from a script", "--type", "bug"}},
		{"comment with -m", []string{"comment", id, "-m", "from a script"}},
		{"close", []string{"close", id}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runWithDeadPipe(t, bin, dir, tc.args...); got.code != 0 {
				t.Errorf("exit %d, stderr %q", got.code, got.stderr)
			}
		})
	}
}

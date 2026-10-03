package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// editIn edits a fixture issue through `git issue edit` with a scripted
// editor, and returns what the command printed.
func editIn(t *testing.T, bin, dir, id, buffer string, args ...string) result {
	t.Helper()
	env, _ := fakeEditor(t, buffer)
	return gitIssueEnv(t, bin, dir, []string{env}, append([]string{"edit", id}, args...)...)
}

// edit hands the issue as it stands to the editor, and writes back what
// changed there.
func TestEdit(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	added := gitIssue(t, bin, dir, "add", "-t", "Original title", "-d", "Original body",
		"--type", "bug", "-l", "design,storage", "-a", "bob@example.com", "-m", "v1")
	id := strings.TrimSpace(added.stdout)

	// The buffer opens on the issue's title and description; the rest of its
	// fields are echoed, read-only, inside the ignored block.
	env, opened := fakeEditor(t, "A new title\n\nA rewritten body.\n")
	got := gitIssueEnv(t, bin, dir, []string{env}, "edit", id)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}

	buffer, err := os.ReadFile(opened)
	if err != nil {
		t.Fatal(err)
	}
	if title, _, _ := strings.Cut(string(buffer), "\n"); title != "# Original title" {
		t.Errorf("first line is %q, want the current title as a heading", title)
	}
	editable, block, found := strings.Cut(string(buffer), "<!---")
	if !found || !strings.Contains(block, "-->") {
		t.Fatalf("no ignored block in the buffer\n%s", buffer)
	}
	if !strings.Contains(editable, "Original body") {
		t.Errorf("the current description was not prefilled\n%s", buffer)
	}
	// The metadata is echoed in the block, and only there.
	for _, want := range []string{"bug", "design, storage", "bob@example.com", "v1"} {
		if !strings.Contains(block, want) {
			t.Errorf("ignored block is missing %q\n%s", want, block)
		}
		if strings.Contains(editable, want) {
			t.Errorf("%q leaked into the editable part of the buffer\n%s", want, buffer)
		}
	}

	// The report names only the fields the buffer can move.
	if !strings.Contains(got.stdout, "updated "+id) {
		t.Errorf("no updated line\n%s", got.stdout)
	}
	if want := "title, description changed"; !strings.Contains(got.stdout, want) {
		t.Errorf("want %q\n%s", want, got.stdout)
	}

	shown := gitIssue(t, bin, dir, id).stdout
	// Title and body changed; the metadata the buffer only echoed is untouched.
	for _, want := range []string{"A new title", "A rewritten body.", "Type:      bug", "Labels:    design, storage", "Milestone: v1"} {
		if !strings.Contains(shown, want) {
			t.Errorf("show is missing %q\n%s", want, shown)
		}
	}
	for _, gone := range []string{"Original title", "Original body"} {
		if strings.Contains(shown, gone) {
			t.Errorf("show still carries %q\n%s", gone, shown)
		}
	}
}

// show is read-only now: editing is a separate command, and show refuses the
// flag rather than silently ignoring it.
func TestShowDoesNotEdit(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title").stdout)

	env, opened := fakeEditor(t, "Edited\n\n")
	for _, args := range [][]string{{"show", id, "--edit"}, {"show", "-e", id}, {id, "--edit"}} {
		got := gitIssueEnv(t, bin, dir, []string{env}, args...)
		if got.code == 0 {
			t.Errorf("%v was accepted: %q", args, got.stdout)
		}
		if _, err := os.Stat(opened); err == nil {
			t.Errorf("%v opened an editor", args)
		}
	}
}

// An edit appends one event per changed field and touches nothing else. This
// is the invariant that makes concurrent edits to different fields merge: a
// snapshot would carry every field's value and clobber the lot.
func TestEditWritesOneEventPerChange(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title", "-l", "a,b").stdout)
	before := noteLines(t, dir, id)

	got := gitIssueEnv(t, bin, dir, noEditor, "edit", id, "-l", "c", "--remove-label", "a")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}

	after := noteLines(t, dir, id)
	// Every line that was there is still there, byte for byte.
	if len(after) <= len(before) || strings.Join(after[:len(before)], "\n") != strings.Join(before, "\n") {
		t.Fatalf("the existing events did not survive the append\n--- before ---\n%s\n--- after ---\n%s",
			strings.Join(before, "\n"), strings.Join(after, "\n"))
	}

	added := after[len(before):]
	if len(added) != 2 {
		t.Fatalf("want one remove and one add, got %d events:\n%s", len(added), strings.Join(added, "\n"))
	}
	if !strings.Contains(added[0], `"op":"label.remove"`) || !strings.Contains(added[0], `"ref":"`) {
		t.Errorf("first new event is not a remove naming an add: %s", added[0])
	}
	// Addressed by event id, never by value: the removed label's name must not
	// appear in the remove at all.
	if strings.Contains(added[0], `"val"`) {
		t.Errorf("the remove carries a value: %s", added[0])
	}
	if !strings.Contains(added[1], `"op":"label.add"`) || !strings.Contains(added[1], `"val":"c"`) {
		t.Errorf("second new event is not the added label: %s", added[1])
	}
}

// A cleared field is set to null, which docs/issues.md gives as the way to say
// "no milestone" — not to the empty string, and not by deleting an event.
func TestEditClearsWithNull(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title", "-m", "v1").stdout)

	if got := gitIssueEnv(t, bin, dir, noEditor, "edit", id, "--milestone", "none"); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}

	lines := noteLines(t, dir, id)
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"op":"milestone"`) || !strings.Contains(last, `"val":null`) {
		t.Errorf("want a null milestone, got %s", last)
	}
	if shown := gitIssue(t, bin, dir, id).stdout; strings.Contains(shown, "Milestone") {
		t.Errorf("a cleared milestone is still rendered\n%s", shown)
	}
}

// An edit that changes nothing writes nothing: no event, no commit, no ref
// movement.
func TestEditWithoutChanges(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title", "-d", "Body").stdout)
	before := gitIn(t, dir, "rev-parse", "refs/notes/issues/open")

	// An editor that leaves the buffer exactly as it found it.
	got := gitIssueEnv(t, bin, dir, []string{"GIT_EDITOR=true"}, "edit", id)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "nothing changed") {
		t.Errorf("want 'nothing changed', got %q", got.stdout)
	}
	if after := gitIn(t, dir, "rev-parse", "refs/notes/issues/open"); after != before {
		t.Error("an edit that changed nothing moved the ref")
	}
}

// Blanking the title aborts, and aborting writes nothing.
func TestEditEmptyTitleAborts(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title").stdout)
	before := gitIn(t, dir, "rev-parse", "refs/notes/issues/open")

	got := editIn(t, bin, dir, id, "\n\nbody")
	if got.code != 1 || !strings.Contains(got.stderr, "aborting") {
		t.Errorf("exit %d, stderr %q", got.code, got.stderr)
	}
	if after := gitIn(t, dir, "rev-parse", "refs/notes/issues/open"); after != before {
		t.Error("an aborted edit moved the ref")
	}
}

// Without an editor there is nothing to open, and saying so beats hanging on
// a program nobody can see.
func TestEditWithoutEditor(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title").stdout)

	got := gitIssueEnv(t, bin, dir, noEditor, "edit", id)
	if got.code != 1 || !strings.Contains(got.stderr, "no editor to open") {
		t.Errorf("exit %d, stderr %q", got.code, got.stderr)
	}
}

// noteLines is an entity's blob, split into lines, read with stock git.
func noteLines(t *testing.T, dir, id string) []string {
	t.Helper()
	cmd := exec.Command("git", "notes", "--ref=issues/open", "show", fullID(t, dir, id))
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("notes show: %v", err)
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

// A field flag writes the change straight away. The editor is what opens when
// nothing else said what to change, the way git commit opens one without -m —
// so a flag needs no editor at all, and this asserts that with none available.
func TestEditFieldFlagsWithoutEditor(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Original", "-l", "design").stdout)

	got := gitIssueEnv(t, bin, dir, noEditor, "edit", id, "-t", "A new title", "--type", "bug")
	if got.code != 0 {
		t.Fatalf("edit: exit %d, stderr %q", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "title, type changed") {
		t.Errorf("did not report what changed: %q", got.stdout)
	}

	shown := gitIssue(t, bin, dir, "show", id).stdout
	if !strings.Contains(shown, "A new title") {
		t.Errorf("the title did not change:\n%s", shown)
	}
	hasHeader(t, shown, "Type", "bug")
	hasHeader(t, shown, "Labels", "design") // untouched by a flag that never named it
}

// -l adds to the list rather than replacing it, and --remove-label takes one
// member away. Replacing is what the editor does, where the whole list is in
// front of you.
func TestEditListFlagsAddAndRemove(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title", "-l", "design").stdout)

	if got := gitIssueEnv(t, bin, dir, noEditor, "edit", id, "-l", "ui,perf"); got.code != 0 {
		t.Fatalf("add labels: %s", got.stderr)
	}
	hasHeader(t, gitIssue(t, bin, dir, "show", id).stdout, "Labels", "design, perf, ui")

	if got := gitIssueEnv(t, bin, dir, noEditor, "edit", id, "--remove-label", "design"); got.code != 0 {
		t.Fatalf("remove label: %s", got.stderr)
	}
	hasHeader(t, gitIssue(t, bin, dir, "show", id).stdout, "Labels", "perf, ui")
}

// 'none' empties a field, whichever kind it is — the same word the filters
// already use for "has none", rather than a --no-<field> flag apiece.
func TestEditNoneEmptiesAField(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title", "-l", "design,ui", "-m", "v1").stdout)

	if got := gitIssueEnv(t, bin, dir, noEditor, "edit", id, "-l", "none", "--milestone", "none"); got.code != 0 {
		t.Fatalf("clear: %s", got.stderr)
	}
	shown := gitIssue(t, bin, dir, "show", id).stdout
	for _, gone := range []string{"Labels:", "Milestone:"} {
		if strings.Contains(shown, gone) {
			t.Errorf("%s survived being emptied:\n%s", gone, shown)
		}
	}

	// A cleared scalar is null rather than an empty string — see
	// docs/issues.md — which is what reads back as deliberately unset.
	var cleared bool
	for _, line := range noteLines(t, dir, id) {
		if strings.Contains(line, `"op":"milestone"`) && strings.Contains(line, `"val":null`) {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("no null milestone event was written:\n%s", strings.Join(noteLines(t, dir, id), "\n"))
	}

	// A title has no cleared state, since an issue needs one, so the word is
	// simply a title there.
	if got := gitIssueEnv(t, bin, dir, noEditor, "edit", id, "-t", "none"); got.code != 0 {
		t.Fatalf("title: %s", got.stderr)
	}
	if shown := gitIssue(t, bin, dir, "show", id).stdout; !strings.Contains(shown, "\n    none\n") {
		t.Errorf("'none' was not taken as a title:\n%s", shown)
	}
}

// --parent files an issue under another, and 'none' detaches it. The link is
// a scalar on the child, so detaching writes an event with an empty ref rather
// than removing anything.
func TestEditParent(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	epic := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Epic", "--type", "epic").stdout)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Child").stdout)

	if got := gitIssueEnv(t, bin, dir, noEditor, "edit", id, "--parent", epic); got.code != 0 {
		t.Fatalf("file under: %s", got.stderr)
	}
	hasHeader(t, gitIssue(t, bin, dir, "show", id).stdout, "Parent", epic+"  Epic")

	if got := gitIssueEnv(t, bin, dir, noEditor, "edit", id, "--parent", "none"); got.code != 0 {
		t.Fatalf("detach: %s", got.stderr)
	}
	if shown := gitIssue(t, bin, dir, "show", id).stdout; strings.Contains(shown, "Parent:") {
		t.Errorf("the issue is still filed somewhere:\n%s", shown)
	}
	if shown := gitIssue(t, bin, dir, "show", epic).stdout; strings.Contains(shown, "Children:") {
		t.Errorf("the epic still claims a child:\n%s", shown)
	}
}

// A loop is refused where it can be seen. That is a courtesy and not a
// guarantee — the field is last-write-wins, so two clones can still make one
// between them, which is why a tree breaks cycles when it draws them.
func TestEditParentRefusesALoop(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	epic := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Epic").stdout)
	child := strings.TrimSpace(gitIssue(t, bin, dir, "add", epic, "-t", "Child").stdout)
	grand := strings.TrimSpace(gitIssue(t, bin, dir, "add", child, "-t", "Grandchild").stdout)

	if got := gitIssueEnv(t, bin, dir, noEditor, "edit", child, "--parent", child); got.code == 0 {
		t.Errorf("an issue was filed under itself")
	}
	got := gitIssueEnv(t, bin, dir, noEditor, "edit", epic, "--parent", grand)
	if got.code == 0 || !strings.Contains(got.stderr, "loop") {
		t.Errorf("a loop through a grandchild was allowed: exit %d, %q", got.code, got.stderr)
	}
}

// The link is shown read-only in the ignored block, so an editor session sees
// it but cannot change it: saving the buffer renames the issue and leaves the
// parent in place.
func TestEditorCannotDetachAnIssue(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	epic := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Epic").stdout)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", epic, "-t", "Child").stdout)

	env, opened := fakeEditor(t, "Renamed in the editor\n\nbody")
	if got := gitIssueEnv(t, bin, dir, []string{env}, "edit", id); got.code != 0 {
		t.Fatalf("edit: %s", got.stderr)
	}

	buffer, err := os.ReadFile(opened)
	if err != nil {
		t.Fatal(err)
	}
	// The link is echoed, but only inside the block the round trip discards.
	editable, block, _ := strings.Cut(string(buffer), "<!---")
	if strings.Contains(editable, "parent") {
		t.Errorf("the link was in the editable part of the buffer:\n%s", buffer)
	}
	if !strings.Contains(block, "parent") || !strings.Contains(block, epic) {
		t.Errorf("the link was not echoed for reference:\n%s", buffer)
	}
	shown := gitIssue(t, bin, dir, "show", id).stdout
	if !strings.Contains(shown, "Renamed in the editor") {
		t.Errorf("the editor's title did not land:\n%s", shown)
	}
	hasHeader(t, shown, "Parent", epic+"  Epic")
}

// -m is a comment's text on this command, so it may not also be the milestone,
// and a field flag has nothing to say about a comment.
func TestEditFlagsAndCommentsDoNotMix(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Title").stdout)
	entry := strings.TrimSpace(gitIssue(t, bin, dir, "comment", id, "-m", "first").stdout)

	if got := gitIssueEnv(t, bin, dir, noEditor, "edit", id, "-m", "text"); got.code == 0 {
		t.Errorf("-m was accepted for an issue")
	}
	got := gitIssueEnv(t, bin, dir, noEditor, "edit", id, entry, "-t", "new title")
	if got.code == 0 || !strings.Contains(got.stderr, "only its text") {
		t.Errorf("a field flag was accepted for a comment: exit %d, %q", got.code, got.stderr)
	}
}

// headerLine finds one line of a rendering's aligned key/value block. The block is
// padded to the widest key it happens to hold, so a test asserts on the value
// rather than on how far it was indented.
func headerLine(shown, key string) (string, bool) {
	for _, line := range strings.Split(shown, "\n") {
		if rest, ok := strings.CutPrefix(line, key+":"); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// hasHeader asserts that a rendering carries this key with this value.
func hasHeader(t *testing.T, shown, key, want string) {
	t.Helper()
	got, ok := headerLine(shown, key)
	if !ok {
		t.Errorf("no %s: line\n%s", key, shown)
		return
	}
	if got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// emptyRepo is a repository with no issues in it at all, for the tests that
// file their own. The fixture is a fixed set of blobs and stays that way.
func emptyRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "hdweiss@gmail.com"},
		{"config", "user.name", "Henning Weiss"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return dir
}

// hierarchy files a small tree in a fresh repo and returns the ids, so the
// tests below assert on shape rather than on whatever the fixture happens to
// hold. The fixture's own parent points outside the repository, which is a
// case worth keeping there and a poor base for these.
//
//	epic
//	├─ bug
//	│  └─ filters
//	└─ push
//	loose
func hierarchy(t *testing.T, bin, dir string) (epic, bug, filters, push, loose string) {
	t.Helper()
	add := func(args ...string) string {
		t.Helper()
		got := gitIssue(t, bin, dir, append([]string{"add"}, args...)...)
		if got.code != 0 {
			t.Fatalf("add %v: %s", args, got.stderr)
		}
		return strings.TrimSpace(got.stdout)
	}
	epic = add("-t", "Azure DevOps bridge", "--type", "epic")
	bug = add(epic, "-t", "Area picker drops the subtree", "--type", "bug")
	filters = add(bug, "-t", "Raw-state filters", "--type", "task")
	push = add(epic, "-t", "Push", "--type", "task")
	loose = add("-t", "Anchor blobs are pruned by gc")
	return epic, bug, filters, push, loose
}

// lines is the listing split into rows, with the ids on the front of each.
func lines(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// `add <id>` files the new issue under that one, and show reads the link back
// from both ends: the parent it names, and the issues that name it.
func TestAddFilesUnderAnIssue(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	epic, bug, filters, push, _ := hierarchy(t, bin, dir)

	child := gitIssue(t, bin, dir, "show", bug).stdout
	if !strings.Contains(child, "Parent:   "+epic+"  Azure DevOps bridge") {
		t.Errorf("the child does not name its parent, or does not resolve it:\n%s", child)
	}
	if !strings.Contains(child, "Children: "+filters+"  Raw-state filters") {
		t.Errorf("the child does not list its own child:\n%s", child)
	}

	// Several children print as one block: the first on the header line, the
	// rest aligned under it rather than repeating the key. Which one leads is
	// a listing's own order, and these were filed in the same second, so it is
	// the id tie-break — not something to assert.
	parent := gitIssue(t, bin, dir, "show", epic).stdout
	first, rest := push, bug
	if strings.Contains(parent, "Children: "+bug) {
		first, rest = bug, push
	}
	if !strings.Contains(parent, "Children: "+first) || !strings.Contains(parent, "\n          "+rest) {
		t.Errorf("the epic does not list both children as one block:\n%s", parent)
	}
	if strings.Contains(parent, "Parent:") {
		t.Errorf("the epic has no parent and should print no such line:\n%s", parent)
	}
}

// An id after `list` is the same slot `add` uses: what this command hangs off.
// It selects the whole subtree beneath that issue — the same issues whether the
// listing is drawn as a tree or, down a pipe, flat.
func TestListUnderAnIssue(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	epic, bug, filters, push, loose := hierarchy(t, bin, dir)

	got := gitIssue(t, bin, dir, "list", epic)
	if got.code != 0 {
		t.Fatalf("list under the epic: %s", got.stderr)
	}
	if n := len(lines(got.stdout)); n != 3 {
		t.Fatalf("listed %d issues under the epic, want its whole subtree of 3:\n%s", n, got.stdout)
	}
	for _, want := range []string{bug, push, filters} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("%s missing from the epic's subtree:\n%s", want, got.stdout)
		}
	}
	if strings.Contains(got.stdout, epic) {
		t.Errorf("the named issue is not itself filed under the epic; it should not list flat:\n%s", got.stdout)
	}

	// `none` is the same slot with an empty parent, but "filed under nothing"
	// is read one level deep — the unparented roots, not every issue.
	roots := gitIssue(t, bin, dir, "list", "none")
	if n := len(lines(roots.stdout)); n != 2 {
		t.Fatalf("listed %d issues under nothing, want 2:\n%s", n, roots.stdout)
	}
	for _, want := range []string{epic, loose} {
		if !strings.Contains(roots.stdout, want) {
			t.Errorf("%s missing from the unfiled issues:\n%s", want, roots.stdout)
		}
	}
}

// --tree nests, keeps the id/status/type columns as a grid, and puts the spine
// in the title column.
func TestListTree(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	epic, bug, filters, push, loose := hierarchy(t, bin, dir)

	got := gitIssue(t, bin, dir, "list", "--tree")
	if got.code != 0 {
		t.Fatalf("list --tree: %s", got.stderr)
	}
	rows := lines(got.stdout)
	if len(rows) != 5 {
		t.Fatalf("a tree printed %d rows, want the 5 a flat listing prints:\n%s", len(rows), got.stdout)
	}

	// Every row opens with its own id whatever depth it sits at, so the id
	// column stays a grid to scan down and copy out of.
	at := map[string]int{}
	for i, row := range rows {
		id, _, _ := strings.Cut(row, " ")
		at[id] = i
	}
	for _, id := range []string{epic, bug, filters, push, loose} {
		if _, ok := at[id]; !ok {
			t.Fatalf("%s is not the first thing on any row:\n%s", id, got.stdout)
		}
	}

	// The two roots are undrawn, and every child sits below its own parent
	// with a connector. The five are created in the same second, so which
	// root comes first is the id tie-break rather than anything to assert.
	for _, id := range []string{epic, loose} {
		if row := rows[at[id]]; strings.ContainsAny(row, "├└│") {
			t.Errorf("a root is drawn with a spine: %q", row)
		}
	}
	for _, pair := range []struct{ child, parent string }{
		{bug, epic}, {push, epic}, {filters, bug},
	} {
		if at[pair.child] <= at[pair.parent] {
			t.Errorf("%s is listed above its parent %s:\n%s", pair.child, pair.parent, got.stdout)
		}
	}
	if row := rows[at[filters]]; !strings.Contains(row, "└─ Raw-state filters") {
		t.Errorf("the grandchild is not drawn under its parent: %q", row)
	}
	if at[filters] != at[bug]+1 {
		t.Errorf("the grandchild is not directly below its parent:\n%s", got.stdout)
	}
	if strings.Contains(got.stdout, TreeMarkText) {
		t.Errorf("nothing here hangs off a missing parent, so nothing should be marked:\n%s", got.stdout)
	}
}

// A subtree is drawn under the issue it was asked for: that issue is the root,
// so it is listed, and it carries no "has a parent you cannot see" mark —
// the reader just named it.
func TestListTreeUnderAnIssue(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	_, bug, filters, _, _ := hierarchy(t, bin, dir)

	got := gitIssue(t, bin, dir, "list", "--tree", bug)
	rows := lines(got.stdout)
	if len(rows) != 2 {
		t.Fatalf("subtree printed %d rows, want the bug and its child:\n%s", len(rows), got.stdout)
	}
	if !strings.HasPrefix(rows[0], bug) || strings.Contains(rows[0], TreeMarkText) {
		t.Errorf("the named issue is not the unmarked root of its own subtree: %q", rows[0])
	}
	if !strings.HasPrefix(rows[1], filters) || !strings.Contains(rows[1], "└─") {
		t.Errorf("the child is not drawn under it: %q", rows[1])
	}
}

// A filter selects the rows; the tree only nests what was selected. So a
// listing prints the same issues either way, and a child whose parent the
// filter dropped roots — marked, because it does have one.
func TestListTreeMarksAParentTheFilterDropped(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	_, bug, _, _, _ := hierarchy(t, bin, dir)

	got := gitIssue(t, bin, dir, "list", "--tree", "--type", "bug")
	rows := lines(got.stdout)
	if len(rows) != 1 {
		t.Fatalf("printed %d rows, want the one bug:\n%s", len(rows), got.stdout)
	}
	if !strings.HasPrefix(rows[0], bug) || !strings.Contains(rows[0], TreeMarkText) {
		t.Errorf("the bug's parent is filtered out and the row does not say so: %q", rows[0])
	}
}

// --tree draws a spine and --format medium prints a block per issue; there is
// nowhere in a header block to put one, so the two are refused together rather
// than one quietly winning.
func TestListTreeRefusesMedium(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	got := gitIssue(t, bin, dir, "list", "--tree", "--format", "medium")
	if got.code == 0 || !strings.Contains(got.stderr, "use one") {
		t.Errorf("exit %d, stderr %q", got.code, got.stderr)
	}
}

// An id with no word shows an issue and a listing flag may not turn that into a
// listing. The message says where to type it instead.
func TestBareIDRefusesTree(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	epic, _, _, _, _ := hierarchy(t, bin, dir)

	for _, flag := range []string{"--tree", "--no-tree"} {
		got := gitIssue(t, bin, dir, epic, flag)
		if got.code == 0 || !strings.Contains(got.stderr, "git issue list "+epic) {
			t.Errorf("%s: exit %d, stderr %q", flag, got.code, got.stderr)
		}
	}
}

// TreeMarkText is what marks a root that hangs off something the listing does
// not hold. Spelled out here rather than imported so that a change to the
// rendering has to be made deliberately in both places.
const TreeMarkText = "↑ "

// A link other than a parent is written on one end and read from both: the
// issue that owns it names what it points at, and the far end derives the
// inverse. --no-rel takes it away again by naming the kind alone.
func TestRelationsReadFromBothEnds(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	epic, bug, _, _, loose := hierarchy(t, bin, dir)

	if got := gitIssue(t, bin, dir, "edit", bug, "--rel", "blocked-by:"+loose); got.code != 0 {
		t.Fatalf("linking: %s", got.stderr)
	}

	blocked := gitIssue(t, bin, dir, "show", bug).stdout
	hasHeader(t, blocked, "Blocked by", loose+"  Anchor blobs are pruned by gc")
	blocker := gitIssue(t, bin, dir, "show", loose).stdout
	hasHeader(t, blocker, "Blocks", bug+"  Area picker drops the subtree")
	// The inverse is derived, never stored: the blocker's own blob says nothing.
	if body := gitIssue(t, bin, dir, "log", "-p", loose).stdout; strings.Contains(body, "rel.add") {
		t.Errorf("the far end stored an event for a link it does not own:\n%s", body)
	}

	// Filing is still its own spelling of the same field, and the two coexist.
	hasHeader(t, blocked, "Parent", epic+"  Azure DevOps bridge")

	if got := gitIssue(t, bin, dir, "edit", bug, "--no-rel", "blocked-by"); got.code != 0 {
		t.Fatalf("unlinking: %s", got.stderr)
	}
	if got := gitIssue(t, bin, dir, "show", bug).stdout; strings.Contains(got, "Blocked by") {
		t.Errorf("the link survived --no-rel:\n%s", got)
	}
	if got := gitIssue(t, bin, dir, "show", loose).stdout; strings.Contains(got, "Blocks") {
		t.Errorf("the derived inverse survived the removal:\n%s", got)
	}
}

// `add --rel` files a link at creation, the way the positional files a parent.
// A symmetric kind prints once on both ends, however many of them stored it.
func TestAddWritesRelations(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	_, bug, _, _, loose := hierarchy(t, bin, dir)

	got := gitIssue(t, bin, dir, "add", "-t", "Repack after import", "--rel", "related:"+bug, "--rel", "blocked-by:"+loose)
	if got.code != 0 {
		t.Fatalf("add with links: %s", got.stderr)
	}
	id := strings.TrimSpace(got.stdout)

	shown := gitIssue(t, bin, dir, "show", id).stdout
	hasHeader(t, shown, "Related", bug+"  Area picker drops the subtree")
	hasHeader(t, shown, "Blocked by", loose+"  Anchor blobs are pruned by gc")
	// The far end of a symmetric link shows it once, under the same name.
	far := gitIssue(t, bin, dir, "show", bug).stdout
	hasHeader(t, far, "Related", id+"  Repack after import")
	if strings.Count(far, id) != 1 {
		t.Errorf("the symmetric link printed more than once:\n%s", far)
	}
}

// A symmetric link has no dependent end, so a repository that has seen it from
// both sides holds two members saying the same thing — which is what a pull
// produces, since a tracker stores such a link on both objects. Letting go of
// one member would leave the link standing, so `--no-rel` lets go of both.
//
// The asymmetric kinds are the control: their far end stores nothing, so
// nothing there can need retracting.
func TestSymmetricLinksLetGoAtBothEnds(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	_, bug, _, _, loose := hierarchy(t, bin, dir)

	// Writing one is still one event at one end. Nothing here files a link
	// twice; the second member only ever arrives from somewhere else.
	if got := gitIssue(t, bin, dir, "edit", bug, "--rel", "related:"+loose); got.code != 0 {
		t.Fatalf("linking: %s", got.stderr)
	}
	if body := gitIssue(t, bin, dir, "log", "-p", loose).stdout; strings.Contains(body, "rel.add") {
		t.Errorf("the far end stored an event for a link the near end wrote:\n%s", body)
	}
	// And letting go of it is one issue's business while it is one member.
	got := gitIssue(t, bin, dir, "edit", bug, "--no-rel", "related")
	if got.code != 0 {
		t.Fatalf("unlinking: %s", got.stderr)
	}
	if strings.Contains(got.stdout, "also detached") {
		t.Errorf("dropping a link only this end held wrote to the far end:\n%s", got.stdout)
	}

	// Both ends write the link, as two clones or a bridged import would.
	for _, args := range [][]string{
		{"edit", bug, "--rel", "related:" + loose},
		{"edit", loose, "--rel", "related:" + bug},
	} {
		if got := gitIssue(t, bin, dir, args...); got.code != 0 {
			t.Fatalf("%v: %s", args, got.stderr)
		}
	}
	// Two members, one link: a reader shows it once from either end.
	hasHeader(t, gitIssue(t, bin, dir, "show", bug).stdout, "Related", loose+"  Anchor blobs are pruned by gc")
	hasHeader(t, gitIssue(t, bin, dir, "show", loose).stdout, "Related", bug+"  Area picker drops the subtree")

	got = gitIssue(t, bin, dir, "edit", bug, "--no-rel", "related")
	if got.code != 0 {
		t.Fatalf("unlinking: %s", got.stderr)
	}
	// The command wrote to an issue nobody named, so it has to say which.
	if !strings.Contains(got.stdout, "also detached from "+loose) {
		t.Errorf("the far-end write went unreported:\n%s", got.stdout)
	}

	for _, id := range []string{bug, loose} {
		if shown := gitIssue(t, bin, dir, "show", id).stdout; strings.Contains(shown, "Related") {
			t.Errorf("%s still holds the link its other end let go of:\n%s", id, shown)
		}
	}

	// An asymmetric link is stored at one end only, so dropping it is one
	// issue's business and must not reach across.
	if got := gitIssue(t, bin, dir, "edit", bug, "--rel", "blocked-by:"+loose); got.code != 0 {
		t.Fatalf("linking: %s", got.stderr)
	}
	got = gitIssue(t, bin, dir, "edit", bug, "--no-rel", "blocked-by")
	if got.code != 0 {
		t.Fatalf("unlinking: %s", got.stderr)
	}
	if strings.Contains(got.stdout, "also detached") {
		t.Errorf("dropping a one-ended link wrote to the far end:\n%s", got.stdout)
	}
}

// An id that names nothing is refused rather than written: a link to something
// this repository does not hold is a typo far more often than it is a
// cross-repository reference, which docs/issues.md cannot yet spell anyway.
func TestRelationsRefuseWhatTheyCannotResolve(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	_, bug, _, _, _ := hierarchy(t, bin, dir)

	for _, args := range [][]string{
		{"edit", bug, "--rel", "blocked-by:deadbeef"},
		{"edit", bug, "--rel", "blocked-by"},
		{"edit", bug, "--rel", "blocked-by:" + bug},
	} {
		if got := gitIssue(t, bin, dir, args...); got.code == 0 {
			t.Errorf("%v was accepted:\n%s", args, got.stdout)
		}
	}
}

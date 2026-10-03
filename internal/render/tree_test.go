package render

import (
	"bytes"
	"strings"
	"testing"
)

// row is one issue for these tests: an id short enough to read, and a creation
// date, since siblings order on it.
func row(id string, created int64) Row {
	return Row{ID: id, Created: created, Status: "open", Title: id}
}

// ids is what Tree put out, in order, so a test asserts on the shape rather
// than on a rendered string.
func ids(rows []Row) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func prefixes(rows []Row) map[string]string {
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Prefix
	}
	return out
}

// A forest nests under its roots, siblings keep a listing's own order — newest
// first — and each row carries the spine it is drawn with.
func TestTreeNestsAndDraws(t *testing.T) {
	rows := []Row{
		row("epic", 100),
		row("bug", 200),
		row("filters", 300),
		row("push", 250),
		row("loose", 400),
	}
	parent := map[string]string{
		"bug":     "epic",
		"filters": "bug",
		"push":    "epic",
	}

	got, cycles := Tree(rows, parent)
	if len(cycles) != 0 {
		t.Fatalf("cycles = %v, want none", cycles)
	}

	// Roots newest first — loose at 400 before epic at 100 — and each root's
	// children below it in that same order, so push at 250 precedes bug at
	// 200 and the whole listing reads the way a flat one reads.
	want := []string{"loose", "epic", "push", "bug", "filters"}
	if diff := strings.Join(ids(got), ","); diff != strings.Join(want, ",") {
		t.Errorf("order = %s, want %s", diff, strings.Join(want, ","))
	}

	// bug is the last of epic's children, so the level below it is drawn with
	// a gap rather than a bar: there is no sibling left for a bar to lead to.
	for id, want := range map[string]string{
		"loose":   "",
		"epic":    "",
		"push":    treeBranch,
		"bug":     treeLast,
		"filters": treeGap + treeLast,
	} {
		if got := prefixes(got)[id]; got != want {
			t.Errorf("prefix of %s = %q, want %q", id, got, want)
		}
	}
}

// An issue whose parent is not in the listing is a root, and says so. The
// parent may be filtered out, in another repository, or not imported — all the
// same case to a reader, and none of them a reason to hide the issue.
func TestTreeMarksARootThatHasAParent(t *testing.T) {
	rows := []Row{row("orphan", 100), row("free", 200)}
	got, _ := Tree(rows, map[string]string{"orphan": "somewhere-else"})

	if n := len(got); n != len(rows) {
		t.Fatalf("printed %d rows, want %d — a tree lists what a flat listing lists", n, len(rows))
	}
	if p := prefixes(got)["orphan"]; p != TreeMark {
		t.Errorf("prefix of orphan = %q, want %q", p, TreeMark)
	}
	if p := prefixes(got)["free"]; p != "" {
		t.Errorf("prefix of free = %q, want empty", p)
	}
}

// `parent` is last-write-wins, so two clones can file each other's issue under
// the other while offline and both be right. The renderer breaks the loop
// rather than walking it forever, dropping exactly one edge — the lowest id's,
// so every clone breaks the same cycle in the same place.
func TestTreeBreaksACycle(t *testing.T) {
	rows := []Row{row("aaa", 100), row("bbb", 200), row("ccc", 300)}
	parent := map[string]string{"aaa": "bbb", "bbb": "aaa", "ccc": "aaa"}

	got, cycles := Tree(rows, parent)
	if len(got) != 3 {
		t.Fatalf("printed %d rows, want 3", len(got))
	}
	if len(cycles) != 1 || cycles[0] != "aaa" {
		t.Fatalf("cycles = %v, want [aaa]", cycles)
	}
	// aaa loses its edge and roots; bbb keeps its edge and nests under it.
	if p := prefixes(got)["bbb"]; p == "" {
		t.Errorf("bbb roots too, want it nested under aaa")
	}
}

// A self-parent is a cycle of one, and must not hang.
func TestTreeBreaksASelfParent(t *testing.T) {
	rows := []Row{row("solo", 100)}
	got, cycles := Tree(rows, map[string]string{"solo": "solo"})
	if len(got) != 1 || len(cycles) != 1 {
		t.Fatalf("rows %d, cycles %v", len(got), cycles)
	}
}

// The spine is counted against the line's width like every other column, so a
// title still trims to fit rather than wrapping onto a second row.
func TestTreePrefixCountsAgainstTheTitle(t *testing.T) {
	r := Row{ID: "aaaaaaaaaaaabbbb", Status: "open", Title: "a title long enough to be cut", Prefix: treeBranch}
	st := measure([]Row{r}, false)
	st.width = 40

	got := formatRow(r, st)
	if !strings.Contains(got, treeBranch) {
		t.Fatalf("spine missing from %q", got)
	}
	if n := displayWidth(got); n > st.width {
		t.Errorf("row is %d columns wide, over the %d available:\n%q", n, st.width, got)
	}
}

// WriteTree prints what it is given, in the order it is given. Sorting again
// would flatten the forest Tree just built.
func TestWriteTreeKeepsTreeOrder(t *testing.T) {
	rows, _ := Tree([]Row{row("epic", 100), row("kid", 200)}, map[string]string{"kid": "epic"})

	var b bytes.Buffer
	WriteTree(&b, rows, nil)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "epic") || !strings.Contains(lines[1], "kid") {
		t.Errorf("wrote:\n%s", b.String())
	}
}

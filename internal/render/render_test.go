package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hdweiss/git-issue/internal/entity"
)

// A listing's columns are measured on the rows being printed, so a tracker
// that never sets a type does not pay for the column.
func TestFormatRowColumns(t *testing.T) {
	rows := []Row{
		{ID: "aaaaaaaaaaaabbbb", Status: "open", Title: "no type here"},
		{ID: "ccccccccccccdddd", Status: "closed", Type: "bug", Title: "typed", Labels: []string{"design", "bug"}, Comments: 2},
	}

	st := measure(rows, false)
	if st.status != 6 || st.kind != 3 {
		t.Fatalf("measured %+v, want status 6 and kind 3", st)
	}
	want := []string{
		"aaaaaaaaaaaa  open         no type here",
		"cccccccccccc  closed  bug  typed  [bug, design]  💬 2",
	}
	for i, r := range rows {
		if got := formatRow(r, st); got != want[i] {
			t.Errorf("row %d = %q, want %q", i, got, want[i])
		}
	}

	// Drop the typed row and the type column goes with it.
	if st := measure(rows[:1], false); st.kind != 0 {
		t.Errorf("kind = %d for rows with no type, want 0", st.kind)
	}
	if got, want := formatRow(rows[0], measure(rows[:1], false)), "aaaaaaaaaaaa  open  no type here"; got != want {
		t.Errorf("untyped listing = %q, want %q", got, want)
	}
}

// Labels are sorted here rather than trusted from the caller: the fold builds
// them from a map, and anything reaching output has to be in a fixed order.
func TestFormatRowSortsLabelsWithoutMutating(t *testing.T) {
	labels := []string{"stale", "bug", "design"}
	r := Row{ID: "abcdef012345", Status: "open", Title: "t", Labels: labels}
	if got, want := formatRow(r, measure([]Row{r}, false)), "abcdef012345  open  t  [bug, design, stale]"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
	if labels[0] != "stale" {
		t.Errorf("the caller's slice was reordered: %v", labels)
	}
}

// An id needs as many characters as it takes to part it from its nearest
// neighbour, and one more. A set of one needs a single character; an id nobody
// measured needs all of itself, which is the answer that cannot mislead.
func TestUniquify(t *testing.T) {
	u := Uniquify([]string{"abc123", "abc456", "b00", "abd"})
	for id, want := range map[string]int{
		"abc123": 4, // parts from abc456 at the fourth
		"abc456": 4,
		"abd":    3, // parts from the abc pair at the third
		"b00":    1,
	} {
		if got := u.Len(id); got != want {
			t.Errorf("Len(%q) = %d, want %d", id, got, want)
		}
	}
	if got := u.Len("ffff"); got != 4 {
		t.Errorf("an unmeasured id needs %d, want all 4 of itself", got)
	}
	if got := Uniquify([]string{"abc123"}).Len("abc123"); got != 1 {
		t.Errorf("a set of one needs %d characters, want 1", got)
	}
	// A nil set has measured nothing, so every id counts as needing all of
	// itself — a rendering that was handed no measurement paints ids whole.
	var none Unique
	if got := none.Len("abc123"); got != 6 {
		t.Errorf("nil set gave %d, want 6", got)
	}
}

// An id is painted in two pieces, the way jj colours a change id: the prefix a
// reader has to type in yellow, the rest of the fixed-width column in grey.
// Piped, neither piece carries an escape byte.
func TestFormatRowFadesTheUnneededTail(t *testing.T) {
	rows := []Row{
		{ID: "abcdef012345", Status: "open", Title: "t"},
		{ID: "abcdef999999", Status: "open", Title: "t"},
		{ID: "b00000000000", Status: "open", Title: "t"},
	}
	uniq := Uniquify([]string{rows[0].ID, rows[1].ID, rows[2].ID})

	st := measure(rows, true)
	st.uniq = uniq
	want := []string{
		"\x1b[33mabcdef0\x1b[0m\x1b[90m12345\x1b[0m",
		"\x1b[33mabcdef9\x1b[0m\x1b[90m99999\x1b[0m",
		"\x1b[33mb\x1b[0m\x1b[90m00000000000\x1b[0m",
	}
	for i, r := range rows {
		if got := formatRow(r, st); !strings.HasPrefix(got, want[i]) {
			t.Errorf("row %d = %q, want it to open with %q", i, got, want[i])
		}
	}

	plain := measure(rows, false)
	plain.uniq = uniq
	if got := formatRow(rows[0], plain); got != "abcdef012345  open  t" {
		t.Errorf("piped = %q, want no escape bytes", got)
	}
}

// A comment id is measured against its own entity's thread and nothing else,
// which is the scope State.FindComment resolves in. So a thread of a few
// entries addresses in one or two characters however long the entity ids
// around it are.
func TestThreadUniqIsScopedToItsEntity(t *testing.T) {
	thread := []entity.Comment{
		{Event: entity.Event{ID: "a1b2c3d4"}},
		{Event: entity.Event{ID: "a1ffffff"}},
		{Event: entity.Event{ID: "90000000"}},
	}
	u := threadUniq(thread)
	for id, want := range map[string]int{"a1b2c3d4": 3, "a1ffffff": 3, "90000000": 1} {
		if got := u.Len(id); got != want {
			t.Errorf("Len(%q) = %d, want %d", id, got, want)
		}
	}
	// An entity id that happens to share a prefix is not in this set, so it
	// does not lengthen anything: the two are resolved separately.
	if got := u.Len("a1b2c3d4e5f6"); got != 12 {
		t.Errorf("an entity id was measured against the thread: %d", got)
	}
}

// WriteComment and WriteDetail share one walk, so an entry printed on its own
// is byte for byte the block the whole issue printed for it — the replies
// under it included, and only the indent removed.
func TestWriteCommentMatchesItsBlockInTheDetail(t *testing.T) {
	body := func(s string) entity.Value {
		return entity.Value{Present: true, Kind: entity.KindString, Str: s}
	}
	root := entity.Comment{Event: entity.Event{ID: "a1b2c3d4", A: "one@example.com", C: 1}, Body: body("First.")}
	reply := entity.Comment{Event: entity.Event{ID: "90000000", A: "two@example.com", C: 2, Ref: root.ID()}, Body: body("A reply.")}
	other := entity.Comment{Event: entity.Event{ID: "a1ffffff", A: "one@example.com", C: 3}, Body: body("Unrelated.")}

	d := Detail{
		Kind:  "issue",
		ID:    "ffff0000",
		Title: "A title",
		State: entity.State{
			Thread:    []entity.Comment{root, reply, other},
			Reactions: []entity.Reaction{{Author: "two@example.com", Value: "+1", Target: root.ID()}},
		},
		Loc: time.UTC,
	}

	var full, alone bytes.Buffer
	WriteDetail(&full, d, Uniquify([]string{d.ID}))
	WriteComment(&alone, d, root)

	// The block runs from this entry's own line up to the blank line that
	// separates it from the next root entry.
	rest := full.String()[strings.Index(full.String(), "comment "+root.ID()):]
	block := rest[:strings.Index(rest, "\ncomment "+other.ID())]
	if got := alone.String(); got != block {
		t.Errorf("differs\n--- in the detail ---\n%s\n--- on its own ---\n%s", block, got)
	}
	// The reply is part of that block, and the entry beside it is not.
	if !strings.Contains(alone.String(), "A reply.") || strings.Contains(alone.String(), "Unrelated.") {
		t.Errorf("showed the wrong entries:\n%s", alone.String())
	}
	// Nothing precedes the entry, so there is no blank line to open with.
	if strings.HasPrefix(alone.String(), "\n") {
		t.Errorf("opened with a blank line:\n%q", alone.String())
	}
}

// An entity type puts its own vocabulary on a thread entry through Detail.Entry
// — where a review comment sits, say — and this package prints it without ever
// learning what it means. Author and date stay first, and an entry the hook says
// nothing about carries nothing extra.
func TestEntryHeadersComeFromTheCaller(t *testing.T) {
	body := func(s string) entity.Value {
		return entity.Value{Present: true, Kind: entity.KindString, Str: s}
	}
	root := entity.Comment{Event: entity.Event{ID: "a1b2c3d4", A: "one@example.com", C: 1}, Body: body("First.")}
	other := entity.Comment{Event: entity.Event{ID: "90000000", A: "two@example.com", C: 2}, Body: body("Unrelated.")}

	d := Detail{
		Kind:  "review",
		ID:    "ffff0000",
		Title: "A title",
		State: entity.State{Thread: []entity.Comment{root, other}},
		Loc:   time.UTC,
		Entry: func(c entity.Comment) []Header {
			if c.ID() != root.ID() {
				return nil
			}
			return []Header{
				{Key: "File", Value: "main.go:5-6"},
				{Key: "Revision", Value: "a46a42cdac39"},
			}
		},
	}

	var buf bytes.Buffer
	WriteDetail(&buf, d, Uniquify([]string{d.ID}))
	out := buf.String()

	block := out[strings.Index(out, "comment "+root.ID()):]
	block = block[:strings.Index(block, "comment "+other.ID())]
	for _, want := range []string{"Author:   one@example.com", "File:     main.go:5-6", "Revision: a46a42cdac39"} {
		if !strings.Contains(block, want) {
			t.Errorf("the entry is missing %q:\n%s", want, block)
		}
	}
	if strings.Index(block, "Author:") > strings.Index(block, "File:") {
		t.Errorf("the caller's headers should follow author and date:\n%s", block)
	}

	rest := out[strings.Index(out, "comment "+other.ID()):]
	if strings.Contains(rest, "File:") {
		t.Errorf("an entry the caller said nothing about carries nothing:\n%s", rest)
	}
}

// A prefix longer than the column is painted whole rather than half-faded:
// every character on screen is one the reader needs.
func TestPaintIDNeedsMoreThanIsShown(t *testing.T) {
	if got, want := paintID("abcdef012345", 40, true), "\x1b[33mabcdef012345\x1b[0m"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

// With colour the id is yellow like a commit line in git log, and the status
// word gives way to a dot: green for open and purple for closed. An
// unrecognised status is non-terminal, so it dots green.
//
// The ids here go unmeasured, so they paint whole — the two-tone case is
// TestFormatRowFadesTheUnneededTail.
func TestFormatRowColor(t *testing.T) {
	st := measure(nil, true)
	for _, tc := range []struct {
		row  Row
		want string
	}{
		{Row{ID: "abcdef012345", Status: "open", Title: "t"}, "\x1b[33mabcdef012345\x1b[0m  \x1b[32m●\x1b[0m  t"},
		{Row{ID: "abcdef012345", Status: "closed", Closed: true, Title: "t"}, "\x1b[33mabcdef012345\x1b[0m  \x1b[35m●\x1b[0m  t"},
		{Row{ID: "abcdef012345", Status: "in progress", Title: "t"}, "\x1b[33mabcdef012345\x1b[0m  \x1b[32m●\x1b[0m  t"},
	} {
		if got := formatRow(tc.row, st); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.row.Status, got, tc.want)
		}
	}
}

// With colour on, the id keeps its yellow, labels are bold and right-justified,
// and the comment count is pushed to the right edge behind them.
func TestFormatRowColorTail(t *testing.T) {
	r := Row{ID: "abcdef012345", Status: "open", Title: "t", Labels: []string{"storage"}, Comments: 5}
	st := measure([]Row{r}, true)
	st.width = 40

	got := formatRow(r, st)
	want := "\x1b[33mabcdef012345\x1b[0m  \x1b[32m●\x1b[0m  t       \x1b[1m[storage]\x1b[0m  \U0001f4ac 5"
	if got != want {
		t.Fatalf("= %q, want %q", got, want)
	}
	// The tail lands on the right edge: the visible line is exactly the width.
	if n := displayWidth(stripANSI(got)); n != 40 {
		t.Errorf("visible width %d, want 40: %q", n, got)
	}
}

// The type column is an emoji on a terminal and the plain word when piped, so a
// redirected listing carries nothing a plain pager cannot place.
func TestFormatRowTypeCell(t *testing.T) {
	r := Row{ID: "abcdef012345", Status: "open", Type: "bug", TypeIcon: "\U0001f41e", Title: "t"}

	if got := formatRow(r, measure([]Row{r}, false)); got != "abcdef012345  open  bug  t" {
		t.Errorf("piped = %q, want the word", got)
	}
	got := formatRow(r, measure([]Row{r}, true))
	if !strings.Contains(got, "\U0001f41e") || strings.Contains(got, "bug") {
		t.Errorf("terminal = %q, want the emoji and not the word", got)
	}
}

// On a terminal the comment balloon sits in the same column on every row, so a
// count of 1 and a count of 100 line up: the number is right-aligned in a field
// as wide as the listing's largest.
func TestFormatRowBalloonAligns(t *testing.T) {
	rows := []Row{
		{ID: "aaaaaaaaaaaa", Status: "open", Title: "few", Comments: 1},
		{ID: "bbbbbbbbbbbb", Status: "open", Title: "many", Comments: 100},
	}
	st := measure(rows, false)
	st.width = 60

	col := -1
	for _, r := range rows {
		line := formatRow(r, st)
		got := strings.Index(line, "\U0001f4ac")
		if col == -1 {
			col = got
		} else if got != col {
			t.Errorf("balloon at column %d, want %d: %q", got, col, line)
		}
		if !strings.HasSuffix(line, "\U0001f4ac   1") && !strings.HasSuffix(line, "\U0001f4ac 100") {
			t.Errorf("count not right-aligned in a 3-wide field: %q", line)
		}
	}

	// Piped, the count stays tight — no padding gaps in grep output.
	if got := formatRow(rows[0], measure(rows, false)); !strings.HasSuffix(got, "\U0001f4ac 1") {
		t.Errorf("piped balloon should be tight: %q", got)
	}
}

// On a terminal the label block is right-justified, so its closing bracket
// lands in the same column whatever the title's length; piped, it just flows.
func TestFormatRowLabelsRightJustify(t *testing.T) {
	rows := []Row{
		{ID: "aaaaaaaaaaaa", Status: "open", Title: "short", Labels: []string{"api"}},
		{ID: "bbbbbbbbbbbb", Status: "open", Title: "a much longer title here", Labels: []string{"api"}},
	}
	st := measure(rows, false)
	st.width = 50

	for _, r := range rows {
		line := formatRow(r, st)
		if !strings.HasSuffix(line, "[api]") {
			t.Fatalf("labels not at the right edge: %q", line)
		}
		if displayWidth(line) != 50 {
			t.Errorf("line is %d columns, want 50: %q", displayWidth(line), line)
		}
	}

	// Piped: labels flow two spaces behind the title, no justification.
	if got := formatRow(rows[0], measure(rows, false)); got != "aaaaaaaaaaaa  open  short  [api]" {
		t.Errorf("piped = %q", got)
	}
}

func stripANSI(s string) string {
	for {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			return s
		}
		j := strings.IndexByte(s[i:], 'm')
		if j < 0 {
			return s
		}
		s = s[:i] + s[i+j+1:]
	}
}

// A writer that is not a terminal never gets escape codes, which is what keeps
// a piped listing greppable and the goldens stable.
func TestWriteListIsPlainWhenPiped(t *testing.T) {
	var b bytes.Buffer
	WriteList(&b, []Row{{ID: "abcdef012345", Status: "open", Title: "t"}}, nil)
	if strings.Contains(b.String(), "\x1b") {
		t.Errorf("escape codes reached a non-terminal writer: %q", b.String())
	}
}

// A listing opens on the newest issue, like `git log` opens on the newest
// commit: creation date descending, and equal timestamps fall back to the id
// in the same descending direction.
func TestWriteListNewestFirst(t *testing.T) {
	var b bytes.Buffer
	WriteList(&b, []Row{
		{ID: "aaaaaaaaaaaa", Status: "open", Title: "oldest", Created: 100},
		{ID: "cccccccccccc", Status: "open", Title: "newest", Created: 300},
		{ID: "bbbbbbbbbbbb", Status: "open", Title: "middle", Created: 200},
		{ID: "dddddddddddd", Status: "open", Title: "tie with newest", Created: 300},
	}, nil)
	var ids []string
	for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		ids = append(ids, strings.Fields(line)[0])
	}
	want := []string{"dddddddddddd", "cccccccccccc", "bbbbbbbbbbbb", "aaaaaaaaaaaa"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", ids, want)
	}
}

// An issue with no title and no metadata must not trail the spaces its columns
// padded out to.
func TestFormatRowHasNoTrailingSpace(t *testing.T) {
	rows := []Row{{ID: "abcdef012345", Status: "open", Type: "bug", Title: "typed"}, {ID: "abcdef012346", Status: "open"}}
	got := formatRow(rows[1], measure(rows, false))
	if got != strings.TrimRight(got, " ") {
		t.Errorf("= %q, want no trailing space", got)
	}
}

// A line that would wrap loses title, never metadata: the labels and the count
// are what the columns promised, and a wrapped line costs two rows to say what
// one was meant to.
func TestFormatRowTruncatesTitle(t *testing.T) {
	r := Row{ID: "abcdef012345", Status: "open", Title: "cat_sort_uniq reorders the blob", Labels: []string{"storage"}, Comments: 5}
	st := measure([]Row{r}, false)

	full := formatRow(r, st)
	if displayWidth(full) != 68 {
		t.Fatalf("untruncated row is %d columns: %q", displayWidth(full), full)
	}
	// A budget the row already fits in changes nothing.
	st.width = 68
	if got := formatRow(r, st); got != full {
		t.Errorf("a row that fits was still cut: %q", got)
	}

	// 20 columns of lead plus 17 of metadata, so 38 is the narrowest line that
	// can still hold a title at all.
	for _, width := range []int{67, 55, 45, 38} {
		st.width = width
		got := formatRow(r, st)
		if displayWidth(got) > width {
			t.Errorf("width %d produced %d columns: %q", width, displayWidth(got), got)
		}
		if !strings.Contains(got, "…") {
			t.Errorf("width %d did not mark the cut: %q", width, got)
		}
		// The tail survives whatever happens to the title.
		if !strings.HasSuffix(got, "[storage]  \U0001f4ac 5") {
			t.Errorf("width %d lost metadata: %q", width, got)
		}
	}

	// Narrower than the metadata itself, there is nothing left to give: the
	// title goes entirely and the line still overflows. Dropping a label to
	// make it fit would lose information the columns promised.
	st.width = 30
	if got, want := formatRow(r, st), "abcdef012345  open    [storage]  \U0001f4ac 5"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

// The ellipsis follows a word rather than floating a space away from one.
func TestTrimDropsTrailingSpace(t *testing.T) {
	// Budget 5 cuts inside the space after "cat", which goes with the cut.
	if got, want := Trim("cat sort uniq", 5), "cat…"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
	if got, want := Trim("short", 8), "short"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

// Squeezed, the title collapses rather than the line wrapping — the row still
// carries its id, status and labels, which is more than a wrapped line gives.
func TestTrimCollapsesRatherThanWrapping(t *testing.T) {
	const title = "cat_sort_uniq reorders the blob"
	for _, tc := range []struct {
		budget int
		want   string
	}{{4, "cat…"}, {2, "c…"}, {1, "…"}, {0, ""}, {-20, ""}} {
		if got := Trim(title, tc.budget); got != tc.want {
			t.Errorf("budget %d = %q, want %q", tc.budget, got, tc.want)
		}
	}
}

// A width of zero means no terminal and so no limit, which is what keeps piped
// output whole.
func TestNoTruncationWhenNotATerminal(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("very long title ", 20))
	r := Row{ID: "abcdef012345", Status: "open", Title: long}
	if got := formatRow(r, measure([]Row{r}, false)); !strings.Contains(got, long) {
		t.Errorf("a listing with no width limit was truncated: %q", got)
	}
	var b bytes.Buffer
	WriteList(&b, []Row{r}, nil)
	if !strings.Contains(b.String(), long) {
		t.Errorf("a piped listing was truncated: %q", b.String())
	}
}

// Widths are counted in columns, not runes or bytes: the comment balloon and a
// CJK title take two each, and a combining mark takes none.
func TestDisplayWidth(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want int
	}{
		{"plain", 5},
		{"— ✓", 3},          // narrow, despite being three bytes each
		{"✅", 2},            // ✅ is added to the wide table by hand
		{"✅ 3", 4},          // ✅ two columns, then a space and a digit
		{"\U0001f4ac 5", 4}, // the balloon is two columns, then a space and a digit
		{"日本語", 6},          // CJK
		{"e\u0301", 1},      // e plus a combining acute
	} {
		if got := displayWidth(tc.s); got != tc.want {
			t.Errorf("displayWidth(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

// A prefixed row is measured with its prefix, so `rm` cannot overflow by the
// width of the word "removed".
func TestWriteRowCountsItsPrefix(t *testing.T) {
	var b bytes.Buffer
	WriteRow(&b, "removed ", Row{ID: "abcdef012345", Status: "open", Title: "t"}, nil)
	if got, want := b.String(), "removed abcdef012345  open  t\n"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
	if narrow(0, 8) != 0 {
		t.Error("a prefix narrowed an unlimited width")
	}
	if got := narrow(20, 8); got != 12 {
		t.Errorf("narrow(20, 8) = %d, want 12", got)
	}
}

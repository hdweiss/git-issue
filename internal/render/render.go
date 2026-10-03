// Package render formats folded state for a terminal.
//
// The layout deliberately echoes git's own: an aligned `Key: value` block like
// `git log --format=fuller`, bodies indented four spaces under their header
// like `git log`, and dates in git's default format. It is entity-agnostic —
// callers supply the header lines their vocabulary calls for — so a pull
// request renders through this same code.
package render

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hdweiss/git-issue/internal/entity"
)

// IDWidth is how far an entity id is abbreviated for display. Ids abbreviate
// like object names do; twelve hex characters is plenty.
const IDWidth = 12

// Abbrev truncates an id for display.
func Abbrev(id string) string {
	if len(id) <= IDWidth {
		return id
	}
	return id[:IDWidth]
}

// Unique records, for each id it was measured over, how many leading
// characters name that id and nothing else in the set — the shortest
// abbreviation a reader can actually type.
type Unique map[string]int

// Uniquify measures every id against every other. It answers ahead of time the
// question Store.Resolve answers when a reader types an abbreviation, so a
// rendering can show where an id stops being ambiguous rather than leaving it
// to be discovered by getting it wrong.
//
// Measure over the whole set the reader could name, not over the rows about to
// be printed: a filtered listing measured against itself would advertise
// prefixes that Resolve then refuses.
func Uniquify(ids []string) Unique {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)

	u := make(Unique, len(sorted))
	for i, id := range sorted {
		// Sorted, the only ids that can share a long prefix with this one are
		// its two neighbours; whichever shares more decides.
		n := 1
		if i > 0 {
			n = max(n, commonPrefix(id, sorted[i-1])+1)
		}
		if i < len(sorted)-1 {
			n = max(n, commonPrefix(id, sorted[i+1])+1)
		}
		u[id] = min(n, len(id))
	}
	return u
}

// Len is how much of id a reader has to type. An id the set never saw counts
// as needing all of itself: nothing here knows what it competes with, and
// over-stating the prefix is the direction that cannot mislead.
func (u Unique) Len(id string) int {
	if n, ok := u[id]; ok {
		return n
	}
	return len(id)
}

// commonPrefix is how many leading bytes a and b share. Bytes rather than
// runes because ids are hex.
func commonPrefix(a, b string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// Date formats a timestamp the way git's default date format does, e.g.
// "Wed Aug 26 10:16:45 2026 +0200".
//
// The day of month is unpadded, which Go's own "Mon Jan _2" reference layout
// cannot express — it pads to width two — so it is spliced in by hand.
func Date(ts int64, loc *time.Location) string {
	t := time.Unix(ts, 0).In(loc)
	return t.Format("Mon Jan") + " " + strconv.Itoa(t.Day()) + " " + t.Format("15:04:05 2006 -0700")
}

// Header is one line of an aligned key/value block. A header whose value is
// empty is dropped before the column width is computed.
//
// More is what a header says beyond its first line: each entry prints on a
// line of its own, aligned under the value rather than repeating the key. A
// field that holds several things — an issue's children — reads as one block
// that way, where a repeated key would read as several fields with one name.
type Header struct {
	Key   string
	Value string
	More  []string
}

func writeHeaders(w io.Writer, pairs []Header, indent int) {
	present := make([]Header, 0, len(pairs))
	width := 0
	for _, p := range pairs {
		if p.Value == "" {
			continue
		}
		present = append(present, p)
		if len(p.Key) > width {
			width = len(p.Key)
		}
	}
	if len(present) == 0 {
		return
	}
	width += 2

	pad := strings.Repeat(" ", indent)
	for _, p := range present {
		fmt.Fprintf(w, "%s%-*s%s\n", pad, width, p.Key+":", p.Value)
		for _, more := range p.More {
			fmt.Fprintf(w, "%s%-*s%s\n", pad, width, "", more)
		}
	}
}

// writeBody indents a value under its header. Escaped newlines expand back to
// real ones only here, at the display boundary — never in the stored event.
func writeBody(w io.Writer, text string, indent int) {
	pad := strings.Repeat(" ", indent)
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintln(w, strings.TrimRightFunc(pad+line, unicode.IsSpace))
	}
}

// Row is one line of a listing.
type Row struct {
	ID      string
	Created int64 // create event's ts; only used to order the listing
	Status  string
	// Closed is whether Status is a terminal one. The entity type decides
	// that, not this package — it has no vocabulary of its own — and a
	// listing needs it to colour the status dot.
	Closed bool
	// Type is the issue's type as written; TypeIcon is the same thing mapped to
	// an emoji. A terminal listing shows the icon, a piped one shows the word —
	// picking the emoji is the caller's job, since only it has the vocabulary.
	Type     string
	TypeIcon string
	Title    string
	Labels   []string
	Comments int

	// Notes are extra cells the row carries after its labels — a review's
	// check tally and verdict counts — with NoteIcons the same information
	// written in glyphs. A terminal shows the glyphs and a piped listing the
	// words, exactly the split Type/TypeIcon makes: what a person scans is
	// dense, what a program greps is words. Picking the glyph is the caller's
	// job, since only it has the vocabulary.
	Notes     []string
	NoteIcons []string

	// Prefix is drawn immediately before the title and counted against the
	// line's width, so a title still trims to fit. Tree puts a forest's spine
	// here; a flat listing leaves it empty.
	//
	// It sits in the title column rather than at the left edge on purpose. The
	// id, status and type columns are what a reader scans down and copies out
	// of, and they stay a grid; the title is already the elastic part of the
	// line, so it is the part that can afford an indent.
	Prefix string
}

// typeCell is what the type column holds: the emoji on a terminal, the type
// name itself when the listing is piped, so piped output stays greppable and
// free of characters a plain pager cannot place.
func (r Row) typeCell(color bool) string {
	if color && r.TypeIcon != "" {
		return r.TypeIcon
	}
	return r.Type
}

// noteCell is what the note column holds, for the same reason typeCell exists:
// glyphs on a terminal, words down a pipe.
// Two spaces between notes, not one: the word forms contain commas of their
// own ("2 passed, 1 failed"), so a single space would run two notes into one
// list.
func (r Row) noteCell(color bool) string {
	notes := r.Notes
	if color && len(r.NoteIcons) > 0 {
		notes = r.NoteIcons
	}
	return strings.Join(notes, "  ")
}

// WriteList prints a listing sorted by creation date, newest first — so a
// freshly added issue lands at the top, the way `git log` opens on the newest
// commit. Ties (same wall-clock second, or no create event) break on
// abbreviated id for a deterministic order.
//
// uniq is how much of each id is enough to name it; see Uniquify. A nil one
// paints every id whole, which is what this did before ids were measured.
func WriteList(w io.Writer, rows []Row, uniq Unique) {
	writeRows(w, sortRows(rows), uniq)
}

// WriteTree prints rows in the order they are given, without sorting them.
// Tree has already put them in the order a forest reads in, and sorting again
// would flatten it back into a listing.
func WriteTree(w io.Writer, rows []Row, uniq Unique) {
	writeRows(w, rows, uniq)
}

func writeRows(w io.Writer, rows []Row, uniq Unique) {
	st := styleFor(w, rows, uniq)
	for _, r := range rows {
		fmt.Fprintln(w, formatRow(r, st))
	}
}

// WriteRow prints one row behind a prefix, with the prefix counted against the
// line's width — `rm` reports what it removed this way.
func WriteRow(w io.Writer, prefix string, r Row, uniq Unique) {
	st := styleFor(w, []Row{r}, uniq)
	st.width = narrow(st.width, displayWidth(prefix))
	fmt.Fprint(w, prefix)
	fmt.Fprintln(w, formatRow(r, st))
}

// narrow takes n columns off a line budget, leaving "no limit" alone.
func narrow(width, n int) int {
	if width <= 0 {
		return width
	}
	return max(width-n, 1)
}

// Change is one row plus the status letter a sync reported for it — git's own
// A, M and D.
type Change struct {
	Status byte
	Row    Row
}

// WriteChanges prints a sync summary: the same columns as a listing, each line
// prefixed with its status, in the same order a listing would show them. The
// order matters more than it looks — the rows line up with what `list` prints
// straight afterwards, rather than being a second arrangement to read.
func WriteChanges(w io.Writer, changes []Change, uniq Unique) {
	rows := make([]Row, 0, len(changes))
	status := make(map[string]byte, len(changes))
	for _, c := range changes {
		rows = append(rows, c.Row)
		status[c.Row.ID] = c.Status
	}
	rows = sortRows(rows)
	st := styleFor(w, rows, uniq)
	st.width = narrow(st.width, 3) // the " A " each line is prefixed with
	for _, r := range rows {
		fmt.Fprintf(w, " %c %s\n", status[r.ID], formatRow(r, st))
	}
}

// SortRows orders rows the way a listing orders them: newest first. Exported
// so that a caller assembling rows outside a listing — the children under one
// issue, say — puts them in the same order the listing would, rather than
// growing a second idea of what "first" means.
func SortRows(rows []Row) []Row { return sortRows(rows) }

func sortRows(rows []Row) []Row {
	rows = slices.Clone(rows)
	slices.SortFunc(rows, func(a, b Row) int {
		return byCreation(a.Created, a.ID, b.Created, b.ID)
	})
	return rows
}

// byCreation orders a listing newest first: by creation date descending, ties
// broken on the abbreviated id — also descending, so the tie-break reads as a
// continuation of the same order rather than a reversal within each second.
func byCreation(aCreated int64, aID string, bCreated int64, bID string) int {
	if aCreated != bCreated {
		return cmp.Compare(bCreated, aCreated)
	}
	return strings.Compare(Abbrev(bID), Abbrev(aID))
}

// style is how one listing renders: the widths its fixed columns were measured
// at, and whether the status column is a coloured dot rather than a word.
//
// Widths come from the rows being printed rather than from a constant, so a
// tracker that never sets a type does not pay for the column, and one whose
// statuses are all "open" does not indent past "reopened".
type style struct {
	color  bool
	status int // width of the status word; unused when color is set
	kind   int // width of the type column; zero drops the column entirely
	digits int // widest comment count in the listing, so the balloon lines up

	// uniq is how much of each id names it uniquely, so the rest can be faded.
	// Nil is not a listing of one thing — it is "not measured", and paints
	// every id whole.
	uniq Unique

	// width is how many columns a line has before the terminal wraps it, or
	// zero for no limit. Zero is what a listing that is not going to a
	// terminal gets: piped output is never truncated, so it stays greppable
	// and diffable however narrow the window that started it happened to be.
	width int
}

// styleFor measures how a listing renders on w: its column widths, whether it
// can carry colour, and how much room a line has.
func styleFor(w io.Writer, rows []Row, uniq Unique) style {
	st := measure(rows, colorEnabled(w))
	st.uniq = uniq
	st.width = terminalColumns(w)
	return st
}

// terminalColumns is how wide the listing's terminal is, or 0 when it is not
// going to one.
func terminalColumns(w io.Writer) int {
	if f := display(w); f != nil {
		return terminalWidth(f)
	}
	return 0
}

// display is the terminal a listing will be seen on, or nil when it will not
// be seen on one. It is w itself when w is that terminal, and the terminal
// standing behind w when w is a pager's pipe — a listing paged through less is
// still measured for colour and width against the screen it lands on, the same
// way `git log --color` keeps its colour through a pager.
func display(w io.Writer) *os.File {
	if t, ok := w.(interface{ Terminal() *os.File }); ok {
		return t.Terminal()
	}
	if f, ok := w.(*os.File); ok {
		return f
	}
	return nil
}

func measure(rows []Row, color bool) style {
	st := style{color: color}
	for _, r := range rows {
		if n := displayWidth(r.Status); n > st.status {
			st.status = n
		}
		if n := displayWidth(r.typeCell(color)); n > st.kind {
			st.kind = n
		}
		if r.Comments > 0 {
			if n := len(strconv.Itoa(r.Comments)); n > st.digits {
				st.digits = n
			}
		}
	}
	return st
}

// formatRow lays out one line: id, status, type, title, then the labels and the
// comment count the issue carries.
//
// On a terminal the tail is built from the right edge inward — the count in a
// fixed-width slot so the balloon lines up whatever the number of digits, the
// labels right-justified just left of it — and the title, the elastic part, is
// trimmed to whatever room is left. A line that will not fit loses title, never
// a label or the count. Piped, there is no edge to justify against, so the tail
// just flows two spaces behind the title and the count keeps its natural width;
// that keeps a grepped or diffed listing tight and stable.
func formatRow(r Row, st style) string {
	// lead is what the columns before the title cost, in display columns.
	// Counted as it is built rather than measured off the finished string,
	// because the id and label colours carry escape bytes that take no column.
	lead := len(Abbrev(r.ID)) + 2
	statusCol := ""
	if st.color {
		lead++ // the status dot
	} else {
		statusCol = padded(r.Status, st.status)
		lead += st.status
	}
	lead += 2 // the gap after the status column
	typeCol := ""
	if st.kind > 0 {
		typeCol = padded(r.typeCell(st.color), st.kind)
		lead += st.kind + 2
	}
	lead += displayWidth(r.Prefix)

	labels := ""
	if len(r.Labels) > 0 {
		sorted := slices.Clone(r.Labels)
		slices.Sort(sorted)
		// Bracketed, because a bare list runs into whatever it sits next to —
		// there is no column boundary out here, only a double space.
		labels = "[" + strings.Join(sorted, ", ") + "]"
	}
	// The notes ride in the same right-justified block as the labels, two
	// spaces behind them, so one gutter holds everything a row says about
	// itself beyond its title.
	if notes := r.noteCell(st.color); notes != "" {
		if labels != "" {
			labels += "  "
		}
		labels += notes
	}

	var b strings.Builder
	b.WriteString(paintID(Abbrev(r.ID), st.uniq.Len(r.ID), st.color))
	b.WriteString("  ")
	if st.color {
		b.WriteString(dot(r.Closed))
	} else {
		b.WriteString(statusCol)
	}
	b.WriteString("  ")
	if st.kind > 0 {
		b.WriteString(typeCol)
		b.WriteString("  ")
	}

	// Piped: the tail flows two spaces behind the title, the count at its
	// natural width, so grep and diff output stays tight.
	if st.width == 0 {
		b.WriteString(r.Prefix)
		b.WriteString(r.Title)
		if labels != "" {
			b.WriteString("  ")
			b.WriteString(paint(labels, colorLabel, st.color))
		}
		if r.Comments > 0 {
			fmt.Fprintf(&b, "  \U0001f4ac %d", r.Comments)
		}
		return strings.TrimRight(b.String(), " ")
	}

	// Terminal: lay the tail out from the right edge inward.
	countSlot := 0
	if st.digits > 0 {
		countSlot = 3 + st.digits // "💬 " plus a count right-aligned to st.digits
	}
	labelEnd := st.width
	if countSlot > 0 {
		labelEnd -= countSlot + 2 // a two-space gutter before the slot
	}
	titleEnd := labelEnd
	if labels != "" {
		titleEnd -= displayWidth(labels) + 2
	}

	title := Trim(r.Title, titleEnd-lead)
	b.WriteString(r.Prefix)
	b.WriteString(title)
	col := lead + displayWidth(title)

	if labels != "" {
		gap := max(2, labelEnd-displayWidth(labels)-col)
		b.WriteString(strings.Repeat(" ", gap))
		b.WriteString(paint(labels, colorLabel, st.color))
		col += gap + displayWidth(labels)
	}
	if r.Comments > 0 {
		count := fmt.Sprintf("\U0001f4ac %*d", max(st.digits, len(strconv.Itoa(r.Comments))), r.Comments)
		gap := max(2, st.width-displayWidth(count)-col)
		b.WriteString(strings.Repeat(" ", gap))
		b.WriteString(count)
	}

	// An issue with no title and no tail would otherwise pad out to the title
	// column and stop there.
	return strings.TrimRight(b.String(), " ")
}

// Trim shortens a title to fit budget columns, marking the cut with an
// ellipsis. Trailing spaces go with the cut, so the ellipsis follows a word
// rather than floating away from one.
//
// There is no floor under which it gives up and lets the line wrap. A budget
// of one column still says something — the row keeps its id, its status and
// its labels, and only the title collapses — whereas a wrapped line costs two
// rows to say what one was meant to. A budget of none means the metadata alone
// is wider than the terminal, which nothing here can fix.
func Trim(s string, budget int) string {
	if displayWidth(s) <= budget {
		return s
	}
	if budget <= 0 {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		w := runeWidth(r)
		if n+w > budget-1 { // one column held back for the ellipsis
			break
		}
		b.WriteRune(r)
		n += w
	}
	return strings.TrimRight(b.String(), " ") + "\u2026"
}

// displayWidth is how many terminal columns s occupies.
//
// Approximated rather than looked up: combining marks take no column of their
// own, the East Asian wide blocks and the emoji planes take two, and
// everything else takes one. That is enough to keep a truncated title off the
// next line, and it needs no Unicode tables beyond the ones the standard
// library already carries.
func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

func runeWidth(r rune) int {
	switch {
	case unicode.Is(unicode.Mn, r):
		return 0
	case unicode.Is(wide, r):
		return 2
	}
	return 1
}

// wide is the East Asian Wide and Fullwidth blocks plus the emoji planes — the
// ranges whose characters take two columns. The comment balloon a row ends with
// is one of them. U+2705 (✅) is added by hand: it is the type column's task
// icon and terminals render it double-width, but it sits outside the planes and
// its neighbours (U+2713 ✓ among them) are narrow, so the block cannot widen.
var wide = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x1100, Hi: 0x115f, Stride: 1}, {Lo: 0x2705, Hi: 0x2705, Stride: 1},
		{Lo: 0x2e80, Hi: 0x303e, Stride: 1},
		{Lo: 0x3041, Hi: 0x33ff, Stride: 1}, {Lo: 0x3400, Hi: 0x4dbf, Stride: 1},
		{Lo: 0x4e00, Hi: 0x9fff, Stride: 1}, {Lo: 0xa000, Hi: 0xa4cf, Stride: 1},
		{Lo: 0xac00, Hi: 0xd7a3, Stride: 1}, {Lo: 0xf900, Hi: 0xfaff, Stride: 1},
		{Lo: 0xfe30, Hi: 0xfe6f, Stride: 1}, {Lo: 0xff00, Hi: 0xff60, Stride: 1},
		{Lo: 0xffe0, Hi: 0xffe6, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x1f300, Hi: 0x1f64f, Stride: 1}, {Lo: 0x1f680, Hi: 0x1f6ff, Stride: 1},
		{Lo: 0x1f900, Hi: 0x1f9ff, Stride: 1}, {Lo: 0x20000, Hi: 0x3fffd, Stride: 1},
	},
}

// padded returns s followed by enough spaces to fill width display columns.
// Width is counted in columns, not runes or bytes, so an emoji type or a status
// spelled with a wide character does not shift everything after it.
func padded(s string, width int) string {
	if n := width - displayWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// ANSI colours for a listing, matching git's own: the id is yellow like a
// commit line in `git log`, and labels are bold.
//
// colorRest greys out the tail of an id that is only there because the column
// has a fixed width.
//
// Bright black rather than the same yellow dimmed: dim is SGR 2, an attribute
// rather than a colour, and plenty of terminals drop it — which renders the
// tail in the prefix's own yellow and loses the distinction entirely. A colour
// is either supported or it is not.
const (
	colorID    = "\x1b[33m"
	colorRest  = "\x1b[90m"
	colorLabel = "\x1b[1m"
	colorReset = "\x1b[0m"
)

// paint wraps s in an ANSI code when colour is on, and returns it untouched
// when it is not — so a piped listing carries no escape bytes.
func paint(s, code string, color bool) string {
	if !color {
		return s
	}
	return code + s + colorReset
}

// paintID paints an id in two parts, the way jj colours a change id: the
// leading characters that name it uniquely in yellow, everything after them
// faint. The reader sees how much to type without losing the digits that
// confirm which entity they are looking at.
//
// need counts against the full id, so an abbreviation shorter than the prefix
// it takes to be unique is painted whole — which is honest: every character
// shown is one the reader needs.
func paintID(id string, need int, color bool) string {
	if !color {
		return id
	}
	if need >= len(id) {
		return colorID + id + colorReset
	}
	return colorID + id[:need] + colorReset + colorRest + id[need:] + colorReset
}

// dot renders a status as a coloured bullet, following gh's convention: green
// for an open issue, purple for a closed one.
//
// There are only two colours because there are only two states a reader can
// act on. docs/issues.md requires an unrecognised status to be treated as
// non-terminal, so it dots green — the word itself is still there when colour
// is off, and in --format=medium.
func dot(closed bool) string {
	if closed {
		return "\x1b[35m\u25cf" + colorReset
	}
	return "\x1b[32m\u25cf" + colorReset
}

// colorEnabled reports whether w can carry ANSI colour: a character device,
// with NO_COLOR unset and a terminal that is not "dumb".
//
// git's own color.ui is deliberately not consulted. Reading it would mean
// handing this package a repository, and every writer here is either a
// terminal, a pager standing in front of one, or a test buffer.
func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f := display(w)
	if f == nil {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Detail is everything a full rendering of one entity needs. Headers are
// supplied by the caller because they are that entity type's vocabulary.
type Detail struct {
	Kind        string // the word before the id, e.g. "issue"
	ID          string
	Headers     []Header
	Title       string
	Description string
	State       entity.State
	Loc         *time.Location

	// Entry supplies the headers one thread entry carries beyond its author and
	// date — where in the code a review comment sits, say. It is the same
	// division of labour as Headers: the vocabulary belongs to the entity type,
	// and this package never learns what a review is. nil for a type that adds
	// none, which is every rendering that does not set it.
	Entry func(entity.Comment) []Header
}

// LogEntry is one entry of a `--format=medium` listing: the same Detail a full
// rendering uses, plus the creation date the listing orders on.
type LogEntry struct {
	Created int64
	Detail  Detail
}

// WriteLog prints a listing in the shape of `git log`: each entity's header
// block, then its title alone as the body, entries separated by a blank line.
//
// It is WriteDetail with the description and the comment forest left off, and
// shares writeSummary with it deliberately — a listing and a full rendering of
// the same issue must not drift into two different layouts.
func WriteLog(w io.Writer, entries []LogEntry, uniq Unique) {
	entries = slices.Clone(entries)
	slices.SortFunc(entries, func(a, b LogEntry) int {
		return byCreation(a.Created, a.Detail.ID, b.Created, b.Detail.ID)
	})
	color := colorEnabled(w)
	for i, e := range entries {
		if i > 0 {
			fmt.Fprintln(w)
		}
		writeSummary(w, e.Detail, uniq, color)
	}
}

// writeSummary prints the part every rendering of an entity opens with: the
// kind and id, the header block, and the title.
func writeSummary(w io.Writer, d Detail, uniq Unique, color bool) {
	fmt.Fprintf(w, "%s %s\n", d.Kind, paintID(d.ID, uniq.Len(d.ID), color))
	writeHeaders(w, d.Headers, 0)
	fmt.Fprintln(w)
	writeBody(w, d.Title, 4)
}

// WriteDetail prints one entity in full: headers, title, description,
// reactions, then the comment forest.
func WriteDetail(w io.Writer, d Detail, uniq Unique) {
	t := newThread(w, d)

	writeSummary(w, d, uniq, t.color)
	if d.Description != "" {
		fmt.Fprintln(w)
		writeBody(w, d.Description, 4)
	}
	// A reaction to the entity as a whole targets the create event, whose id
	// is the entity id.
	if by, ok := t.reactions[d.ID]; ok {
		fmt.Fprintln(w)
		writeBody(w, "Reactions: "+summarize(by), 4)
	}

	// The summary is already printed, so every entry below it is separated
	// from what precedes it.
	t.wrote = true
	t.walk(t.children[""], 0)
}

// WriteComment prints one entry of a thread and the replies underneath it.
//
// It is the rendering WriteDetail gives that same entry in place, minus the
// issue around it — the walk is shared, so zooming in on a comment can never
// show it differently from how the whole issue showed it. Its id is measured
// against the thread it belongs to, not against the prefix that named it, so
// what is bright here is what is bright there.
func WriteComment(w io.Writer, d Detail, c entity.Comment) {
	newThread(w, d).walk([]entity.Comment{c}, 0)
}

// thread is the shared state a comment forest is printed from: the entity's
// reactions and reply structure, and how much of each comment id has to be
// bright.
type thread struct {
	w         io.Writer
	loc       *time.Location
	reactions map[string]map[string][]string
	children  map[string][]entity.Comment
	ids       Unique
	entry     func(entity.Comment) []Header
	color     bool
	// wrote records whether anything has been printed yet, since entries are
	// separated by a blank line rather than preceded by one: a comment printed
	// on its own opens the output and must not start with one.
	wrote bool
}

func newThread(w io.Writer, d Detail) *thread {
	return &thread{
		w:         w,
		loc:       d.Loc,
		reactions: entity.ReactionsByTarget(d.State.Reactions),
		children:  entity.Forest(d.State.Thread),
		ids:       threadUniq(d.State.Thread),
		entry:     d.Entry,
		color:     colorEnabled(w),
	}
}

// walk prints these entries and, one indent deeper, the replies to each.
func (t *thread) walk(entries []entity.Comment, depth int) {
	for _, c := range entries {
		indent := depth * 4
		if t.wrote {
			fmt.Fprintln(t.w)
		}
		t.wrote = true

		fmt.Fprintf(t.w, "%scomment %s\n", strings.Repeat(" ", indent), paintID(c.ID(), t.ids.Len(c.ID()), t.color))
		headers := []Header{
			{Key: "Author", Value: c.Event.A},
			{Key: "Date", Value: Date(c.Event.TS, t.loc)},
		}
		if t.entry != nil {
			headers = append(headers, t.entry(c)...)
		}
		writeHeaders(t.w, headers, indent)
		fmt.Fprintln(t.w)

		if c.Retracted {
			writeBody(t.w, "(comment retracted)", indent+4)
		} else {
			writeBody(t.w, c.Body.Display(), indent+4)
		}
		if by, ok := t.reactions[c.ID()]; ok && !c.Retracted {
			fmt.Fprintln(t.w)
			writeBody(t.w, "Reactions: "+summarize(by), indent+4)
		}
		t.walk(t.children[c.ID()], depth+1)
	}
}

// threadUniq measures a thread's comment ids against each other and nothing
// else. That is the scope State.FindComment resolves in — a comment prefix
// competes only with the comments of its own entity — so a comment id here
// fades far earlier than an entity id does, which is the point: a thread of a
// dozen entries is one or two characters to address.
func threadUniq(thread []entity.Comment) Unique {
	ids := make([]string, 0, len(thread))
	for _, c := range thread {
		ids = append(ids, c.ID())
	}
	return Uniquify(ids)
}

// summarize renders a target's reactions as "value (count)", by value.
func summarize(byValue map[string][]string) string {
	values := make([]string, 0, len(byValue))
	for v := range byValue {
		values = append(values, v)
	}
	slices.Sort(values)

	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, fmt.Sprintf("%s (%d)", v, len(byValue[v])))
	}
	return strings.Join(parts, "  ")
}

package review

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/render"
)

// Status resolves the status field. A review with no status event is open, and
// an unrecognised value is preserved rather than coerced.
func Status(st entity.State) string {
	if v := st.Scalar("status"); v.Truthy() {
		return v.Display()
	}
	return StatusOpen
}

// Head is the commit the review currently describes, or "".
//
// The resolving `head.sha`, not the resolution of the `head` branch name: the
// name moves, differs per clone, and routinely stops resolving at all once the
// branch is deleted after a merge. Anchors, verdicts and checks all address
// this.
func Head(st entity.State) string { return st.Scalar("head.sha").Display() }

// Base is the branch this review merges into.
func Base(st entity.State) string { return st.Scalar("base").Display() }

// HeadRef is the branch this review merges from, remote-qualified where it has
// a remote.
func HeadRef(st entity.State) string { return st.Scalar("head").Display() }

// Draft reports whether the review is still a draft — not soliciting verdicts.
// A convention rather than an enforcement mechanism: nothing in the object
// store stops anyone approving a draft.
func Draft(st entity.State) bool { return st.Scalar("draft").Truthy() }

// Branches is the `base ← head` pair a listing and a header show, or "" where
// the review names neither.
func Branches(st entity.State) string {
	base, head := Base(st), HeadRef(st)
	switch {
	case base != "" && head != "":
		return base + " ← " + head
	case head != "":
		return "← " + head
	case base != "":
		return base + " ←"
	}
	return ""
}

// Icon is the glyph a terminal listing puts in the type column.
//
// A review has no bug/feature/task axis, so the column carries its lifecycle
// instead — which is the thing a person scanning a list of reviews is actually
// looking for, and the one piece of state the status dot alone cannot show
// (draft and open are both non-terminal).
func Icon(st entity.State) string {
	switch {
	case Draft(st):
		return "📝"
	case Status(st) == StatusMerged:
		return "🔀"
	case Terminal(Status(st)):
		return "🚫"
	default:
		return "🔍"
	}
}

// Word is Icon's piped equivalent: the same distinction, in a word.
func Word(st entity.State) string {
	if Draft(st) {
		return "draft"
	}
	return Status(st)
}

// Row summarises one review for a listing.
//
// checks is the summary of the head commit's check runs, or the zero value
// where the checks ref holds none — a caller that has not opened that ref
// passes nothing, and the listing simply carries no check column.
func Row(id string, st entity.State, checks ChecksSummary) render.Row {
	comments := 0
	for _, c := range st.Thread {
		if !c.Retracted {
			comments++
		}
	}
	status := Status(st)

	// What a reader needs at a glance and cannot get from a dot: how the checks
	// ran, and how many people have taken a position. Written twice — once in
	// glyphs for a terminal, once in words for a pipe — because a listing that
	// is grepped should not have ✓ in it.
	var notes, icons []string
	if s := checks.String(); s != "" {
		notes = append(notes, checks.Plain())
		icons = append(icons, s)
	}
	// Only the verdicts that describe the current revision are counted. A
	// stale approval is shown in full by `show` and is not a number a listing
	// can qualify, so counting it here would read as an approval that stands.
	if n := len(Approvals(st)); n > 0 {
		notes = append(notes, fmt.Sprintf("%d approving", n))
		icons = append(icons, fmt.Sprintf("👍%d", n))
	}
	if n := len(ChangesRequested(st)); n > 0 {
		notes = append(notes, fmt.Sprintf("%d blocking", n))
		icons = append(icons, fmt.Sprintf("✋%d", n))
	}

	return render.Row{
		ID:        id,
		Created:   created(st),
		Status:    status,
		Closed:    Terminal(status),
		Type:      Word(st),
		TypeIcon:  Icon(st),
		Title:     title(st),
		Labels:    slices.Clone(st.List("label")),
		Comments:  comments,
		Notes:     notes,
		NoteIcons: icons,
	}
}

// Log summarises one review for a `git log`-shaped listing.
func Log(id string, st entity.State, loc *time.Location, links Links, checks []Check) render.LogEntry {
	return render.LogEntry{Created: created(st), Detail: Detail(id, st, loc, links, checks, nil)}
}

// Links is what a rendering can say about a review's relations that the
// review's own blob does not know: what the entities it points at are called,
// and what points back at it.
//
// A review's targets routinely live in the other type's namespace — what a
// review closes is an issue — so a caller fills this from both refs. A zero
// Links renders each relation as a bare id, which is what a caller with no ref
// to read falls back to.
type Links struct {
	Titles  map[string]string
	Inverse map[string][]string
}

func (l Links) describe(id, note string) string {
	out := render.Abbrev(id) + "  (unknown)"
	if title, ok := l.Titles[id]; ok {
		out = render.Abbrev(id) + "  " + title
	}
	if note != "" {
		out += "  — " + strings.Join(strings.Fields(note), " ")
	}
	return out
}

func created(st entity.State) int64 {
	if st.Create == nil {
		return 0
	}
	return st.Create.TS
}

func title(st entity.State) string {
	if t := st.Scalar("title").Display(); t != "" {
		return t
	}
	return "(no title)"
}

// Detail maps folded state onto the generic renderer.
//
// checks and repo are what the blob cannot answer on its own: the head commit's
// check runs live on their own ref, and whether an anchor still describes the
// head is a question about the object store. Both are optional — a nil repo
// renders every thread where it was written and says nothing about currency,
// which is exactly the honest answer when this clone does not hold the history.
func Detail(id string, st entity.State, loc *time.Location, links Links, checks []Check, repo Repo) render.Detail {
	var author, date string
	if st.Create != nil {
		author = st.Create.A
		date = render.Date(st.Create.TS, loc)
	}

	locked := ""
	if st.Scalar("locked").Truthy() {
		locked = st.Scalar("lock.reason").Display()
	}

	status := Status(st)
	reason := ""
	if Terminal(status) {
		reason = st.Scalar("status.reason").Display()
	}
	if Draft(st) && !Terminal(status) {
		status += " (draft)"
	}

	head := Head(st)
	revision := ""
	if head != "" {
		revision = render.Abbrev(head)
	}

	headers := []render.Header{
		{Key: "Author", Value: author},
		{Key: "Date", Value: date},
		{Key: "Status", Value: status},
		{Key: "Reason", Value: reason},
		{Key: "Branches", Value: Branches(st)},
		{Key: "Revision", Value: revision},
		{Key: "Labels", Value: joinSorted(st.List("label"))},
		{Key: "Reviewers", Value: joinSorted(st.List("assignee"))},
		{Key: "Milestone", Value: st.Scalar("milestone").Display()},
	}
	headers = append(headers, verdictHeader(st)...)
	headers = append(headers, checksHeader(checks)...)
	headers = append(headers, relationHeaders(st, links)...)
	headers = append(headers, render.Header{Key: "Locked", Value: locked})

	return render.Detail{
		Kind:        Type,
		ID:          id,
		Headers:     headers,
		Title:       title(st),
		Description: st.Scalar("description").Display(),
		State:       st,
		Loc:         loc,
		Entry:       entryHeaders(st, repo),
	}
}

// entryHeaders is what a thread entry carries beyond its author and date: where
// in the code it sits, the commit it was written against, and whether somebody
// has since resolved it.
//
// The place belongs on the entry rather than only in a summary at the end,
// because it is what the entry is about: a comment that says "this should return
// an error" is unreadable without the line it was left on. The revision goes
// with it because the line number only means anything against that commit — the
// same reason an anchor stores one — and it is how a reader who wants the code
// gets there: `git show <revision>:<path>` needs both halves.
//
// Only a thread's root carries an anchor; replies inherit it, and repeating it
// under every reply would say the same thing five times in a forest that is
// already indented to show the relationship.
func entryHeaders(st entity.State, repo Repo) func(entity.Comment) []render.Header {
	threads := Threads(st, repo)
	byRoot := make(map[string]Thread, len(threads))
	for _, t := range threads {
		if t.Anchored || t.Resolved {
			byRoot[t.Root.ID()] = t
		}
	}
	if len(byRoot) == 0 {
		return nil
	}

	return func(c entity.Comment) []render.Header {
		t, ok := byRoot[c.ID()]
		if !ok {
			return nil
		}
		var out []render.Header
		if t.Anchored {
			where := t.Anchor.Where()
			// The left side of a split diff is the file as it was before the
			// change, so a line number there counts in a different file from the
			// same number on the right.
			if t.Anchor.Side == SideLeft {
				where += "  (before the change)"
			}
			out = append(out, render.Header{Key: "File", Value: where})

			revision := render.Abbrev(t.Anchor.Revision)
			if label := t.Currency.Label(); revision != "" && label != "" {
				revision += "  " + label
			}
			out = append(out, render.Header{Key: "Revision", Value: revision})
		}
		if t.Resolved {
			out = append(out, render.Header{Key: "Resolved", Value: "yes"})
		}
		return out
	}
}

// verdictHeader lists each person's current position, marking the ones cast
// against an older revision.
//
// A stale verdict is shown rather than dropped, and its revision is never
// rewritten to the current head: whether it still counts is policy, decided
// upstream by branch protection, and a client's job is to report it.
func verdictHeader(st entity.State) []render.Header {
	verdicts := VerdictsOf(st)
	if len(verdicts) == 0 {
		return nil
	}
	values := make([]string, 0, len(verdicts))
	for _, v := range verdicts {
		line := v.Value + "  " + v.Author
		if v.Stale {
			line += "  (stale, on " + render.Abbrev(v.Revision) + ")"
		}
		values = append(values, line)
	}
	return []render.Header{{Key: "Verdicts", Value: values[0], More: values[1:]}}
}

// checksHeader lists the head commit's check runs.
func checksHeader(checks []Check) []render.Header {
	if len(checks) == 0 {
		return nil
	}
	values := make([]string, 0, len(checks))
	for _, c := range checks {
		line := c.Conclusion + "  " + c.Name
		if c.URL != "" {
			line += "  " + c.URL
		}
		values = append(values, line)
	}
	return []render.Header{{Key: "Checks", Value: values[0], More: values[1:]}}
}

// relationHeaders renders one header per kind of relation the review stores,
// each followed by the inverse a reader derived for that kind.
func relationHeaders(st entity.State, links Links) []render.Header {
	stored := map[string][]Relation{}
	for _, r := range Relations(st) {
		stored[r.Kind] = append(stored[r.Kind], r)
	}

	kinds := make([]string, 0, len(stored)+len(links.Inverse))
	for kind := range stored {
		kinds = append(kinds, kind)
	}
	for kind := range links.Inverse {
		if _, ok := stored[kind]; !ok {
			kinds = append(kinds, kind)
		}
	}

	var out []render.Header
	header := func(key string, values []string) {
		if len(values) == 0 {
			return
		}
		out = append(out, render.Header{Key: key, Value: values[0], More: values[1:]})
	}

	for _, kind := range SortKinds(kinds) {
		k, known := KnownKind(kind)
		label := k.Label
		if label == "" {
			label = kind
		}

		var mine []string
		seen := map[string]bool{}
		for _, r := range stored[kind] {
			seen[r.Target] = true
			mine = append(mine, links.describe(r.Target, r.Note))
		}

		if k.Symmetric {
			for _, id := range links.Inverse[kind] {
				if !seen[id] {
					mine = append(mine, links.describe(id, ""))
				}
			}
			header(label, mine)
			continue
		}

		header(label, mine)
		if !known {
			continue
		}
		var theirs []string
		for _, id := range links.Inverse[kind] {
			theirs = append(theirs, links.describe(id, ""))
		}
		header(k.Inverse, theirs)
	}
	return out
}

func joinSorted(values []string) string {
	values = slices.Clone(values)
	slices.Sort(values)
	return strings.Join(values, ", ")
}

// Thread is one conversation on a review: its root entry, where it sits in the
// code, whether it has been resolved, and how well its anchor still describes
// the head.
type Thread struct {
	Root     entity.Comment
	Replies  int
	Anchor   Anchor
	Anchored bool
	Resolved bool
	Currency Currency
}

// Threads are the review's conversations, in (c, id) order, roots only.
//
// Resolution and anchoring are properties of the root: only the root's anchor
// is read, and a resolve names the root. A reply carrying either is preserved
// on disk and ignored here.
func Threads(st entity.State, repo Repo) []Thread {
	forest := entity.Forest(st.Thread)
	head := Head(st)

	var out []Thread
	for _, root := range forest[""] {
		t := Thread{
			Root:     root,
			Replies:  countReplies(forest, root.ID()),
			Resolved: Resolved(st, root.ID()),
			Currency: Unknown,
		}
		if a, ok := AnchorOf(st, root.ID()); ok {
			t.Anchor, t.Anchored = a, true
			t.Currency = a.Currency(repo, head)
		}
		out = append(out, t)
	}
	return out
}

func countReplies(forest map[string][]entity.Comment, id string) int {
	n := 0
	for _, c := range forest[id] {
		n += 1 + countReplies(forest, c.ID())
	}
	return n
}

// Unresolved are the threads nobody has marked resolved.
//
// A retracted root with no surviving replies is not one: the person who asked
// withdrew it. One with replies still is, because the replies are other
// people's and a tombstone does not cascade.
func Unresolved(threads []Thread) []Thread {
	var out []Thread
	for _, t := range threads {
		if t.Resolved {
			continue
		}
		if t.Root.Retracted && t.Replies == 0 {
			continue
		}
		out = append(out, t)
	}
	return out
}

// OpenQuestions are the unresolved threads that are **anchored** — the ones a
// `status` reports as standing in the way, and an agent reads as work to do.
//
// Anchoring is what separates a question about the code from a remark about the
// review. A thread pinned to a line is somebody asking for something at that
// line; a top-level comment is discussion, and the person who wrote it said what
// they wanted to say. Counting both would make every review that anyone spoke
// on permanently unready — including the comment a `request-changes` posts
// alongside itself, which the verdict already reports.
//
// Resolving an unanchored thread is still allowed and still recorded. It simply
// does not block, because nothing here can tell a question from an aside, and
// blocking on the guess is the direction that produces a false stop.
func OpenQuestions(threads []Thread) []Thread {
	var out []Thread
	for _, t := range Unresolved(threads) {
		if t.Anchored {
			out = append(out, t)
		}
	}
	return out
}

// Resolvable are the threads resolution is a meaningful question about: the
// anchored ones. It is the denominator of "n of m resolved".
func Resolvable(threads []Thread) []Thread {
	var out []Thread
	for _, t := range threads {
		if t.Anchored {
			out = append(out, t)
		}
	}
	return out
}

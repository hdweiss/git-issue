package issue

import (
	"slices"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/render"
)

// Status resolves the status field. An issue with no status event is open, and
// an unrecognised value is preserved rather than coerced — wrongly archiving
// something is worse than leaving it listed.
func Status(st entity.State) string {
	if v := st.Scalar("status"); v.Truthy() {
		return v.Display()
	}
	return StatusOpen
}

// Row summarises one issue for a listing.
func Row(id string, st entity.State) render.Row {
	comments := 0
	for _, c := range st.Thread {
		if !c.Retracted {
			comments++
		}
	}
	status := Status(st)
	return render.Row{
		ID:       id,
		Created:  created(st),
		Status:   status,
		Closed:   Terminal(status),
		Type:     st.Scalar("type").Display(),
		TypeIcon: typeIcon(st.Scalar("type").Display()),
		Title:    title(st),
		Labels:   st.List("label"),
		Comments: comments,
	}
}

// Log summarises one issue for a `git log`-shaped listing: the same headers a
// full rendering carries, without the description or the thread.
func Log(id string, st entity.State, loc *time.Location, links Links) render.LogEntry {
	return render.LogEntry{Created: created(st), Detail: Detail(id, st, loc, links)}
}

// Links is what a rendering can say about an issue's relations that the issue's
// own blob does not know: what the entities it points at are called, and what
// points back at it.
//
// The blob holds a kind and an id, and an id is forty characters of hex that a
// reader then has to go and look up. Resolving it is a question about other
// entities, which only a caller holding the ref can answer — so it is passed in
// rather than reached for, the same way the header vocabulary is.
//
// A zero Links renders what the blob alone supports: each relation as a bare
// id, and no derived inverses. That is what a caller with no ref to read falls
// back to.
type Links struct {
	// Titles maps an entity id onto its title. An id it does not hold renders
	// as itself, which is honest: a target can be filtered out, archived, in
	// another repository, or not imported yet, and none of those is a failure.
	Titles map[string]string
	// Inverse holds the relations pointing *at* this issue, keyed by the kind
	// as the other end stored it: Inverse["parent"] is the issues filed under
	// this one. Derived by the caller, never stored (docs/issues.md).
	Inverse map[string][]string
}

// describe renders one link as the id a reader would type, followed by what it
// is called and, where there is one, the note saying why the link is there. An
// id nothing knows the title of says so rather than pretending the hex is an
// answer.
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

// created is the creation timestamp a listing orders on. An entity whose
// create event never arrived counts as the oldest thing there is — it lands at
// the bottom of a newest-first listing — which is the only ordering that does
// not depend on the fold.
func created(st entity.State) int64 {
	if st.Create == nil {
		return 0
	}
	return st.Create.TS
}

// title is the issue's title, or a stand-in — an untitled issue must still
// occupy its column rather than leaving a blank gap in the listing.
func title(st entity.State) string {
	if t := st.Scalar("title").Display(); t != "" {
		return t
	}
	return "(no title)"
}

// Detail maps folded state onto the generic renderer. The header list is this
// type's vocabulary — a pull request would supply its own.
func Detail(id string, st entity.State, loc *time.Location, links Links) render.Detail {
	var author, date string
	if st.Create != nil {
		author = st.Create.A
		date = render.Date(st.Create.TS, loc)
	}

	// lock.reason is only meaningful while the issue is locked.
	locked := ""
	if st.Scalar("locked").Truthy() {
		locked = st.Scalar("lock.reason").Display()
	}

	// status.reason says why the issue reached a terminal status; a reader must
	// ignore it while the status is non-terminal (docs/issues.md), where it can
	// linger as a stale pairing after a reopen.
	status := Status(st)
	reason := ""
	if Terminal(status) {
		reason = st.Scalar("status.reason").Display()
	}

	headers := []render.Header{
		{Key: "Author", Value: author},
		{Key: "Date", Value: date},
		{Key: "Status", Value: status},
		{Key: "Reason", Value: reason},
		{Key: "Type", Value: st.Scalar("type").Display()},
		{Key: "Labels", Value: joinSorted(st.List("label"))},
		{Key: "Assignees", Value: joinSorted(st.List("assignee"))},
		{Key: "Milestone", Value: st.Scalar("milestone").Display()},
	}
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
	}
}

// relationHeaders renders one header per kind of relation the issue stores,
// each followed by the inverse a reader derived for that kind — "Parent", then
// "Children". A kind with nothing on either end prints nothing, the way an
// empty scalar does.
//
// Two display rules come out of docs/issues.md rather than out of the renderer:
//
//   - A symmetric kind is one header, not two. The same link written from both
//     ends is one relation, so the stored and derived targets merge and dedupe.
//   - An unknown kind renders only on the end that stored it. Inverting is
//     mechanical, but *naming* the inverse is not — this client does not know
//     what the other end of `ado.affects` is called, and inventing a word for
//     it would assert a direction nobody wrote.
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

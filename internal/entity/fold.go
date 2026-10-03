// The fold: resolving a set of events into current state. The three field
// kinds — scalar, list, thread — are the complete menu. See doc.go.

package entity

import (
	"fmt"
	"slices"
	"strings"
)

// Vocabulary is the type-specific part a caller contributes.
//
// Only scalars need naming. Lists and threads are recognised by the shape of
// the op — the `.add`/`.remove`/`.note` suffixes and the `comment` family — so
// a field kind the caller never mentions still folds, including one belonging
// to an entity type this client has never heard of.
type Vocabulary struct {
	// Scalars resolve last-write-wins on val.
	Scalars []string
}

// With returns the vocabulary extended by another.
//
// This is how a bridge's namespaced scalars reach the fold without the entity
// type ever naming a platform: the type contributes its portable vocabulary,
// each bridge contributes its own, and the command composes them. A client
// that composes nothing still folds complete state, because core state may
// never depend on a namespaced op (docs/blob-format.md).
func (v Vocabulary) With(other Vocabulary) Vocabulary {
	return Vocabulary{
		Scalars: append(append([]string(nil), v.Scalars...), other.Scalars...),
	}
}

// Comment is one entry of a thread. Its address is its own event id.
type Comment struct {
	Event Event
	// Body is the latest edit if the entry was edited, else the original val.
	Body Value
	// Retracted hides this entry's own body. Its replies survive: a cascade
	// would let one person destroy other people's content as a side effect of
	// withdrawing their own.
	Retracted bool
}

func (c Comment) ID() string { return c.Event.ID }

// Parent is the entry this one replies to, or "" for a root entry.
func (c Comment) Parent() string { return c.Event.Ref }

// Reaction is identified by the triple (author, value, target), never by the
// id of the react  Two events agreeing on all three are the same
// reaction however many times they were emitted — a double click, or the same
// person reacting from two offline replicas.
type Reaction struct {
	Author string
	Value  string
	Target string
}

// Member is one surviving entry of a list field: the pair that identifies it,
// and the id of the add event that put it there.
//
// The id is carried rather than derived later because a remove must name the
// add it undoes, never the value — two clones adding the same label produce
// two adds, and a remove by value would take both (docs/blob-format.md).
type Member struct {
	ID  string
	Val Value
	// Ref is set when the member points at another entity rather than being a
	// bare value: a relation's target. A member is the pair (Val, Ref), so a
	// list of pointers keyed on Val alone would collapse distinct members —
	// which in a grow-only set is unrecoverable.
	Ref string
	// Note is the latest annotation of this member, or the zero Value. It
	// resolves like a comment edit: an event addressing the add by id, highest
	// (c, id) winning.
	Note Value
}

// Key identifies a member for comparison: the pair, never the add's id. Two
// adds of one pair are two events and one member.
func (m Member) Key() string { return m.Val.Display() + "\x00" + m.Ref }

// State is folded entity state.
type State struct {
	// Create is the entity's create event, absent only from a malformed blob.
	Create *Event
	// Events is every parseable event, in (c, id) order.
	Events []Event
	// Scalars holds the resolved value of each scalar field.
	Scalars map[string]Value
	// Lists holds each list field's surviving members, in (c, id) order.
	Lists map[string][]Member
	// Reactions are deduped by triple, in first-seen order.
	Reactions []Reaction
	// Thread is every entry, in (c, id) order, retracted ones included.
	Thread []Comment
	// Annotations holds thread-entry annotations: the outer key is the entry's
	// id, the inner one the annotation's name — the part of the op after
	// "comment." — and the value the highest (c, id) among them.
	//
	// This is the annotation rule a list member already lives under, applied to
	// the thread kind: an event addressing another by id, last write winning.
	// `comment.edit` and `comment.remove` are the two the format names and both
	// are folded above; anything else an entity type spells this way — a review
	// comment's anchor, whether its thread is resolved — lands here without
	// this package knowing what it means.
	Annotations map[string]map[string]Value
}

// Annotation returns one thread-entry annotation, or the zero Value.
func (s State) Annotation(comment, name string) Value { return s.Annotations[comment][name] }

// Scalar returns a resolved scalar, or the zero Value if unset.
func (s State) Scalar(op string) Value { return s.Scalars[op] }

// List returns a value-only list field's members as strings, each once. A field
// whose members point somewhere — a relation — wants Distinct instead, which
// keeps the target the value alone does not carry.
func (s State) List(op string) []string {
	distinct := s.Distinct(op)
	out := make([]string, 0, len(distinct))
	for _, m := range distinct {
		out = append(out, m.Val.Display())
	}
	return out
}

// Distinct returns one Member per surviving member, in (c, id) order, keeping
// the earliest add of each.
//
// docs/blob-format.md defines presence per *member*, and a member is the pair
// (val, ref): two adds of one pair are two adds of one member, so it appears
// once however many events put it there — two clones adding the same label, or
// a push whose change comes back as the tracker's own event, both produce
// exactly that. Two adds agreeing on val alone are two members, which is why
// the pair and not the value is the key.
//
// Members keeps every add, because a remove has to name each one individually.
func (s State) Distinct(op string) []Member {
	members := s.Lists[op]
	out := make([]Member, 0, len(members))
	seen := make(map[string]bool, len(members))
	for _, m := range members {
		if seen[m.Key()] {
			continue
		}
		seen[m.Key()] = true
		out = append(out, m)
	}
	return out
}

// Members returns a resolved list field's members with the id of each one's
// add event, which is what a writer needs to remove one.
func (s State) Members(op string) []Member { return s.Lists[op] }

// FindComment matches an abbreviated event id against this entity's thread.
//
// Thread entries abbreviate like object names do, and an ambiguous prefix is
// refused rather than resolved arbitrarily — the same rule Store.Resolve
// applies to an entity id, for the same reason.
//
// Deliberately a prefix of the id and never an ordinal. Entries are ordered by
// (c, id), so one merged in from another clone lands wherever its clock puts
// it, which is routinely in the middle: an ordinal would silently renumber
// every entry after it on a pull, and an edit typed from a listing read a
// moment earlier would rewrite somebody else's comment. Ids do not move.
//
// Resolution is scoped to the one entity, so a prefix competes only with that
// entity's own comments and stays short in practice.
func (s State) FindComment(prefix string) (Comment, error) {
	if prefix == "" {
		return Comment{}, fmt.Errorf("no comment given")
	}

	var matches []Comment
	for _, c := range s.Thread {
		if strings.HasPrefix(c.ID(), prefix) {
			matches = append(matches, c)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Comment{}, fmt.Errorf("unknown comment '%s'", prefix)
	default:
		return Comment{}, fmt.Errorf("ambiguous comment '%s': matches %d comments", prefix, len(matches))
	}
}

// SameAs reports whether two folded states say the same thing.
//
// Folded state, not events: a blob can grow without anything a reader sees
// changing. That is the ordinary outcome of pushing to a bridge — the tracker
// records a change of its own for what this clone asked it to do, and the next
// import writes that event faithfully, saying what the blob already said. The
// entity is genuinely different at the byte level and identical at every level
// anybody looks at.
//
// Deliberately not a comparison of event sets, and deliberately not of add ids:
// two adds of one member are one member, so a list is compared by its members —
// each one's pair and its annotation, never the id of the add behind it.
func (s State) SameAs(other State) bool {
	if len(s.Scalars) != len(other.Scalars) {
		return false
	}
	for op, v := range s.Scalars {
		if w, ok := other.Scalars[op]; !ok || v != w {
			return false
		}
	}

	if len(s.Lists) != len(other.Lists) {
		return false
	}
	for field := range s.Lists {
		mine, theirs := s.Distinct(field), other.Distinct(field)
		if len(mine) != len(theirs) {
			return false
		}
		for i, m := range mine {
			if m.Key() != theirs[i].Key() || m.Note != theirs[i].Note {
				return false
			}
		}
	}

	if len(s.Thread) != len(other.Thread) {
		return false
	}
	for i, c := range s.Thread {
		d := other.Thread[i]
		if c.ID() != d.ID() || c.Body != d.Body || c.Retracted != d.Retracted || c.Parent() != d.Parent() {
			return false
		}
	}

	if len(s.Annotations) != len(other.Annotations) {
		return false
	}
	for comment, mine := range s.Annotations {
		theirs, ok := other.Annotations[comment]
		if !ok || len(mine) != len(theirs) {
			return false
		}
		for name, v := range mine {
			if w, ok := theirs[name]; !ok || v != w {
				return false
			}
		}
	}

	return slices.Equal(s.Reactions, other.Reactions)
}

// NextClock is the clock a new event appended to this entity should carry: one
// past the highest already there.
//
// Lamport, not wall time — an event written on a clone that has not seen these
// events will collide, and that is exactly what the (c, id) tie-break is for.
func (s State) NextClock() int64 {
	var max int64
	for _, ev := range s.Events {
		if ev.C > max {
			max = ev.C
		}
	}
	return max + 1
}

// Fold resolves events into current state.
//
// Events are sorted by (c, id) first, so "last write wins" is a plain
// assignment in loop order and needs no comparison per field.
func Fold(vocab Vocabulary, events []Event) State {
	events = slices.Clone(events)
	slices.SortFunc(events, Less)

	st := State{
		Events:      events,
		Scalars:     map[string]Value{},
		Lists:       map[string][]Member{},
		Annotations: map[string]map[string]Value{},
	}

	type listAdd struct {
		field string
		val   Value
		ref   string
	}
	var (
		addOrder  []string
		adds      = map[string]listAdd{}
		notes     = map[string]Value{}
		reactions = map[string]Reaction{}
		removed   = map[string]bool{}

		commentOrder []string
		comments     = map[string]Event{}
		bodies       = map[string]Value{}
		retracted    = map[string]bool{}
	)

	for _, ev := range events {
		switch {
		case ev.Op == "create":
			if st.Create == nil {
				e := ev
				st.Create = &e
			}

		case slices.Contains(vocab.Scalars, ev.Op):
			st.Scalars[ev.Op] = ev.Val

		// Checked before the generic .remove branch below: a comment
		// tombstone hides one entry, while a list remove retracts a member.
		case ev.Op == "comment.remove" && ev.Ref != "":
			retracted[ev.Ref] = true

		case ev.Op == "react":
			addOrder = append(addOrder, ev.ID)
			reactions[ev.ID] = Reaction{Author: ev.A, Value: ev.Val.Display(), Target: ev.Ref}

		case hasSuffix(ev.Op, ".add"):
			addOrder = append(addOrder, ev.ID)
			adds[ev.ID] = listAdd{field: ev.Op[:len(ev.Op)-len(".add")], val: ev.Val, ref: ev.Ref}

		case hasSuffix(ev.Op, ".remove") && ev.Ref != "":
			removed[ev.Ref] = true

		// An annotation addresses the add event, so events being in (c, id)
		// order makes the last one win by plain assignment. One naming an add
		// this reader does not hold falls out below, where nothing reads it.
		case hasSuffix(ev.Op, ".note") && ev.Ref != "":
			notes[ev.Ref] = ev.Val

		case ev.Op == "comment":
			commentOrder = append(commentOrder, ev.ID)
			comments[ev.ID] = ev

		case ev.Op == "comment.edit" && ev.Ref != "":
			// Events are already ordered, so the last edit simply wins.
			bodies[ev.Ref] = ev.Val

		// Any other comment.<name> addressing an entry is an annotation of it,
		// resolving the way an edit does. Reached only after edit and remove
		// have had their branches, and after the .add/.remove/.note suffixes
		// above have had theirs, so nothing that already means something is
		// swallowed here.
		case strings.HasPrefix(ev.Op, "comment.") && ev.Ref != "":
			name := ev.Op[len("comment."):]
			if st.Annotations[ev.Ref] == nil {
				st.Annotations[ev.Ref] = map[string]Value{}
			}
			st.Annotations[ev.Ref][name] = ev.Val
		}
		// Anything else is vocabulary this client does not know. It is folded
		// into nothing and preserved on disk untouched.
	}

	// A member is present iff it has at least one add whose id is named by no
	// remove. Concurrent add and remove therefore resolve add-wins: a remove
	// can only retract adds it has actually seen.
	seen := map[Reaction]bool{}
	for _, id := range addOrder {
		if removed[id] {
			continue
		}
		if r, ok := reactions[id]; ok {
			if !seen[r] {
				seen[r] = true
				st.Reactions = append(st.Reactions, r)
			}
			continue
		}
		a := adds[id]
		st.Lists[a.field] = append(st.Lists[a.field], Member{ID: id, Val: a.val, Ref: a.ref, Note: notes[id]})
	}

	for _, id := range commentOrder {
		ev := comments[id]
		c := Comment{Event: ev, Body: ev.Val, Retracted: retracted[id]}
		if edited, ok := bodies[id]; ok {
			c.Body = edited
		}
		st.Thread = append(st.Thread, c)
	}
	return st
}

func hasSuffix(s, suffix string) bool {
	return len(s) > len(suffix) && s[len(s)-len(suffix):] == suffix
}

// Forest groups thread entries by parent, keyed by parent id with "" for the
// roots.
//
// An entry whose parent is unknown belongs at root and must never be hidden
// pending its parent's arrival: parents go missing routinely — partial fetch,
// a parent never pushed — and a fold that waits for one loses somebody's
// comment with no error.
func Forest(thread []Comment) map[string][]Comment {
	known := make(map[string]bool, len(thread))
	for _, c := range thread {
		known[c.ID()] = true
	}

	children := map[string][]Comment{}
	for _, c := range thread {
		parent := c.Parent()
		if !known[parent] {
			parent = ""
		}
		children[parent] = append(children[parent], c)
	}
	for _, siblings := range children {
		slices.SortFunc(siblings, func(a, b Comment) int { return Less(a.Event, b.Event) })
	}
	return children
}

// ReactionsByTarget groups reactions by the event they target, then by value.
func ReactionsByTarget(reactions []Reaction) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for _, r := range reactions {
		if out[r.Target] == nil {
			out[r.Target] = map[string][]string{}
		}
		out[r.Target][r.Value] = append(out[r.Target][r.Value], r.Author)
	}
	return out
}

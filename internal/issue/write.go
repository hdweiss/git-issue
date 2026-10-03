package issue

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/entity"
)

// Fields are the values an issue is born with. Only Title is required; every
// other field is written only when it carries something, so an issue created
// with nothing but a title holds exactly the events docs/issues.md says it
// should and no empty ones.
type Fields struct {
	Title       string
	Description string
	Type        string
	Milestone   string

	// Status is the issue's status, and StatusReason why it reached it. Status
	// has no cleared state — an issue with no status event is open, and
	// `git issue reopen` is how you get back there — so an empty Status means
	// "leave it alone", never "clear it". FieldsOf fills Status with the
	// resolved value (open, if there is no event), so an edit that does not
	// touch it round-trips unchanged. StatusReason clears to null like any
	// other scalar.
	Status       string
	StatusReason string

	Labels    []string
	Assignees []string

	// Relations is the whole set of links the issue should end with, or nil
	// for "nobody said". Nil and empty differ: empty asks for every relation
	// to be retracted, which is what detaching an issue from everything means.
	//
	// FieldsOf never sets it, deliberately. An issue's fields make a round trip
	// through an editor, and a field that can be blanked by deleting a line is
	// a field an editor can silently detach; leaving this one nil on the way in
	// means the editor has no path to it at all. It is written by
	// `add <parent>` and by `edit --parent` / `--rel`, which say so explicitly.
	Relations []Relation
}

// Create writes a new issue and returns its id.
//
// The entity id is the create event's own blob hash, so the create event has
// to be built before anything can be keyed by it. Note that the object is
// never written out: git notes keys are tree path strings, not object
// pointers, and a blob minted only to produce an id would not survive gc or a
// fresh clone anyway.
func Create(s *entity.Store, author string, ts int64, f Fields) (string, error) {
	if f.Title == "" {
		return "", fmt.Errorf("an issue needs a title")
	}

	create, err := entity.NewEvent(s.Format(), entity.FormatVersion, 1, ts, author, "create", entity.Str(Type), "")
	if err != nil {
		return "", err
	}

	// Field-level events, never a whole-record snapshot: a snapshot lets a
	// late writer silently clobber an unrelated concurrent field change. The
	// clock counts up across every field, so the order they were typed in is
	// the order they resolve in — which only matters if two of them ever name
	// the same field, and none of these do.
	events := []entity.Event{create}
	c := int64(1)
	add := func(op, val string) error {
		if val == "" {
			return nil
		}
		c++
		e, err := entity.NewEvent(s.Format(), entity.FormatVersion, c, ts, author, op, entity.Str(val), "")
		if err != nil {
			return err
		}
		events = append(events, e)
		return nil
	}
	// A list field is one add event per member, addressable individually:
	// that is what lets a later remove name the add it undoes rather than the
	// value, which is what makes the OR-Set converge (docs/issues.md).
	addAll := func(op string, vals []string) error {
		for _, v := range dedupe(vals) {
			if err := add(op, v); err != nil {
				return err
			}
		}
		return nil
	}

	// The order below is the order the events are written in, and the only
	// thing it decides is the clock each one carries. No two of these name the
	// same field, so nothing here resolves against anything else.
	for _, w := range []struct {
		op   string
		vals []string
	}{
		{"title", []string{f.Title}},
		{"status", []string{StatusOpen}},
		{"description", []string{f.Description}},
		{"type", []string{f.Type}},
		{"milestone", []string{f.Milestone}},
		{"label.add", f.Labels},
		{"assignee.add", f.Assignees},
	} {
		if err := addAll(w.op, w.vals); err != nil {
			return "", err
		}
	}

	// A relation carries its kind in val and its target in ref, so it cannot go
	// through add above. Written last, and only where it points somewhere: an
	// issue born linked to nothing needs no event saying so.
	for _, r := range dedupeRelations(f.Relations) {
		c++
		e, err := entity.NewEvent(s.Format(), entity.FormatVersion, c, ts, author, RelAdd, entity.Str(r.Kind), r.Target)
		if err != nil {
			return "", err
		}
		events = append(events, e)
	}

	if err := write(s, author, ts, change{id: create.ID, events: events}); err != nil {
		return "", err
	}
	return create.ID, nil
}

// Comment appends one entry to an issue's thread and returns the entry's id.
//
// The entry's own id is returned rather than the issue's because that id is the
// address: an edit, a retraction and a reaction all name the comment event, and
// none of them can be written without it (docs/blob-format.md). parent is the
// id of the entry this one replies to, or "" to start a new one at the root.
func Comment(s *entity.Store, author string, ts int64, id string, st entity.State, body string, parent string) (string, error) {
	body, err := commentBody(body)
	if err != nil {
		return "", err
	}

	e, err := entity.NewEvent(s.Format(), entity.FormatVersion, st.NextClock(), ts, author, "comment", entity.Str(body), parent)
	if err != nil {
		return "", err
	}
	if err := write(s, author, ts, change{id: id, title: FieldsOf(st).Title, events: []entity.Event{e}}); err != nil {
		return "", err
	}
	return e.ID, nil
}

// EditComment rewrites one thread entry's body, and reports whether it wrote
// anything: an edit that says what the entry already says writes nothing, the
// way an issue edit that changes nothing does.
//
// The edit event addresses the entry by id and carries the whole new body. It
// does not replace the original event — nothing ever does — so the thread keeps
// both, and a reader that folds them sees the last edit win.
func EditComment(s *entity.Store, author string, ts int64, id string, st entity.State, c entity.Comment, body string) (bool, error) {
	body, err := commentBody(body)
	if err != nil {
		return false, err
	}
	if c.Retracted {
		return false, fmt.Errorf("comment %s is retracted", c.ID())
	}
	if body == c.Body.Display() {
		return false, nil
	}

	e, err := entity.NewEvent(s.Format(), entity.FormatVersion, st.NextClock(), ts, author, "comment.edit", entity.Str(body), c.ID())
	if err != nil {
		return false, err
	}
	return true, write(s, author, ts, change{id: id, title: FieldsOf(st).Title, events: []entity.Event{e}})
}

// RemoveComment retracts one thread entry, and reports whether it wrote
// anything.
//
// A tombstone hides the entry's body; it does not remove the text from the blob
// (docs/blob-format.md), and it does not cascade to replies — a cascade would
// let one person destroy other people's content as a side effect of withdrawing
// their own.
func RemoveComment(s *entity.Store, author string, ts int64, id string, st entity.State, c entity.Comment) (bool, error) {
	if c.Retracted {
		return false, nil
	}

	e, err := entity.NewEvent(s.Format(), entity.FormatVersion, st.NextClock(), ts, author, "comment.remove", entity.Value{}, c.ID())
	if err != nil {
		return false, err
	}
	return true, write(s, author, ts, change{id: id, title: FieldsOf(st).Title, events: []entity.Event{e}})
}

// commentBody normalises what someone typed and refuses an empty comment, the
// way an empty title aborts an issue. Trailing blank lines an editor leaves
// behind are not content and would otherwise change the entry's id.
func commentBody(body string) (string, error) {
	body = strings.TrimRight(body, " \t\n")
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("a comment needs a body")
	}
	return body, nil
}

// write files an action onto the ref: one commit, authored by whoever is
// writing, saying what they did.
//
// It goes through Apply rather than `git notes add`/`append` so that a local
// write and a bridged one produce the same kind of commit. Apply appends and
// never rewrites, which is the invariant the raw notes commands were being used
// for in the first place: an existing line is preserved byte for byte,
// including lines written by a client that knows ops this one does not.
// change is one entity's worth of a write: the events, and what each remove
// retracted so the commit message can name it.
type change struct {
	id     string
	title  string
	events []entity.Event
	// removed maps a retracted add's event id to what it said, for the commit
	// message: a remove addresses an add by id and carries no value at all.
	removed map[string]string
}

// write commits one or more entities' events.
//
// More than one because a symmetric link is stored at both ends, so retracting
// it is a change to two issues — and it goes in one Apply so that a run
// interrupted between them cannot leave the two ends disagreeing.
func write(s *entity.Store, author string, ts int64, changes ...change) error {
	var incoming []entity.Incoming
	for _, c := range changes {
		if len(c.events) == 0 {
			continue
		}
		incoming = append(incoming, entity.Incoming{
			ID: c.id,
			Actions: []entity.Action{{
				Author:  s.Repo.Author(author, time.Unix(ts, 0)),
				Message: Action{ID: c.id, Title: c.title, Events: c.events, Removed: c.removed}.Message(),
				Events:  c.events,
			}},
		})
	}
	if len(incoming) == 0 {
		return nil
	}
	_, err := s.Apply("", incoming)
	return err
}

// dedupe drops blanks and repeats, keeping the order given. Two adds of the
// same label are two events the fold would collapse anyway; writing one is
// simply the honest record of what was asked for.
func dedupe(vals []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vals {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// FieldsOf reads an issue's current values back out of folded state. It is the
// inverse of what Create writes, and what an editor is handed to amend.
//
// Parent is left nil on purpose — see the field's own comment. What an editor
// is not shown, an editor cannot detach.
func FieldsOf(st entity.State) Fields {
	return Fields{
		Title:        st.Scalar("title").Display(),
		Description:  st.Scalar("description").Display(),
		Type:         st.Scalar("type").Display(),
		Milestone:    st.Scalar("milestone").Display(),
		Status:       Status(st),
		StatusReason: st.Scalar("status.reason").Display(),
		Labels:       st.List("label"),
		Assignees:    st.List("assignee"),
	}
}

// Update appends the events that carry an issue from its current state to the
// given fields, and reports which fields it touched. An issue that already
// says this writes nothing and reports nothing.
//
// One event per changed field, never a snapshot of all of them: a snapshot
// written from a stale read silently reverts every field somebody else changed
// meanwhile, while a field-level event only ever contends for its own field.
//
// It also reports every *other* issue it wrote to. Retracting a symmetric link
// is a change to both ends — see farEnds — and a command that wrote to an issue
// nobody named has to say so.
func Update(s *entity.Store, author string, ts int64, id string, st entity.State, want Fields) (changed []string, detached []string, err error) {
	if want.Title == "" {
		return nil, nil, fmt.Errorf("an issue needs a title")
	}

	var (
		events []entity.Event
		c      = st.NextClock()
		// What each remove event retracts, so the commit message can name the
		// label or assignee that went away: the event itself addresses an add
		// by id and carries no value at all.
		removed = map[string]string{}
	)
	emit := func(op string, val entity.Value, ref string) error {
		e, err := entity.NewEvent(s.Format(), entity.FormatVersion, c, ts, author, op, val, ref)
		if err != nil {
			return err
		}
		c++
		events = append(events, e)
		return nil
	}

	// A scalar that was cleared is set to null rather than to the empty
	// string: null is what docs/issues.md gives as "no milestone", and it
	// reads back as deliberately unset rather than as a value that happens to
	// be blank.
	have := FieldsOf(st)

	// status has no cleared state, so an empty want.Status is "leave it": only
	// a concrete value that differs writes an event. `reopen` sets it to open,
	// `close` to closed, `edit --status` to whatever a bridge uses.
	if want.Status != "" && want.Status != have.Status {
		if err := emit("status", entity.Str(want.Status), ""); err != nil {
			return nil, nil, err
		}
		changed = append(changed, "status")
	}

	for _, f := range []struct{ op, name, from, to string }{
		{"title", "title", have.Title, want.Title},
		{"description", "description", have.Description, want.Description},
		{"type", "type", have.Type, want.Type},
		{"milestone", "milestone", have.Milestone, want.Milestone},
		{"status.reason", "reason", have.StatusReason, want.StatusReason},
	} {
		if f.from == f.to {
			continue
		}
		val := entity.Str(f.to)
		if f.to == "" {
			val = entity.Null()
		}
		if err := emit(f.op, val, ""); err != nil {
			return nil, nil, err
		}
		changed = append(changed, f.name)
	}

	for _, f := range []struct {
		field string
		want  []string
	}{
		{"label", want.Labels},
		{"assignee", want.Assignees},
	} {
		touched, err := reconcile(st, f.field, f.want, removed, emit)
		if err != nil {
			return nil, nil, err
		}
		if touched {
			changed = append(changed, f.field+"s")
		}
	}

	// The relations, reconciled like any other list: an add per new link, and a
	// remove naming each add whose link is gone. Nil means no flag mentioned
	// them, which is not the same as an empty set asking for all of them to go.
	var others []change
	if want.Relations != nil {
		dropped, touched, err := reconcileRelations(st, want.Relations, removed, emit)
		if err != nil {
			return nil, nil, err
		}
		if touched {
			changed = append(changed, "relations")
		}
		// A symmetric link is stored at whichever end wrote it, so letting go of
		// it here is only half of letting go of it.
		if others, err = farEnds(s, author, ts, id, dropped); err != nil {
			return nil, nil, err
		}
	}

	if len(events) == 0 && len(others) == 0 {
		return nil, nil, nil
	}
	// One Apply for both ends: a link that is gone at one end and standing at
	// the other is exactly the state this is here to avoid.
	near := change{id: id, title: have.Title, events: events, removed: removed}
	if err := write(s, author, ts, append([]change{near}, others...)...); err != nil {
		return nil, nil, err
	}
	for _, c := range others {
		detached = append(detached, c.id)
	}
	return changed, detached, nil
}

// reconcile brings one list field to the wanted members: an add per new value,
// and a remove per surviving add whose value is gone.
//
// The removes name add events, not values. A value present twice — two clones
// having added the same label — is therefore two removes, which is exactly
// right: retracting only one of them would leave the label in place.
func reconcile(st entity.State, field string, want []string, removed map[string]string, emit func(op string, val entity.Value, ref string) error) (bool, error) {
	want = dedupe(want)
	touched := false

	present := map[string]bool{}
	for _, m := range st.Members(field) {
		value := m.Val.Display()
		present[value] = true
		if slices.Contains(want, value) {
			continue
		}
		if err := emit(field+".remove", entity.Value{}, m.ID); err != nil {
			return touched, err
		}
		removed[m.ID] = value
		touched = true
	}

	for _, value := range want {
		if present[value] {
			continue
		}
		if err := emit(field+".add", entity.Str(value), ""); err != nil {
			return touched, err
		}
		touched = true
	}
	return touched, nil
}

// reconcileRelations is reconcile for the one list whose members point at
// something: it keys on the pair (kind, target) rather than on a value, which
// is what tells one kind pointing at two entities apart from one link
// (docs/blob-format.md).
//
// This is also where "at most one parent" is enforced, to the extent anything
// can enforce it: a caller that wants a single-valued kind hands in the one
// relation it wants, every other member of that kind is not in the wanted set,
// and each gets a remove naming its own add. Two clones doing that concurrently
// still merge to two parents — no writer can prevent that — which is why a
// reader picks a winner and shows the rest.
// It reports the pairs it retracted alongside whether it wrote anything at all.
// The two are not the same question — an update that only adds a link has
// touched the field and dropped nothing — and the dropped pairs are what the
// far end of a symmetric link is worked out from.
func reconcileRelations(st entity.State, want []Relation, removed map[string]string, emit func(op string, val entity.Value, ref string) error) (dropped []Relation, touched bool, err error) {
	want = dedupeRelations(want)

	wanted := make(map[string]bool, len(want))
	for _, r := range want {
		wanted[r.Key()] = true
	}

	present := map[string]bool{}
	for _, m := range st.Members(RelField) {
		if m.Ref == "" {
			continue
		}
		r := Relation{Kind: m.Val.Display(), Target: m.Ref}
		present[r.Key()] = true
		if wanted[r.Key()] {
			continue
		}
		if err := emit(RelRemove, entity.Value{}, m.ID); err != nil {
			return dropped, touched, err
		}
		// What the remove retracted, for the commit message: the event itself
		// addresses an add by id and carries neither kind nor target.
		removed[m.ID] = r.Kind + " " + r.Target
		dropped = append(dropped, r)
		touched = true
	}

	for _, r := range want {
		if present[r.Key()] {
			continue
		}
		if err := emit(RelAdd, entity.Str(r.Kind), r.Target); err != nil {
			return dropped, touched, err
		}
		touched = true
	}
	return dropped, touched, nil
}

// farEnds retracts the other end of every symmetric link this update dropped.
//
// A symmetric kind has no dependent end — either issue may write the link, and
// a reader treats the unordered pair as one — so a repository that has seen
// both ends holds two members saying the same thing. Retracting only the near
// one leaves the link standing: the fold still finds a member, and on a bridged
// issue the next pull imports the far end's surviving member straight back.
// Both members are the link, so both go.
//
// This is the one write that reaches an issue nobody named, which is why Update
// reports the ones it touched rather than doing it quietly.
//
// A target this repository does not hold has no far end to retract, and that is
// ordinary rather than an error: a relation is not promised to name something
// local (see Parent).
func farEnds(s *entity.Store, author string, ts int64, id string, dropped []Relation) ([]change, error) {
	kinds := map[string][]string{}
	for _, r := range dropped {
		if k, ok := KnownKind(r.Kind); !ok || !k.Symmetric {
			continue
		}
		kinds[r.Target] = append(kinds[r.Target], r.Kind)
	}
	if len(kinds) == 0 {
		return nil, nil
	}

	notes, err := s.Notes()
	if err != nil {
		return nil, err
	}
	held := notes[:0]
	for _, n := range notes {
		if kinds[n.Entity] != nil {
			held = append(held, n)
		}
	}

	var out []change
	err = s.Each(held, func(target string, st entity.State) error {
		far := change{id: target, title: st.Scalar("title").Display(), removed: map[string]string{}}
		c := st.NextClock()
		for _, m := range st.Members(RelField) {
			kind := m.Val.Display()
			if m.Ref != id || !slices.Contains(kinds[target], kind) {
				continue
			}
			e, err := entity.NewEvent(s.Format(), entity.FormatVersion, c, ts, author, RelRemove, entity.Value{}, m.ID)
			if err != nil {
				return err
			}
			c++
			far.events = append(far.events, e)
			far.removed[m.ID] = kind + " " + id
		}
		if len(far.events) > 0 {
			out = append(out, far)
		}
		return nil
	})
	return out, err
}

// dedupeRelations drops incomplete and repeated links, keeping the order given.
// Two adds of one pair are one member, so writing both is noise rather than a
// second link.
func dedupeRelations(rels []Relation) []Relation {
	seen := map[string]bool{}
	var out []Relation
	for _, r := range rels {
		if r.Kind == "" || r.Target == "" || seen[r.Key()] {
			continue
		}
		seen[r.Key()] = true
		out = append(out, r)
	}
	return out
}

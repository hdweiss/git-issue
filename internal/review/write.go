package review

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/entity"
)

// Fields are the values a review is born with, and what an edit brings it to.
// Only Title is required; every other field is written only when it carries
// something.
type Fields struct {
	Title       string
	Description string
	Milestone   string

	// Base is the branch being merged into, a bare name — the base always lives
	// in the repository the review targets, so there is nothing for a remote to
	// qualify. Head is the branch being merged from, remote-qualified where the
	// review has a remote, because a fork's head has no meaning as a bare name.
	Base string
	Head string
	// HeadSHA is the commit the review currently describes. It exists rather
	// than being resolved from Head because Head is a moving name whose
	// resolution differs per clone and which routinely stops resolving at all —
	// a branch deleted after a merge is the normal end of a review's life.
	HeadSHA string

	// Status has no cleared state — a review with no status event is open — so
	// an empty Status means "leave it alone", never "clear it".
	Status       string
	StatusReason string

	// Draft is whether the review is soliciting verdicts. A convention, not an
	// enforcement mechanism: nothing in the object store stops anyone approving
	// a draft.
	Draft *bool

	Labels    []string
	Assignees []string

	// Relations is the whole set of links the review should end with, or nil
	// for "nobody said". Nil and empty differ: empty asks for every relation to
	// be retracted.
	//
	// FieldsOf never sets it, for the reason internal/issue gives: a field an
	// editor can blank by deleting a line is a field an editor can silently
	// detach.
	Relations []Relation
}

// Create writes a new review and returns its id.
func Create(s *entity.Store, author string, ts int64, f Fields) (string, error) {
	if f.Title == "" {
		return "", fmt.Errorf("a review needs a title")
	}

	create, err := entity.NewEvent(s.Format(), entity.FormatVersion, 1, ts, author, "create", entity.Str(Type), "")
	if err != nil {
		return "", err
	}

	events := []entity.Event{create}
	c := int64(1)
	emit := func(op string, val entity.Value, ref string) error {
		c++
		e, err := entity.NewEvent(s.Format(), entity.FormatVersion, c, ts, author, op, val, ref)
		if err != nil {
			return err
		}
		events = append(events, e)
		return nil
	}
	add := func(op, val string) error {
		if val == "" {
			return nil
		}
		return emit(op, entity.Str(val), "")
	}
	addAll := func(op string, vals []string) error {
		for _, v := range dedupe(vals) {
			if err := add(op, v); err != nil {
				return err
			}
		}
		return nil
	}

	for _, w := range []struct {
		op   string
		vals []string
	}{
		{"title", []string{f.Title}},
		{"status", []string{StatusOpen}},
		{"description", []string{f.Description}},
		{"base", []string{f.Base}},
		{"head", []string{f.Head}},
		{"head.sha", []string{f.HeadSHA}},
		{"milestone", []string{f.Milestone}},
		{"label.add", f.Labels},
		{"assignee.add", f.Assignees},
	} {
		if err := addAll(w.op, w.vals); err != nil {
			return "", err
		}
	}

	// Only written when true. A review that is not a draft needs no event
	// saying so, the same way one linked to nothing needs no relation.
	if f.Draft != nil && *f.Draft {
		if err := emit("draft", entity.Bool(true), ""); err != nil {
			return "", err
		}
	}

	for _, r := range dedupeRelations(f.Relations) {
		if err := emit(RelAdd, entity.Str(r.Kind), r.Target); err != nil {
			return "", err
		}
	}

	if err := write(s, author, ts, change{id: create.ID, events: events}); err != nil {
		return "", err
	}
	return create.ID, nil
}

// Comment appends one entry to a review's thread and returns the entry's id.
//
// An anchor, where one is given, is written in the same action: a review
// comment and where it sits are one thing a person did, and splitting them
// across two commits would leave a window in which the comment floats.
func Comment(s *entity.Store, author string, ts int64, id string, st entity.State, body string, parent string, anchor *Anchor) (string, error) {
	body, err := commentBody(body)
	if err != nil {
		return "", err
	}

	c := st.NextClock()
	e, err := entity.NewEvent(s.Format(), entity.FormatVersion, c, ts, author, "comment", entity.Str(body), parent)
	if err != nil {
		return "", err
	}
	events := []entity.Event{e}

	if anchor != nil {
		a, err := entity.NewEvent(s.Format(), entity.FormatVersion, c+1, ts, author, AnchorOp, entity.Str(anchor.String()), e.ID)
		if err != nil {
			return "", err
		}
		events = append(events, a)
	}

	if err := write(s, author, ts, change{id: id, title: titleOf(st), events: events}); err != nil {
		return "", err
	}
	return e.ID, nil
}

// EditComment rewrites one thread entry's body, and reports whether it wrote
// anything.
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
	return true, write(s, author, ts, change{id: id, title: titleOf(st), events: []entity.Event{e}})
}

// RemoveComment retracts one thread entry, and reports whether it wrote
// anything. A tombstone hides the entry's body; it does not erase the text and
// it does not cascade to replies.
func RemoveComment(s *entity.Store, author string, ts int64, id string, st entity.State, c entity.Comment) (bool, error) {
	if c.Retracted {
		return false, nil
	}

	e, err := entity.NewEvent(s.Format(), entity.FormatVersion, st.NextClock(), ts, author, "comment.remove", entity.Value{}, c.ID())
	if err != nil {
		return false, err
	}
	return true, write(s, author, ts, change{id: id, title: titleOf(st), events: []entity.Event{e}})
}

// SetResolved marks a thread resolved or unresolved, and reports whether it
// wrote anything.
//
// The annotation addresses the thread's *root* entry, which is what makes
// resolution a property of the conversation rather than of the last thing said
// in it — so a caller naming a reply has its root resolved instead.
//
// Resolution is display state, not retraction: a resolved thread still folds,
// still renders, and still carries every word in it. The difference from a
// tombstone is the difference between addressed and withdrawn.
//
// message, when given, is posted as a reply in the same action. Resolving
// without saying why is allowed and common; saying why is a second event, not a
// second command.
func SetResolved(s *entity.Store, author string, ts int64, id string, st entity.State, root string, resolved bool, message string) (bool, error) {
	if Resolved(st, root) == resolved && message == "" {
		return false, nil
	}

	c := st.NextClock()
	var events []entity.Event

	if message != "" {
		body, err := commentBody(message)
		if err != nil {
			return false, err
		}
		e, err := entity.NewEvent(s.Format(), entity.FormatVersion, c, ts, author, "comment", entity.Str(body), root)
		if err != nil {
			return false, err
		}
		events = append(events, e)
		c++
	}

	if Resolved(st, root) != resolved {
		e, err := entity.NewEvent(s.Format(), entity.FormatVersion, c, ts, author, ResolveOp, entity.Bool(resolved), root)
		if err != nil {
			return false, err
		}
		events = append(events, e)
	}

	if len(events) == 0 {
		return false, nil
	}
	return true, write(s, author, ts, change{id: id, title: titleOf(st), events: events})
}

// SetVerdict records one person's position on the review, and reports whether
// it wrote anything.
//
// The event's ref is the head.sha the verdict is cast against, so a reader can
// tell later that it describes an older revision. That is stored rather than
// inferred because inferring it would mean comparing timestamps, and ts must
// never influence resolution (docs/blob-format.md).
//
// Casting a verdict supersedes the author's previous one under the reader rule
// in VerdictsOf, so nothing is retracted here: both members stay in the blob,
// and the record that they once approved survives.
func SetVerdict(s *entity.Store, author string, ts int64, id string, st entity.State, value, message string) (bool, error) {
	head := Head(st)

	// A repeat of what this author already says, against the same revision,
	// with nothing to add.
	if have, ok := VerdictBy(st, author); ok && have.Value == value && have.Revision == head && message == "" {
		return false, nil
	}

	c := st.NextClock()
	var events []entity.Event

	if message != "" {
		body, err := commentBody(message)
		if err != nil {
			return false, err
		}
		e, err := entity.NewEvent(s.Format(), entity.FormatVersion, c, ts, author, "comment", entity.Str(body), "")
		if err != nil {
			return false, err
		}
		events = append(events, e)
		c++
	}

	e, err := entity.NewEvent(s.Format(), entity.FormatVersion, c, ts, author, VerdictAdd, entity.Str(value), head)
	if err != nil {
		return false, err
	}
	events = append(events, e)

	return true, write(s, author, ts, change{id: id, title: titleOf(st), events: events})
}

// Dismiss retracts one verdict by naming the add that cast it.
//
// This is the only write in the vocabulary aimed at another person's event.
// Retracting your own verdict needs no operation — casting another supersedes
// it — so this exists for the case where somebody else's approval is being
// taken off the review, and a caller should say whose it was.
func Dismiss(s *entity.Store, author string, ts int64, id string, st entity.State, verdict Verdict, message string) (bool, error) {
	c := st.NextClock()
	var events []entity.Event

	if message != "" {
		body, err := commentBody(message)
		if err != nil {
			return false, err
		}
		e, err := entity.NewEvent(s.Format(), entity.FormatVersion, c, ts, author, "comment", entity.Str(body), "")
		if err != nil {
			return false, err
		}
		events = append(events, e)
		c++
	}

	e, err := entity.NewEvent(s.Format(), entity.FormatVersion, c, ts, author, VerdictRemove, entity.Value{}, verdict.ID)
	if err != nil {
		return false, err
	}
	events = append(events, e)

	removed := map[string]string{verdict.ID: verdict.Value + " by " + verdict.Author}
	return true, write(s, author, ts, change{id: id, title: titleOf(st), events: events, removed: removed})
}

// commentBody normalises what someone typed and refuses an empty comment.
func commentBody(body string) (string, error) {
	body = strings.TrimRight(body, " \t\n")
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("a comment needs a body")
	}
	return body, nil
}

// change is one entity's worth of a write: the events, and what each remove
// retracted so the commit message can name it.
type change struct {
	id      string
	title   string
	events  []entity.Event
	removed map[string]string
}

// write files an action onto the ref: one commit, authored by whoever is
// writing, saying what they did.
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

// dedupe drops blanks and repeats, keeping the order given.
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

// FieldsOf reads a review's current values back out of folded state. It is the
// inverse of what Create writes, and what an editor is handed to amend.
func FieldsOf(st entity.State) Fields {
	draft := st.Scalar("draft").Truthy()
	return Fields{
		Title:        st.Scalar("title").Display(),
		Description:  st.Scalar("description").Display(),
		Milestone:    st.Scalar("milestone").Display(),
		Base:         st.Scalar("base").Display(),
		Head:         st.Scalar("head").Display(),
		HeadSHA:      Head(st),
		Status:       Status(st),
		StatusReason: st.Scalar("status.reason").Display(),
		Draft:        &draft,
		Labels:       st.List("label"),
		Assignees:    st.List("assignee"),
	}
}

// Update appends the events that carry a review from its current state to the
// given fields, and reports which fields it touched. A review that already says
// this writes nothing and reports nothing.
//
// One event per changed field, never a snapshot: a snapshot written from a
// stale read silently reverts every field somebody else changed meanwhile.
//
// It also reports every *other* review it wrote to. Retracting a symmetric link
// is a change to both ends — see farEnds.
func Update(s *entity.Store, author string, ts int64, id string, st entity.State, want Fields) (changed []string, detached []string, err error) {
	if want.Title == "" {
		return nil, nil, fmt.Errorf("a review needs a title")
	}

	var (
		events  []entity.Event
		c       = st.NextClock()
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

	have := FieldsOf(st)

	// status has no cleared state, so an empty want.Status is "leave it".
	if want.Status != "" && want.Status != have.Status {
		if err := emit("status", entity.Str(want.Status), ""); err != nil {
			return nil, nil, err
		}
		changed = append(changed, "status")
	}

	for _, f := range []struct{ op, name, from, to string }{
		{"title", "title", have.Title, want.Title},
		{"description", "description", have.Description, want.Description},
		{"base", "base", have.Base, want.Base},
		{"head", "head", have.Head, want.Head},
		{"head.sha", "revision", have.HeadSHA, want.HeadSHA},
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

	// A boolean scalar, written as a boolean rather than as the string "true":
	// nil means no flag mentioned it.
	if want.Draft != nil && *want.Draft != *have.Draft {
		if err := emit("draft", entity.Bool(*want.Draft), ""); err != nil {
			return nil, nil, err
		}
		changed = append(changed, "draft")
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

	var others []change
	if want.Relations != nil {
		dropped, touched, err := reconcileRelations(st, want.Relations, removed, emit)
		if err != nil {
			return nil, nil, err
		}
		if touched {
			changed = append(changed, "relations")
		}
		if others, err = farEnds(s, author, ts, id, dropped); err != nil {
			return nil, nil, err
		}
	}

	if len(events) == 0 && len(others) == 0 {
		return nil, nil, nil
	}
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
// and a remove per surviving add whose value is gone. The removes name add
// events, not values.
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
// something: it keys on the pair (kind, target) rather than on a value.
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
// A symmetric kind has no dependent end, so a repository that has seen both
// ends holds two members saying the same thing, and retracting only the near
// one leaves the link standing. This is the one write that reaches a review
// nobody named, which is why Update reports the ones it touched.
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

// WriteChecks records check results against one commit on the checks ref.
//
// The commit sha is the note key — the one key in this system that names an
// object that actually exists — so there is no create event and no entity id to
// derive. A commit sha is already the coordination-free content-addressed
// identifier that entity ids are derived to obtain.
//
// Superseded members are retracted where this writer can see them, which is the
// reconciliation a writer should do and a reader must not depend on.
func WriteChecks(store *entity.Store, author string, ts int64, commit string, checks []Check) (bool, error) {
	if commit == "" {
		return false, fmt.Errorf("a check names the commit it ran against")
	}

	var st entity.State
	if note, err := store.Resolve(commit, "commit"); err == nil {
		if loaded, err := store.Load(note); err == nil {
			st = loaded
		}
	}

	have := map[string]Check{}
	for _, c := range ChecksOf(st) {
		have[c.Name] = c
	}

	var (
		events  []entity.Event
		c       = st.NextClock()
		removed = map[string]string{}
	)
	for _, want := range checks {
		if old, ok := have[want.Name]; ok {
			if old.String() == want.String() {
				continue
			}
			e, err := entity.NewEvent(store.Format(), entity.FormatVersion, c, ts, author, CheckRemove, entity.Value{}, old.ID)
			if err != nil {
				return false, err
			}
			c++
			events = append(events, e)
			removed[old.ID] = old.Name + " " + old.Conclusion
		}
		e, err := entity.NewEvent(store.Format(), entity.FormatVersion, c, ts, author, CheckAdd, entity.Str(want.String()), "")
		if err != nil {
			return false, err
		}
		c++
		events = append(events, e)
	}
	if len(events) == 0 {
		return false, nil
	}

	return true, write(store, author, ts, change{
		id:      commit,
		title:   "checks on " + commit,
		events:  events,
		removed: removed,
	})
}

// titleOf is the review's title as a commit message needs it.
func titleOf(st entity.State) string { return st.Scalar("title").Display() }

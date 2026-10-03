package adoissue

import (
	"strings"
	"testing"

	"github.com/hdweiss/git-issue/internal/bridge"
	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

var vocab = issue.Vocabulary.With(Vocabulary)

// ledger is the origin ledger's two answers, in a map.
type ledger struct {
	entities map[string]string // upstream -> local entity
	comments map[string]string // "<entity> <event>" -> upstream comment
}

func newLedger() ledger {
	return ledger{entities: map[string]string{}, comments: map[string]string{}}
}

func (l ledger) Comment(entity, event string) (string, bool) {
	up, ok := l.comments[entity+" "+event]
	return up, ok
}

func (l ledger) ClaimedComment(upstream string) bool {
	for _, v := range l.comments {
		if v == upstream {
			return true
		}
	}
	return false
}

func (l ledger) Entity(upstream string) (string, bool) {
	id, ok := l.entities[upstream]
	return id, ok
}

// Upstream is the same map read backwards, which is what pushing a link needs.
func (l ledger) Upstream(entity string) (string, bool) {
	for upstream, id := range l.entities {
		if id == entity {
			return upstream, true
		}
	}
	return "", false
}

// mirror is one clone's copy of an entity plus the ledger lines for one
// tracker, which is everything a push and an import need between them.
type mirror struct {
	id     string
	events []entity.Event
	led    ledger
}

func newMirror(id string) *mirror { return &mirror{id: id, led: newLedger()} }

func (m *mirror) state() entity.State { return entity.Fold(vocab, m.events) }

func (m *mirror) candidate(upstream string) bridge.Candidate {
	return bridge.Candidate{ID: m.id, Events: m.events, State: m.state(), Upstream: upstream}
}

// add appends an event the way a local write does, taking the next clock from
// folded state.
func (m *mirror) add(t *testing.T, op string, val entity.Value, ref, nonce string) entity.Event {
	t.Helper()
	e, err := entity.NewEventWithNonce(entity.SHA1, entity.Event{
		V: entity.FormatVersion, C: m.state().NextClock(), TS: 1772700000,
		A: "a@example.com", Op: op, N: nonce, Ref: ref, Val: val,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.events = append(m.events, e)
	return e
}

// union merges imported events the way the blob does: a grow-only set of whole
// lines, adding only what is not already there.
func (m *mirror) union(imported []entity.Event) {
	have := map[string]bool{}
	for _, e := range m.events {
		have[string(e.Raw)] = true
	}
	for _, e := range imported {
		if !have[string(e.Raw)] {
			have[string(e.Raw)] = true
			m.events = append(m.events, e)
		}
	}
}

// pushAndPull runs one full cycle against a tracker: plan, apply, record the
// mappings, then import what the tracker now says and union it in. It returns
// how many issues the plan had to send.
func pushAndPull(t *testing.T, p *Pusher, m *mirror, upstream *string) int {
	t.Helper()

	plan, err := p.Plan([]bridge.Candidate{m.candidate(*upstream)}, m.led)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", plan.Conflicts)
	}

	for _, d := range plan.Deltas {
		result, err := p.Apply(m.candidate(*upstream), d)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if result.Upstream != "" {
			*upstream = result.Upstream
			m.led.entities[result.Upstream] = m.id
		}
		for _, c := range result.Comments {
			m.led.comments[m.id+" "+c.EventID] = c.Upstream
		}
	}

	// Now read the tracker back, exactly as a pull would, honouring the ledger
	// so a comment this clone posted is not imported as a second one.
	items, err := p.Client.FetchIDs(p.Target, []int{WorkItemID(*upstream)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("read back %d work items, want 1", len(items))
	}
	imported, err := Import(entity.SHA1, p.Target, items[0], m.led)
	if err != nil {
		t.Fatal(err)
	}
	m.union(imported.Events())
	for _, c := range imported.Comments {
		m.led.comments[m.id+" "+c.EventID] = c.Upstream
	}
	return len(plan.Deltas)
}

func newPusher(f *fakeADO, m *mirror) *Pusher {
	return NewPusher(f.client(), f.target(), entity.SHA1, vocab, m.led)
}

// The property the whole design rests on: push, pull back what was pushed, and
// the next push has nothing to do.
//
// Comparing folded values rather than event sets is what makes this terminate.
// A change sent to a tracker comes back as that tracker's own event carrying
// the same value, so the blob holds two events saying one thing — and a
// comparison on event identity would find work to do on every cycle, forever.
func TestMirrorLoopTerminates(t *testing.T) {
	f := newFakeADO(t)
	m := newMirror("4f2a1c9")
	m.add(t, "create", entity.Str(issue.Type), "", "aaaaaaaaaaaaaaaa")
	m.add(t, "title", entity.Str("Filed locally"), "", "bbbbbbbbbbbbbbbb")
	m.add(t, "description", entity.Str("A body."), "", "cccccccccccccccc")
	comment := m.add(t, "comment", entity.Str("First."), "", "dddddddddddddddd")
	_ = comment

	p := newPusher(f, m)
	upstream := ""

	if sent := pushAndPull(t, p, m, &upstream); sent != 1 {
		t.Fatalf("first cycle sent %d deltas, want 1", sent)
	}
	if upstream == "" {
		t.Fatal("the create earned no upstream id")
	}
	for cycle := 2; cycle <= 4; cycle++ {
		if sent := pushAndPull(t, p, m, &upstream); sent != 0 {
			t.Fatalf("cycle %d sent %d deltas, want none", cycle, sent)
		}
	}

	// And the tracker holds what was pushed.
	id := WorkItemID(upstream)
	if got := f.fieldOf(id, adoapi.FieldTitle); got != "Filed locally" {
		t.Errorf("upstream title = %q", got)
	}
	if len(f.items[id].comments) != 1 {
		t.Errorf("upstream has %d comments, want 1", len(f.items[id].comments))
	}
}

// A local edit reaches the tracker once, and the cycle settles again.
func TestEditPushesOnceAndSettles(t *testing.T) {
	f := newFakeADO(t)
	m := newMirror("4f2a1c9")
	m.add(t, "create", entity.Str(issue.Type), "", "aaaaaaaaaaaaaaaa")
	m.add(t, "title", entity.Str("Before"), "", "bbbbbbbbbbbbbbbb")

	p := newPusher(f, m)
	upstream := ""
	pushAndPull(t, p, m, &upstream)
	pushAndPull(t, p, m, &upstream)

	m.add(t, "title", entity.Str("After"), "", "eeeeeeeeeeeeeeee")
	if sent := pushAndPull(t, p, m, &upstream); sent != 1 {
		t.Fatalf("the edit sent %d deltas, want 1", sent)
	}
	if got := f.fieldOf(WorkItemID(upstream), adoapi.FieldTitle); got != "After" {
		t.Errorf("upstream title = %q, want After", got)
	}
	for cycle := 1; cycle <= 3; cycle++ {
		if sent := pushAndPull(t, p, m, &upstream); sent != 0 {
			t.Fatalf("cycle %d after the edit sent %d deltas, want none", cycle, sent)
		}
	}
}

// The load-bearing negative on the write side. An area is scope rather than
// state, so nothing local holds one — and a push must therefore never move a
// work item that already exists, whatever area this clone is scoped to.
func TestPushNeverMovesAnExistingWorkItem(t *testing.T) {
	f := newFakeADO(t)
	id := f.file(map[string]string{
		adoapi.FieldType:     "Bug",
		adoapi.FieldTitle:    "Filed upstream",
		adoapi.FieldState:    "Active",
		adoapi.FieldAreaPath: `MyProj\Mobile`,
	})

	m := newMirror("4f2a1c9")
	upstream := Origin(f.target(), id)
	m.led.entities[upstream] = m.id

	// Import it, then change something locally, then push from a clone scoped
	// to a different area entirely.
	items, err := f.client().FetchIDs(f.target(), []int{id}, nil)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := Import(entity.SHA1, f.target(), items[0], m.led)
	if err != nil {
		t.Fatal(err)
	}
	m.id = imported.ID
	m.led.entities[upstream] = m.id
	m.union(imported.Events())
	m.add(t, "title", entity.Str("Renamed here"), "", "ffffffffffffffff")

	target := f.target()
	target.Area = "Web/Auth"
	p := NewPusher(f.client(), target, entity.SHA1, vocab, m.led)

	plan, err := p.Plan([]bridge.Candidate{m.candidate(upstream)}, m.led)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deltas) != 1 {
		t.Fatalf("plan has %d deltas, want 1", len(plan.Deltas))
	}
	if _, err := p.Apply(m.candidate(upstream), plan.Deltas[0]); err != nil {
		t.Fatal(err)
	}

	if got := f.fieldOf(id, adoapi.FieldTitle); got != "Renamed here" {
		t.Errorf("the rename did not reach the tracker: %q", got)
	}
	if got := f.fieldOf(id, adoapi.FieldAreaPath); got != `MyProj\Mobile` {
		t.Errorf("the push moved the work item to %q; it must keep the area it had", got)
	}
	for _, ops := range f.patches {
		for _, op := range ops {
			if strings.Contains(op.Path, adoapi.FieldAreaPath) {
				t.Errorf("an update patch wrote %s: %+v", adoapi.FieldAreaPath, op)
			}
		}
	}
}

// The one place an area is ever written: where a push files a work item it
// creates.
func TestCreateFilesInTheScopedArea(t *testing.T) {
	f := newFakeADO(t)
	m := newMirror("4f2a1c9")
	m.add(t, "create", entity.Str(issue.Type), "", "aaaaaaaaaaaaaaaa")
	m.add(t, "title", entity.Str("New work"), "", "bbbbbbbbbbbbbbbb")

	target := f.target()
	target.Area = "Web/Auth"
	p := NewPusher(f.client(), target, entity.SHA1, vocab, m.led)

	upstream := ""
	pushAndPull(t, p, m, &upstream)

	if got := f.fieldOf(WorkItemID(upstream), adoapi.FieldAreaPath); got != `MyProj\Web\Auth` {
		t.Errorf("created work item filed under %q, want MyProj\\Web\\Auth", got)
	}
}

// A stale revision fails the write rather than overwriting it. This is the one
// place Azure DevOps' API is stronger than GitHub's, and it only helps if the
// patch actually carries the test.
func TestPatchCarriesTheRevisionTest(t *testing.T) {
	f := newFakeADO(t)
	id := f.file(map[string]string{
		adoapi.FieldType:  "Bug",
		adoapi.FieldTitle: "Filed upstream",
		adoapi.FieldState: "Active",
	})

	err := f.client().UpdateWorkItem(f.target(), adoapi.WorkItem{ID: id, Rev: 1},
		[]adoapi.Field{{Name: adoapi.FieldTitle, Value: "Fine"}}, adoapi.LinkOps{})
	if err != nil {
		t.Fatalf("a current rev was refused: %v", err)
	}
	if got := f.fieldOf(id, adoapi.FieldTitle); got != "Fine" {
		t.Errorf("title = %q", got)
	}

	// The work item has moved on; the same rev must now be refused.
	if err := f.client().UpdateWorkItem(f.target(), adoapi.WorkItem{ID: id, Rev: 1},
		[]adoapi.Field{{Name: adoapi.FieldTitle, Value: "Clobbered"}}, adoapi.LinkOps{}); err == nil {
		t.Fatal("a stale rev was accepted; a concurrent edit would be overwritten")
	}
	if got := f.fieldOf(id, adoapi.FieldTitle); got == "Clobbered" {
		t.Error("the stale write landed anyway")
	}
}

// Tags are one string upstream and a set here, so a push has to rewrite the
// whole field — and must not drop what it did not know about.
func TestTagsAreMergedNotReplaced(t *testing.T) {
	f := newFakeADO(t)
	target := f.target()
	p := NewPusher(f.client(), target, entity.SHA1, vocab, nil)

	current := adoapi.WorkItem{Tags: []string{"ux", "regression"}}
	d := issue.Delta{LabelsAdded: []string{"triage"}, LabelsRemoved: []string{"ux"}}

	tags, changed := p.tags(current.Tags, d)
	if !changed {
		t.Fatal("a label delta produced no change")
	}
	if got := adoapi.JoinTags(tags); got != "regression; triage" {
		t.Errorf("tags = %q, want %q", got, "regression; triage")
	}

	// No label change at all must leave the field alone rather than rewriting
	// it with what it already holds.
	if _, changed := p.tags(current.Tags, issue.Delta{}); changed {
		t.Error("an empty label delta rewrote the tags field")
	}
}

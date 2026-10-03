package giteaissue

import (
	"testing"

	"github.com/hdweiss/git-issue/internal/bridge"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

// mirror is one clone's copy of an entity plus the ledger for one tracker.
type mirror struct {
	id     string
	events []entity.Event
	led    ledger
}

func newMirror(id string) *mirror {
	return &mirror{id: id, led: newLedger()}
}

func (m *mirror) state() entity.State { return entity.Fold(vocab, m.events) }

func (m *mirror) candidate(upstream string) bridge.Candidate {
	return bridge.Candidate{ID: m.id, Events: m.events, State: m.state(), Upstream: upstream}
}

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
// mappings, then import what the tracker now says and union it in.
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

	issues, err := p.Client.Issues(p.Target, []int64{parseNumber(*upstream)})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("read back %d issues, want 1", len(issues))
	}
	imported, err := Import(entity.SHA1, p.Target, issues[0], m.led)
	if err != nil {
		t.Fatal(err)
	}
	m.union(imported.Events())
	for _, c := range imported.Comments {
		m.led.comments[m.id+" "+c.EventID] = c.Upstream
	}
	return len(plan.Deltas)
}

// The property the whole design rests on: push, pull back what was pushed, and
// the next push has nothing to do.
func TestMirrorLoopTerminates(t *testing.T) {
	f := newFakeGitea(t)
	p := NewPusher(f.client(), fakeTarget(), entity.SHA1, vocab, nil)

	m := newMirror("4f2a1c9local")
	m.add(t, "create", entity.Str(issue.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("Crash on empty input"), "", "2222222222222222")
	m.add(t, "description", entity.Str("Steps follow."), "", "3333333333333333")
	m.add(t, "label.add", entity.Str("bug"), "", "4444444444444444")
	m.add(t, "comment", entity.Str("First note."), "", "5555555555555555")

	upstream := ""
	if sent := pushAndPull(t, p, m, &upstream); sent != 1 {
		t.Fatalf("first cycle sent %d deltas, want 1", sent)
	}
	if upstream == "" {
		t.Fatal("the create recorded no upstream id")
	}
	if sent := pushAndPull(t, p, m, &upstream); sent != 0 {
		t.Errorf("second cycle sent %d deltas, want none", sent)
	}
	if sent := pushAndPull(t, p, m, &upstream); sent != 0 {
		t.Errorf("third cycle sent %d deltas, want none", sent)
	}

	st := m.state()
	if got := st.Scalar("title").Display(); got != "Crash on empty input" {
		t.Errorf("title after the round trip = %q", got)
	}
	if got := st.List("label"); len(got) != 1 || got[0] != "bug" {
		t.Errorf("labels after the round trip = %v, want one bug", got)
	}
	if len(st.Thread) != 1 {
		t.Errorf("thread after the round trip has %d entries, want 1", len(st.Thread))
	}
}

// A local edit after the issue is upstream goes once, and only once.
func TestEditPushesOnceAndSettles(t *testing.T) {
	f := newFakeGitea(t)
	p := NewPusher(f.client(), fakeTarget(), entity.SHA1, vocab, nil)

	m := newMirror("4f2a1c9local")
	m.add(t, "create", entity.Str(issue.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("Original"), "", "2222222222222222")

	upstream := ""
	pushAndPull(t, p, m, &upstream)

	m.add(t, "title", entity.Str("Renamed locally"), "", "6666666666666666")
	m.add(t, "label.add", entity.Str("urgent"), "", "7777777777777777")
	m.add(t, "comment", entity.Str("Why it changed."), "", "8888888888888888")

	if sent := pushAndPull(t, p, m, &upstream); sent != 1 {
		t.Fatalf("the edit sent %d deltas, want 1", sent)
	}
	if sent := pushAndPull(t, p, m, &upstream); sent != 0 {
		t.Errorf("the edit was sent again: %d deltas", sent)
	}

	iss := f.issues[parseNumber(upstream)]
	if iss.Title != "Renamed locally" {
		t.Errorf("upstream title = %q", iss.Title)
	}
	if len(iss.Labels) != 1 || iss.Labels[0] != "urgent" {
		t.Errorf("upstream labels = %v", iss.Labels)
	}
	if len(iss.Comments) != 1 {
		t.Errorf("upstream has %d comments, want 1", len(iss.Comments))
	}
	if got := m.state().Scalar("title").Display(); got != "Renamed locally" {
		t.Errorf("local title after the round trip = %q", got)
	}
}

// Closing locally reaches the tracker and settles.
func TestClosePushes(t *testing.T) {
	f := newFakeGitea(t)
	p := NewPusher(f.client(), fakeTarget(), entity.SHA1, vocab, nil)

	m := newMirror("4f2a1c9local")
	m.add(t, "create", entity.Str(issue.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("Will be closed"), "", "2222222222222222")

	upstream := ""
	pushAndPull(t, p, m, &upstream)

	m.add(t, "status", entity.Str(issue.StatusClosed), "", "9999999999999999")

	if sent := pushAndPull(t, p, m, &upstream); sent != 1 {
		t.Fatalf("the close sent %d deltas, want 1", sent)
	}
	if got := f.issues[parseNumber(upstream)].State; got != "closed" {
		t.Errorf("upstream state = %q", got)
	}
	if sent := pushAndPull(t, p, m, &upstream); sent != 0 {
		t.Errorf("the close was sent again: %d deltas", sent)
	}
	if got := issue.Status(m.state()); got != issue.StatusClosed {
		t.Errorf("local status after the round trip = %q", got)
	}
}

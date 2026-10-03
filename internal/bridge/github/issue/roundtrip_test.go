package ghissue

import (
	"testing"

	"github.com/hdweiss/git-issue/internal/bridge"
	ghapi "github.com/hdweiss/git-issue/internal/bridge/github/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

// mirror is one clone's copy of an entity plus the ledger lines for one tracker,
// which is everything a push and an import need between them.
type mirror struct {
	id     string
	events []entity.Event
	led    ledger
}

func newMirror(id string) *mirror {
	return &mirror{id: id, led: ledger{comments: map[string]string{}}}
}

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

// union merges imported events in the way the blob does: a grow-only set of
// whole lines, adding only what is not already there.
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
		}
		for _, c := range result.Comments {
			m.led.comments[m.id+" "+c.EventID] = c.Upstream
		}
	}

	// Now read the tracker back, exactly as a pull would, honouring the ledger
	// so a comment this clone posted is not imported as a second one.
	issues, err := p.Client.FetchNodes([]string{*upstream})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("read back %d issues, want 1", len(issues))
	}
	imported, err := Import(entity.SHA1, issues[0], m.led)
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
//
// Comparing folded values rather than event sets is what makes this terminate.
// A change sent to a tracker comes back as that tracker's own event carrying the
// same value, so the blob holds two events saying one thing — and a comparison
// on event identity would find work to do on every cycle, forever.
func TestMirrorLoopTerminates(t *testing.T) {
	f := newFakeGitHub(t)
	p := NewPusher(f.client(), fakeTarget(), entity.SHA1, vocab, nil)

	m := newMirror("4f2a1c9local")
	m.add(t, "create", entity.Str(issue.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("Crash on empty input"), "", "2222222222222222")
	m.add(t, "description", entity.Str("Steps follow."), "", "3333333333333333")
	m.add(t, "label.add", entity.Str("bug"), "", "4444444444444444")
	m.add(t, "comment", entity.Str("First note."), "", "5555555555555555")

	upstream := ""

	// Cycle one files the issue.
	if sent := pushAndPull(t, p, m, &upstream); sent != 1 {
		t.Fatalf("first cycle sent %d deltas, want 1", sent)
	}
	if upstream == "" {
		t.Fatal("the create recorded no upstream id")
	}

	// Cycle two must send nothing. This is the assertion the design owes.
	if sent := pushAndPull(t, p, m, &upstream); sent != 0 {
		t.Errorf("second cycle sent %d deltas, want none", sent)
	}
	// And a third, in case the second merely happened to be quiet.
	if sent := pushAndPull(t, p, m, &upstream); sent != 0 {
		t.Errorf("third cycle sent %d deltas, want none", sent)
	}

	// What came back is one issue, not two, and one comment, not two.
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
	f := newFakeGitHub(t)
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

	if got := f.issues[upstream].Title; got != "Renamed locally" {
		t.Errorf("upstream title = %q", got)
	}
	if got := f.issues[upstream].Labels; len(got) != 1 || got[0] != "urgent" {
		t.Errorf("upstream labels = %v", got)
	}
	if got := len(f.issues[upstream].Comments); got != 1 {
		t.Errorf("upstream has %d comments, want 1", got)
	}
	// Locally the rename came back as the tracker's own title event too, and
	// the two agree — which is why the second cycle had nothing to say.
	if got := m.state().Scalar("title").Display(); got != "Renamed locally" {
		t.Errorf("local title after the round trip = %q", got)
	}
}

// Closing locally reaches the tracker and settles, reason included.
func TestClosePushes(t *testing.T) {
	f := newFakeGitHub(t)
	p := NewPusher(f.client(), fakeTarget(), entity.SHA1, vocab, nil)

	m := newMirror("4f2a1c9local")
	m.add(t, "create", entity.Str(issue.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("Will be closed"), "", "2222222222222222")

	upstream := ""
	pushAndPull(t, p, m, &upstream)

	m.add(t, "status", entity.Str(issue.StatusClosed), "", "9999999999999999")
	m.add(t, "status.reason", entity.Str("completed"), "", "aaaaaaaaaaaaaaaa")

	if sent := pushAndPull(t, p, m, &upstream); sent != 1 {
		t.Fatalf("the close sent %d deltas, want 1", sent)
	}
	if got := f.issues[upstream].State; got != "CLOSED" {
		t.Errorf("upstream state = %q", got)
	}
	if got := f.issues[upstream].StateReason; got != "COMPLETED" {
		t.Errorf("upstream reason = %q", got)
	}
	if sent := pushAndPull(t, p, m, &upstream); sent != 0 {
		t.Errorf("the close was sent again: %d deltas", sent)
	}
	if got := issue.Status(m.state()); got != issue.StatusClosed {
		t.Errorf("local status after the round trip = %q", got)
	}
}

// Two trackers, one entity: the fork case. Each is diffed independently, so a
// title that conflicts in one does not stop the push to the other.
func TestTwoTrackersAreIndependent(t *testing.T) {
	upstreamFake, forkFake := newFakeGitHub(t), newFakeGitHub(t)
	up := NewPusher(upstreamFake.client(), fakeTarget(), entity.SHA1, vocab, nil)
	fork := NewPusher(forkFake.client(), ghapi2("acme"), entity.SHA1, vocab, nil)

	// One entity, one local state, but a separate ledger and upstream id per
	// tracker — which is exactly what the ledger's one-file-per-tracker layout
	// gives.
	m := newMirror("4f2a1c9local")
	m.add(t, "create", entity.Str(issue.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("Shared issue"), "", "2222222222222222")

	forkLedger := ledger{comments: map[string]string{}}
	upstreamID, forkID := "", ""

	pushAndPull(t, up, m, &upstreamID)

	// Seed the fork from the same entity. It gets its own id there.
	forkMirror := &mirror{id: m.id, events: m.events, led: forkLedger}
	pushAndPull(t, fork, forkMirror, &forkID)

	if upstreamID == "" || forkID == "" {
		t.Fatal("one of the trackers recorded no id")
	}
	if upstreamFake.issues[upstreamID].Title != "Shared issue" {
		t.Errorf("upstream title = %q", upstreamFake.issues[upstreamID].Title)
	}
	if forkFake.issues[forkID].Title != "Shared issue" {
		t.Errorf("fork title = %q", forkFake.issues[forkID].Title)
	}

	// Someone renames it in the fork only. The local copy has not seen that, so
	// a push to the fork conflicts while a push upstream is unaffected.
	forkFake.issues[forkID].Title = "Renamed in the fork"
	forkFake.issues[forkID].Timeline = append(forkFake.issues[forkID].Timeline, fakeEvent{
		Typename: "RenamedTitleEvent", ID: forkFake.eventID(), CreatedAt: forkFake.tick(),
		Prev: "Shared issue", Curr: "Renamed in the fork",
	})
	m.add(t, "title", entity.Str("Renamed locally"), "", "bbbbbbbbbbbbbbbb")

	forkPlan, err := fork.Plan([]bridge.Candidate{
		{ID: m.id, Events: m.events, State: m.state(), Upstream: forkID},
	}, forkLedger)
	if err != nil {
		t.Fatal(err)
	}
	if len(forkPlan.Conflicts) != 1 || forkPlan.Conflicts[0].Field != "title" {
		t.Fatalf("fork plan conflicts = %+v, want one on title", forkPlan.Conflicts)
	}
	if len(forkPlan.Deltas) != 0 {
		t.Errorf("a conflicted issue was still queued: %+v", forkPlan.Deltas)
	}

	upPlan, err := up.Plan([]bridge.Candidate{
		{ID: m.id, Events: m.events, State: m.state(), Upstream: upstreamID},
	}, m.led)
	if err != nil {
		t.Fatal(err)
	}
	if len(upPlan.Conflicts) != 0 {
		t.Errorf("the conflict in one tracker leaked into the other: %+v", upPlan.Conflicts)
	}
	if len(upPlan.Deltas) != 1 {
		t.Fatalf("upstream plan = %+v, want the rename", upPlan.Deltas)
	}
	if got := upPlan.Deltas[0].Set["title"].Display(); got != "Renamed locally" {
		t.Errorf("upstream delta title = %q", got)
	}
}

func fakeTarget() ghapi.Target {
	return ghapi.Target{Host: ghapi.PublicHost, Owner: "hdweiss", Name: "git-issue"}
}

func ghapi2(owner string) ghapi.Target {
	return ghapi.Target{Host: ghapi.PublicHost, Owner: owner, Name: "git-issue"}
}

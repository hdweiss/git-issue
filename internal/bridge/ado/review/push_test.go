package adoreview

import (
	"testing"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/review"
)

const fakeHead = "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d"

// ledger is a review.Ledger built from literals, standing in for the real one.
type ledger struct {
	entities map[string]string // upstream origin -> entity id
	comments map[string]string // "<entity> <event>" -> upstream comment origin
	threads  map[string]string // "<entity> <root event>" -> upstream thread origin
	verdicts map[string]string // "<entity> <verdict event>" -> upstream vote key
}

func newLedger() *ledger {
	return &ledger{
		entities: map[string]string{}, comments: map[string]string{},
		threads: map[string]string{}, verdicts: map[string]string{},
	}
}

func (l *ledger) Entity(upstream string) (string, bool) { v, ok := l.entities[upstream]; return v, ok }

func (l *ledger) Upstream(id string) (string, bool) {
	for up, ent := range l.entities {
		if ent == id {
			return up, true
		}
	}
	return "", false
}

func (l *ledger) Comment(id, event string) (string, bool) {
	v, ok := l.comments[id+" "+event]
	return v, ok
}
func (l *ledger) Thread(id, root string) (string, bool) {
	v, ok := l.threads[id+" "+root]
	return v, ok
}
func (l *ledger) Verdict(id, event string) (string, bool) {
	v, ok := l.verdicts[id+" "+event]
	return v, ok
}

func (l *ledger) ClaimedComment(upstream string) bool {
	for _, v := range l.comments {
		if v == upstream {
			return true
		}
	}
	return false
}

func (l *ledger) ClaimedVerdict(upstream string) (string, bool) {
	for key, v := range l.verdicts {
		if v == upstream {
			for i := 0; i < len(key); i++ {
				if key[i] == ' ' {
					return key[i+1:], true
				}
			}
		}
	}
	return "", false
}

// mirror is one clone's copy of a review plus the ledger lines for one tracker.
type mirror struct {
	id       string
	events   []entity.Event
	led      *ledger
	upstream string
	clock    int64
}

func newMirror(id string) *mirror {
	return &mirror{id: id, led: newLedger(), clock: 1772700000}
}

func (m *mirror) state() entity.State { return entity.Fold(review.Vocabulary, m.events) }

func (m *mirror) candidate() review.Candidate {
	return review.Candidate{ID: m.id, Events: m.events, State: m.state(), Upstream: m.upstream}
}

func (m *mirror) add(t *testing.T, op string, val entity.Value, ref, nonce string) entity.Event {
	t.Helper()
	e, err := entity.NewEventWithNonce(entity.SHA1, entity.Event{
		V: entity.FormatVersion, C: m.state().NextClock(), TS: m.clock,
		A: "a@example.com", Op: op, N: nonce, Ref: ref, Val: val,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.events = append(m.events, e)
	return e
}

// verdict writes a message and a position the way review.SetVerdict does: one
// action, consecutive clocks, so the pairing a push relies on is the real one.
func (m *mirror) verdict(t *testing.T, value, message, revision, nonce string) entity.Event {
	t.Helper()
	if message != "" {
		m.add(t, "comment", entity.Str(message), "", nonce+"c")
	}
	return m.add(t, review.VerdictAdd, entity.Str(value), revision, nonce)
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

// opened seeds a review nothing upstream knows about yet.
func opened(t *testing.T, m *mirror) {
	t.Helper()
	m.add(t, "create", entity.Str(review.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("Fix the area subtree walk"), "", "2222222222222222")
	m.add(t, "description", entity.Str("The picker dropped the subtree."), "", "3333333333333333")
	m.add(t, "base", entity.Str("main"), "", "4444444444444444")
	m.add(t, "head", entity.Str("fix-area-walk"), "", "5555555555555555")
	m.add(t, "head.sha", entity.Str(fakeHead), "", "6666666666666666")
}

func newPusher(f *fakeADO, led *ledger) *Pusher {
	return NewPusher(f.client(), f.target(), entity.SHA1, review.Vocabulary, led, f.self)
}

// importInto reads a pull request off the fake and folds it into the mirror,
// exactly as a pull would, honouring the ledger.
func importInto(t *testing.T, f *fakeADO, m *mirror, prID int) {
	t.Helper()
	got, err := f.client().FetchPull(f.target(), prID)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Import(entity.SHA1, f.target(), got, m.led)
	if err != nil {
		t.Fatal(err)
	}
	if m.id == "" {
		m.id = e.ID
	}
	m.union(e.Events())
	m.upstream = e.Origin
	m.led.entities[e.Origin] = m.id
	recordImported(m, e)
}

func recordImported(m *mirror, e Entity) {
	for _, c := range e.Comments {
		m.led.comments[m.id+" "+c.EventID] = c.Upstream
	}
	for _, c := range e.Threads {
		m.led.threads[m.id+" "+c.EventID] = c.Upstream
	}
	for _, c := range e.Verdicts {
		m.led.verdicts[m.id+" "+c.EventID] = c.Upstream
	}
}

func record(m *mirror, r review.Result) {
	if r.Upstream != "" {
		m.upstream = r.Upstream
		m.led.entities[r.Upstream] = m.id
	}
	for _, c := range r.Comments {
		m.led.comments[m.id+" "+c.EventID] = c.Upstream
	}
	for _, c := range r.Threads {
		m.led.threads[m.id+" "+c.EventID] = c.Upstream
	}
	for _, c := range r.Verdicts {
		m.led.verdicts[m.id+" "+c.EventID] = c.Upstream
	}
}

// cycle runs one full round against the tracker: plan, apply, record, then
// import what the tracker now says and union it in. It returns how many reviews
// the plan had to send.
func cycle(t *testing.T, p *Pusher, m *mirror, f *fakeADO) int {
	t.Helper()
	plan, err := p.Plan([]review.Candidate{m.candidate()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", plan.Conflicts)
	}
	for _, d := range plan.Deltas {
		res, err := p.Apply(m.candidate(), d)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		record(m, res)
	}

	got, err := f.client().FetchPull(f.target(), pullID(m.upstream))
	if err != nil {
		t.Fatal(err)
	}
	imported, err := Import(entity.SHA1, f.target(), got, m.led)
	if err != nil {
		t.Fatal(err)
	}
	m.union(imported.Events())
	recordImported(m, imported)
	return len(plan.Deltas)
}

// The property the whole design rests on: push, pull back what was pushed, and
// the next push has nothing to do — for every shape a review carries.
func TestMirrorLoopTerminates(t *testing.T) {
	f := newFakeADO(t)
	pr := f.seed("Fix the area subtree walk", "The picker dropped the subtree.")
	m := newMirror("")
	importInto(t, f, m, pr.id)
	p := newPusher(f, m.led)

	m.add(t, "title", entity.Str("Fix the area subtree walk, properly"), "", "a1a1a1a1a1a1a1a1")
	m.add(t, "label.add", entity.Str("bug"), "", "b2b2b2b2b2b2b2b2")
	m.add(t, "comment", entity.Str("Ready for a read."), "", "c3c3c3c3c3c3c3c3")
	anchor := review.Anchor{Revision: fakeHead, Path: "internal/issue/area.go", First: 42, Last: 42}
	root := m.add(t, "comment", entity.Str("off by one"), "", "d4d4d4d4d4d4d4d4")
	m.add(t, review.AnchorOp, entity.Str(anchor.String()), root.ID, "d4d4d4d4d4d4d4d4a")
	m.add(t, review.ResolveOp, entity.Bool(true), root.ID, "d4d4d4d4d4d4d4d4r")
	m.verdict(t, review.VerdictApprove, "Looks right.", fakeHead, "e5e5e5e5e5e5e5e5")

	if n := cycle(t, p, m, f); n != 1 {
		t.Fatalf("the first push should send the review, sent %d", n)
	}
	if n := cycle(t, p, m, f); n != 0 {
		t.Fatalf("a second push found %d reviews to send; the loop does not terminate", n)
	}
	if n := cycle(t, p, m, f); n != 0 {
		t.Fatalf("a third push found %d reviews to send", n)
	}
}

// A first push files the pull request with everything the review says.
func TestPushFilesTheReview(t *testing.T) {
	f := newFakeADO(t)
	m := newMirror("aaaa2222local")
	p := newPusher(f, m.led)

	opened(t, m)
	m.add(t, "label.add", entity.Str("bug"), "", "7777777777777777")
	m.add(t, "comment", entity.Str("Ready for a read."), "", "8888888888888888")
	m.verdict(t, review.VerdictApprove, "Looks right.", fakeHead, "9999999999999999")
	cycle(t, p, m, f)

	pr := f.pulls[pullID(m.upstream)]
	if pr == nil {
		t.Fatal("nothing was filed upstream")
	}
	if pr.title != "Fix the area subtree walk" || pr.description != "The picker dropped the subtree." {
		t.Errorf("title/description did not land: %q / %q", pr.title, pr.description)
	}
	if pr.base != "main" || pr.source != "fix-area-walk" {
		t.Errorf("branches did not land: %q -> %q", pr.source, pr.base)
	}
	if len(pr.labels) != 1 || pr.labels[0] != "bug" {
		t.Errorf("labels are %v", pr.labels)
	}
	// The conversation comment and the verdict's message, each its own thread.
	var bodies []string
	for _, th := range pr.threads {
		for _, c := range th.comments {
			bodies = append(bodies, c.content)
		}
	}
	if !contains(bodies, "Ready for a read.") || !contains(bodies, "Looks right.") {
		t.Errorf("comments did not land: %v", bodies)
	}
	if r := pr.reviewers[f.self.ID]; r == nil || r.vote != 10 {
		t.Errorf("the approval vote was not cast: %+v", pr.reviewers)
	}
}

// A scalar changed on both sides is a conflict: the review is skipped whole and
// every other one is left alone.
func TestPushConflict(t *testing.T) {
	f := newFakeADO(t)
	pr := f.seed("Original title", "body")
	m := newMirror("")
	importInto(t, f, m, pr.id)
	p := newPusher(f, m.led)

	m.add(t, "title", entity.Str("Local title"), "", "aaaaaaaaaaaaaaaa")
	pr.title = "Upstream title" // somebody else edited it

	plan, err := p.Plan([]review.Candidate{m.candidate()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) != 1 || plan.Conflicts[0].Field != "title" {
		t.Fatalf("expected a title conflict, got %+v", plan.Conflicts)
	}
	if len(plan.Deltas) != 0 {
		t.Errorf("a conflicted review should send nothing, got %d deltas", len(plan.Deltas))
	}
}

// A locally created review with no head branch is refused, not half-filed.
func TestPushRefusesAHeadlessReview(t *testing.T) {
	f := newFakeADO(t)
	m := newMirror("aaaa3333local")
	p := newPusher(f, m.led)
	m.add(t, "create", entity.Str(review.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("No branch yet"), "", "2222222222222222")

	plan, err := p.Plan([]review.Candidate{m.candidate()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deltas) != 1 || !plan.Deltas[0].Empty() {
		t.Fatalf("a headless create should carry an empty delta, got %+v", plan.Deltas)
	}
	var reason string
	for _, s := range plan.Skipped {
		if s.ID == m.id {
			reason = s.Reason
		}
	}
	if reason == "" {
		t.Fatalf("the skip should say why: %+v", plan.Skipped)
	}
}

// milestone, reviewers and links have no pull request equivalent and are
// reported rather than dropped.
func TestPushReportsUnsupported(t *testing.T) {
	f := newFakeADO(t)
	pr := f.seed("A review", "body")
	m := newMirror("")
	importInto(t, f, m, pr.id)
	p := newPusher(f, m.led)

	m.add(t, "milestone", entity.Str("v2"), "", "aaaaaaaaaaaaaaaa")
	m.add(t, "assignee.add", entity.Str("someone@example.com"), "", "bbbbbbbbbbbbbbbb")

	plan, err := p.Plan([]review.Candidate{m.candidate()})
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]bool{}
	for _, s := range plan.Skipped {
		fields[s.Field] = true
	}
	if !fields["milestone"] || !fields["reviewers"] {
		t.Errorf("milestone and reviewers should be reported unsupported: %+v", plan.Skipped)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

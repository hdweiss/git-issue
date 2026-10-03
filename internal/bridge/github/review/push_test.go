package ghreview

import (
	"strings"
	"testing"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/review"
)

// ledger is a Mapped/Lookup built from literals, standing in for the real one.
type ledger struct {
	entities map[string]string // upstream node id -> entity id
	comments map[string]string // entity id + " " + event id -> upstream id
	threads  map[string]string // entity id + " " + root event id -> upstream thread id
	verdicts map[string]string // entity id + " " + verdict event id -> upstream review id
}

func newLedger() *ledger {
	return &ledger{
		entities: map[string]string{},
		comments: map[string]string{},
		threads:  map[string]string{},
		verdicts: map[string]string{},
	}
}

func (l *ledger) Entity(upstream string) (string, bool) {
	v, ok := l.entities[upstream]
	return v, ok
}

func (l *ledger) Upstream(id string) (string, bool) {
	for upstream, entity := range l.entities {
		if entity == id {
			return upstream, true
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
			_, event, _ := strings.Cut(key, " ")
			return event, true
		}
	}
	return "", false
}

// mirror is one clone's copy of a review plus the ledger lines for one tracker,
// which is everything a push and an import need between them.
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

func (m *mirror) candidate() Candidate {
	return Candidate{ID: m.id, Events: m.events, State: m.state(), Upstream: m.upstream}
}

// add appends an event the way a local write does, taking the next clock from
// folded state.
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
// action, consecutive clocks, so the pairing a push relies on is the real one
// rather than one the test arranged.
func (m *mirror) verdict(t *testing.T, value, message, revision, nonce string) entity.Event {
	t.Helper()
	if message != "" {
		m.add(t, "comment", entity.Str(message), "", nonce+"c")
	}
	return m.add(t, review.VerdictAdd, entity.Str(value), revision, nonce)
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

// cycle runs one full round against the tracker: plan, apply, record the
// mappings, then import what the tracker now says and union it in. It returns
// how many reviews the plan had to send.
func cycle(t *testing.T, p *Pusher, m *mirror) int {
	t.Helper()

	plan, err := p.Plan([]Candidate{m.candidate()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", plan.Conflicts)
	}

	for _, d := range plan.Deltas {
		result, err := p.Apply(m.candidate(), d)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if result.Upstream != "" {
			m.upstream = result.Upstream
			m.led.entities[result.Upstream] = m.id
		}
		for _, c := range result.Comments {
			m.led.comments[m.id+" "+c.EventID] = c.Upstream
		}
		for _, c := range result.Threads {
			m.led.threads[m.id+" "+c.EventID] = c.Upstream
		}
		for _, c := range result.Verdicts {
			m.led.verdicts[m.id+" "+c.EventID] = c.Upstream
		}
	}

	// Read the tracker back, exactly as a pull would, honouring the ledger so
	// what this clone posted is not imported as a second copy.
	pulls, err := p.Client.FetchPullNodes([]string{m.upstream})
	if err != nil {
		t.Fatal(err)
	}
	if len(pulls) != 1 {
		t.Fatalf("read back %d pull requests, want 1", len(pulls))
	}
	imported, err := Import(entity.SHA1, pulls[0], m.led)
	if err != nil {
		t.Fatal(err)
	}
	m.union(imported.Events())
	for _, c := range imported.Comments {
		m.led.comments[m.id+" "+c.EventID] = c.Upstream
	}
	for _, c := range imported.Threads {
		m.led.threads[m.id+" "+c.EventID] = c.Upstream
	}
	return len(plan.Deltas)
}

// opened is a mirror holding a review nothing upstream knows about yet.
func opened(t *testing.T, m *mirror) {
	t.Helper()
	m.add(t, "create", entity.Str(review.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("Fix the area subtree walk"), "", "2222222222222222")
	m.add(t, "description", entity.Str("The picker dropped the subtree."), "", "3333333333333333")
	m.add(t, "base", entity.Str("main"), "", "4444444444444444")
	m.add(t, "head", entity.Str("contributor/fix-area-walk"), "", "5555555555555555")
	m.add(t, "head.sha", entity.Str(fakeHead), "", "6666666666666666")
}

func newPusher(t *testing.T, f *fakeGitHub, led *ledger) *Pusher {
	t.Helper()
	return NewPusher(f.client(), fakeTarget(), entity.SHA1, review.Vocabulary, led)
}

// The property the whole design rests on: push, pull back what was pushed, and
// the next push has nothing to do.
//
// Comparing folded values rather than event sets is what makes this terminate. A
// change sent to a tracker comes back as that tracker's own event carrying the
// same value, so the blob holds two events saying one thing — and a comparison
// on event identity would find work to do on every cycle, forever.
func TestMirrorLoopTerminates(t *testing.T) {
	f := newFakeGitHub(t)
	m := newMirror("aaaa1111local")
	p := newPusher(t, f, m.led)

	opened(t, m)
	m.add(t, "label.add", entity.Str("bug"), "", "7777777777777777")
	m.add(t, "comment", entity.Str("Ready for a read."), "", "8888888888888888")
	m.verdict(t, review.VerdictApprove, "Looks right.",
		fakeHead, "9999999999999999")

	if n := cycle(t, p, m); n != 1 {
		t.Fatalf("the first push should send the review, sent %d", n)
	}
	if n := cycle(t, p, m); n != 0 {
		t.Fatalf("a second push found %d reviews to send; the loop does not terminate", n)
	}
	if n := cycle(t, p, m); n != 0 {
		t.Fatalf("a third push found %d reviews to send", n)
	}
}

// A first push files the pull request with everything the review says, in the
// shapes GitHub actually holds them: a body, a base, labels, a conversation, and
// a submitted review carrying its own message.
func TestPushFilesTheReview(t *testing.T) {
	f := newFakeGitHub(t)
	m := newMirror("aaaa2222local")
	p := newPusher(t, f, m.led)

	opened(t, m)
	m.add(t, "label.add", entity.Str("bug"), "", "7777777777777777")
	m.add(t, "comment", entity.Str("Ready for a read."), "", "8888888888888888")
	m.verdict(t, review.VerdictApprove, "Looks right.",
		fakeHead, "9999999999999999")
	cycle(t, p, m)

	pr := f.pulls[m.upstream]
	if pr == nil {
		t.Fatal("nothing was filed upstream")
	}
	if pr.Title != "Fix the area subtree walk" || pr.Body != "The picker dropped the subtree." {
		t.Errorf("title/body did not land: %q / %q", pr.Title, pr.Body)
	}
	if pr.Base != "main" {
		t.Errorf("base is %q, want main", pr.Base)
	}
	// The fork's head goes up in GitHub's own cross-repository spelling.
	if pr.Head != "contributor:fix-area-walk" {
		t.Errorf("head is %q, want contributor:fix-area-walk", pr.Head)
	}
	if len(pr.Labels) != 1 || pr.Labels[0] != "bug" {
		t.Errorf("labels are %v", pr.Labels)
	}

	// One conversation comment. The verdict's message is *not* among them: it is
	// the review's body, which is where an import reads it back from.
	if len(pr.Comments) != 1 || pr.Comments[0].Body != "Ready for a read." {
		t.Fatalf("conversation is %+v", pr.Comments)
	}
	if len(pr.Reviews) != 1 {
		t.Fatalf("submitted %d reviews, want 1", len(pr.Reviews))
	}
	r := pr.Reviews[0]
	if r.State != "APPROVED" || r.Body != "Looks right." {
		t.Errorf("the verdict did not carry its message: %s / %q", r.State, r.Body)
	}
	if r.CommitOID != fakeHead {
		t.Errorf("the verdict was cast against %q, not the revision it names", r.CommitOID)
	}
}

// An anchored thread goes up as a review thread rather than as a comment, and a
// resolution reaches the thread rather than the entry inside it.
func TestPushOpensAndResolvesThreads(t *testing.T) {
	f := newFakeGitHub(t)
	m := newMirror("aaaa3333local")
	p := newPusher(t, f, m.led)

	opened(t, m)
	root := m.add(t, "comment", entity.Str("This drops the subtree."), "", "aaaaaaaaaaaaaaa1")
	anchor := review.Anchor{
		Revision: fakeHead,
		Path:     "internal/issue/area.go", First: 42, Last: 42,
	}
	m.add(t, review.AnchorOp, entity.Str(anchor.String()), root.ID, "aaaaaaaaaaaaaaa2")
	cycle(t, p, m)

	pr := f.pulls[m.upstream]
	if len(pr.Threads) != 1 {
		t.Fatalf("opened %d review threads, want 1", len(pr.Threads))
	}
	th := pr.Threads[0]
	if th.Path != "internal/issue/area.go" || th.Line != 42 {
		t.Errorf("the thread is anchored at %s:%d", th.Path, th.Line)
	}
	if len(pr.Comments) != 0 {
		t.Errorf("an anchored comment must not also be posted to the conversation: %+v", pr.Comments)
	}
	if th.Resolved {
		t.Error("nothing resolved this thread yet")
	}

	// A reply, and then a resolution: both address the thread this run already
	// knows the id of.
	m.add(t, "comment", entity.Str("Fixed in the next revision."), root.ID, "aaaaaaaaaaaaaaa3")
	m.add(t, review.ResolveOp, entity.Bool(true), root.ID, "aaaaaaaaaaaaaaa4")
	if n := cycle(t, p, m); n != 1 {
		t.Fatalf("the reply and the resolution should be one delta, got %d", n)
	}
	th = f.pulls[m.upstream].Threads[0]
	if len(th.Comments) != 2 {
		t.Errorf("the reply did not land in the thread: %+v", th.Comments)
	}
	if !th.Resolved {
		t.Error("the thread should be resolved upstream")
	}
	if n := cycle(t, p, m); n != 0 {
		t.Errorf("a settled thread should push nothing, pushed %d", n)
	}
}

// A thread that is created and resolved in the same run works: the bridge learns
// the thread id by opening it, and the resolution follows in the same Apply.
func TestPushResolvesAThreadItJustOpened(t *testing.T) {
	f := newFakeGitHub(t)
	m := newMirror("aaaa4444local")
	p := newPusher(t, f, m.led)

	opened(t, m)
	root := m.add(t, "comment", entity.Str("Nit, already handled."), "", "bbbbbbbbbbbbbbb1")
	anchor := review.Anchor{Revision: fakeHead, Path: "a.go", First: 3, Last: 3}
	m.add(t, review.AnchorOp, entity.Str(anchor.String()), root.ID, "bbbbbbbbbbbbbbb2")
	m.add(t, review.ResolveOp, entity.Bool(true), root.ID, "bbbbbbbbbbbbbbb3")
	cycle(t, p, m)

	th := f.pulls[m.upstream].Threads[0]
	if !th.Resolved {
		t.Error("a thread opened resolved should arrive resolved")
	}
}

// Dismissing a verdict this clone submitted reaches the review it was submitted
// as, which only the ledger can say.
func TestPushDismissesAVerdict(t *testing.T) {
	f := newFakeGitHub(t)
	m := newMirror("aaaa5555local")
	p := newPusher(t, f, m.led)

	opened(t, m)
	cast := m.verdict(t, review.VerdictApprove, "", fakeHead, "ccccccccccccccc1")
	cycle(t, p, m)

	if got := f.pulls[m.upstream].Reviews[0].State; got != "APPROVED" {
		t.Fatalf("the verdict is %s", got)
	}

	m.add(t, "comment", entity.Str("Retracting, the base moved."), "", "ccccccccccccccc2")
	m.add(t, review.VerdictRemove, entity.Value{}, cast.ID, "ccccccccccccccc3")
	cycle(t, p, m)

	if got := f.pulls[m.upstream].Reviews[0].State; got != "DISMISSED" {
		t.Errorf("the review should be dismissed upstream, is %s", got)
	}
	if n := cycle(t, p, m); n != 0 {
		t.Errorf("a dismissal already upstream should push nothing, pushed %d", n)
	}
}

// An imported verdict is somebody else's position. A push must never re-cast it
// as its own, and the check that stops it needs no ledger: the event is in the
// state the import produced.
func TestPushDoesNotRecastAnImportedVerdict(t *testing.T) {
	f := newFakeGitHub(t)
	m := newMirror("aaaa6666local")
	p := newPusher(t, f, m.led)

	opened(t, m)
	cycle(t, p, m)

	// Somebody approves upstream, and this clone imports it.
	pr := f.pulls[m.upstream]
	pr.Reviews = append(pr.Reviews, &fakeReview{
		ID: "PRR_theirs", State: "APPROVED", Body: "Ship it.",
		CommitOID: fakeHead, CreatedAt: f.tick(),
	})
	if n := cycle(t, p, m); n != 0 {
		t.Fatalf("importing somebody's approval is not something to push, pushed %d", n)
	}
	if got := len(f.pulls[m.upstream].Reviews); got != 1 {
		t.Fatalf("the push submitted %d reviews; it re-cast an imported verdict", got)
	}
	if len(review.Approvals(m.state())) != 1 {
		t.Errorf("the imported approval should stand: %+v", review.VerdictsOf(m.state()))
	}
}

// A merge is a claim about the code that only an observer may make, so a local
// `merged` is reported unsent rather than turned into a mergePullRequest.
func TestPushRefusesToMerge(t *testing.T) {
	f := newFakeGitHub(t)
	m := newMirror("aaaa7777local")
	p := newPusher(t, f, m.led)

	opened(t, m)
	cycle(t, p, m)

	m.add(t, "status", entity.Str(review.StatusMerged), "", "ddddddddddddddd1")
	plan, err := p.Plan([]Candidate{m.candidate()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deltas) != 0 {
		t.Errorf("a merge is not a delta to send: %+v", plan.Deltas)
	}
	if len(plan.Skipped) != 1 || !strings.Contains(plan.Skipped[0].Reason, "merge is performed upstream") {
		t.Errorf("the skip should say why: %+v", plan.Skipped)
	}
}

// Every relation kind is unwritable on a pull request, and `closes` says the one
// thing a person can act on rather than a generic refusal.
func TestPushReportsUnwritableLinks(t *testing.T) {
	f := newFakeGitHub(t)
	m := newMirror("aaaa8888local")
	p := newPusher(t, f, m.led)

	opened(t, m)
	cycle(t, p, m)

	m.add(t, review.RelAdd, entity.Str(review.KindCloses), "beef1234", "eeeeeeeeeeeeeee1")
	plan, err := p.Plan([]Candidate{m.candidate()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deltas) != 0 {
		t.Errorf("an unwritable link is not a delta: %+v", plan.Deltas)
	}
	if len(plan.Skipped) != 1 || !strings.Contains(plan.Skipped[0].Reason, "Closes #n") {
		t.Errorf("a closes link should point at the description: %+v", plan.Skipped)
	}
}

// A review this clone opened with no head branch cannot be filed: nothing here
// pushes code as a side effect of syncing a comment.
func TestPushRefusesAReviewWithNoHead(t *testing.T) {
	f := newFakeGitHub(t)
	m := newMirror("aaaa9999local")
	p := newPusher(t, f, m.led)

	m.add(t, "create", entity.Str(review.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("A local read"), "", "2222222222222222")

	plan, err := p.Plan([]Candidate{m.candidate()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Skipped) != 1 || !strings.Contains(plan.Skipped[0].Reason, "no head branch") {
		t.Fatalf("the skip should name the missing branch: %+v", plan.Skipped)
	}
	for _, d := range plan.Deltas {
		if d.Create {
			t.Error("a review with no head must not be filed")
		}
	}
}

// Reviewers are stated as a whole set, because requestReviews replaces rather
// than adds — and a name that resolves to no GitHub user is left out rather than
// failing the push, since a team slug looks exactly like a login here.
func TestPushStatesTheWholeReviewerSet(t *testing.T) {
	f := newFakeGitHub(t)
	m := newMirror("bbbb1111local")
	p := newPusher(t, f, m.led)

	opened(t, m)
	m.add(t, "assignee.add", entity.Str("reviewer"), "", "fffffffffffffff1")
	m.add(t, "assignee.add", entity.Str("nobody"), "", "fffffffffffffff2")
	cycle(t, p, m)

	got := f.pulls[m.upstream].Reviewers
	if len(got) != 1 || got[0] != "reviewer" {
		t.Errorf("reviewers are %v; the unresolvable one should be dropped, not fatal", got)
	}
}

// A field both sides moved is a conflict, and a conflicted review is skipped
// whole rather than half-pushed.
func TestPushReportsAConflict(t *testing.T) {
	f := newFakeGitHub(t)
	m := newMirror("bbbb2222local")
	p := newPusher(t, f, m.led)

	opened(t, m)
	cycle(t, p, m)

	f.pulls[m.upstream].Title = "Renamed upstream"
	m.add(t, "title", entity.Str("Renamed locally"), "", "aaaabbbbccccddd1")

	plan, err := p.Plan([]Candidate{m.candidate()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) != 1 || plan.Conflicts[0].Field != "title" {
		t.Fatalf("expected a title conflict: %+v", plan.Conflicts)
	}
	if len(plan.Deltas) != 0 {
		t.Errorf("a conflicted review is skipped whole: %+v", plan.Deltas)
	}
}

// An edit reaches the mutation that addresses the shape it is: a thread entry is
// not an issue comment, and a verdict's message is neither.
func TestPushEditsEachCommentShape(t *testing.T) {
	f := newFakeGitHub(t)
	m := newMirror("bbbb3333local")
	p := newPusher(t, f, m.led)

	opened(t, m)
	plain := m.add(t, "comment", entity.Str("first"), "", "1a1a1a1a1a1a1a1a")
	root := m.add(t, "comment", entity.Str("anchored"), "", "2a2a2a2a2a2a2a2a")
	anchor := review.Anchor{Revision: fakeHead, Path: "a.go", First: 1, Last: 1}
	m.add(t, review.AnchorOp, entity.Str(anchor.String()), root.ID, "3a3a3a3a3a3a3a3a")
	body := m.verdict(t, review.VerdictComment, "a verdict message", fakeHead, "4a4a4a4a4a4a4a4a")
	cycle(t, p, m)

	m.add(t, "comment.edit", entity.Str("first, revised"), plain.ID, "5a5a5a5a5a5a5a5a")
	m.add(t, "comment.edit", entity.Str("anchored, revised"), root.ID, "6a6a6a6a6a6a6a6a")
	// The verdict's message is the one before the verdict in the same action.
	msg, ok := review.VerdictMessages(m.state())[messageOf(t, m, body.ID)]
	if !ok || msg != body.ID {
		t.Fatal("the verdict's message was not recognised")
	}
	m.add(t, "comment.edit", entity.Str("a verdict message, revised"), messageOf(t, m, body.ID), "7a7a7a7a7a7a7a7a")

	// The fake refuses an id sent to the wrong mutation, so reaching the right
	// one is what makes this pass rather than an assertion after the fact.
	cycle(t, p, m)

	pr := f.pulls[m.upstream]
	if pr.Comments[0].Body != "first, revised" {
		t.Errorf("the conversation comment is %q", pr.Comments[0].Body)
	}
	if pr.Threads[0].Comments[0].Body != "anchored, revised" {
		t.Errorf("the thread entry is %q", pr.Threads[0].Comments[0].Body)
	}
	if pr.Reviews[0].Body != "a verdict message, revised" {
		t.Errorf("the review body is %q", pr.Reviews[0].Body)
	}
}

// messageOf is the comment event paired with a verdict, as the delta finds it.
func messageOf(t *testing.T, m *mirror, verdict string) string {
	t.Helper()
	for comment, v := range review.VerdictMessages(m.state()) {
		if v == verdict {
			return comment
		}
	}
	t.Fatalf("no message is paired with verdict %s", verdict)
	return ""
}

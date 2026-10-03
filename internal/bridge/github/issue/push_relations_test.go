package ghissue

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hdweiss/git-issue/internal/bridge"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

// tracker is two clones' worth of the same thing a push needs: several local
// entities and the one ledger that says where each of them lives upstream.
//
// A link is the first thing a push cannot do one issue at a time — its target is
// another entity, and the mutation names that entity's upstream object — so the
// harness has to hold more than one.
type tracker struct {
	t        *testing.T
	fake     *fakeGitHub
	pusher   *Pusher
	led      ledger
	mirrors  map[string]*mirror
	upstream map[string]string
}

func newTracker(t *testing.T) *tracker {
	f := newFakeGitHub(t)
	led := ledger{comments: map[string]string{}, entities: map[string]string{}}
	return &tracker{
		t:        t,
		fake:     f,
		pusher:   NewPusher(f.client(), fakeTarget(), entity.SHA1, vocab, led),
		led:      led,
		mirrors:  map[string]*mirror{},
		upstream: map[string]string{},
	}
}

// issue files a local issue with a title, as `git issue add` does.
func (tr *tracker) issue(id, title, nonce string) *mirror {
	tr.t.Helper()
	m := &mirror{id: id, led: tr.led}
	m.add(tr.t, "create", entity.Str(issue.Type), "", nonce+"0000000000")
	m.add(tr.t, "title", entity.Str(title), "", nonce+"1111111111")
	tr.mirrors[id] = m
	return m
}

// link writes a relation locally, the way `edit --parent` does.
func (tr *tracker) link(m *mirror, kind, target, nonce string) entity.Event {
	tr.t.Helper()
	return m.add(tr.t, issue.RelAdd, entity.Str(kind), target, nonce)
}

// unlink retracts every add standing for one pair, which is what a local
// detach does: a link written here and then imported back from the tracker is
// two members carrying the same pair, and a retraction that named only one of
// them would leave the link standing.
func (tr *tracker) unlink(m *mirror, kind, target, nonce string) int {
	tr.t.Helper()
	n := 0
	for _, member := range m.state().Members(issue.RelField) {
		if member.Val.Display() != kind || member.Ref != target {
			continue
		}
		m.add(tr.t, issue.RelRemove, entity.Value{}, member.ID, fmt.Sprintf("%s%04d", nonce, n))
		n++
	}
	return n
}

// push plans and applies, in the order given, recording what each delta earns
// the way cmd/git-issue's journal does — without which a link could never name
// an issue the same run created.
func (tr *tracker) push(order ...string) bridge.Plan {
	tr.t.Helper()
	var candidates []bridge.Candidate
	byID := map[string]bridge.Candidate{}
	for id, m := range tr.mirrors {
		c := m.candidate(tr.upstream[id])
		candidates = append(candidates, c)
		byID[id] = c
	}

	plan, err := tr.pusher.Plan(candidates, tr.led)
	if err != nil {
		tr.t.Fatal(err)
	}
	deltas := map[string]issue.Delta{}
	for _, d := range plan.Deltas {
		deltas[d.ID] = d
	}
	for _, id := range order {
		d, ok := deltas[id]
		if !ok {
			continue
		}
		result, err := tr.pusher.Apply(byID[id], d)
		if err != nil {
			tr.t.Fatalf("applying %s: %v", id, err)
		}
		if result.Upstream != "" {
			tr.upstream[id] = result.Upstream
			tr.led.entities[result.Upstream] = id
		}
		for _, c := range result.Comments {
			tr.led.comments[id+" "+c.EventID] = c.Upstream
		}
	}
	return plan
}

// pull reads every issue back and unions it in, exactly as a pull would.
func (tr *tracker) pull() {
	tr.t.Helper()
	var ids []string
	for _, up := range tr.upstream {
		ids = append(ids, up)
	}
	issues, err := tr.pusher.Client.FetchNodes(ids)
	if err != nil {
		tr.t.Fatal(err)
	}
	for _, gh := range issues {
		imported, err := Import(entity.SHA1, gh, tr.led)
		if err != nil {
			tr.t.Fatal(err)
		}
		m := tr.mirrors[tr.led.entities[gh.ID]]
		m.union(imported.Events())
		for _, c := range imported.Comments {
			tr.led.comments[m.id+" "+c.EventID] = c.Upstream
		}
	}
}

// relations is what a mirror's folded state says about its links.
func (tr *tracker) relations(id string) []issue.Relation {
	return issue.Relations(tr.mirrors[id].state())
}

const (
	epicID  = "1111111111111111111111111111111111111111"
	taskID  = "2222222222222222222222222222222222222222"
	otherID = "3333333333333333333333333333333333333333"
)

// A tree of issues that has never been pushed goes up whole, and the link
// survives the round trip.
//
// The parent is applied first, which is what cmd/git-issue's inLinkOrder
// guarantees: the mapping the create earns is what the child's link names.
func TestPushCreatesIssuesWithTheirParent(t *testing.T) {
	tr := newTracker(t)
	tr.issue(epicID, "The epic", "aaaa")
	child := tr.issue(taskID, "A task under it", "bbbb")
	tr.link(child, issue.KindParent, epicID, "cccc111111111111")

	plan := tr.push(epicID, taskID)
	if len(plan.Skipped) != 0 {
		t.Errorf("skipped %+v, want nothing", plan.Skipped)
	}
	if len(plan.Deltas) != 2 {
		t.Fatalf("%d deltas, want 2", len(plan.Deltas))
	}

	// GitHub takes the parent in createIssue, so the child was filed under the
	// epic rather than filed and then moved — no event for a link the issue was
	// born with.
	up := tr.fake.issues[tr.upstream[taskID]]
	if up.Parent != tr.upstream[epicID] {
		t.Errorf("upstream parent = %q, want %q", up.Parent, tr.upstream[epicID])
	}
	for _, e := range up.Timeline {
		if strings.Contains(e.Typename, "Parent") {
			t.Errorf("the create raised a %s for a link it was born with", e.Typename)
		}
	}

	// And the mirror settles: what came back says the same thing, so there is
	// nothing left to send.
	tr.pull()
	if got := tr.relations(taskID); len(got) != 1 || got[0].Kind != issue.KindParent || got[0].Target != epicID {
		t.Errorf("after the round trip the task holds %+v", got)
	}
	if plan := tr.push(epicID, taskID); len(plan.Deltas) != 0 {
		for _, d := range plan.Deltas {
			t.Errorf("%s would be pushed again: %v", short(d.ID), d.Fields())
		}
	}
}

// A link written after both issues are upstream is a mutation of its own, and
// it goes exactly once.
func TestPushSendsLinksAddedLater(t *testing.T) {
	tr := newTracker(t)
	tr.issue(epicID, "The epic", "aaaa")
	task := tr.issue(taskID, "A task", "bbbb")
	tr.push(epicID, taskID)
	tr.pull()

	tr.link(task, issue.KindParent, epicID, "cccc111111111111")
	tr.link(task, issue.KindBlockedBy, epicID, "dddd111111111111")

	plan := tr.push(taskID)
	if len(plan.Deltas) != 1 || plan.Deltas[0].ID != taskID {
		t.Fatalf("plan = %+v, want one delta for the task", plan.Deltas)
	}
	if got := plan.Deltas[0].Fields(); len(got) != 1 || got[0] != "links" {
		t.Errorf("delta carries %v, want links alone", got)
	}

	up := tr.fake.issues[tr.upstream[taskID]]
	if up.Parent != tr.upstream[epicID] {
		t.Errorf("upstream parent = %q", up.Parent)
	}
	if len(up.BlockedBy) != 1 || up.BlockedBy[0] != tr.upstream[epicID] {
		t.Errorf("upstream blockedBy = %v", up.BlockedBy)
	}

	// The mirror loop terminates: the links come back as GitHub's own events,
	// carrying the same pairs, so the next push has nothing to do.
	tr.pull()
	if got := tr.relations(taskID); len(got) != 2 {
		t.Errorf("after the round trip the task holds %+v, want two links", got)
	}
	for i := 0; i < 2; i++ {
		if plan := tr.push(taskID); len(plan.Deltas) != 0 {
			t.Fatalf("cycle %d would push %v again", i+2, plan.Deltas[0].Fields())
		}
	}
}

// Detaching locally sends the retraction, and only where the tracker still has
// the link to retract.
func TestPushSendsLinkRemovals(t *testing.T) {
	tr := newTracker(t)
	tr.issue(epicID, "The epic", "aaaa")
	task := tr.issue(taskID, "A task", "bbbb")
	tr.link(task, issue.KindParent, epicID, "cccc111111111111")
	tr.push(epicID, taskID)
	tr.pull()

	// Two members by now: the one written here and the one the pull brought
	// back. Both have to be retracted, which is why the CLI reconciles against
	// the members rather than against the pair.
	if n := tr.unlink(task, issue.KindParent, epicID, "eeee11111111"); n != 2 {
		t.Fatalf("retracted %d adds, want the local one and the imported one", n)
	}
	plan := tr.push(taskID)
	if len(plan.Deltas) != 1 {
		t.Fatalf("plan = %+v, want one delta", plan.Deltas)
	}
	if got := plan.Deltas[0].RelationsRemoved; len(got) != 1 || got[0].Target != epicID {
		t.Fatalf("delta removes %+v, want the parent", got)
	}
	if up := tr.fake.issues[tr.upstream[taskID]]; up.Parent != "" {
		t.Errorf("upstream parent = %q after a detach, want none", up.Parent)
	}

	tr.pull()
	if got := tr.relations(taskID); len(got) != 0 {
		t.Errorf("after the round trip the task still holds %+v", got)
	}
	if plan := tr.push(taskID); len(plan.Deltas) != 0 {
		t.Errorf("the detach would be pushed again: %v", plan.Deltas[0].Fields())
	}
}

// A link this tracker cannot be told about is reported, not attempted and not
// dropped in silence. Each reason is a different fact about GitHub.
func TestPushDeclinesLinksItCannotWrite(t *testing.T) {
	tr := newTracker(t)
	tr.issue(epicID, "The epic", "aaaa")
	task := tr.issue(taskID, "A task", "bbbb")
	tr.push(epicID, taskID)
	tr.pull()

	// No markIssueAsDuplicate mutation exists, GitHub has no `related` link at
	// all, and an issue that has never been pushed has no node to name.
	tr.link(task, issue.KindDuplicate, epicID, "cccc111111111111")
	tr.link(task, issue.KindRelated, epicID, "dddd111111111111")
	tr.link(task, issue.KindParent, otherID, "eeee111111111111")

	plan := tr.push(taskID)
	if len(plan.Deltas) != 0 {
		t.Errorf("plan would send %v, want nothing writable", plan.Deltas[0].Fields())
	}
	if len(plan.Skipped) != 3 {
		t.Fatalf("skipped %+v, want three links declined", plan.Skipped)
	}
	for _, want := range []string{
		"no way to write a duplicate-of link",
		"no way to write a related link",
		"is not on ",
	} {
		found := false
		for _, s := range plan.Skipped {
			if s.Field == "links" && strings.Contains(s.Reason, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no skip says %q\n%+v", want, plan.Skipped)
		}
	}
}

package adoissue

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hdweiss/git-issue/internal/bridge"
	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

// tracker is several local entities and the one ledger that says where each of
// them lives upstream, which is the least a link can be tested against.
//
// A link is the first thing a push cannot do one issue at a time — its target
// is another entity, and the patch names that entity's work item — so the
// harness has to hold more than one. The roundtrip_test mirror holds one.
type tracker struct {
	t        *testing.T
	fake     *fakeADO
	pusher   *Pusher
	led      ledger
	mirrors  map[string]*mirror
	upstream map[string]string
}

func newTracker(t *testing.T) *tracker {
	f := newFakeADO(t)
	led := newLedger()
	return &tracker{
		t:        t,
		fake:     f,
		pusher:   NewPusher(f.client(), f.target(), entity.SHA1, vocab, led),
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

// link writes a relation locally, the way `edit --rel` does.
func (tr *tracker) link(m *mirror, kind, target, nonce string) entity.Event {
	tr.t.Helper()
	return m.add(tr.t, issue.RelAdd, entity.Str(kind), target, nonce)
}

// unlink retracts every add standing for one pair, which is what a local detach
// does: a link written here and then imported back from the tracker is two
// members carrying the same pair, and a retraction that named only one of them
// would leave the link standing.
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
// a work item the same run created.
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
	if len(plan.Conflicts) != 0 {
		tr.t.Fatalf("unexpected conflicts: %+v", plan.Conflicts)
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
			tr.t.Fatalf("applying %s: %v", short(id), err)
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

// pull reads every work item back and unions it in, exactly as a pull would.
func (tr *tracker) pull() {
	tr.t.Helper()
	var ids []int
	for _, up := range tr.upstream {
		ids = append(ids, WorkItemID(up))
	}
	items, err := tr.pusher.Client.FetchIDs(tr.pusher.Target, ids, nil)
	if err != nil {
		tr.t.Fatal(err)
	}
	for _, w := range items {
		imported, err := Import(entity.SHA1, tr.pusher.Target, w, tr.led)
		if err != nil {
			tr.t.Fatal(err)
		}
		m := tr.mirrors[tr.led.entities[Origin(tr.pusher.Target, w.ID)]]
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

// item is the work item one entity was filed as.
func (tr *tracker) item(id string) *fakeItem {
	return tr.fake.items[WorkItemID(tr.upstream[id])]
}

// rels is the link types a work item currently holds, in order.
func (item *fakeItem) rels() []string {
	var out []string
	for _, r := range item.relations {
		out = append(out, r.Rel)
	}
	return out
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
func TestPushCreatesWorkItemsWithTheirLinks(t *testing.T) {
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

	// The link went in the create document, so the work item was filed under the
	// epic rather than filed and then moved: one revision, not two.
	task := tr.item(taskID)
	if got := task.rels(); len(got) != 1 || got[0] != adoapi.RelParent {
		t.Errorf("upstream links = %v, want the parent alone", got)
	}
	if len(task.updates) != 1 {
		t.Errorf("the create left %d revisions; a link it was born with is not history",
			len(task.updates))
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

// Every kind docs/issues.md defines reaches Azure DevOps, which is the whole of
// what makes this the more capable of the two bridges on links — GitHub can
// write two of the four.
func TestPushSendsEveryKind(t *testing.T) {
	tr := newTracker(t)
	tr.issue(epicID, "The epic", "aaaa")
	task := tr.issue(taskID, "A task", "bbbb")
	tr.issue(otherID, "Something else", "cccc")
	tr.push(epicID, taskID, otherID)
	tr.pull()

	tr.link(task, issue.KindParent, epicID, "dddd111111111111")
	tr.link(task, issue.KindBlockedBy, otherID, "eeee111111111111")
	tr.link(task, issue.KindDuplicate, otherID, "ffff111111111111")
	tr.link(task, issue.KindRelated, epicID, "0000111111111111")

	plan := tr.push(taskID)
	if len(plan.Skipped) != 0 {
		t.Errorf("skipped %+v, want nothing", plan.Skipped)
	}
	if len(plan.Deltas) != 1 || plan.Deltas[0].ID != taskID {
		t.Fatalf("plan = %+v, want one delta for the task", plan.Deltas)
	}
	if got := plan.Deltas[0].Fields(); len(got) != 1 || got[0] != "links" {
		t.Errorf("delta carries %v, want links alone", got)
	}

	want := []string{
		adoapi.RelParent,
		adoapi.RelPredecessor,
		adoapi.RelDuplicateOf,
		adoapi.RelRelated,
	}
	if got := tr.item(taskID).rels(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("upstream links = %v, want %v", got, want)
	}
	// Four links, one patch, one revision.
	if got := len(tr.item(taskID).updates); got != 2 {
		t.Errorf("the work item has %d revisions; four links written together are one change", got)
	}

	// The mirror loop terminates: the links come back as the tracker's own
	// revisions, carrying the same pairs, so the next push has nothing to do.
	tr.pull()
	if got := tr.relations(taskID); len(got) != 4 {
		t.Errorf("after the round trip the task holds %+v, want four links", got)
	}
	for i := 0; i < 2; i++ {
		if plan := tr.push(taskID); len(plan.Deltas) != 0 {
			t.Fatalf("cycle %d would push %v again", i+2, plan.Deltas[0].Fields())
		}
	}
}

// Detaching locally sends the retraction — and two at once, which is the case
// the index arithmetic exists for: a patch names a relation by its position, so
// removing the first would renumber the second.
func TestPushSendsLinkRemovals(t *testing.T) {
	tr := newTracker(t)
	tr.issue(epicID, "The epic", "aaaa")
	task := tr.issue(taskID, "A task", "bbbb")
	tr.issue(otherID, "Something else", "cccc")
	tr.link(task, issue.KindParent, epicID, "dddd111111111111")
	tr.link(task, issue.KindBlockedBy, otherID, "eeee111111111111")
	tr.link(task, issue.KindRelated, otherID, "ffff111111111111")
	tr.push(epicID, otherID, taskID)
	tr.pull()

	// Two members per pair by now: the one written here and the one the pull
	// brought back. Both have to be retracted, which is why the CLI reconciles
	// against the members rather than against the pair.
	if n := tr.unlink(task, issue.KindParent, epicID, "1111111111"); n != 2 {
		t.Fatalf("retracted %d parent adds, want the local one and the imported one", n)
	}
	if n := tr.unlink(task, issue.KindBlockedBy, otherID, "2222222222"); n != 2 {
		t.Fatalf("retracted %d blocked-by adds, want two", n)
	}

	plan := tr.push(taskID)
	if len(plan.Deltas) != 1 {
		t.Fatalf("plan = %+v, want one delta", plan.Deltas)
	}
	if got := plan.Deltas[0].RelationsRemoved; len(got) != 2 {
		t.Fatalf("delta removes %+v, want both links", got)
	}

	// The related link is the one that was not retracted, and it is the one that
	// must survive — an off-by-one in the indexes takes it instead.
	if got := tr.item(taskID).rels(); len(got) != 1 || got[0] != adoapi.RelRelated {
		t.Errorf("upstream links = %v after two detaches, want the related link alone", got)
	}

	tr.pull()
	if got := tr.relations(taskID); len(got) != 1 || got[0].Kind != issue.KindRelated {
		t.Errorf("after the round trip the task holds %+v, want the related link alone", got)
	}
	if plan := tr.push(taskID); len(plan.Deltas) != 0 {
		t.Errorf("the detach would be pushed again: %v", plan.Deltas[0].Fields())
	}
}

// A link this tracker cannot be told about is reported, not attempted and not
// dropped in silence.
func TestPushDeclinesLinksItCannotWrite(t *testing.T) {
	tr := newTracker(t)
	task := tr.issue(taskID, "A task", "bbbb")
	tr.push(taskID)
	tr.pull()

	// A kind that arrived through some other bridge has no Azure DevOps link
	// type, and an issue that has never been pushed has no work item to name.
	tr.link(task, "supersedes", taskID, "cccc111111111111")
	tr.link(task, issue.KindParent, otherID, "dddd111111111111")

	plan := tr.push(taskID)
	if len(plan.Deltas) != 0 {
		t.Errorf("plan would send %v, want nothing writable", plan.Deltas[0].Fields())
	}
	if len(plan.Skipped) != 2 {
		t.Fatalf("skipped %+v, want two links declined", plan.Skipped)
	}
	for _, want := range []string{"no link type for a supersedes link", "is not on "} {
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

// A symmetric link comes back from a pull as a member on *each* entity, because
// Azure DevOps stores `Related` on both work items. Letting go of it therefore
// retracts two members and pushes two removals — and one of them finds the link
// already gone, which must be a write that is not made rather than a patch that
// carries nothing but its own precondition.
func TestPushDropsASymmetricLinkFromBothEnds(t *testing.T) {
	tr := newTracker(t)
	epic := tr.issue(epicID, "The epic", "aaaa")
	tr.issue(taskID, "A task", "bbbb")
	tr.link(epic, issue.KindRelated, taskID, "cccc111111111111")
	// The task first: it is the epic's link that names it, so it is the one
	// that needs a work item already, which is what inLinkOrder arranges.
	tr.push(taskID, epicID)
	tr.pull()

	// One link upstream, one member at each end after the pull.
	for _, id := range []string{epicID, taskID} {
		if got := tr.relations(id); len(got) != 1 || got[0].Kind != issue.KindRelated {
			t.Fatalf("%s holds %+v after the pull, want the related link", short(id), got)
		}
	}

	// What `edit --no-rel related` now writes: both members go.
	if n := tr.unlink(tr.mirrors[epicID], issue.KindRelated, taskID, "1111111111"); n != 2 {
		t.Fatalf("retracted %d adds on the epic, want the local one and the imported one", n)
	}
	if n := tr.unlink(tr.mirrors[taskID], issue.KindRelated, epicID, "2222222222"); n != 1 {
		t.Fatalf("retracted %d adds on the task, want the imported one", n)
	}

	before := len(tr.fake.patches)
	plan := tr.push(epicID, taskID)
	if len(plan.Deltas) != 2 {
		t.Fatalf("plan = %+v, want a removal from each end", plan.Deltas)
	}
	// Two deltas, but only one of them is a write: the second finds the link
	// already gone and sends nothing.
	if sent := len(tr.fake.patches) - before; sent != 1 {
		t.Errorf("sent %d patches, want 1: the second end has nothing left to remove", sent)
	}
	if got := tr.item(epicID).rels(); len(got) != 0 {
		t.Errorf("upstream links = %v, want none", got)
	}

	tr.pull()
	for _, id := range []string{epicID, taskID} {
		if got := tr.relations(id); len(got) != 0 {
			t.Errorf("%s still holds %+v after the round trip", short(id), got)
		}
	}
	if plan := tr.push(epicID, taskID); len(plan.Deltas) != 0 {
		t.Errorf("the detach would be pushed again: %v", plan.Deltas[0].Fields())
	}
}

// A link added upstream while this clone was not looking is imported, not
// undone. Only a link this clone once held and has since dropped is retracted,
// which is what keeps a push from reverting somebody else's work.
func TestPushLeavesUpstreamOnlyLinksAlone(t *testing.T) {
	tr := newTracker(t)
	tr.issue(epicID, "The epic", "aaaa")
	tr.issue(taskID, "A task", "bbbb")
	tr.push(epicID, taskID)
	tr.pull()

	// Somebody files the task under the epic in Azure DevOps.
	task := tr.item(taskID)
	task.relations = append(task.relations, tr.fake.link(adoapi.RelParent, WorkItemID(tr.upstream[epicID])))
	task.rev++
	task.updates = append(task.updates, fakeUpdate{
		fields: map[string][2]string{},
		added:  task.relations[len(task.relations)-1:],
		by:     "them@corp.example",
		at:     parseTime("2026-05-01T00:00:00Z"),
	})

	if plan := tr.push(taskID); len(plan.Deltas) != 0 {
		t.Fatalf("the push would send %v, undoing a link it never held", plan.Deltas[0].RelationsRemoved)
	}
	if got := tr.item(taskID).rels(); len(got) != 1 {
		t.Errorf("upstream links = %v, want the one somebody else made", got)
	}

	// And a pull picks it up.
	tr.pull()
	if got := tr.relations(taskID); len(got) != 1 || got[0].Target != epicID {
		t.Errorf("after the pull the task holds %+v, want the parent", got)
	}
}

package giteaissue

import (
	"strings"
	"testing"

	"github.com/hdweiss/git-issue/internal/bridge"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

var vocab = issue.Vocabulary.With(Vocabulary)

// ledger is a Mapped/Lookup built from literals, standing in for the real one.
type ledger struct {
	comments map[string]string // entity id + " " + event id -> upstream id
	entities map[string]string // upstream origin string -> entity id
}

func newLedger() ledger {
	return ledger{comments: map[string]string{}, entities: map[string]string{}}
}

func (l ledger) Entity(upstream string) (string, bool) {
	v, ok := l.entities[upstream]
	return v, ok
}

func (l ledger) Upstream(entity string) (string, bool) {
	for upstream, id := range l.entities {
		if id == entity {
			return upstream, true
		}
	}
	return "", false
}

func (l ledger) Comment(id, event string) (string, bool) {
	v, ok := l.comments[id+" "+event]
	return v, ok
}

func (l ledger) ClaimedComment(upstream string) bool {
	for _, v := range l.comments {
		if v == upstream {
			return true
		}
	}
	return false
}

// TestPlanReportsUnsupportedLinks checks that a delta carrying links Gitea
// cannot write is stripped and reported rather than half-attempted.
func TestPlanReportsUnsupportedLinks(t *testing.T) {
	f := newFakeGitea(t)
	p := NewPusher(f.client(), fakeTarget(), entity.SHA1, vocab, newLedger())

	m := newMirror("child000local")
	m.add(t, "create", entity.Str(issue.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("Has a parent"), "", "2222222222222222")
	m.add(t, issue.RelAdd, entity.Str(issue.KindParent), "epic000local", "3333333333333333")
	m.add(t, issue.RelAdd, entity.Str(issue.KindRelated), "other00local", "4444444444444444")

	plan, err := p.Plan([]bridge.Candidate{m.candidate("")}, m.led)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deltas) != 1 || !plan.Deltas[0].Create {
		t.Fatalf("plan deltas = %+v", plan.Deltas)
	}
	if len(plan.Deltas[0].RelationsAdded) != 0 {
		t.Errorf("unsupported links left in the delta: %+v", plan.Deltas[0].RelationsAdded)
	}
	kinds := map[string]bool{}
	for _, s := range plan.Skipped {
		if s.Field == "links" {
			kinds[s.Reason] = true
		}
	}
	if len(kinds) != 2 {
		t.Errorf("skipped link reasons = %v, want one for parent and one for related", kinds)
	}
}

// TestPlanFailsFastOnMissingRepo checks that a target repository that is not
// there aborts the push before any issue is filed, with a message that says
// what to do.
func TestPlanFailsFastOnMissingRepo(t *testing.T) {
	f := newFakeGitea(t)
	f.repoMissing = true
	p := NewPusher(f.client(), fakeTarget(), entity.SHA1, vocab, newLedger())

	m := newMirror("missing0local")
	m.add(t, "create", entity.Str(issue.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("Nowhere to go"), "", "2222222222222222")

	_, err := p.Plan([]bridge.Candidate{m.candidate("")}, m.led)
	if err == nil {
		t.Fatal("Plan should fail when the repository does not exist")
	}
	if !strings.Contains(err.Error(), "no such repository") {
		t.Errorf("error = %q, want it to name the missing repository", err)
	}
	if len(f.issues) != 0 {
		t.Errorf("Plan filed %d issues; it must write nothing", len(f.issues))
	}
}

// TestUnknownLabelWarnsAndDoesNotFail checks that a label or milestone the
// repository does not have is reported and dropped rather than failing the push.
func TestUnknownLabelWarnsAndDoesNotFail(t *testing.T) {
	f := newFakeGitea(t)
	p := NewPusher(f.client(), fakeTarget(), entity.SHA1, vocab, newLedger())

	m := newMirror("labels00local")
	m.add(t, "create", entity.Str(issue.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("Two labels, one unknown"), "", "2222222222222222")
	m.add(t, "label.add", entity.Str("bug"), "", "3333333333333333")
	m.add(t, "label.add", entity.Str("nonexistent"), "", "4444444444444444")
	m.add(t, "milestone", entity.Str("no-such-milestone"), "", "5555555555555555")

	plan, err := p.Plan([]bridge.Candidate{m.candidate("")}, m.led)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Deltas) != 1 {
		t.Fatalf("plan deltas = %+v", plan.Deltas)
	}
	if got := plan.Deltas[0].LabelsAdded; len(got) != 1 || got[0] != "bug" {
		t.Errorf("delta LabelsAdded = %v, want [bug]", got)
	}
	if _, ok := plan.Deltas[0].Set["milestone"]; ok {
		t.Error("unknown milestone left in the delta")
	}
	fields := map[string]bool{}
	for _, s := range plan.Skipped {
		fields[s.Field] = true
	}
	if !fields["label"] || !fields["milestone"] {
		t.Fatalf("skips missing label and/or milestone: %+v", plan.Skipped)
	}

	result, err := p.Apply(m.candidate(""), plan.Deltas[0])
	if err != nil {
		t.Fatalf("apply must not fail on an unknown label: %v", err)
	}
	iss := f.issues[parseNumber(result.Upstream)]
	if len(iss.Labels) != 1 || iss.Labels[0] != "bug" {
		t.Errorf("upstream labels = %v, want [bug]", iss.Labels)
	}
}

// TestUnknownAssigneeWarnsAndDoesNotFail checks that an assignee the repository
// cannot assign is reported and dropped rather than 422-ing the whole push.
func TestUnknownAssigneeWarnsAndDoesNotFail(t *testing.T) {
	f := newFakeGitea(t)
	p := NewPusher(f.client(), fakeTarget(), entity.SHA1, vocab, newLedger())

	m := newMirror("assign00local")
	m.add(t, "create", entity.Str(issue.Type), "", "1111111111111111")
	m.add(t, "title", entity.Str("Assigned to a stranger"), "", "2222222222222222")
	m.add(t, "assignee.add", entity.Str("alice"), "", "3333333333333333")
	m.add(t, "assignee.add", entity.Str("sudoforge"), "", "4444444444444444")

	plan, err := p.Plan([]bridge.Candidate{m.candidate("")}, m.led)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Deltas) != 1 {
		t.Fatalf("plan deltas = %+v", plan.Deltas)
	}
	if got := plan.Deltas[0].AssigneesAdded; len(got) != 1 || got[0] != "alice" {
		t.Errorf("delta AssigneesAdded = %v, want [alice]", got)
	}
	var skip *bridge.Skip
	for i := range plan.Skipped {
		if plan.Skipped[i].Field == "assignee" {
			skip = &plan.Skipped[i]
		}
	}
	if skip == nil || !strings.Contains(skip.Reason, "sudoforge") {
		t.Fatalf("no assignee skip reported: %+v", plan.Skipped)
	}

	result, err := p.Apply(m.candidate(""), plan.Deltas[0])
	if err != nil {
		t.Fatalf("apply must not fail on an unknown assignee: %v", err)
	}
	iss := f.issues[parseNumber(result.Upstream)]
	if len(iss.Assignees) != 1 || iss.Assignees[0] != "alice" {
		t.Errorf("upstream assignees = %v, want [alice]", iss.Assignees)
	}
}

// TestBlockedByPushes files two issues and links one to the other, then settles.
func TestBlockedByPushes(t *testing.T) {
	f := newFakeGitea(t)
	led := newLedger()
	p := NewPusher(f.client(), fakeTarget(), entity.SHA1, vocab, led)

	blocker := newMirror("blocker00local")
	blocker.led = led
	blocker.add(t, "create", entity.Str(issue.Type), "", "1111111111111111")
	blocker.add(t, "title", entity.Str("Must land first"), "", "2222222222222222")

	blocked := newMirror("blocked00local")
	blocked.led = led
	blocked.add(t, "create", entity.Str(issue.Type), "", "3333333333333333")
	blocked.add(t, "title", entity.Str("Waits on the other"), "", "4444444444444444")
	blocked.add(t, issue.RelAdd, entity.Str(issue.KindBlockedBy), blocker.id, "5555555555555555")

	blockerUp, blockedUp := "", ""
	pushAndPull(t, p, blocker, &blockerUp)
	led.entities[blockerUp] = blocker.id

	pushAndPull(t, p, blocked, &blockedUp)
	led.entities[blockedUp] = blocked.id

	number := parseNumber(blockedUp)
	iss := f.issues[number]
	if len(iss.Dependencies) != 1 {
		t.Fatalf("upstream dependencies = %v, want one", iss.Dependencies)
	}
	if got := parseNumber(blockerUp); iss.Dependencies[0] != got {
		t.Errorf("dependency = %d, want %d", iss.Dependencies[0], got)
	}

	if sent := pushAndPull(t, p, blocked, &blockedUp); sent != 0 {
		t.Errorf("blocked-by was sent again: %d deltas", sent)
	}
}

package ghissue

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hdweiss/git-issue/internal/bridge"
	ghapi "github.com/hdweiss/git-issue/internal/bridge/github/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

var vocab = issue.Vocabulary.With(Vocabulary)

// ledger is a Mapped/Claimed built from a literal, standing in for the real one.
type ledger struct {
	comments map[string]string // entity id + " " + event id -> upstream id
	entities map[string]string // upstream node id -> entity id
}

func (l ledger) Entity(upstream string) (string, bool) {
	v, ok := l.entities[upstream]
	return v, ok
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

// nodesServer answers a nodes(ids:) query from the recorded fixture, so a plan
// can be built with no network. It replies with whichever fixture issues the
// query named.
func nodesServer(t *testing.T) *ghapi.Client {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(fixtureDir, "issues.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded struct {
		Data struct {
			Repository struct {
				Issues struct {
					Nodes []json.RawMessage `json:"nodes"`
				} `json:"issues"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &recorded); err != nil {
		t.Fatal(err)
	}
	byID := map[string]json.RawMessage{}
	for _, n := range recorded.Data.Repository.Issues.Nodes {
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(n, &probe); err != nil {
			t.Fatal(err)
		}
		byID[probe.ID] = n
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables struct {
				IDs []string `json:"ids"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		nodes := make([]json.RawMessage, 0, len(req.Variables.IDs))
		for _, id := range req.Variables.IDs {
			if n, ok := byID[id]; ok {
				nodes = append(nodes, n)
			} else {
				nodes = append(nodes, json.RawMessage("null"))
			}
		}
		out, err := json.Marshal(map[string]any{"data": map[string]any{"nodes": nodes}})
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return ghapi.New(srv.URL, "test-token")
}

func testPusher(t *testing.T) *Pusher {
	t.Helper()
	return NewPusher(nodesServer(t), ghapi.Target{
		Host: ghapi.PublicHost, Owner: "hdweiss", Name: "git-issue",
	}, entity.SHA1, vocab, nil)
}

// candidatesFromFixture imports every fixture issue and presents the result as
// the local state, exactly as a clone that had just pulled would hold it.
func candidatesFromFixture(t *testing.T) ([]bridge.Candidate, ledger) {
	t.Helper()
	led := ledger{comments: map[string]string{}}
	var out []bridge.Candidate
	for _, gh := range fetchFixture(t) {
		e, err := Import(entity.SHA1, gh, nil)
		if err != nil {
			t.Fatal(err)
		}
		events := e.Events()
		out = append(out, bridge.Candidate{
			ID:       e.ID,
			Events:   events,
			State:    entity.Fold(vocab, events),
			Upstream: e.Origin,
		})
		for _, c := range e.Comments {
			led.comments[e.ID+" "+c.EventID] = c.Upstream
		}
	}
	return out, led
}

// The invariant the whole push rests on: a clone holding exactly what the
// tracker holds has nothing to push.
//
// The base is reconstructed by replaying the importer, so this fails loudly if
// Import ever stops being a pure function of what GitHub returned — which is
// also what re-import convergence depends on. It is the cheapest possible guard
// on the most expensive possible bug: a push that rewrites every issue upstream
// on every run.
func TestPlanAgainstItselfIsEmpty(t *testing.T) {
	candidates, led := candidatesFromFixture(t)
	plan, err := testPusher(t).Plan(candidates, led)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Deltas) != 0 {
		for _, d := range plan.Deltas {
			t.Errorf("%s would be pushed: %v", d.ID[:12], d.Fields())
		}
	}
	if len(plan.Conflicts) != 0 {
		t.Errorf("conflicts against itself: %+v", plan.Conflicts)
	}
	if len(plan.Skipped) != 0 {
		t.Errorf("skips against itself: %+v", plan.Skipped)
	}
}

// A local edit on top of an imported issue is what a push is for, and only that
// field goes.
func TestPlanSendsLocalEdits(t *testing.T) {
	candidates, led := candidatesFromFixture(t)

	target := candidates[0]
	edit, err := entity.NewEventWithNonce(entity.SHA1, entity.Event{
		V: entity.FormatVersion, C: target.State.NextClock(), TS: 1772700000,
		A: "a@example.com", Op: "title", N: "abcdef0123456789",
		Val: entity.Str("Renamed locally"),
	})
	if err != nil {
		t.Fatal(err)
	}
	events := append(append([]entity.Event(nil), target.Events...), edit)
	candidates[0] = bridge.Candidate{
		ID: target.ID, Events: events,
		State: entity.Fold(vocab, events), Upstream: target.Upstream,
	}

	plan, err := testPusher(t).Plan(candidates, led)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deltas) != 1 {
		t.Fatalf("plan has %d deltas, want 1", len(plan.Deltas))
	}
	d := plan.Deltas[0]
	if d.ID != target.ID {
		t.Errorf("delta is for %s, want %s", d.ID[:12], target.ID[:12])
	}
	if got := d.Set["title"].Display(); got != "Renamed locally" {
		t.Errorf("delta title = %q", got)
	}
	// Only the field that moved. Everything else came from the import and
	// therefore already agrees.
	if len(d.Set) != 1 {
		t.Errorf("delta carries %v, want title alone", d.Fields())
	}
}

// A local comment with no ledger entry is one to post; the same comment once
// mapped is not. This is what stops a mirror from re-posting every comment.
func TestPlanPostsOnlyUnmappedComments(t *testing.T) {
	candidates, led := candidatesFromFixture(t)

	target := candidates[0]
	comment, err := entity.NewEventWithNonce(entity.SHA1, entity.Event{
		V: entity.FormatVersion, C: target.State.NextClock(), TS: 1772700000,
		A: "a@example.com", Op: "comment", N: "abcdef0123456789",
		Val: entity.Str("Written here, not there"),
	})
	if err != nil {
		t.Fatal(err)
	}
	events := append(append([]entity.Event(nil), target.Events...), comment)
	candidates[0] = bridge.Candidate{
		ID: target.ID, Events: events,
		State: entity.Fold(vocab, events), Upstream: target.Upstream,
	}

	plan, err := testPusher(t).Plan(candidates, led)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deltas) != 1 || len(plan.Deltas[0].CommentsNew) != 1 {
		t.Fatalf("plan = %+v, want one comment to post", plan.Deltas)
	}
	if got := plan.Deltas[0].CommentsNew[0].Entry.Body.Display(); got != "Written here, not there" {
		t.Errorf("queued comment %q", got)
	}

	// Once the ledger names it, the same state has nothing to send — which is
	// the state this clone is in right after the push that posted it.
	led.comments[target.ID+" "+comment.ID] = "IC_justPosted"
	plan, err = testPusher(t).Plan(candidates, led)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deltas) != 0 {
		t.Errorf("a mapped comment was queued again: %+v", plan.Deltas)
	}
}

// An entity the ledger does not know is filed upstream rather than compared
// against nothing.
func TestPlanCreatesUnmapped(t *testing.T) {
	candidates, led := candidatesFromFixture(t)
	candidates[0].Upstream = ""

	plan, err := testPusher(t).Plan(candidates, led)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deltas) != 1 || !plan.Deltas[0].Create {
		t.Fatalf("plan = %+v, want one create", plan.Deltas)
	}
	if plan.Deltas[0].Set["title"].Display() == "" {
		t.Error("a create carries no title")
	}
}

// A ledger entry naming something GitHub will not return is reported, never
// re-filed: the issue may well still exist, and creating a second one is the
// error nothing can undo.
func TestPlanSkipsUnreadableUpstream(t *testing.T) {
	candidates, led := candidatesFromFixture(t)
	candidates = candidates[:1]
	candidates[0].Upstream = "I_deletedOrTransferred"

	plan, err := testPusher(t).Plan(candidates, led)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deltas) != 0 {
		t.Errorf("an unreadable upstream produced a delta: %+v", plan.Deltas)
	}
	if len(plan.Skipped) != 1 || !strings.Contains(plan.Skipped[0].Reason, "not readable") {
		t.Fatalf("Skipped = %+v, want one unreadable", plan.Skipped)
	}
}

// A comment the ledger claims is not imported again. Two entries with the same
// text are legitimately two comments, so nothing downstream could tell a
// duplicate apart — the ledger is the only thing that can.
func TestImportSkipsClaimedComments(t *testing.T) {
	var withComments ghapi.Issue
	for _, gh := range fetchFixture(t) {
		if len(gh.Comments) > 0 {
			withComments = gh
			break
		}
	}
	if withComments.ID == "" {
		t.Skip("no fixture issue has comments")
	}

	full, err := Import(entity.SHA1, withComments, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed := ledger{comments: map[string]string{
		"x " + full.Comments[0].EventID: withComments.Comments[0].ID,
	}}

	trimmed, err := Import(entity.SHA1, withComments, claimed)
	if err != nil {
		t.Fatal(err)
	}
	if len(trimmed.Comments) != len(full.Comments)-1 {
		t.Errorf("claimed import produced %d comment mappings, want %d",
			len(trimmed.Comments), len(full.Comments)-1)
	}
	before := entity.Fold(vocab, full.Events())
	after := entity.Fold(vocab, trimmed.Events())
	if len(after.Thread) != len(before.Thread)-1 {
		t.Errorf("claimed import folded %d entries, want %d", len(after.Thread), len(before.Thread)-1)
	}
	// The entity id is untouched: the create event is what it hashes, and
	// suppressing a comment must never move it.
	if trimmed.ID != full.ID {
		t.Errorf("claiming a comment changed the entity id %s -> %s", full.ID, trimmed.ID)
	}
}

package ghissue

import (
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

// The entity ids the ledger reports for the issues this clone already holds.
// Anything not in here is an issue in another repository, or one this clone has
// never imported — the same thing as far as a relation is concerned.
const (
	epicEntity    = "1111111111111111111111111111111111111111"
	blockerEntity = "2222222222222222222222222222222222222222"
)

func relationLedger() ledger {
	return ledger{entities: map[string]string{
		"I_epic":    epicEntity,
		"I_blocker": blockerEntity,
	}}
}

// fetchRelations reads the recorded relationship response through a real
// client, so the fragments' field names are covered as well as the mapping: a
// `blockingIssue` the query spelled one way and the struct another would show up
// here rather than against the live API.
func fetchRelations(t *testing.T) map[string]ghapi.Issue {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(fixtureDir, "relations.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)

	issues, err := ghapi.New(srv.URL, "test-token").Fetch(ghapi.Target{
		Host: ghapi.PublicHost, Owner: "hdweiss", Name: "git-issue",
	}, ghapi.Filter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ghapi.Issue{}
	for _, gh := range issues {
		out[gh.ID] = gh
	}
	if len(out) != 4 {
		t.Fatalf("fetched %d issues, want 4", len(out))
	}
	return out
}

// rel is one relation event, flattened for comparison.
type rel struct{ op, kind, target, author string }

func relations(t *testing.T, gh ghapi.Issue, known bridge.Lookup) ([]rel, Entity) {
	t.Helper()
	e, err := Import(entity.SHA1, gh, known)
	if err != nil {
		t.Fatal(err)
	}
	var out []rel
	adds := map[string]rel{}
	for _, ev := range e.Events() {
		switch ev.Op {
		case issue.RelAdd:
			r := rel{op: ev.Op, kind: ev.Val.Display(), target: ev.Ref, author: ev.A}
			adds[ev.ID] = r
			out = append(out, r)
		case issue.RelRemove:
			// A removal names the add it retracts rather than a value, so what
			// it retracted is read back through that add.
			was := adds[ev.Ref]
			out = append(out, rel{op: ev.Op, kind: was.kind, target: was.target, author: ev.A})
		}
	}
	return out, e
}

// The timeline replay: a parent and a blocking pair, each stored on the end
// whose state the link constrains, and each with the actor and moment GitHub
// reported rather than the issue's own.
func TestImportsRelationsFromTimeline(t *testing.T) {
	got, _ := relations(t, fetchRelations(t)["I_child"], relationLedger())
	want := []rel{
		{issue.RelAdd, issue.KindParent, epicEntity, "github:hdweiss"},
		{issue.RelAdd, issue.KindBlockedBy, blockerEntity, "github:octocat"},
		{issue.RelRemove, issue.KindBlockedBy, blockerEntity, "github:octocat"},
	}
	if len(got) != len(want) {
		t.Fatalf("imported %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("relation %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A link to an issue this clone does not hold is left unwritten. A relation's
// target is an entity id, which cannot be derived from a node id without
// fetching the issue it names — and that is also the cross-repository case,
// since an issue in another repository is not in this ledger.
func TestUnknownRelationTargetIsNotWritten(t *testing.T) {
	got, _ := relations(t, fetchRelations(t)["I_child"], relationLedger())
	for _, r := range got {
		if r.target == "" || r.target == "I_elsewhere" {
			t.Errorf("wrote a relation with an unresolved target: %+v", r)
		}
	}
	// And with no ledger at all, nothing resolves and nothing is written —
	// which is what a plan that was handed no ledger must not mistake for a
	// difference to push.
	if got, _ := relations(t, fetchRelations(t)["I_child"], nil); len(got) != 0 {
		t.Errorf("imported %d relations with no ledger, want none", len(got))
	}
}

// A removal must name the specific addition it retracts, so that the OR-Set is
// populated properly rather than reconstructed from a final state — and so that
// the commit message can say what was unlinked.
func TestRelationRemovalReferencesItsAddition(t *testing.T) {
	_, e := relations(t, fetchRelations(t)["I_child"], relationLedger())

	adds := map[string]string{}
	removals := 0
	for _, ev := range e.Events() {
		switch ev.Op {
		case issue.RelAdd:
			adds[ev.ID] = ev.Val.Display() + " " + ev.Ref
		case issue.RelRemove:
			removals++
			if ev.Ref == "" {
				t.Fatal("rel.remove carries no ref")
			}
			if want := issue.KindBlockedBy + " " + blockerEntity; adds[ev.Ref] != want {
				t.Errorf("rel.remove points at %q, want the %s addition", adds[ev.Ref], want)
			}
		}
	}
	if removals != 1 {
		t.Fatalf("%d removals, want 1", removals)
	}

	var said bool
	for _, a := range e.Actions {
		if strings.Contains(a.Message, "Unlink "+issue.KindBlockedBy) {
			said = true
		}
	}
	if !said {
		t.Errorf("no commit message says what was unlinked:\n%s", messages(e))
	}
}

// Current state fills in what the timeline did not say, and only that. A
// transferred issue has a parent and no history of acquiring one; an issue whose
// replay already produced the link must not import it twice.
func TestReconcilesRelationsTheTimelineDidNotCover(t *testing.T) {
	fixture := fetchRelations(t)

	got, e := relations(t, fixture["I_transferred"], relationLedger())
	want := rel{issue.RelAdd, issue.KindParent, epicEntity, "github:hdweiss"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("transferred issue imported %+v, want just %+v", got, want)
	}
	// Reconciled events join the action that filed the issue, which is the
	// earliest moment a link with no event of its own can be attributed to.
	if c := e.Events()[0].C; relClock(t, e) != c {
		t.Errorf("reconciled relation is at c=%d, want the create's %d", relClock(t, e), c)
	}

	// The child's parent is in both the timeline and the current state, and
	// imports once.
	got, _ = relations(t, fixture["I_child"], relationLedger())
	if n := len(ofKind(got, issue.KindParent)); n != 1 {
		t.Errorf("%d parent relations for an issue with one, want 1", n)
	}
}

// A duplicate is always reconciled: GitHub raises MarkedAsDuplicateEvent on the
// canonical issue naming the duplicate, so the duplicate's own timeline says
// nothing and `duplicateOf` is the only source there is.
//
// The close reason is a separate fact and keeps arriving separately: an issue
// can be closed as a duplicate of nothing in particular.
func TestImportsDuplicateOfAsRelation(t *testing.T) {
	got, e := relations(t, fetchRelations(t)["I_dup"], relationLedger())
	want := rel{issue.RelAdd, issue.KindDuplicate, epicEntity, "github:octocat"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("imported %+v, want just %+v", got, want)
	}
	st := entity.Fold(issue.Vocabulary, e.Events())
	if reason := st.Scalar("status.reason").Display(); reason != "duplicate" {
		t.Errorf("status.reason = %q, want duplicate", reason)
	}
	if got := issue.RelationsOf(st, issue.KindDuplicate); len(got) != 1 || got[0].Target != epicEntity {
		t.Errorf("folds to %+v, want one duplicate-of %s", got, epicEntity)
	}
}

// Relations must not break the property the whole bridge rests on: a second
// import produces the same bytes, so re-syncing adds nothing.
func TestRelationImportIsIdempotent(t *testing.T) {
	for id, gh := range fetchRelations(t) {
		first, err := Import(entity.SHA1, gh, relationLedger())
		if err != nil {
			t.Fatal(err)
		}
		second, err := Import(entity.SHA1, gh, relationLedger())
		if err != nil {
			t.Fatal(err)
		}
		if string(first.Blob()) != string(second.Blob()) {
			t.Errorf("%s differs on re-import\n--- first ---\n%s\n--- second ---\n%s",
				id, first.Blob(), second.Blob())
		}
		if first.ID != second.ID {
			t.Errorf("%s changed identity between imports", id)
		}
	}
}

// An issue's identity must not depend on what this clone happens to know: the
// create event is a pure function of the response, so the same issue imported
// with and without a ledger is the same entity.
func TestRelationsDoNotChangeIdentity(t *testing.T) {
	for id, gh := range fetchRelations(t) {
		with, err := Import(entity.SHA1, gh, relationLedger())
		if err != nil {
			t.Fatal(err)
		}
		without, err := Import(entity.SHA1, gh, nil)
		if err != nil {
			t.Fatal(err)
		}
		if with.ID != without.ID {
			t.Errorf("%s: id %s with a ledger, %s without", id, with.ID, without.ID)
		}
	}
}

// A batch routinely holds both ends of a link, and the ledger learns where an
// issue is filed only as the batch is imported. So an import reports what it
// could not resolve, and the caller reads those issues again once the rest of
// the batch is filed — which costs no requests, and is what stops a link
// between two issues that arrived together from waiting for a run that a
// `--since` watermark may never make.
//
// This is the two-pass loop in cmd/git-issue/pull_github.go, in miniature.
func TestABatchResolvesLinksWithinItself(t *testing.T) {
	fixture := fetchRelations(t)
	// Worst case: every issue that depends on another comes before it.
	order := []string{"I_dup", "I_transferred", "I_child", "I_epic"}
	led := ledger{entities: map[string]string{}}

	deferred := map[string]bool{}
	for _, node := range order {
		e, err := Import(entity.SHA1, fixture[node], led)
		if err != nil {
			t.Fatal(err)
		}
		led.entities[e.Origin] = e.ID
		if e.Unresolved > 0 {
			deferred[node] = true
		}
	}
	for _, node := range []string{"I_dup", "I_transferred", "I_child"} {
		if !deferred[node] {
			t.Errorf("%s links to an issue filed after it and was not deferred", node)
		}
	}
	// The epic links to nothing, so nothing is gained by reading it again.
	if deferred["I_epic"] {
		t.Error("an issue with no links was deferred")
	}

	// The second pass, with the batch filed, writes every link that names an
	// issue in it.
	for _, node := range []string{"I_dup", "I_transferred", "I_child"} {
		got, _ := relations(t, fixture[node], led)
		if len(got) == 0 {
			t.Errorf("%s still wrote no relations after the batch was filed", node)
		}
		for _, r := range got {
			if r.target != led.entities["I_epic"] && r.target != led.entities["I_blocker"] {
				t.Errorf("%s: relation %+v names something outside the batch", node, r)
			}
		}
	}
	// A link no pass can resolve stays unresolved and stays counted: the child
	// blocks on an issue outside the batch, twice over — the add and its
	// removal — and on one in another repository. That costs one extra local
	// import per run and nothing else.
	if e, _ := Import(entity.SHA1, fixture["I_child"], led); e.Unresolved != 3 {
		t.Errorf("child reports %d unresolved links, want 3", e.Unresolved)
	}
}

func ofKind(rels []rel, kind string) []rel {
	var out []rel
	for _, r := range rels {
		if r.kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// relClock is the clock of the single relation event an entity carries.
func relClock(t *testing.T, e Entity) int64 {
	t.Helper()
	for _, ev := range e.Events() {
		if ev.Op == issue.RelAdd {
			return ev.C
		}
	}
	t.Fatal("no relation event")
	return 0
}

func messages(e Entity) string {
	var b strings.Builder
	for _, a := range e.Actions {
		b.WriteString(a.Message)
		b.WriteString("\n")
	}
	return b.String()
}

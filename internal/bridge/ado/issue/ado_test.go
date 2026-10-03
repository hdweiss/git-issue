package adoissue

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

// fixtureDir holds the recorded API responses and the blobs they must produce.
const fixtureDir = "../../../../testdata/ado"

// fetchFixture serves the recorded responses to a real client, so the test
// covers the API layer's unmarshalling as well as the mapping. Anything the
// two disagree about — a field name, a null, an identity spelled as a string
// rather than an object — shows up here rather than against a live server.
//
// The server is an on-prem URL, which needs no special support: an Azure
// DevOps Server lives on an arbitrary host, so httptest's is as good as any.
func fetchFixture(t *testing.T) (adoapi.Target, []adoapi.WorkItem) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Basic OnRlc3QtdG9rZW4="; got != want {
			t.Errorf("Authorization = %q, want a basic-auth PAT", got)
		}
		w.Header().Set("Content-Type", "application/json")

		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/wit/wiql"):
			w.Write(fixture(t, "wiql.json"))
		case strings.HasSuffix(path, "/wit/workitemsbatch"):
			w.Write(fixture(t, "batch.json"))
		default:
			if m := regexp.MustCompile(`/workItems/(\d+)/comments/(\d+)/versions$`).FindStringSubmatch(path); m != nil {
				w.Write(fixture(t, fmt.Sprintf("comment-versions-%s-%s.json", m[1], m[2])))
				return
			}
			if m := regexp.MustCompile(`/workItems/(\d+)/comments$`).FindStringSubmatch(path); m != nil {
				w.Write(fixture(t, "comments-"+m[1]+".json"))
				return
			}
			if m := regexp.MustCompile(`/workItems/(\d+)/updates$`).FindStringSubmatch(path); m != nil {
				if r.URL.Query().Get("$skip") != "0" {
					w.Write([]byte(`{"count":0,"value":[]}`))
					return
				}
				w.Write(fixture(t, "updates-"+m[1]+".json"))
				return
			}
			t.Errorf("unexpected request %s", path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	target, err := adoapi.ParseTarget(srv.URL + "/DefaultCollection/MyProj/_git/repo")
	if err != nil {
		t.Fatal(err)
	}
	items, err := adoapi.New("test-token").Fetch(target, adoapi.Filter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("fetched %d work items, want 2", len(items))
	}
	return target, items
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// importFixture is the target the fixture describes, pinned rather than taken
// from the test server: the entity ids derive from it, so a port that changes
// per run would change every golden blob.
func importFixture(t *testing.T) (adoapi.Target, []Entity) {
	t.Helper()
	_, items := fetchFixture(t)

	pinned := adoapi.Target{Base: "https://dev.azure.com", Collection: "contoso", Project: "MyProj"}
	out := make([]Entity, 0, len(items))
	for _, w := range items {
		e, err := Import(entity.SHA1, pinned, w, nil)
		if err != nil {
			t.Fatalf("importing %d: %v", w.ID, err)
		}
		out = append(out, e)
	}
	return pinned, out
}

func blobs(t *testing.T) map[string]string {
	t.Helper()
	_, entities := importFixture(t)
	out := map[string]string{}
	for _, e := range entities {
		out[e.ID] = string(e.Blob())
	}
	return out
}

// The import is pinned byte for byte. Entity ids are the hash of the create
// event's bytes, so any change here is a change to the identity of every work
// item this bridge will ever import — it must be deliberate.
func TestImportMatchesGolden(t *testing.T) {
	for id, body := range blobs(t) {
		name := "issue-" + id[:12] + ".txt"
		path := filepath.Join(fixtureDir, name)
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("no golden %s (id %s):\n%s", name, id, body)
			continue
		}
		if body != string(want) {
			t.Errorf("%s differs\n--- want ---\n%s\n--- got ---\n%s", name, want, body)
		}
	}
}

// Every event's id must be the git hash of its own canonical line, and the
// entity's id must be its create event's. That is what makes an entity id
// verifiable with stock git and reproducible by any other implementation.
func TestImportIsSelfVerifying(t *testing.T) {
	_, entities := importFixture(t)
	for _, e := range entities {
		for _, ev := range e.Events() {
			id, err := entity.HashLine(entity.SHA1, ev.Raw)
			if err != nil {
				t.Fatal(err)
			}
			if id != ev.ID {
				t.Errorf("event id %s is not the hash of its line %s", ev.ID, id)
			}
		}
		if e.Events()[0].Op != "create" || e.Events()[0].ID != e.ID {
			t.Errorf("%s: entity id is not the create event's id", e.ID)
		}
	}
}

// A second import must produce the same bytes, or every re-sync duplicates
// every entity. This is what deriving nonces and timestamps rather than
// generating them buys, and it is the single property most worth a test.
func TestImportIsIdempotent(t *testing.T) {
	first, second := blobs(t), blobs(t)
	if len(first) != len(second) {
		t.Fatalf("entity count changed between runs: %d then %d", len(first), len(second))
	}
	for id, body := range first {
		if second[id] != body {
			t.Errorf("%s differs on re-import\n--- first ---\n%s\n--- second ---\n%s", id, body, second[id])
		}
	}
}

// The load-bearing negative. A work item's area path is a fact about the work
// item in Azure DevOps, not about this entity — it is scope, not state — so no
// event, and therefore no byte of any blob, may mention it. The fixture files
// one work item under MyProj\Web\Auth precisely so this can be asserted.
func TestAreaNeverReachesTheBlob(t *testing.T) {
	_, items := fetchFixture(t)
	if items[0].AreaPath == "" {
		t.Fatal("the fixture no longer sets an area path, so this proves nothing")
	}

	for id, body := range blobs(t) {
		for _, forbidden := range []string{"AreaPath", "Web\\\\Auth", "Web/Auth", "ado.area"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s mentions %q:\n%s", id, forbidden, body)
			}
		}
	}
}

// The nonce derivations are published in docs/bridge-ado.md, and two bridges
// that hash different strings fork the identity of every work item they both
// import. This pins the exact input strings.
func TestNonceIsPublishedDerivation(t *testing.T) {
	target := adoapi.Target{Base: "https://dev.azure.com", Collection: "contoso", Project: "MyProj"}

	if got, want := Origin(target, 1234), "ado:dev.azure.com/contoso#1234"; got != want {
		t.Errorf("Origin = %q, want %q", got, want)
	}
	if got, want := CommentOrigin(target, 1234, 7), "ado:dev.azure.com/contoso#1234/comments/7"; got != want {
		t.Errorf("CommentOrigin = %q, want %q", got, want)
	}
	for _, input := range []string{
		"ado:dev.azure.com/contoso#1234",
		"ado:dev.azure.com/contoso#1234/comments/7",
		"ado:dev.azure.com/contoso#1234/revisions/2/title",
		"ado:dev.azure.com/contoso#1234:title",
	} {
		sum := sha256.Sum256([]byte(input))
		if want := hex.EncodeToString(sum[:])[:16]; Nonce(input) != want {
			t.Errorf("Nonce(%q) = %q, want %q", input, Nonce(input), want)
		}
	}
}

// The collection, not the project, is what identity is keyed on — so the same
// work item read through two projects of one collection is one entity.
func TestIdentityFollowsTheCollection(t *testing.T) {
	a := adoapi.Target{Base: "https://dev.azure.com", Collection: "contoso", Project: "MyProj"}
	b := adoapi.Target{Base: "https://dev.azure.com", Collection: "contoso", Project: "Moved"}
	if Origin(a, 1234) != Origin(b, 1234) {
		t.Errorf("a work item moved between projects changed identity: %s vs %s", Origin(a, 1234), Origin(b, 1234))
	}
}

// System.History is the comment text, surfaced a second way. The comments API
// is the authoritative view of it, so replaying the revision feed's copy would
// file every comment twice.
func TestHistoryFieldIsNotReplayed(t *testing.T) {
	for id, body := range blobs(t) {
		if strings.Contains(body, "must not be replayed") {
			t.Errorf("%s imported System.History as an event:\n%s", id, body)
		}
	}
}

// knows builds the origin ledger's answer for a few work items, which is what
// a relation needs: a link names an entity id, and no upstream id yields one
// without asking.
func knows(entities map[string]string) ledger {
	l := newLedger()
	l.entities = entities
	return l
}

// A work item's links import as relations, in both directions of the OR-Set:
// an add carries the kind and the target, and a removal names the add it
// retracts rather than the link's value.
//
// Only the reverse half of each directional family is mapped. Hierarchy-Forward
// is the same edge seen from the parent, Dependency-Forward the same edge seen
// from the blocker, Duplicate-Forward the same edge seen from the canonical —
// and docs/issues.md stores an edge once, on the dependent end, so importing
// them here would file the epic under its own child.
func TestLinksImportAsRelations(t *testing.T) {
	target := adoapi.Target{Base: "https://dev.azure.com", Collection: "contoso", Project: "MyProj"}
	url := func(id int) string {
		return "https://dev.azure.com/contoso/_apis/wit/workItems/" + strconv.Itoa(id)
	}
	known := knows(map[string]string{
		Origin(target, 10): "1111111111111111111111111111111111111111",
		Origin(target, 20): "2222222222222222222222222222222222222222",
		Origin(target, 30): "3333333333333333333333333333333333333333",
		Origin(target, 40): "4444444444444444444444444444444444444444",
		Origin(target, 50): "5555555555555555555555555555555555555555",
	})

	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	who := adoapi.Identity{UniqueName: "dev@example.com"}
	item := adoapi.WorkItem{
		ID: 5, Rev: 4, Type: "Bug", Title: "Anchor blobs are pruned by gc",
		State: "New", CreatedBy: who, CreatedAt: at, ChangedAt: at,
		Updates: []adoapi.Update{
			{Rev: 2, By: who, At: at.Add(time.Hour), RelationsAdded: []adoapi.Relation{
				{Rel: adoapi.RelParent, URL: url(10)},
				{Rel: adoapi.RelRelated, URL: url(20)},
				{Rel: adoapi.RelPredecessor, URL: url(40)},
				{Rel: adoapi.RelDuplicateOf, URL: url(50)},
				// The far halves of three edges this work item does not own, and
				// a link to a work item this repository does not hold.
				{Rel: adoapi.RelChild, URL: url(30)},
				{Rel: adoapi.RelSuccessor, URL: url(30)},
				{Rel: adoapi.RelDuplicate, URL: url(30)},
				{Rel: adoapi.RelParent, URL: url(99)},
			}},
			{Rev: 3, By: who, At: at.Add(2 * time.Hour), RelationsRemoved: []adoapi.Relation{
				{Rel: adoapi.RelRelated, URL: url(20)},
			}},
		},
	}

	e, err := Import(entity.SHA1, target, item, known)
	if err != nil {
		t.Fatal(err)
	}

	var adds, removes []entity.Event
	for _, ev := range e.Events() {
		switch ev.Op {
		case "rel.add":
			adds = append(adds, ev)
		case "rel.remove":
			removes = append(removes, ev)
		case "parent", "duplicate.of":
			t.Errorf("the import wrote %s, an op docs/issues.md no longer defines", ev.Op)
		}
	}

	want := []struct{ kind, target string }{
		{issue.KindParent, known.entities[Origin(target, 10)]},
		{issue.KindRelated, known.entities[Origin(target, 20)]},
		{issue.KindBlockedBy, known.entities[Origin(target, 40)]},
		{issue.KindDuplicate, known.entities[Origin(target, 50)]},
	}
	if len(adds) != len(want) {
		t.Fatalf("wrote %d rel.add events, want %d: one per link whose end this work item owns",
			len(adds), len(want))
	}
	for i, w := range want {
		if got := adds[i].Val.Display(); got != w.kind {
			t.Errorf("rel.add %d imported as kind %q, want %q", i, got, w.kind)
		}
		if adds[i].Ref != w.target {
			t.Errorf("rel.add %d points at %s, want %s", i, adds[i].Ref, w.target)
		}
	}

	if len(removes) != 1 {
		t.Fatalf("wrote %d rel.remove events, want 1", len(removes))
	}
	if removes[0].Ref != adds[1].ID {
		t.Errorf("the remove names %s, want the id of the add it retracts (%s)", removes[0].Ref, adds[1].ID)
	}
}

// The links a work item currently holds are imported even when no revision
// reports them, which is the case for one whose history is unavailable or does
// not go back far enough. They must not be imported twice when the replay
// already saw them, and a link the replay saw *retracted* must stay retracted.
func TestCurrentLinksAreReconciled(t *testing.T) {
	target := adoapi.Target{Base: "https://dev.azure.com", Collection: "contoso", Project: "MyProj"}
	url := func(id int) string {
		return "https://dev.azure.com/contoso/_apis/wit/workItems/" + strconv.Itoa(id)
	}
	known := knows(map[string]string{
		Origin(target, 10): "1111111111111111111111111111111111111111",
		Origin(target, 40): "4444444444444444444444444444444444444444",
	})
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	who := adoapi.Identity{UniqueName: "dev@example.com"}
	item := adoapi.WorkItem{
		ID: 5, Rev: 1, Type: "Bug", Title: "No history at all",
		State: "New", CreatedBy: who, CreatedAt: at, ChangedAt: at,
		Relations: []adoapi.Relation{
			{Rel: adoapi.RelParent, URL: url(10)},
			{Rel: adoapi.RelPredecessor, URL: url(40)},
		},
	}

	// What the folded entity says its links are, which is the only question
	// worth asking: two adds and a remove for one pair is still no link.
	links := func(e Entity) []issue.Relation {
		return issue.Relations(entity.Fold(vocab, e.Events()))
	}

	e, err := Import(entity.SHA1, target, item, known)
	if err != nil {
		t.Fatal(err)
	}
	if got := links(e); len(got) != 2 {
		t.Fatalf("a work item with no revisions imported %+v, want both of its links", got)
	}

	// The same links, this time reported by a revision as well: the replay wrote
	// them, so reconciliation must not write them again.
	item.Updates = []adoapi.Update{{Rev: 2, By: who, At: at.Add(time.Hour), RelationsAdded: item.Relations}}
	if e, err = Import(entity.SHA1, target, item, known); err != nil {
		t.Fatal(err)
	}
	adds := 0
	for _, ev := range e.Events() {
		if ev.Op == issue.RelAdd {
			adds++
		}
	}
	if adds != 2 {
		t.Errorf("wrote %d rel.add events, want 2: the replay already had both", adds)
	}
}

// A link the revisions added and then removed is gone, and reconciliation must
// not resurrect it from an array that no longer carries it either.
func TestARetractedLinkIsNotReconciledBack(t *testing.T) {
	target := adoapi.Target{Base: "https://dev.azure.com", Collection: "contoso", Project: "MyProj"}
	url := "https://dev.azure.com/contoso/_apis/wit/workItems/10"
	known := knows(map[string]string{Origin(target, 10): "1111111111111111111111111111111111111111"})
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	who := adoapi.Identity{UniqueName: "dev@example.com"}

	e, err := Import(entity.SHA1, target, adoapi.WorkItem{
		ID: 5, Rev: 3, Type: "Bug", Title: "Detached again",
		State: "New", CreatedBy: who, CreatedAt: at, ChangedAt: at,
		Updates: []adoapi.Update{
			{Rev: 2, By: who, At: at.Add(time.Hour), RelationsAdded: []adoapi.Relation{
				{Rel: adoapi.RelParent, URL: url},
			}},
			{Rev: 3, By: who, At: at.Add(2 * time.Hour), RelationsRemoved: []adoapi.Relation{
				{Rel: adoapi.RelParent, URL: url},
			}},
		},
	}, known)
	if err != nil {
		t.Fatal(err)
	}
	if got := issue.Relations(entity.Fold(vocab, e.Events())); len(got) != 0 {
		t.Errorf("the entity holds %+v after the link was removed upstream", got)
	}
}

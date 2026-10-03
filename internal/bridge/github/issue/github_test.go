package ghissue

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ghapi "github.com/hdweiss/git-issue/internal/bridge/github/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

// fixtureDir is the recorded GraphQL response and the blobs it must produce.
const fixtureDir = "../../../../testdata/github"

// fetchFixture serves the recorded response to a real client, so the test
// covers the query layer's unmarshalling as well as the mapping. Anything the
// two disagree about — a field name, a null, a union member — shows up here
// rather than against the live API.
func fetchFixture(t *testing.T) []ghapi.Issue {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(fixtureDir, "issues.json"))
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
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
	if len(issues) != 3 {
		t.Fatalf("fetched %d issues, want 3", len(issues))
	}
	return issues
}

// blobs maps each imported entity id to its note body.
func blobs(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, gh := range fetchFixture(t) {
		e, err := Import(entity.SHA1, gh, nil)
		if err != nil {
			t.Fatalf("importing %s: %v", gh.URL, err)
		}
		out[e.ID] = string(e.Blob())
	}
	return out
}

// The import is pinned byte for byte. Entity ids are the hash of the create
// event's bytes, so any change here is a change to the identity of every
// GitHub issue this bridge will ever import — it must be deliberate.
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
	for _, gh := range fetchFixture(t) {
		e, err := Import(entity.SHA1, gh, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range e.Events() {
			id, err := entity.HashLine(entity.SHA1, ev.Raw)
			if err != nil {
				t.Fatal(err)
			}
			if id != ev.ID {
				t.Errorf("%s: event id %s is not the hash of its line %s", gh.URL, ev.ID, id)
			}
		}
		if e.Events()[0].Op != "create" || e.Events()[0].ID != e.ID {
			t.Errorf("%s: entity id is not the create event's id", gh.URL)
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

// The folded state is what a reader actually sees, so the mapping is asserted
// through the fold rather than only against bytes.
func TestImportFoldsToExpectedState(t *testing.T) {
	// The bridge contributes no vocabulary of its own any more: where an issue
	// lives upstream is recorded on the origin ledger, not in the blob.
	vocab := issue.Vocabulary.With(Vocabulary)
	states := map[string]entity.State{}
	for _, gh := range fetchFixture(t) {
		e, err := Import(entity.SHA1, gh, nil)
		if err != nil {
			t.Fatal(err)
		}
		states[gh.URL] = entity.Fold(vocab, e.Events())
	}

	rich := states["https://github.com/hdweiss/git-issue/issues/12"]
	for _, tc := range []struct{ field, want string }{
		// The last rename wins, not the first, and not the create-time title.
		{"title", "Notes merge reorders lines permanently"},
		{"status", "closed"},
		{"status.reason", "completed"},
		{"milestone", "v1"},
		{"type", "bug"},
		{"lock.reason", "resolved"},
	} {
		if got := rich.Scalar(tc.field).Display(); got != tc.want {
			t.Errorf("issue 12 %s = %q, want %q", tc.field, got, tc.want)
		}
	}
	if !rich.Scalar("locked").Truthy() {
		t.Error("issue 12 should be locked")
	}
	// needs-triage was added and then removed; storage was added and stayed.
	// confirmed was never in the timeline and comes from reconciliation.
	assertSet(t, "issue 12 labels", rich.List("label"), "storage", "confirmed")
	assertSet(t, "issue 12 assignees", rich.List("assignee"), "hdweiss")
	if len(rich.Thread) != 2 {
		t.Errorf("issue 12 has %d comments, want 2", len(rich.Thread))
	}

	// A deleted account is GitHub's ghost, not an empty author.
	minimal := states["https://github.com/hdweiss/git-issue/issues/13"]
	if got := minimal.Create.A; got != "github:dependabot[bot]" {
		t.Errorf("issue 13 author = %q", got)
	}
	assertSet(t, "issue 13 labels", minimal.List("label"), "storage")
	// Open is the implied status, so an open issue imports with no status
	// event at all — which is what keeps a reconciled close from tying with it.
	if minimal.Scalar("status").Present {
		t.Error("an open issue should carry no status event")
	}
	if got := issue.Status(minimal); got != issue.StatusOpen {
		t.Errorf("issue 13 folds to status %q, want open", got)
	}
	if minimal.Scalar("description").Present {
		t.Error("issue 13 has an empty body and should carry no description event")
	}

	// No timeline at all: current state has to be reconstructed from what
	// GitHub reports as true now.
	transferred := states["https://github.com/hdweiss/git-issue/issues/14"]
	if got := transferred.Create.A; got != "github:ghost" {
		t.Errorf("issue 14 author = %q, want github:ghost", got)
	}
	for _, tc := range []struct{ field, want string }{
		{"status", "closed"},
		{"status.reason", "not_planned"},
		{"milestone", "v2"},
	} {
		if got := transferred.Scalar(tc.field).Display(); got != tc.want {
			t.Errorf("issue 14 %s = %q, want %q", tc.field, got, tc.want)
		}
	}
	assertSet(t, "issue 14 assignees", transferred.List("assignee"), "octocat")
}

// A label removed upstream must retract the specific addition it named, so a
// removal that arrives before its addition — which a set merge permits — still
// resolves correctly.
func TestLabelRemovalReferencesItsAddition(t *testing.T) {
	for _, gh := range fetchFixture(t) {
		e, err := Import(entity.SHA1, gh, nil)
		if err != nil {
			t.Fatal(err)
		}
		adds := map[string]string{}
		for _, ev := range e.Events() {
			if ev.Op == "label.add" {
				adds[ev.ID] = ev.Val.Str
			}
		}
		for _, ev := range e.Events() {
			if ev.Op != "label.remove" {
				continue
			}
			if ev.Ref == "" {
				t.Errorf("%s: label.remove carries no ref", gh.URL)
			} else if adds[ev.Ref] != "needs-triage" {
				t.Errorf("%s: label.remove points at %q, want the needs-triage addition", gh.URL, adds[ev.Ref])
			}
		}
	}
}

// Nonces are the published derivation, not an implementation detail: another
// bridge that spelled them differently would fork every entity's identity.
func TestNonceIsPublishedDerivation(t *testing.T) {
	// The first 16 hex characters of SHA-256("I_kwDOAbCdEf6AaAaA").
	if got, want := Nonce("I_kwDOAbCdEf6AaAaA"), sha256Prefix("I_kwDOAbCdEf6AaAaA"); got != want {
		t.Errorf("Nonce = %s, want %s", got, want)
	}
	if len(Nonce("x")) != 16 {
		t.Errorf("nonce is %d characters, want 16", len(Nonce("x")))
	}
}

func TestAuthorAndReason(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"hdweiss", "github:hdweiss"},
		{"dependabot[bot]", "github:dependabot[bot]"},
		{"", "github:ghost"},
	} {
		if got := Author(tc.in); got != tc.want {
			t.Errorf("Author(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"COMPLETED", "completed"},
		{"NOT_PLANNED", "not_planned"},
		{"DUPLICATE", "duplicate"},
		// A reopened issue has not reached a terminal status, so it has no
		// reason for having reached one.
		{"REOPENED", ""},
		{"", ""},
	} {
		if got := Reason(tc.in); got != tc.want {
			t.Errorf("Reason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func assertSet(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, v := range got {
		set[v] = true
	}
	if len(set) != len(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
		return
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("%s = %v, want %v", what, got, want)
			return
		}
	}
}

func sha256Prefix(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

func TestImportRejectsNothingSilently(t *testing.T) {
	// A blob must be a sequence of newline-terminated canonical lines: the
	// merge strategy is line-based, so a value that reached a second physical
	// line would interleave with another writer's on merge.
	for _, body := range blobs(t) {
		if !strings.HasSuffix(body, "\n") {
			t.Error("blob does not end in a newline")
		}
		for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
			if !entity.IsCanonical([]byte(line)) {
				t.Errorf("line is not canonical: %s", line)
			}
		}
	}
}

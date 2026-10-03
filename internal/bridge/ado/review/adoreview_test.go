package adoreview

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
	"github.com/hdweiss/git-issue/internal/entity"
)

// -update rewrites the golden blobs from the current import. Run it after a
// deliberate mapping change and read the diff.
var update = flag.Bool("update", false, "rewrite golden files")

const fixtureDir = "../../../../testdata/ado"

// fetchFixture serves the recorded pull request responses to a real client, so
// the test covers adoapi's unmarshalling as well as this package's mapping.
func fetchFixture(t *testing.T) (adoapi.Target, []adoapi.PullRequest) {
	t.Helper()

	fixture := func(name string) []byte {
		body, err := os.ReadFile(filepath.Join(fixtureDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	sub := regexp.MustCompile(`/pullRequests/(\d+)/([a-z]+)$`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(strings.ToLower(path), "/pullrequests"):
			w.Write(fixture("pr-list.json"))
		default:
			if m := sub.FindStringSubmatch(path); m != nil {
				name := "pr-" + m[1] + "-" + m[2] + ".json"
				if _, err := os.Stat(filepath.Join(fixtureDir, name)); err == nil {
					w.Write(fixture(name))
					return
				}
				w.Write([]byte(`{"value":[]}`))
				return
			}
			t.Errorf("unexpected request %s", path)
		}
	}))
	t.Cleanup(srv.Close)

	target, err := adoapi.ParseTarget(srv.URL + "/testorg/MyProj/_git/repo")
	if err != nil {
		t.Fatal(err)
	}
	pulls, err := adoapi.New("test-token").FetchPulls(target, adoapi.PullFilter{All: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pulls) != 3 {
		t.Fatalf("fetched %d pull requests, want 3", len(pulls))
	}
	return target, pulls
}

// importFixture pins the target rather than taking the test server's address:
// the entity ids derive from it, so a port that changes per run would change
// every golden blob.
func importFixture(t *testing.T) []Entity {
	t.Helper()
	_, pulls := fetchFixture(t)

	pinned := adoapi.Target{Base: "https://dev.azure.com", Collection: "testorg", Project: "MyProj", Repo: "repo"}
	out := make([]Entity, 0, len(pulls))
	for _, pr := range pulls {
		e, err := Import(entity.SHA1, pinned, pr, nil)
		if err != nil {
			t.Fatalf("importing %d: %v", pr.ID, err)
		}
		out = append(out, e)
	}
	return out
}

func blobs(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range importFixture(t) {
		out[e.ID] = string(e.Blob())
	}
	return out
}

// The import is pinned byte for byte: entity ids are the hash of the create
// event's bytes, so any change here changes the identity of every pull request
// this bridge will ever import.
func TestImportMatchesGolden(t *testing.T) {
	for id, body := range blobs(t) {
		name := "review-" + id[:12] + ".txt"
		path := filepath.Join(fixtureDir, name)
		if *update {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
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

// Every event's id is the git hash of its own canonical line, and the entity's
// id is its create event's.
func TestImportIsSelfVerifying(t *testing.T) {
	for _, e := range importFixture(t) {
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
// every review.
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

// A check is a fact about a commit and never reaches a review's blob.
func TestChecksNeverReachTheBlob(t *testing.T) {
	var withChecks int
	for _, e := range importFixture(t) {
		withChecks += len(e.Checks)
		body := string(e.Blob())
		for _, forbidden := range []string{"check.add", "quality-gate", "sonarqube"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s mentions %q:\n%s", e.ID, forbidden, body)
			}
		}
	}
	if withChecks == 0 {
		t.Fatal("the fixture no longer carries a status, so this proves nothing")
	}
}

// The anchor names the commit of the iteration the thread was left against, not
// wherever the branch has moved to since.
func TestAnchorUsesTheIterationCommit(t *testing.T) {
	for _, e := range importFixture(t) {
		body := string(e.Blob())
		if !strings.Contains(body, "comment.anchor") {
			continue
		}
		if !strings.Contains(body, "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d internal/issue/area.go 42-44") {
			t.Errorf("anchor is not the iteration-2 commit and range:\n%s", body)
		}
	}
}

// A vote of -5 ("waiting for the author") is not a position on the change, and
// a system comment is not a comment. Neither reaches the blob.
func TestSoftSignalsAreNotImported(t *testing.T) {
	all := strings.Join(mapValues(blobs(t)), "\n")
	if strings.Contains(all, "Bob voted 10") {
		t.Error("a system comment was imported as a comment")
	}
	// Carol's -10 is a request-changes; nobody in the fixture votes -5, and the
	// two approvals (10 and 5) are the only other verdicts.
	if got := strings.Count(all, "verdict.add"); got != 3 {
		t.Errorf("verdict.add count = %d, want 3 (approve, approve, request-changes)", got)
	}
}

func TestOriginIsAPublishedDerivation(t *testing.T) {
	target := adoapi.Target{Base: "https://dev.azure.com", Collection: "testorg", Project: "MyProj", Repo: "repo"}
	if got, want := target.PullRequestOrigin(22), "ado:dev.azure.com/testorg/repo/pullRequests/22"; got != want {
		t.Errorf("PullRequestOrigin = %q, want %q", got, want)
	}
	for _, input := range []string{
		"ado:dev.azure.com/testorg/repo/pullRequests/22",
		"ado:dev.azure.com/testorg/repo/pullRequests/22:title",
	} {
		sum := sha256.Sum256([]byte(input))
		if want := hex.EncodeToString(sum[:])[:16]; Nonce(input) != want {
			t.Errorf("Nonce(%q) = %q, want %q", input, Nonce(input), want)
		}
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

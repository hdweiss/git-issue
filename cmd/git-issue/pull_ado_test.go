package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// adoServer replays the recorded responses the mapping tests use.
//
// The target is an on-prem URL, which needs nothing test-only: an Azure DevOps
// Server lives on an arbitrary host under an arbitrary virtual directory, so
// httptest's address is a perfectly ordinary one. That is the same property
// GitHub Enterprise gives the GitHub test, arrived at from the other side.
func adoServer(t *testing.T) *httptest.Server {
	t.Helper()

	fixture := func(name string) []byte {
		body, err := os.ReadFile(filepath.Join("../../testdata/ado", name))
		if err != nil {
			t.Fatal(err)
		}
		return body
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/_apis/projects/"):
			w.Write([]byte(`{"name":"MyProj","id":"proj-guid"}`))
		case strings.HasSuffix(path, "/classificationnodes/areas"):
			w.Write([]byte(`{"name":"MyProj","children":[{"name":"Web","children":[{"name":"Auth"}]}]}`))
		case strings.HasSuffix(path, "/wit/wiql"):
			w.Write(fixture("wiql.json"))
		case strings.HasSuffix(path, "/wit/workitemsbatch"):
			w.Write(fixture("batch.json"))
		default:
			if m := regexp.MustCompile(`/workItems/(\d+)/comments/(\d+)/versions$`).FindStringSubmatch(path); m != nil {
				w.Write(fixture(fmt.Sprintf("comment-versions-%s-%s.json", m[1], m[2])))
				return
			}
			if m := regexp.MustCompile(`/workItems/(\d+)/comments$`).FindStringSubmatch(path); m != nil {
				w.Write(fixture("comments-" + m[1] + ".json"))
				return
			}
			if m := regexp.MustCompile(`/workItems/(\d+)/updates$`).FindStringSubmatch(path); m != nil {
				if r.URL.Query().Get("$skip") != "0" {
					w.Write([]byte(`{"count":0,"value":[]}`))
					return
				}
				w.Write(fixture("updates-" + m[1] + ".json"))
				return
			}
			t.Errorf("unexpected request %s", path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The whole bridge path from the command line: resolve the target and the
// area, fetch, map, union into the notes ref, report.
func TestPullADO(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)
	srv := adoServer(t)
	target := "ado:" + srv.URL + "/DefaultCollection/MyProj/_git/repo"

	got := gitIssueEnv(t, bin, bob, []string{"AZURE_DEVOPS_EXT_PAT=secret"}, "pull", target)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{
		"Login fails on the second attempt",
		"Remember the last used area",
		"2 issues changed, 2 added(+)",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("pull output missing %q\n%s", want, got.stdout)
		}
	}
	// A bridge import fetches no ref, so it has no git fetch ref-update line.
	if strings.Contains(got.stdout, "->") {
		t.Errorf("bridge pull printed a fetch ref-update line:\n%s", got.stdout)
	}

	// One commit per upstream action, as docs/storage-model.md asks: the work
	// item being filed, each revision, each comment. Not one per run, which
	// would collapse everyone's changes onto whoever ran the import.
	log := gitIn(t, bob, "log", "--oneline", "refs/notes/issues/open")
	// A revision is one action however many fields it changed, so the grammar
	// summarises a multi-field one as "Update" rather than inventing an order
	// among them.
	for _, want := range []string{
		`Create issue "Login fails"`,
		`Update "Login fails"`,
		`Comment on "Login fails on the second attempt"`,
		`Edit a comment on "Login fails on the second attempt"`,
		`Retract a comment on "Login fails on the second attempt"`,
		`Set status "Resolved" on "Login fails on the second attempt"`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing subject %q:\n%s", want, log)
		}
	}

	// Attribution survives: each commit is authored by whoever did the thing,
	// not by whoever ran the import.
	authors := gitIn(t, bob, "log", "--format=%an <%ae>", "refs/notes/issues/open")
	for _, want := range []string{"Ann Poe <ann.poe@corp.example>", "Jane Roe <Jane.Roe@corp.example>"} {
		if !strings.Contains(authors, want) {
			t.Errorf("commit authors missing %q:\n%s", want, authors)
		}
	}

	// The states are Azure DevOps' own, verbatim. Mapping them onto open and
	// closed across bridges is deliberately deferred; see docs/bridge-ado.md.
	show := gitIssue(t, bin, bob, "list", "--state", "Resolved")
	if !strings.Contains(show.stdout, "Login fails on the second attempt") {
		t.Errorf("--state Resolved did not match the raw upstream state:\n%s", show.stdout)
	}

	// Nothing anywhere mentions the area the work item is filed under.
	blobs := gitIn(t, bob, "log", "-p", "refs/notes/issues/open")
	for _, forbidden := range []string{"AreaPath", "Web/Auth", `Web\Auth`, "ado.area"} {
		if strings.Contains(blobs, forbidden) {
			t.Errorf("the notes ref mentions %q, which is scope and not state", forbidden)
		}
	}
}

// A second pull must find nothing to do. This is the property the whole nonce
// derivation exists for, and the cheapest place to notice losing it.
func TestPullADOConverges(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)
	srv := adoServer(t)
	target := "ado:" + srv.URL + "/DefaultCollection/MyProj/_git/repo"
	env := []string{"AZURE_DEVOPS_EXT_PAT=secret"}

	if got := gitIssueEnv(t, bin, bob, env, "pull", target); got.code != 0 {
		t.Fatalf("first pull: exit %d: %s", got.code, got.stderr)
	}
	before := gitIn(t, bob, "rev-parse", "refs/notes/issues/open")

	// --full, so the watermark cannot be what makes the second run quiet.
	got := gitIssueEnv(t, bin, bob, env, "pull", "--full", target)
	if got.code != 0 {
		t.Fatalf("second pull: exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "Already up to date") {
		t.Errorf("a re-import was not a no-op:\n%s", got.stdout)
	}
	if after := gitIn(t, bob, "rev-parse", "refs/notes/issues/open"); after != before {
		t.Errorf("re-import moved the notes ref: %s -> %s", before, after)
	}
}

// An area written on the command line scopes the query and is not saved: it is
// a question, not a checkpoint, the same rule --since follows.
func TestPullADOAreaIsNotPersisted(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)

	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/_apis/projects/"):
			w.Write([]byte(`{"name":"MyProj"}`))
		case strings.HasSuffix(r.URL.Path, "/wit/wiql"):
			body := make([]byte, r.ContentLength)
			r.Body.Read(body)
			queries = append(queries, string(body))
			w.Write([]byte(`{"workItems":[]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	target := "ado:" + srv.URL + "/DefaultCollection/MyProj/_git/repo"

	got := gitIssueEnv(t, bin, bob, []string{"AZURE_DEVOPS_EXT_PAT=secret"}, "pull", target+"#Web/Auth")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if len(queries) != 1 {
		t.Fatalf("got %d queries, want 1", len(queries))
	}
	// An area always means its subtree. There is no exact-node form, so UNDER
	// is the only operator an area can produce. The separators are doubled
	// because the query is inside a JSON string.
	if !strings.Contains(queries[0], `[System.AreaPath] UNDER 'MyProj\\Web\\Auth'`) {
		t.Errorf("query is not scoped to the area's subtree:\n%s", queries[0])
	}

	config := gitIn(t, bob, "config", "--local", "--list")
	if strings.Contains(config, ".area=") {
		t.Errorf("an area given on the command line was saved:\n%s", config)
	}
}

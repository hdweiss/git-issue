package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// fakeGitHub serves the recorded GraphQL response to whatever asks, which is
// enough to drive the whole import: the command builds a real client, the
// client runs its real query, and only the transport is a fixture.
//
// The one query answered differently is nodes(ids:), which a push's plan makes
// to read current upstream state. It is served from the same fixture, re-shaped
// into the response that query expects — so a plan sees exactly what the import
// saw, and any difference it reports is a local change rather than an artefact
// of two fixtures disagreeing.
//
// Mutations are refused outright. Everything here is a read, and a test that
// silently accepted a write would stop telling us whether --dry-run writes.
func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile("../../testdata/github/pulls.json")
	if err != nil {
		t.Fatal(err)
	}

	var fixture struct {
		Data struct {
			Repository struct {
				PullRequests struct {
					Nodes []json.RawMessage `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	byID := map[string]json.RawMessage{}
	for _, n := range fixture.Data.Repository.PullRequests.Nodes {
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
			Query     string `json:"query"`
			Variables struct {
				IDs []string `json:"ids"`
			} `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(req.Query, "mutation"):
			json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{
				{"message": "fake github: this test serves reads only"}}})
		case strings.Contains(req.Query, "nodes(ids:"):
			nodes := make([]json.RawMessage, 0, len(req.Variables.IDs))
			for _, id := range req.Variables.IDs {
				nodes = append(nodes, byID[id])
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": nodes}})
		default:
			w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// pullFixture runs an import of the fixture into a fresh repository.
func pullFixture(t *testing.T, bin, dir string, args ...string) result {
	t.Helper()
	return pullFrom(t, bin, dir, fakeGitHub(t), args...)
}

// pullFrom is pullFixture against a server the caller already has.
//
// A push has to reach the same one: the tracker a ledger is keyed by includes
// the host, and a second httptest server listens on a second port — so two
// servers would be two trackers, and the push would find nothing recorded.
func pullFrom(t *testing.T, bin, dir string, srv *httptest.Server, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"pull", "--token", "test-token",
		"github:" + srv.URL + "/hdweiss/git-issue"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"TZ=Europe/Berlin", "GIT_EDITOR=", "VISUAL=", "EDITOR=",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("pull: %v\n%s%s", err, out.String(), errb.String())
		}
		code = exit.ExitCode()
	}
	if code != 0 {
		t.Fatalf("pull: exit %d\n%s%s", code, out.String(), errb.String())
	}
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

func TestPullImportsPullRequests(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)

	r := pullFixture(t, bin, dir)
	if !strings.Contains(r.stdout, "2 reviews changed") {
		t.Errorf("pull did not report the import:\n%s%s", r.stdout, r.stderr)
	}

	out := mustRun(t, bin, dir, "list", "--no-tree")
	for _, want := range []string{"Fix the area subtree walk", "Retire the old walker"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing is missing %q:\n%s", want, out)
		}
	}
	// merged and draft are distinct from open and closed, and the listing says
	// which in words when piped.
	if !strings.Contains(out, "merged") || !strings.Contains(out, "draft") {
		t.Errorf("listing does not show the lifecycle:\n%s", out)
	}
}

// A bridge import writes check runs to their own ref, keyed by commit, and
// never into a review's blob.
func TestPullWritesChecksToTheirOwnRef(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	pullFixture(t, bin, dir)

	head := "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d"
	out := mustRun(t, bin, dir, "checks", "--commit", head)
	if !strings.Contains(out, "sonarqube/quality-gate") || !strings.Contains(out, "fail") {
		t.Errorf("checks did not land:\n%s", out)
	}
	if !strings.Contains(out, "GitHub-Actions/build") {
		t.Errorf("the check run is not qualified by its app:\n%s", out)
	}

	// On its own ref, and not on the reviews ref.
	if !refExists(t, dir, "refs/notes/checks/runs") {
		t.Error("the checks ref should exist")
	}
	blob := gitOut(t, dir, "cat-file", "-p", "refs/notes/reviews/open^{tree}")
	if strings.Contains(blob, "check") {
		t.Error("no check may reach the reviews ref")
	}
}

// The whole point of `status` is that it answers from what the clone holds, so
// an imported review reports its upstream state without a second request.
func TestStatusOfAnImportedReview(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	pullFixture(t, bin, dir)

	id := ""
	for _, line := range strings.Split(mustRun(t, bin, dir, "list", "--no-tree"), "\n") {
		if strings.Contains(line, "Fix the area subtree walk") {
			id = strings.Fields(line)[0]
		}
	}
	if id == "" {
		t.Fatal("the imported review is not in the listing")
	}

	r := gitReview(t, bin, dir, "status", id)
	if r.code != 1 {
		t.Fatalf("the failing quality gate should block: exit %d\n%s", r.code, r.stdout)
	}
	if !strings.Contains(r.stdout, "check failing") {
		t.Errorf("status should name the failing check:\n%s", r.stdout)
	}
	// The approval is against the current head and counts; the earlier request
	// for changes was superseded rather than retracted.
	if !strings.Contains(r.stdout, "1 approving, 0 requesting changes") {
		t.Errorf("verdicts did not resolve per author:\n%s", r.stdout)
	}

	out := mustRun(t, bin, dir, "show", id)
	if !strings.Contains(out, "internal/issue/area.go:42") {
		t.Errorf("the review thread's anchor is missing:\n%s", out)
	}
	if !strings.Contains(out, "resolved") {
		t.Errorf("the thread is resolved upstream:\n%s", out)
	}
	if !strings.Contains(out, "contributor/fix-area-walk") {
		t.Errorf("the head should name the fork that holds it:\n%s", out)
	}
}

// A re-import adds nothing: every nonce derives from stable upstream identity,
// so the second run writes byte-identical events and the ref does not move.
func TestPullIsIdempotent(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	pullFixture(t, bin, dir)
	before := gitOut(t, dir, "rev-parse", "refs/notes/reviews/open")

	// --full, because the watermark would otherwise make the second run a
	// no-op for a reason other than the one under test.
	r := pullFixture(t, bin, dir, "--full")
	after := gitOut(t, dir, "rev-parse", "refs/notes/reviews/open")

	if before != after {
		t.Errorf("a re-import moved the ref: %s -> %s\n%s", before[:8], after[:8], r.stdout)
	}
	if !strings.Contains(r.stdout, "Already up to date.") {
		t.Errorf("a re-import should say so:\n%s", r.stdout)
	}
}

// The ledger records where each review lives upstream, on its own ref, so a
// later import converges on the review already here.
func TestPullRecordsTheLedger(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	pullFixture(t, bin, dir)

	if !refExists(t, dir, "refs/git-issue/origins") {
		t.Fatal("the origin ledger should exist")
	}
	tree := gitOut(t, dir, "ls-tree", "-r", "--name-only", "refs/git-issue/origins")
	if !strings.Contains(tree, "hdweiss/git-issue") {
		t.Errorf("the ledger is not keyed by tracker:\n%s", tree)
	}
	// A review line, and a thread line for the review thread's root.
	blob := gitOut(t, dir, "cat-file", "-p", "refs/git-issue/origins:"+firstLine(tree))
	for _, want := range []string{"review ", "thread ", "url "} {
		if !strings.Contains(blob, want) {
			t.Errorf("the ledger has no %q line:\n%s", want, blob)
		}
	}
}

func firstLine(s string) string {
	return strings.SplitN(strings.TrimSpace(s), "\n", 2)[0]
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func refExists(t *testing.T, dir, ref string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = dir
	return cmd.Run() == nil
}

// A remote that carries no reviews ref is told apart from a remote that does not
// exist, and the message says which ref was missing rather than leaving somebody
// to work out what "this ref" meant.
func TestPullGitSaysWhichRefIsMissing(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	bareRemote(t, dir)

	r := gitReview(t, bin, dir, "pull", "origin")
	if r.code == 0 {
		t.Fatalf("a remote with no reviews ref has nothing to give:\n%s", r.stdout)
	}
	for _, want := range []string{"refs/notes/reviews/open", "nothing has been pushed there yet"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("the message is missing %q:\n%s", want, r.stderr)
		}
	}
}

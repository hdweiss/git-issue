package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeADO serves the recorded pull request responses to a real client: the
// command builds a real adoapi.Client, the client runs its real requests, and
// only the transport is a fixture. A sub-resource with no fixture answers with
// an empty list, which is what a pull request that has no threads or statuses
// really returns.
//
// Writes are refused. Everything a pull does is a read; a test that silently
// accepted a mutation would stop telling us the pull writes nothing upstream.
func fakeADO(t *testing.T) *httptest.Server {
	t.Helper()

	fixture := func(name string) []byte {
		body, err := os.ReadFile(filepath.Join("../../testdata/ado", name))
		if err != nil {
			t.Fatalf("fixture %s: %v", name, err)
		}
		return body
	}

	sub := regexp.MustCompile(`/pullRequests/(\d+)/([a-z]+)$`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "fake ado: reads only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		switch {
		case strings.Contains(path, "/_apis/projects/"):
			w.Write([]byte(`{"name":"MyProj"}`))
		case strings.HasSuffix(strings.ToLower(path), "/pullrequests"):
			w.Write(fixture("pr-list.json"))
		default:
			if m := sub.FindStringSubmatch(path); m != nil {
				name := "pr-" + m[1] + "-" + m[2] + ".json"
				if _, err := os.Stat(filepath.Join("../../testdata/ado", name)); err == nil {
					w.Write(fixture(name))
					return
				}
				w.Write([]byte(`{"value":[]}`))
				return
			}
			t.Errorf("fake ado: unexpected request %s", path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// pullADOFixture imports the recorded pull requests into a fresh repository.
func pullADOFixture(t *testing.T, bin, dir string, args ...string) result {
	t.Helper()
	return pullADOFrom(t, bin, dir, fakeADO(t), args...)
}

// pullADOFrom is pullADOFixture against a server the caller already has. A
// re-import has to reach the same one: the tracker a ledger and a watermark are
// keyed by includes the host, and a second httptest server listens on a second
// port.
func pullADOFrom(t *testing.T, bin, dir string, srv *httptest.Server, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"pull", "--token", "test-token",
		"ado:" + srv.URL + "/testorg/MyProj/_git/repo"}, args...)...)
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
		t.Fatalf("pull ado: exit %d\n%s%s", code, out.String(), errb.String())
	}
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

func TestPullADOImportsPullRequests(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)

	r := pullADOFixture(t, bin, dir)
	if !strings.Contains(r.stdout, "3 reviews changed") {
		t.Errorf("pull did not report the import:\n%s%s", r.stdout, r.stderr)
	}

	out := mustRun(t, bin, dir, "list", "--no-tree")
	for _, want := range []string{"Fix the area subtree walk", "Retire the old walker", "WIP: async walk"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing is missing %q:\n%s", want, out)
		}
	}
	// A completed pull request is terminal, and a draft is distinct from an
	// ordinary open one; the listing says which in words when piped.
	if !strings.Contains(out, "Completed") || !strings.Contains(out, "draft") {
		t.Errorf("listing does not show the lifecycle:\n%s", out)
	}
}

// A bridge import writes check runs to their own ref, keyed by commit, and never
// into a review's blob.
func TestPullADOWritesChecksToTheirOwnRef(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	pullADOFixture(t, bin, dir)

	head := "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d"
	out := mustRun(t, bin, dir, "checks", "--commit", head)
	if !strings.Contains(out, "sonarqube/quality-gate") || !strings.Contains(out, "fail") {
		t.Errorf("checks did not land:\n%s", out)
	}

	if !refExists(t, dir, "refs/notes/checks/runs") {
		t.Error("the checks ref should exist")
	}
	blob := gitOut(t, dir, "cat-file", "-p", "refs/notes/reviews/open^{tree}")
	if strings.Contains(blob, "check") {
		t.Error("no check may reach the reviews ref")
	}
}

// status answers from what the clone holds: an imported review reports its
// verdicts, threads and checks without a second request.
func TestPullADOStatusOfAnImportedReview(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	pullADOFixture(t, bin, dir)

	id := listID(t, bin, dir, "Fix the area subtree walk")
	r := gitReview(t, bin, dir, "status", id)
	if r.code != 1 {
		t.Fatalf("the failing quality gate should block: exit %d\n%s", r.code, r.stdout)
	}
	if !strings.Contains(r.stdout, "1 approving, 1 requesting changes") {
		t.Errorf("votes did not become verdicts:\n%s", r.stdout)
	}

	out := mustRun(t, bin, dir, "show", id)
	if !strings.Contains(out, "internal/issue/area.go:42") {
		t.Errorf("the review thread's anchor is missing:\n%s", out)
	}
	if !strings.Contains(out, "resolved") {
		t.Errorf("the fixed thread should import as resolved:\n%s", out)
	}
}

// A fork's head names the repository that holds the branch, so a reader can
// fetch it.
func TestPullADOForkHeadNamesTheFork(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	pullADOFixture(t, bin, dir)

	out := mustRun(t, bin, dir, "show", listID(t, bin, dir, "Retire the old walker"))
	if !strings.Contains(out, "repo-fork/retire-walker") {
		t.Errorf("the head should name the fork that holds it:\n%s", out)
	}
}

// A re-import adds nothing: every nonce derives from stable upstream identity,
// so the second run writes byte-identical events and the ref does not move.
func TestPullADOIsIdempotent(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	srv := fakeADO(t)
	pullADOFrom(t, bin, dir, srv)
	before := gitOut(t, dir, "rev-parse", "refs/notes/reviews/open")

	r := pullADOFrom(t, bin, dir, srv, "--full")
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
func TestPullADORecordsTheLedger(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	pullADOFixture(t, bin, dir)

	if !refExists(t, dir, "refs/git-issue/origins") {
		t.Fatal("the origin ledger should exist")
	}
	tree := gitOut(t, dir, "ls-tree", "-r", "--name-only", "refs/git-issue/origins")
	if !strings.Contains(tree, "testorg/MyProj") {
		t.Errorf("the ledger is not keyed by tracker:\n%s", tree)
	}
	blob := gitOut(t, dir, "cat-file", "-p", "refs/git-issue/origins:"+firstLine(tree))
	for _, want := range []string{"review ", "thread ", "url "} {
		if !strings.Contains(blob, want) {
			t.Errorf("the ledger has no %q line:\n%s", want, blob)
		}
	}
}

func listID(t *testing.T, bin, dir, title string) string {
	t.Helper()
	for _, line := range strings.Split(mustRun(t, bin, dir, "list", "--no-tree"), "\n") {
		if strings.Contains(line, title) {
			return strings.Fields(line)[0]
		}
	}
	t.Fatalf("no review titled %q in the listing", title)
	return ""
}

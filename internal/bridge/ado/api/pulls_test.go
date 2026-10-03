package adoapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func pullServer(t *testing.T) (Target, *Client) {
	t.Helper()
	fixture := func(name string) []byte {
		body, err := os.ReadFile(filepath.Join("../../../../testdata/ado", name))
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	sub := regexp.MustCompile(`/pullRequests/(\d+)/([a-z]+)$`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(strings.ToLower(path), "/pullrequests"):
			w.Write(fixture("pr-list.json"))
		default:
			if m := sub.FindStringSubmatch(path); m != nil {
				name := "pr-" + m[1] + "-" + m[2] + ".json"
				if _, err := os.Stat(filepath.Join("../../../../testdata/ado", name)); err == nil {
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

	target, err := ParseTarget(srv.URL + "/testorg/MyProj/_git/repo")
	if err != nil {
		t.Fatal(err)
	}
	return target, New("test-token")
}

func TestFetchPulls(t *testing.T) {
	target, client := pullServer(t)
	pulls, err := client.FetchPulls(target, PullFilter{All: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pulls) != 3 {
		t.Fatalf("got %d pull requests, want 3", len(pulls))
	}

	byID := map[int]PullRequest{}
	for _, pr := range pulls {
		byID[pr.ID] = pr
	}

	pr := byID[22]
	if pr.SourceRef != "fix-area-walk" || pr.TargetRef != "main" {
		t.Errorf("refs not stripped of refs/heads/: %q -> %q", pr.SourceRef, pr.TargetRef)
	}
	if pr.HeadCommit != "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d" {
		t.Errorf("HeadCommit = %q", pr.HeadCommit)
	}
	if len(pr.Reviewers) != 2 || pr.Reviewers[0].Vote != 10 || pr.Reviewers[1].Vote != -10 {
		t.Errorf("reviewers/votes not read: %+v", pr.Reviewers)
	}
	if pr.Reviewers[0].Identity.UniqueName != "bob@example.com" {
		t.Errorf("reviewer identity not read: %+v", pr.Reviewers[0].Identity)
	}
	if len(pr.Labels) != 1 || pr.Labels[0] != "bug" {
		t.Errorf("labels = %v", pr.Labels)
	}

	// The anchored thread carries a file context and an iteration that resolves
	// to a commit.
	var anchored PRThread
	for _, th := range pr.Threads {
		if th.FilePath != "" {
			anchored = th
		}
	}
	if anchored.FilePath != "/internal/issue/area.go" || anchored.RightStart != 42 || anchored.RightEnd != 44 {
		t.Errorf("thread context not read: %+v", anchored)
	}
	if anchored.IterationID != 2 {
		t.Errorf("iteration id = %d, want 2", anchored.IterationID)
	}
	if sha, ok := pr.Iteration(anchored.IterationID); !ok || sha != "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d" {
		t.Errorf("iteration 2 -> %q (%v)", sha, ok)
	}
	if len(pr.WorkItems) != 1 || pr.WorkItems[0] != 42 {
		t.Errorf("linked work items = %v", pr.WorkItems)
	}
	if len(pr.Statuses) != 1 || pr.Statuses[0].State != "failed" || pr.Statuses[0].Genre != "sonarqube" {
		t.Errorf("statuses = %+v", pr.Statuses)
	}

	// A completed pull request from a fork.
	if byID[23].Status != "completed" || byID[23].ForkRepo != "repo-fork" {
		t.Errorf("fork/status not read: %+v", byID[23])
	}
	if byID[23].ClosedAt.IsZero() {
		t.Error("a completed pull request has a closed date")
	}
	if !byID[24].IsDraft {
		t.Error("pr 24 is a draft")
	}
}

// The age filter drops a pull request closed before the cutoff without fetching
// its feeds, and keeps an active one whatever its creation date.
func TestFetchPullsSinceSkipsOldClosed(t *testing.T) {
	target, client := pullServer(t)
	since := time.Date(2024, 4, 15, 0, 0, 0, 0, time.UTC)
	pulls, err := client.FetchPulls(target, PullFilter{All: true, Since: since}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range pulls {
		if pr.ID == 23 {
			t.Errorf("pull request 23 closed 2024-04-03 should have been filtered out by --since %s", since)
		}
	}
	if len(pulls) != 2 {
		t.Errorf("got %d pull requests, want 2 (22 active, 24 active)", len(pulls))
	}
}

package adoapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capture is one request the client made, kept so a test can assert its shape.
type capture struct {
	method string
	path   string
	query  string
	body   map[string]any
}

func mutationServer(t *testing.T, reply func(path string) string) (Target, *Client, *[]capture) {
	t.Helper()
	var seen []capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c := capture{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &c.body); err != nil {
				var arr []any
				if json.Unmarshal(raw, &arr) != nil {
					t.Errorf("body is neither object nor array: %s", raw)
				}
			}
		}
		seen = append(seen, c)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reply(r.URL.Path)))
	}))
	t.Cleanup(srv.Close)

	target, err := ParseTarget(srv.URL + "/DefaultCollection/MyProj/_git/repo")
	if err != nil {
		t.Fatal(err)
	}
	return target, New("test-token"), &seen
}

func TestCreatePullShapesTheRequest(t *testing.T) {
	target, client, seen := mutationServer(t, func(string) string {
		return `{"pullRequestId":7,"_links":{"web":{"href":"https://example/pr/7"}}}`
	})

	got, err := client.CreatePull(target, NewPullInput{
		SourceRef: "feature/x", TargetRef: "main", Title: "T", Description: "D", Draft: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 7 || got.URL != "https://example/pr/7" {
		t.Errorf("create returned %+v", got)
	}

	c := (*seen)[0]
	if c.method != http.MethodPost || !strings.HasSuffix(c.path, "/_apis/git/repositories/repo/pullrequests") {
		t.Errorf("wrong endpoint: %s %s", c.method, c.path)
	}
	if c.body["sourceRefName"] != "refs/heads/feature/x" || c.body["targetRefName"] != "refs/heads/main" {
		t.Errorf("branch names not qualified: %+v", c.body)
	}
	if c.body["isDraft"] != true {
		t.Errorf("draft not sent: %+v", c.body)
	}
}

func TestPullThreadContextForAnAnchoredComment(t *testing.T) {
	target, client, seen := mutationServer(t, func(string) string {
		return `{"id":3,"comments":[{"id":11}]}`
	})

	tid, cid, err := client.AddPullThread(target, 7, NewThreadInput{
		Body: "off by one", Path: "internal/area.go", First: 40, Last: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tid != 3 || cid != 11 {
		t.Errorf("thread/comment ids: %d/%d", tid, cid)
	}

	ctx, _ := (*seen)[0].body["threadContext"].(map[string]any)
	if ctx["filePath"] != "/internal/area.go" {
		t.Errorf("file path not rooted: %+v", ctx)
	}
	start, _ := ctx["rightFileStart"].(map[string]any)
	end, _ := ctx["rightFileEnd"].(map[string]any)
	if start["line"] != float64(40) || end["line"] != float64(42) {
		t.Errorf("line range not carried: %+v", ctx)
	}
}

func TestSetReviewerVoteAddressesTheIdentity(t *testing.T) {
	target, client, seen := mutationServer(t, func(string) string { return `{"vote":10}` })

	if err := client.SetReviewerVote(target, 7, "self-guid", 10); err != nil {
		t.Fatal(err)
	}
	c := (*seen)[0]
	if c.method != http.MethodPut || !strings.HasSuffix(c.path, "/pullRequests/7/reviewers/self-guid") {
		t.Errorf("wrong endpoint: %s %s", c.method, c.path)
	}
	if c.body["vote"] != float64(10) {
		t.Errorf("vote not sent: %+v", c.body)
	}

	if err := client.SetReviewerVote(target, 7, "", 10); err == nil {
		t.Error("an unknown identity should be refused")
	}
}

func TestConnectionDataReadsTheAuthenticatedUser(t *testing.T) {
	target, client, _ := mutationServer(t, func(string) string {
		return `{"authenticatedUser":{"id":"g","providerDisplayName":"Bot","properties":{"Account":{"$value":"bot@example.com"}}}}`
	})
	who, err := client.ConnectionData(target)
	if err != nil {
		t.Fatal(err)
	}
	if who.ID != "g" || who.UniqueName != "bot@example.com" || who.EventAuthor() != "ado:bot@example.com" {
		t.Errorf("identity: %+v", who)
	}
}

func TestDefaultBranchIsBare(t *testing.T) {
	target, client, _ := mutationServer(t, func(string) string {
		return `{"defaultBranch":"refs/heads/trunk"}`
	})
	branch, err := client.DefaultBranch(target)
	if err != nil {
		t.Fatal(err)
	}
	if branch != "trunk" {
		t.Errorf("default branch %q, want trunk", branch)
	}
}

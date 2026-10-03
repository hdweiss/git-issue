package ghapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// pullServer stands in for GitHub over one import of `pulls` pull requests: the
// identity walk, then the detail batches. It records the size of every batch it
// was asked for, and fail decides which of them come back as a 502.
func pullServer(t *testing.T, pulls int, fail func(request, size int) bool) (*httptest.Server, *[]int, *[]string) {
	t.Helper()
	var (
		sizes   []int
		notices []string
		mu      sync.Mutex
		request int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string `json:"query"`
			Variables struct {
				IDs []string `json:"ids"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}

		// The identity walk: every pull request, by id, in one page.
		if !strings.Contains(req.Query, "nodes(ids:") {
			nodes := make([]string, 0, pulls)
			for i := 0; i < pulls; i++ {
				nodes = append(nodes, fmt.Sprintf(`{"id":"PR%d","updatedAt":"2026-01-01T00:00:00Z"}`, i))
			}
			fmt.Fprintf(w, `{"data":{"repository":{"pullRequests":{"totalCount":%d,"pageInfo":{"hasNextPage":false},"nodes":[%s]}}}}`,
				pulls, strings.Join(nodes, ","))
			return
		}

		mu.Lock()
		request++
		n := request
		sizes = append(sizes, len(req.Variables.IDs))
		mu.Unlock()

		if fail(n, len(req.Variables.IDs)) {
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `{"errors":[{"message":"Something went wrong while executing your query. This may be the result of a timeout"}]}`)
			return
		}
		nodes := make([]string, 0, len(req.Variables.IDs))
		for _, id := range req.Variables.IDs {
			nodes = append(nodes, fmt.Sprintf(`{"id":%q,"number":1,"title":"t","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z","state":"OPEN"}`, id))
		}
		fmt.Fprintf(w, `{"data":{"nodes":[%s]}}`, strings.Join(nodes, ","))
	}))
	t.Cleanup(srv.Close)
	return srv, &sizes, &notices
}

// batchSizes is the sizes asked for, with consecutive repeats collapsed, which
// is what a shrink looks like from outside.
func batchSizes(sizes []int) []int {
	var out []int
	for _, s := range sizes {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

func testClient(t *testing.T, srv *httptest.Server, notices *[]string) *Client {
	t.Helper()
	c := New(srv.URL, "t")
	c.Sleep = func(time.Duration) {}
	c.Notice = func(s string) { *notices = append(*notices, s) }
	return c
}

// GitHub answers a query it could not finish in time with a 502, and often the
// very next attempt at the same query succeeds. Failing the whole import on the
// first one would make a large repository unreadable for no reason.
func TestTransient502IsRetried(t *testing.T) {
	srv, sizes, notices := pullServer(t, pullDetailBatch, func(request, size int) bool { return request == 1 })
	client := testClient(t, srv, notices)

	pulls, err := client.FetchPulls(Target{Host: PublicHost, Owner: "o", Name: "n"}, Filter{}, nil)
	if err != nil {
		t.Fatalf("a 502 that clears on the retry should not fail the import: %v", err)
	}
	if len(pulls) != pullDetailBatch {
		t.Errorf("%d pull requests imported, want %d", len(pulls), pullDetailBatch)
	}
	// Retried as it was: nothing about a transient failure says the batch was
	// too big, so it is not split.
	if got := batchSizes(*sizes); len(got) != 1 || got[0] != pullDetailBatch {
		t.Errorf("batch sizes went %v, want one batch of %d", got, pullDetailBatch)
	}
	if len(*notices) != 0 {
		t.Errorf("a retry that worked is not worth a notice: %v", *notices)
	}
}

// A repository whose batches GitHub will not finish however many times they are
// asked for is read in smaller ones. The same pull requests arrive, in more
// requests, which beats an import that cannot run at all.
func TestPersistent502SplitsTheBatch(t *testing.T) {
	// Anything above two pull requests at once is refused however often it is
	// asked for — the shape of a repository whose reviews are simply heavy.
	srv, sizes, notices := pullServer(t, pullDetailBatch, func(request, size int) bool { return size > 2 })
	client := testClient(t, srv, notices)

	pulls, err := client.FetchPulls(Target{Host: PublicHost, Owner: "o", Name: "n"}, Filter{}, nil)
	if err != nil {
		t.Fatalf("the import should have got through on a smaller batch: %v", err)
	}
	// Nothing is lost by splitting: every pull request the walk named is here,
	// in the order it named them.
	if len(pulls) != pullDetailBatch {
		t.Fatalf("%d pull requests imported, want %d", len(pulls), pullDetailBatch)
	}
	for i, pr := range pulls {
		if want := fmt.Sprintf("PR%d", i); pr.ID != want {
			t.Errorf("pull request %d is %s, want %s — a split reordered the results", i, pr.ID, want)
		}
	}
	// It starts at the full batch and halves its way down to a size the server
	// will answer, and never asks for more than it started with.
	if got := slices.Max(*sizes); got != pullDetailBatch {
		t.Errorf("the largest batch asked for was %d, want %d", got, pullDetailBatch)
	}
	if got := slices.Min(*sizes); got > 2 {
		t.Errorf("batch sizes went %v; they should have reached the size the server answers", batchSizes(*sizes))
	}
	if len(*notices) == 0 {
		t.Error("splitting a batch should be reported")
	}
}

// Splitting stops at one pull request. There is nothing left to divide, so the
// failure is reported rather than retried forever — and it carries what GitHub
// said, not just the status.
func TestBatch502AtOnePullRequestGivesUp(t *testing.T) {
	srv, _, notices := pullServer(t, pullDetailBatch, func(request, size int) bool { return true })
	client := testClient(t, srv, notices)

	_, err := client.FetchPulls(Target{Host: PublicHost, Owner: "o", Name: "n"}, Filter{}, nil)
	if err == nil {
		t.Fatal("a server that refuses every size should fail the import")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("the error should carry GitHub's own account of it: %v", err)
	}
}

// The detail of a hundred pull requests is fetched in parallel batches, and what
// comes back is still in the order the identity walk named them: a fold depends
// on it, and so does the watermark.
func TestParallelDetailKeepsTheWalksOrder(t *testing.T) {
	const pulls = 95
	srv, sizes, notices := pullServer(t, pulls, func(request, size int) bool { return false })
	client := testClient(t, srv, notices)

	got, err := client.FetchPulls(Target{Host: PublicHost, Owner: "o", Name: "n"}, Filter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != pulls {
		t.Fatalf("%d pull requests, want %d", len(got), pulls)
	}
	for i, pr := range got {
		if want := fmt.Sprintf("PR%d", i); pr.ID != want {
			t.Fatalf("pull request %d is %s, want %s", i, pr.ID, want)
		}
	}
	// Ten batches for ninety-five pull requests, and one identity walk that the
	// server answered without being counted here.
	if len(*sizes) != 10 {
		t.Errorf("%d detail requests for %d pull requests, want 10", len(*sizes), pulls)
	}
}

// A limit is applied to the walk, so the pull requests beyond it cost nothing at
// all — no detail request is ever made for them.
func TestLimitStopsTheWalkBeforeTheDetail(t *testing.T) {
	srv, sizes, notices := pullServer(t, 95, func(request, size int) bool { return false })
	client := testClient(t, srv, notices)

	got, err := client.FetchPulls(Target{Host: PublicHost, Owner: "o", Name: "n"}, Filter{Limit: 12}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 12 {
		t.Errorf("%d pull requests, want the 12 asked for", len(got))
	}
	if len(*sizes) != 2 {
		t.Errorf("%d detail requests for a limit of 12, want 2", len(*sizes))
	}
}

// A non-200 body still carries GraphQL's explanation often enough to be worth
// reading: "http 502" says nothing a person can act on.
func TestErrorMessagesComeFromANon200Body(t *testing.T) {
	e := &Error{Status: http.StatusBadGateway, Messages: graphQLMessages(
		[]byte(`{"errors":[{"message":"Something went wrong while executing your query"}]}`))}
	if !strings.Contains(e.Error(), "Something went wrong") {
		t.Errorf("the message was dropped: %s", e.Error())
	}
	if !e.TooHeavy() || !e.Retryable() {
		t.Error("a 502 is both retryable and a reason to ask for less")
	}

	// A body that is not JSON at all — an HTML error page from a proxy — leaves
	// the status to speak for itself rather than replacing it with a parse
	// error.
	plain := &Error{Status: http.StatusBadGateway, Messages: graphQLMessages([]byte("<html>502</html>"))}
	if !strings.Contains(plain.Error(), "502") {
		t.Errorf("the status should still be reported: %s", plain.Error())
	}
}

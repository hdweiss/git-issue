package giteaapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseTarget(t *testing.T) {
	for _, tc := range []struct {
		in                   string
		scheme, host, prefix string
		owner, repo          string
		wantErr              bool
	}{
		{in: "https://codeberg.org/owner/repo", host: "codeberg.org", owner: "owner", repo: "repo"},
		{in: "https://codeberg.org/owner/repo.git", host: "codeberg.org", owner: "owner", repo: "repo"},
		{in: "https://gitea.example.com/team/proj/", host: "gitea.example.com", owner: "team", repo: "proj"},
		{in: "http://localhost:3000/me/thing", scheme: "http", host: "localhost:3000", owner: "me", repo: "thing"},
		{in: "git@gitea.example.com:me/thing.git", host: "gitea.example.com", owner: "me", repo: "thing"},
		{in: "ssh://git@gitea.example.com/me/thing.git", host: "gitea.example.com", owner: "me", repo: "thing"},
		{in: "https://example.com/gitea/me/thing", host: "example.com", prefix: "/gitea", owner: "me", repo: "thing"},
		{in: "gitea.example.com/me/thing", host: "gitea.example.com", owner: "me", repo: "thing"},
		{in: "me/thing", wantErr: true},
		{in: "", wantErr: true},
		{in: "https://gitea.example.com/onlyowner", wantErr: true},
	} {
		got, err := ParseTarget(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseTarget(%q) = %+v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", tc.in, err)
			continue
		}
		if got.Host != tc.host || got.Owner != tc.owner || got.Repo != tc.repo || got.Prefix != tc.prefix {
			t.Errorf("ParseTarget(%q) = %+v", tc.in, got)
		}
		if tc.scheme != "" && got.Scheme != tc.scheme {
			t.Errorf("ParseTarget(%q) scheme = %q, want %q", tc.in, got.Scheme, tc.scheme)
		}
	}
}

func TestTargetDerivations(t *testing.T) {
	tg := Target{Host: "gitea.example.com", Owner: "team", Repo: "proj"}
	if got := tg.API(); got != "https://gitea.example.com/api/v1" {
		t.Errorf("API() = %q", got)
	}
	if got := tg.String(); got != "gitea.example.com/team/proj" {
		t.Errorf("String() = %q", got)
	}
	if got := tg.Origin(42); got != "gitea:gitea.example.com/team/proj#42" {
		t.Errorf("Origin(42) = %q", got)
	}
	if got := tg.CommentOrigin(42, 7); got != "gitea:gitea.example.com/team/proj#42/comments/7" {
		t.Errorf("CommentOrigin = %q", got)
	}

	sub := Target{Host: "example.com", Prefix: "/gitea", Owner: "a", Repo: "b"}
	if got := sub.API(); got != "https://example.com/gitea/api/v1" {
		t.Errorf("subpath API() = %q", got)
	}
}

func TestTokenPrecedence(t *testing.T) {
	t.Setenv("GITEA_TOKEN", "from-env")
	if tok, src, _ := Token(nil, "gitea.example.com", "from-flag"); tok != "from-flag" || src != FromFlag {
		t.Errorf("flag should win: %q %q", tok, src)
	}
	if tok, src, _ := Token(nil, "gitea.example.com", ""); tok != "from-env" || src != "GITEA_TOKEN" {
		t.Errorf("env should be used: %q %q", tok, src)
	}
}

func TestVersionProbe(t *testing.T) {
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"version":"1.22.0"}`))
	}))
	defer gitea.Close()
	if v, err := New(gitea.URL+"/api/v1", "").Version(); err != nil || v != "1.22.0" {
		t.Errorf("Version() = %q, %v", v, err)
	}

	notGitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`<html>not found</html>`))
	}))
	defer notGitea.Close()
	if _, err := New(notGitea.URL+"/api/v1", "").Version(); err == nil {
		t.Error("Version() against a non-Gitea host should fail")
	}
}

// TestFetchSkipsEmptyFeeds checks that an issue never touched since it was
// filed costs one request (the list page it came on), not four.
func TestFetchSkipsEmptyFeeds(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path]++
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues"):
			// Two issues: #1 untouched, #2 changed and with a comment.
			w.Write([]byte(`[
			  {"number":1,"title":"quiet","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","comments":0,"user":{"login":"a"}},
			  {"number":2,"title":"busy","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-02-01T00:00:00Z","comments":1,"user":{"login":"a"}}
			]`))
		case strings.HasSuffix(r.URL.Path, "/issues/2/comments"):
			w.Write([]byte(`[{"id":9,"body":"hi","created_at":"2026-02-01T00:00:00Z","user":{"login":"b"}}]`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	issues, err := New(srv.URL+"/api/v1", "").Fetch(Target{Owner: "o", Repo: "r"}, Filter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues", len(issues))
	}
	if seen["/api/v1/repos/o/r/issues/1/comments"] != 0 ||
		seen["/api/v1/repos/o/r/issues/1/timeline"] != 0 ||
		seen["/api/v1/repos/o/r/issues/1/dependencies"] != 0 {
		t.Errorf("the untouched issue was hydrated anyway: %v", seen)
	}
	if seen["/api/v1/repos/o/r/issues/2/comments"] != 1 ||
		seen["/api/v1/repos/o/r/issues/2/timeline"] != 1 {
		t.Errorf("the changed issue was not fully hydrated: %v", seen)
	}
}

// TestFetchRemembersDepsDisabled checks that a repository with dependency
// tracking off is asked once, not once per issue.
func TestFetchRemembersDepsDisabled(t *testing.T) {
	var deps int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues"):
			w.Write([]byte(`[
			  {"number":1,"title":"a","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-02-01T00:00:00Z","comments":0,"user":{"login":"x"}},
			  {"number":2,"title":"b","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-02-01T00:00:00Z","comments":0,"user":{"login":"x"}},
			  {"number":3,"title":"c","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-02-01T00:00:00Z","comments":0,"user":{"login":"x"}}
			]`))
		case strings.Contains(r.URL.Path, "/dependencies"):
			atomic.AddInt32(&deps, 1)
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"dependencies are disabled"}`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	c := New(srv.URL+"/api/v1", "")
	c.Workers = 1 // deterministic: the first 404 lands before the others start
	if _, err := c.Fetch(Target{Owner: "o", Repo: "r"}, Filter{}, nil); err != nil {
		t.Fatal(err)
	}
	if deps != 1 {
		t.Errorf("dependencies asked %d times, want 1", deps)
	}
}

// TestIssuesEachStreams checks the streaming read: every issue the server has
// reaches the callback, a 404 is skipped rather than failing the batch, and the
// process never holds more than a worker's worth at once.
func TestIssuesEachStreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/7"):
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"issue does not exist"}`))
		case strings.Contains(r.URL.Path, "/issues/") && !strings.Contains(r.URL.Path, "/comments") &&
			!strings.Contains(r.URL.Path, "/timeline") && !strings.Contains(r.URL.Path, "/dependencies"):
			n := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
			w.Write([]byte(`{"number":` + n + `,"title":"t","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","user":{"login":"x"}}`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	c := New(srv.URL+"/api/v1", "")
	c.Workers = 3

	var (
		mu   sync.Mutex
		seen []int64
		live int
		peak int
	)
	err := c.IssuesEach(Target{Owner: "o", Repo: "r"}, []int64{1, 2, 3, 4, 5, 6, 7, 8}, func(iss Issue) error {
		mu.Lock()
		seen = append(seen, iss.Number)
		live++
		if live > peak {
			peak = live
		}
		live--
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("IssuesEach: %v", err)
	}
	if len(seen) != 7 {
		t.Errorf("callback ran %d times, want 7 (issue 7 is a 404)", len(seen))
	}
	for _, n := range seen {
		if n == 7 {
			t.Errorf("the 404 issue reached the callback")
		}
	}
	if peak != 1 {
		t.Errorf("callback ran concurrently (peak %d), want single-goroutine", peak)
	}
}

// TestPagedWalkEndsOnAnUnpaginatedEndpoint is the regression test for the read
// that could not end.
//
// Gitea's issue-comments endpoint ignores page and limit and answers with the
// whole thread every time. A walk that only stopped on a short page therefore
// asked for page after page, appending the same comments to the same slice
// until the process was killed — so any issue with at least pageSize comments
// was unreadable, on a push and on a pull alike.
func TestPagedWalkEndsOnAnUnpaginatedEndpoint(t *testing.T) {
	const held = 120 // more than one page, and the server sends them all every time
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		var b strings.Builder
		b.WriteByte('[')
		for i := 0; i < held; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"id":%d,"body":"c","created_at":"2026-01-01T00:00:00Z","user":{"login":"x"}}`, i+1)
		}
		b.WriteByte(']')
		w.Header().Set("X-Total-Count", strconv.Itoa(held))
		w.Write([]byte(b.String()))
	}))
	defer srv.Close()

	got, err := New(srv.URL+"/api/v1", "").comments(Target{Owner: "o", Repo: "r"}, 1)
	if err != nil {
		t.Fatalf("comments: %v", err)
	}
	if len(got) != held {
		t.Errorf("read %d comments, want %d — the thread was collected more than once", len(got), held)
	}
	if hits != 1 {
		t.Errorf("server saw %d requests, want 1: the walk kept asking for pages it had already been given", hits)
	}
}

// TestPagedWalkEndsOnTheServersOwnTotal checks the other stop: an endpoint that
// does paginate, whose last page happens to be exactly full, ends on the total
// it reported rather than on an extra empty request.
func TestPagedWalkEndsOnTheServersOwnTotal(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		var b strings.Builder
		b.WriteByte('[')
		for i := 0; i < 50; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"id":%d,"body":"c","created_at":"2026-01-01T00:00:00Z","user":{"login":"x"}}`, int(n)*100+i)
		}
		b.WriteByte(']')
		w.Header().Set("X-Total-Count", "100")
		w.Write([]byte(b.String()))
	}))
	defer srv.Close()

	got, err := New(srv.URL+"/api/v1", "").comments(Target{Owner: "o", Repo: "r"}, 1)
	if err != nil {
		t.Fatalf("comments: %v", err)
	}
	if len(got) != 100 || hits != 2 {
		t.Errorf("read %d comments in %d requests, want 100 in 2", len(got), hits)
	}
}

// TestIssuesEachReadsTheList checks the batched read: where the list is the
// cheaper way to the wanted issues it is used instead of a request each, it
// hands over only what was asked for, and a number the list does not carry is
// left out rather than failing the batch.
func TestIssuesEachReadsTheList(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path]++
		mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/issues") {
			w.Write([]byte(`[]`))
			return
		}
		w.Header().Set("X-Total-Count", "4")
		w.Write([]byte(listPage(1, 2, 3, 4)))
	}))
	defer srv.Close()

	c := New(srv.URL+"/api/v1", "")
	var got []int64
	err := c.IssuesEach(Target{Owner: "o", Repo: "r"}, []int64{1, 3, 9}, func(iss Issue) error {
		got = append(got, iss.Number)
		return nil
	})
	if err != nil {
		t.Fatalf("IssuesEach: %v", err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("callback saw %v, want [1 3] — 9 is not in the list", got)
	}
	if n := seen["/api/v1/repos/o/r/issues"]; n != 1 {
		t.Errorf("list read %d times, want 1", n)
	}
	for path, n := range seen {
		if path != "/api/v1/repos/o/r/issues" {
			t.Errorf("%s was read %d times; the list already carried the issues", path, n)
		}
	}
}

// TestIssuesEachWalksListPages checks that the walk carries on past the first
// page for a wanted issue that is on a later one.
func TestIssuesEachWalksListPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/issues") {
			w.Write([]byte(`[]`))
			return
		}
		var numbers []int64
		start := int64(1)
		if r.URL.Query().Get("page") == "2" {
			start = 51
		}
		for n := start; n < start+50 && n <= 60; n++ {
			numbers = append(numbers, n)
		}
		w.Header().Set("X-Total-Count", "60")
		w.Write([]byte(listPage(numbers...)))
	}))
	defer srv.Close()

	c := New(srv.URL+"/api/v1", "")
	var got []int64
	err := c.IssuesEach(Target{Owner: "o", Repo: "r"}, []int64{1, 55}, func(iss Issue) error {
		got = append(got, iss.Number)
		return nil
	})
	if err != nil {
		t.Fatalf("IssuesEach: %v", err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 55 {
		t.Errorf("callback saw %v, want [1 55]", got)
	}
}

// TestIssuesEachSkipsTheListWhenItIsLonger checks the other half of the choice:
// a push naming a couple of issues in a big repository must not page through
// the whole tracker to find them.
func TestIssuesEachSkipsTheListWhenItIsLonger(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path]++
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues"):
			w.Header().Set("X-Total-Count", "500")
			w.Write([]byte(listPage(1, 2, 3)))
		case strings.Contains(r.URL.Path, "/comments") || strings.Contains(r.URL.Path, "/timeline") ||
			strings.Contains(r.URL.Path, "/dependencies"):
			w.Write([]byte(`[]`))
		default:
			n := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
			w.Write([]byte(`{"number":` + n + `,"title":"t","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","user":{"login":"x"}}`))
		}
	}))
	defer srv.Close()

	c := New(srv.URL+"/api/v1", "")
	n := 0
	if err := c.IssuesEach(Target{Owner: "o", Repo: "r"}, []int64{2, 3}, func(Issue) error { n++; return nil }); err != nil {
		t.Fatalf("IssuesEach: %v", err)
	}
	if n != 2 {
		t.Errorf("callback ran %d times, want 2", n)
	}
	if seen["/api/v1/repos/o/r/issues/2"] != 1 || seen["/api/v1/repos/o/r/issues/3"] != 1 {
		t.Errorf("the issues were not read one at a time: %v", seen)
	}
	if got := seen["/api/v1/repos/o/r/issues"]; got != 1 {
		t.Errorf("list read %d times, want 1 — only the probe that reads the total", got)
	}
}

// listPage renders one page of the issue list: quiet issues, so nothing needs
// hydrating and a per-issue request is unambiguously the batched read failing.
func listPage(numbers ...int64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, n := range numbers {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"number":%d,"title":"t","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","comments":0,"user":{"login":"x"}}`, n)
	}
	b.WriteByte(']')
	return b.String()
}

// TestIssuesEachStopsOnCallbackError checks that the first error from the
// callback ends the read and comes back out.
func TestIssuesEachStopsOnCallbackError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/comments") || strings.Contains(r.URL.Path, "/timeline") ||
			strings.Contains(r.URL.Path, "/dependencies") {
			w.Write([]byte(`[]`))
			return
		}
		n := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
		w.Write([]byte(`{"number":` + n + `,"title":"t","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","user":{"login":"x"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/api/v1", "")
	c.Workers = 2

	boom := fmt.Errorf("stop here")
	got := c.IssuesEach(Target{Owner: "o", Repo: "r"}, []int64{1, 2, 3, 4, 5, 6}, func(Issue) error {
		return boom
	})
	if got != boom {
		t.Fatalf("IssuesEach error = %v, want %v", got, boom)
	}
}

func TestRetryOnServerError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"version":"1.22.0"}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/api/v1", "")
	c.Sleep = func(time.Duration) {}
	if v, err := c.Version(); err != nil || v != "1.22.0" {
		t.Fatalf("Version() = %q, %v after retries", v, err)
	}
	if hits != 3 {
		t.Errorf("server saw %d hits, want 3", hits)
	}
}

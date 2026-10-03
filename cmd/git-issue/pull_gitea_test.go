package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// giteaFake is a tiny in-memory Gitea for the command-line tests: enough of the
// REST surface to pull a couple of issues, push a change back, and pull again.
// The CLI drives it with sequential requests, so it needs no locking.
type giteaFake struct {
	issues  map[int64]*giteaFakeIssue
	nextNum int64
	nextID  int64
}

type giteaFakeIssue struct {
	Number    int64
	Title     string
	Body      string
	State     string
	Labels    []string
	Comments  []map[string]any
	Timeline  []map[string]any
	CreatedAt string
}

func newGiteaFake() *giteaFake {
	f := &giteaFake{issues: map[int64]*giteaFakeIssue{}}
	f.add("Login fails on empty input", "Steps to reproduce follow.")
	f.add("Docs are out of date", "")
	return f
}

func (f *giteaFake) add(title, body string) *giteaFakeIssue {
	f.nextNum++
	iss := &giteaFakeIssue{
		Number: f.nextNum, Title: title, Body: body, State: "open",
		CreatedAt: "2026-02-01T10:00:00Z",
	}
	f.issues[iss.Number] = iss
	return iss
}

func (f *giteaFake) render(iss *giteaFakeIssue) map[string]any {
	labels := []any{}
	for i, name := range iss.Labels {
		labels = append(labels, map[string]any{"id": int64(i + 1), "name": name})
	}
	return map[string]any{
		"id": iss.Number, "number": iss.Number,
		"html_url":   fmt.Sprintf("https://gitea.example.com/acme/proj/issues/%d", iss.Number),
		"title":      iss.Title,
		"body":       iss.Body,
		"state":      iss.State,
		"user":       map[string]any{"login": "octo"},
		"labels":     labels,
		"assignees":  []any{},
		"comments":   len(iss.Comments),
		"created_at": iss.CreatedAt,
		"updated_at": "2026-02-02T09:00:00Z",
	}
}

var (
	reGiteaIssue    = regexp.MustCompile(`^/api/v1/repos/acme/proj/issues/(\d+)$`)
	reGiteaComments = regexp.MustCompile(`^/api/v1/repos/acme/proj/issues/(\d+)/comments$`)
	reGiteaTimeline = regexp.MustCompile(`^/api/v1/repos/acme/proj/issues/(\d+)/timeline$`)
	reGiteaDeps     = regexp.MustCompile(`^/api/v1/repos/acme/proj/issues/(\d+)/dependencies$`)
	reGiteaLabels   = regexp.MustCompile(`^/api/v1/repos/acme/proj/issues/(\d+)/labels$`)
)

func (f *giteaFake) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{}
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&body)
		}
		p := r.URL.Path
		enc := json.NewEncoder(w)

		switch {
		case p == "/api/v1/version":
			enc.Encode(map[string]any{"version": "1.22.0"})
		case p == "/api/v1/repos/acme/proj":
			enc.Encode(map[string]any{"full_name": "acme/proj", "has_issues": true})
		case p == "/api/v1/repos/acme/proj/issues" && r.Method == "GET":
			var out []any
			for n := int64(1); n <= f.nextNum; n++ {
				if iss, ok := f.issues[n]; ok {
					out = append(out, f.render(iss))
				}
			}
			enc.Encode(out)
		case p == "/api/v1/repos/acme/proj/issues" && r.Method == "POST":
			iss := f.add(gvStr(body["title"]), gvStr(body["body"]))
			enc.Encode(f.render(iss))
		case p == "/api/v1/repos/acme/proj/labels":
			enc.Encode([]any{map[string]any{"id": 1, "name": "bug"}})
		case strings.HasPrefix(p, "/api/v1/orgs/") && strings.HasSuffix(p, "/labels"):
			w.WriteHeader(404)
			enc.Encode(map[string]any{"message": "org does not exist"})
		case p == "/api/v1/repos/acme/proj/milestones":
			enc.Encode([]any{})
		case p == "/api/v1/repos/acme/proj/assignees":
			enc.Encode([]any{map[string]any{"login": "octo"}, map[string]any{"login": "pusher"}})
		case reGiteaComments.MatchString(p) && r.Method == "GET":
			enc.Encode(f.issues[atoi(reGiteaComments.FindStringSubmatch(p)[1])].Comments)
		case reGiteaComments.MatchString(p) && r.Method == "POST":
			n := atoi(reGiteaComments.FindStringSubmatch(p)[1])
			f.nextID++
			c := map[string]any{
				"id": f.nextID, "body": gvStr(body["body"]),
				"created_at": "2026-02-03T09:00:00Z", "updated_at": "2026-02-03T09:00:00Z",
				"user": map[string]any{"login": "pusher"},
			}
			f.issues[n].Comments = append(f.issues[n].Comments, c)
			enc.Encode(c)
		case reGiteaTimeline.MatchString(p):
			enc.Encode(f.issues[atoi(reGiteaTimeline.FindStringSubmatch(p)[1])].Timeline)
		case reGiteaDeps.MatchString(p):
			enc.Encode([]any{})
		case reGiteaLabels.MatchString(p) && r.Method == "POST":
			n := atoi(reGiteaLabels.FindStringSubmatch(p)[1])
			f.issues[n].Labels = append(f.issues[n].Labels, "bug")
			f.issues[n].Timeline = append(f.issues[n].Timeline, map[string]any{
				"id": nextTimelineID(f), "type": "label", "body": "1",
				"created_at": "2026-02-03T09:00:00Z",
				"user":       map[string]any{"login": "pusher"},
				"label":      map[string]any{"id": 1, "name": "bug"},
			})
			enc.Encode([]any{})
		case reGiteaIssue.MatchString(p) && r.Method == "GET":
			n := atoi(reGiteaIssue.FindStringSubmatch(p)[1])
			iss, ok := f.issues[n]
			if !ok {
				w.WriteHeader(404)
				enc.Encode(map[string]any{"message": "not found"})
				return
			}
			enc.Encode(f.render(iss))
		case reGiteaIssue.MatchString(p) && r.Method == "PATCH":
			n := atoi(reGiteaIssue.FindStringSubmatch(p)[1])
			iss := f.issues[n]
			if v, ok := body["title"]; ok && gvStr(v) != iss.Title {
				iss.Timeline = append(iss.Timeline, map[string]any{
					"id": nextTimelineID(f), "type": "change_title",
					"created_at": "2026-02-03T09:00:00Z",
					"user":       map[string]any{"login": "pusher"},
					"old_title":  iss.Title, "new_title": gvStr(v),
				})
				iss.Title = gvStr(v)
			}
			if v, ok := body["body"]; ok {
				iss.Body = gvStr(v)
			}
			enc.Encode(f.render(iss))
		default:
			t.Errorf("giteaFake: unhandled %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func nextTimelineID(f *giteaFake) int64 { f.nextID++; return f.nextID }

func atoi(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }

// The whole bridge path from the command line: resolve the target, fetch, map,
// union into the notes ref, report.
func TestPullGitea(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)
	srv := newGiteaFake().server(t)
	target := "gitea:" + srv.URL + "/acme/proj"

	got := gitIssueEnv(t, bin, bob, []string{"GITEA_TOKEN=secret"}, "pull", target)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{
		"Login fails on empty input",
		"Docs are out of date",
		"2 issues changed, 2 added(+)",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("pull output missing %q\n%s", want, got.stdout)
		}
	}

	// A second pull finds nothing to do — the nonce derivation's whole point.
	again := gitIssueEnv(t, bin, bob, []string{"GITEA_TOKEN=secret"}, "pull", target)
	if !strings.Contains(again.stdout, "Already up to date") && !strings.Contains(again.stdout, "0 issues") {
		t.Errorf("second pull was not a no-op:\n%s", again.stdout)
	}
}

// Edit locally, push to Gitea, and see it accepted; then --refresh re-imports.
func TestPushGitea(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)
	srv := newGiteaFake().server(t)
	target := "gitea:" + srv.URL + "/acme/proj"

	if got := gitIssueEnv(t, bin, bob, []string{"GITEA_TOKEN=secret"}, "pull", target); got.code != 0 {
		t.Fatalf("pull: %s", got.stderr)
	}

	list := gitIssue(t, bin, bob, "list")
	id := firstID(t, list.stdout)
	if r := gitIssue(t, bin, bob, "edit", id, "-t", "Login fails on empty input, revised"); r.code != 0 {
		t.Fatalf("edit: %s", r.stderr)
	}

	push := gitIssueEnv(t, bin, bob, []string{"GITEA_TOKEN=secret"}, "push", "-y", target)
	if push.code != 0 {
		t.Fatalf("push exit %d: %s\n%s", push.code, push.stderr, push.stdout)
	}
	if !strings.Contains(push.stdout, "1 issue pushed") {
		t.Errorf("push output:\n%s", push.stdout)
	}

	// A second push has nothing to do.
	again := gitIssueEnv(t, bin, bob, []string{"GITEA_TOKEN=secret"}, "push", "-y", target)
	if !strings.Contains(again.stdout, "Everything up-to-date") {
		t.Errorf("second push was not a no-op:\n%s", again.stdout)
	}
}

// A label the repository does not have is a warning, and one warning covers
// every issue that names it rather than one line each.
func TestPushGiteaCollapsesRepeatedWarnings(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)
	srv := newGiteaFake().server(t)
	target := "gitea:" + srv.URL + "/acme/proj"

	if got := gitIssueEnv(t, bin, bob, []string{"GITEA_TOKEN=secret"}, "pull", target); got.code != 0 {
		t.Fatalf("pull: %s", got.stderr)
	}

	var ids []string
	for _, line := range strings.Split(stripANSI(gitIssue(t, bin, bob, "list").stdout), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) > 0 && isHexPrefix(f[0]) && len(f[0]) >= 7 {
			ids = append(ids, f[0])
		}
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 issues, got %v", ids)
	}
	for _, id := range ids {
		if r := gitIssue(t, bin, bob, "edit", id, "-l", "ghostlabel"); r.code != 0 {
			t.Fatalf("edit %s: %s", id, r.stderr)
		}
	}

	push := gitIssueEnv(t, bin, bob, []string{"GITEA_TOKEN=secret"}, "push", "-y", target)
	if push.code != 0 {
		t.Fatalf("push exit %d: %s\n%s", push.code, push.stderr, push.stdout)
	}
	if n := strings.Count(push.stdout, `has no label "ghostlabel"`); n != 1 {
		t.Errorf("label warning appeared %d times, want 1:\n%s", n, push.stdout)
	}
	if !strings.Contains(push.stdout, "2 issues") {
		t.Errorf("collapsed warning did not name the count:\n%s", push.stdout)
	}
}

func gvStr(v any) string { s, _ := v.(string); return s }

// firstID pulls the first entity id out of a `list` rendering.
func firstID(t *testing.T, listing string) string {
	t.Helper()
	for _, line := range strings.Split(listing, "\n") {
		f := strings.Fields(strings.TrimSpace(stripANSI(line)))
		if len(f) > 0 && isHexPrefix(f[0]) && len(f[0]) >= 7 {
			return f[0]
		}
	}
	t.Fatalf("no id in listing:\n%s", listing)
	return ""
}

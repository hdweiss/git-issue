package giteaissue

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	giteaapi "github.com/hdweiss/git-issue/internal/bridge/gitea/api"
)

// fakeGitea is a Gitea that remembers what was written to it.
//
// The recorded-response fixtures cover reading; this covers the round trip,
// which is the one write-back rests on: what a push sends must read back as the
// same folded state, or a mirror never settles.
type fakeGitea struct {
	t           *testing.T
	issues      map[int64]*fakeIssue
	comments    map[int64]*fakeComment
	labels      map[string]int64
	milestone   map[string]int64
	assignable  []string
	repoMissing bool
	nextIssue   int64
	nextID      int64
	clock       time.Time
}

type fakeIssue struct {
	Number       int64
	Title        string
	Body         string
	State        string
	Milestone    string
	Labels       []string
	Assignees    []string
	Comments     []int64
	Dependencies []int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Timeline     []fakeEvent
}

type fakeComment struct {
	ID        int64
	Issue     int64
	Body      string
	CreatedAt time.Time
}

type fakeEvent struct {
	ID              int64
	Type            string
	CreatedAt       time.Time
	Label           string
	Body            string
	Assignee        string
	RemovedAssignee bool
	OldTitle        string
	NewTitle        string
	Milestone       string
}

func newFakeGitea(t *testing.T) *fakeGitea {
	return &fakeGitea{
		t:          t,
		issues:     map[int64]*fakeIssue{},
		comments:   map[int64]*fakeComment{},
		labels:     map[string]int64{"bug": 1, "storage": 2, "confirmed": 3, "urgent": 4},
		milestone:  map[string]int64{"v1": 1, "v2": 2},
		assignable: []string{"pusher", "octo", "alice"},
		clock:      time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

func (f *fakeGitea) tick() time.Time {
	f.clock = f.clock.Add(time.Minute)
	return f.clock
}

func (f *fakeGitea) id() int64 {
	f.nextID++
	return f.nextID
}

func (f *fakeGitea) client() *giteaapi.Client {
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	return giteaapi.New(srv.URL+"/api/v1", "test-token")
}

func fakeTarget() giteaapi.Target {
	return giteaapi.Target{Scheme: "http", Host: "gitea.test", Owner: "acme", Repo: "git-issue"}
}

var (
	reIssue    = regexp.MustCompile(`/repos/acme/git-issue/issues/(\d+)$`)
	reComments = regexp.MustCompile(`/repos/acme/git-issue/issues/(\d+)/comments$`)
	reTimeline = regexp.MustCompile(`/repos/acme/git-issue/issues/(\d+)/timeline$`)
	reDeps     = regexp.MustCompile(`/repos/acme/git-issue/issues/(\d+)/dependencies$`)
	reLabels   = regexp.MustCompile(`/repos/acme/git-issue/issues/(\d+)/labels$`)
	reLabelOne = regexp.MustCompile(`/repos/acme/git-issue/issues/(\d+)/labels/(\d+)$`)
	reCommentO = regexp.MustCompile(`/repos/acme/git-issue/issues/comments/(\d+)$`)
)

func (f *fakeGitea) serve(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "" && got != "token test-token" {
		f.t.Errorf("Authorization = %q", got)
	}
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	body := map[string]any{}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}

	writeJSON := func(v any) { json.NewEncoder(w).Encode(v) }
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		writeJSON(map[string]any{"message": msg})
	}

	switch {
	case strings.HasSuffix(path, "/api/v1/version"):
		writeJSON(map[string]any{"version": "1.22.0+fake"})

	case strings.HasSuffix(path, "/repos/acme/git-issue"):
		if f.repoMissing {
			fail(http.StatusNotFound, "repository does not exist [id: 0, uid: 0, owner_name: , name: ]")
			return
		}
		writeJSON(map[string]any{
			"full_name": "acme/git-issue", "has_issues": true,
			"permissions": map[string]any{"push": true},
		})

	case reComments.MatchString(path) && r.Method == http.MethodGet:
		n := num(reComments.FindStringSubmatch(path)[1])
		writeJSON(f.renderComments(n))
	case reComments.MatchString(path) && r.Method == http.MethodPost:
		n := num(reComments.FindStringSubmatch(path)[1])
		writeJSON(f.addComment(n, str(body["body"])))

	case reTimeline.MatchString(path):
		n := num(reTimeline.FindStringSubmatch(path)[1])
		writeJSON(f.renderTimeline(n))

	case reDeps.MatchString(path) && r.Method == http.MethodGet:
		n := num(reDeps.FindStringSubmatch(path)[1])
		writeJSON(f.renderDeps(n))
	case reDeps.MatchString(path) && r.Method == http.MethodPost:
		n := num(reDeps.FindStringSubmatch(path)[1])
		f.checkDepRepo(body)
		f.setDep(n, int64(toFloat(body["index"])), true)
		writeJSON(map[string]any{})
	case reDeps.MatchString(path) && r.Method == http.MethodDelete:
		n := num(reDeps.FindStringSubmatch(path)[1])
		f.checkDepRepo(body)
		if !f.setDep(n, int64(toFloat(body["index"])), false) {
			fail(http.StatusNotFound, "dependency not found")
			return
		}
		writeJSON(map[string]any{})

	case reLabels.MatchString(path) && r.Method == http.MethodPost:
		n := num(reLabels.FindStringSubmatch(path)[1])
		f.addLabels(n, body["labels"])
		writeJSON([]any{})
	case reLabelOne.MatchString(path) && r.Method == http.MethodDelete:
		m := reLabelOne.FindStringSubmatch(path)
		f.removeLabel(num(m[1]), num(m[2]))
		w.WriteHeader(http.StatusNoContent)

	case reIssue.MatchString(path) && r.Method == http.MethodGet:
		n := num(reIssue.FindStringSubmatch(path)[1])
		iss, ok := f.issues[n]
		if !ok {
			fail(http.StatusNotFound, "issue not found")
			return
		}
		writeJSON(f.renderIssue(iss))
	case reIssue.MatchString(path) && r.Method == http.MethodPatch:
		n := num(reIssue.FindStringSubmatch(path)[1])
		iss, ok := f.issues[n]
		if !ok {
			fail(http.StatusNotFound, "issue not found")
			return
		}
		f.editIssue(iss, body)
		writeJSON(f.renderIssue(iss))

	case strings.HasSuffix(path, "/repos/acme/git-issue/issues") && r.Method == http.MethodPost:
		writeJSON(f.renderIssue(f.createIssue(body)))
	case strings.HasSuffix(path, "/repos/acme/git-issue/issues") && r.Method == http.MethodGet:
		// X-Total-Count as the real server sends it: a push reads it to decide
		// whether the list is a cheaper way to the issues than one request each,
		// so without it these tests would only ever exercise the other branch.
		list := f.listIssues()
		w.Header().Set("X-Total-Count", strconv.Itoa(len(list)))
		writeJSON(list)

	case strings.HasSuffix(path, "/repos/acme/git-issue/labels"):
		writeJSON(f.renderLabels())
	case strings.HasPrefix(path, "/api/v1/orgs/") && strings.HasSuffix(path, "/labels"):
		// acme is a user in these tests, so it has no org labels.
		fail(http.StatusNotFound, "org does not exist")
	case strings.HasSuffix(path, "/repos/acme/git-issue/milestones"):
		writeJSON(f.renderMilestones())
	case strings.HasSuffix(path, "/repos/acme/git-issue/assignees"):
		out := []any{}
		for _, name := range f.assignable {
			out = append(out, map[string]any{"login": name})
		}
		writeJSON(out)

	case reCommentO.MatchString(path) && r.Method == http.MethodPatch:
		id := num(reCommentO.FindStringSubmatch(path)[1])
		if c, ok := f.comments[id]; ok {
			c.Body = str(body["body"])
		}
		writeJSON(map[string]any{})
	case reCommentO.MatchString(path) && r.Method == http.MethodDelete:
		id := num(reCommentO.FindStringSubmatch(path)[1])
		f.deleteComment(id)
		w.WriteHeader(http.StatusNoContent)

	default:
		f.t.Errorf("fake gitea: unhandled %s %s", r.Method, path)
		fail(http.StatusNotFound, "unhandled")
	}
}

func (f *fakeGitea) createIssue(body map[string]any) *fakeIssue {
	f.nextIssue++
	now := f.tick()
	iss := &fakeIssue{
		Number:    f.nextIssue,
		Title:     str(body["title"]),
		Body:      str(body["body"]),
		State:     "open",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if b, _ := body["closed"].(bool); b {
		iss.State = "closed"
	}
	for _, id := range toInts(body["labels"]) {
		iss.Labels = appendUnique(iss.Labels, f.labelName(id))
	}
	for _, a := range toStrings(body["assignees"]) {
		iss.Assignees = appendUnique(iss.Assignees, a)
	}
	if mid := int64(toFloat(body["milestone"])); mid != 0 {
		iss.Milestone = f.milestoneName(mid)
	}
	f.issues[iss.Number] = iss
	return iss
}

func (f *fakeGitea) editIssue(iss *fakeIssue, body map[string]any) {
	defer func() { iss.UpdatedAt = f.tick() }()
	if v, ok := body["title"]; ok && str(v) != iss.Title {
		iss.Timeline = append(iss.Timeline, fakeEvent{
			ID: f.id(), Type: giteaapi.TLTitle, CreatedAt: f.tick(),
			OldTitle: iss.Title, NewTitle: str(v),
		})
		iss.Title = str(v)
	}
	if v, ok := body["body"]; ok {
		iss.Body = str(v)
	}
	if v, ok := body["state"]; ok && str(v) != iss.State {
		iss.State = str(v)
		kind := giteaapi.TLReopen
		if iss.State == "closed" {
			kind = giteaapi.TLClose
		}
		iss.Timeline = append(iss.Timeline, fakeEvent{ID: f.id(), Type: kind, CreatedAt: f.tick()})
	}
	if v, ok := body["milestone"]; ok {
		mid := int64(toFloat(v))
		name := ""
		if mid != 0 {
			name = f.milestoneName(mid)
		}
		if name != iss.Milestone {
			iss.Milestone = name
			iss.Timeline = append(iss.Timeline, fakeEvent{
				ID: f.id(), Type: giteaapi.TLMilestone, CreatedAt: f.tick(), Milestone: name,
			})
		}
	}
	if v, ok := body["assignees"]; ok {
		want := toStrings(v)
		for _, a := range want {
			if !containsFold(iss.Assignees, a) {
				iss.Timeline = append(iss.Timeline, fakeEvent{
					ID: f.id(), Type: giteaapi.TLAssignees, CreatedAt: f.tick(), Assignee: a,
				})
			}
		}
		for _, a := range iss.Assignees {
			if !containsFold(want, a) {
				iss.Timeline = append(iss.Timeline, fakeEvent{
					ID: f.id(), Type: giteaapi.TLAssignees, CreatedAt: f.tick(),
					Assignee: a, RemovedAssignee: true,
				})
			}
		}
		iss.Assignees = want
	}
}

func (f *fakeGitea) addLabels(n int64, raw any) {
	iss := f.issues[n]
	defer func() { iss.UpdatedAt = f.tick() }()
	for _, id := range toInts(raw) {
		name := f.labelName(id)
		if name == "" || containsFold(iss.Labels, name) {
			continue
		}
		iss.Labels = append(iss.Labels, name)
		iss.Timeline = append(iss.Timeline, fakeEvent{
			ID: f.id(), Type: giteaapi.TLLabel, CreatedAt: f.tick(), Label: name, Body: "1",
		})
	}
}

func (f *fakeGitea) removeLabel(n, labelID int64) {
	iss := f.issues[n]
	iss.UpdatedAt = f.tick()
	name := f.labelName(labelID)
	if !containsFold(iss.Labels, name) {
		return
	}
	iss.Labels = removeStr(iss.Labels, name)
	iss.Timeline = append(iss.Timeline, fakeEvent{
		ID: f.id(), Type: giteaapi.TLLabel, CreatedAt: f.tick(), Label: name, Body: "",
	})
}

// checkDepRepo stands in for Gitea's own behaviour: the dependencies endpoint
// switches to a cross-repository lookup unless the body names the same owner and
// repo as the issue in the path, and an empty name resolves to nothing and 404s.
func (f *fakeGitea) checkDepRepo(body map[string]any) {
	// Gitea binds the repo name as `json:"repo"`; "name" is silently ignored and
	// the call then 404s on an empty repo.
	if str(body["owner"]) != "acme" || str(body["repo"]) != "git-issue" {
		f.t.Errorf("dependency body owner/repo = %q/%q, want acme/git-issue", body["owner"], body["repo"])
	}
}

func (f *fakeGitea) setDep(n, blocking int64, add bool) bool {
	iss := f.issues[n]
	iss.UpdatedAt = f.tick()
	has := false
	for _, d := range iss.Dependencies {
		if d == blocking {
			has = true
		}
	}
	if add {
		if !has {
			iss.Dependencies = append(iss.Dependencies, blocking)
		}
		return true
	}
	if !has {
		return false
	}
	var out []int64
	for _, d := range iss.Dependencies {
		if d != blocking {
			out = append(out, d)
		}
	}
	iss.Dependencies = out
	return true
}

func (f *fakeGitea) addComment(n int64, body string) map[string]any {
	c := &fakeComment{ID: f.id(), Issue: n, Body: body, CreatedAt: f.tick()}
	f.comments[c.ID] = c
	f.issues[n].Comments = append(f.issues[n].Comments, c.ID)
	f.issues[n].UpdatedAt = c.CreatedAt
	return map[string]any{
		"id": c.ID, "body": c.Body,
		"created_at": c.CreatedAt.Format(time.RFC3339),
		"updated_at": c.CreatedAt.Format(time.RFC3339),
		"user":       map[string]any{"login": "pusher"},
	}
}

func (f *fakeGitea) deleteComment(id int64) {
	c, ok := f.comments[id]
	if !ok {
		return
	}
	delete(f.comments, id)
	iss := f.issues[c.Issue]
	var out []int64
	for _, x := range iss.Comments {
		if x != id {
			out = append(out, x)
		}
	}
	iss.Comments = out
}

func (f *fakeGitea) renderIssue(iss *fakeIssue) map[string]any {
	labels := []any{}
	for _, name := range iss.Labels {
		labels = append(labels, map[string]any{"id": f.labels[name], "name": name})
	}
	assignees := []any{}
	for _, a := range iss.Assignees {
		assignees = append(assignees, map[string]any{"login": a})
	}
	var milestone any
	if iss.Milestone != "" {
		milestone = map[string]any{"id": f.milestone[iss.Milestone], "title": iss.Milestone}
	}
	return map[string]any{
		"id": iss.Number, "number": iss.Number,
		"html_url":   fmt.Sprintf("http://gitea.test/acme/git-issue/issues/%d", iss.Number),
		"title":      iss.Title,
		"body":       iss.Body,
		"state":      iss.State,
		"user":       map[string]any{"login": "pusher"},
		"labels":     labels,
		"assignees":  assignees,
		"milestone":  milestone,
		"comments":   len(iss.Comments),
		"created_at": iss.CreatedAt.Format(time.RFC3339),
		"updated_at": iss.UpdatedAt.Format(time.RFC3339),
	}
}

func (f *fakeGitea) listIssues() []any {
	var out []any
	for n := int64(1); n <= f.nextIssue; n++ {
		if iss, ok := f.issues[n]; ok {
			out = append(out, f.renderIssue(iss))
		}
	}
	return out
}

func (f *fakeGitea) renderComments(n int64) []any {
	out := []any{}
	for _, id := range f.issues[n].Comments {
		c := f.comments[id]
		out = append(out, map[string]any{
			"id": c.ID, "body": c.Body,
			"created_at": c.CreatedAt.Format(time.RFC3339),
			"updated_at": c.CreatedAt.Format(time.RFC3339),
			"user":       map[string]any{"login": "pusher"},
		})
	}
	return out
}

func (f *fakeGitea) renderTimeline(n int64) []any {
	out := []any{}
	for _, e := range f.issues[n].Timeline {
		item := map[string]any{
			"id": e.ID, "type": e.Type,
			"created_at": e.CreatedAt.Format(time.RFC3339),
			"user":       map[string]any{"login": "pusher"},
		}
		switch e.Type {
		case giteaapi.TLLabel:
			item["label"] = map[string]any{"id": f.labels[e.Label], "name": e.Label}
			item["body"] = e.Body
		case giteaapi.TLAssignees:
			item["assignee"] = map[string]any{"login": e.Assignee}
			item["removed_assignee"] = e.RemovedAssignee
		case giteaapi.TLTitle:
			item["old_title"], item["new_title"] = e.OldTitle, e.NewTitle
		case giteaapi.TLMilestone:
			if e.Milestone != "" {
				item["milestone"] = map[string]any{"title": e.Milestone}
			}
		}
		out = append(out, item)
	}
	return out
}

func (f *fakeGitea) renderDeps(n int64) []any {
	out := []any{}
	for _, d := range f.issues[n].Dependencies {
		out = append(out, map[string]any{
			"number":     d,
			"repository": map[string]any{"full_name": "acme/git-issue"},
		})
	}
	return out
}

func (f *fakeGitea) renderLabels() []any {
	out := []any{}
	for name, id := range f.labels {
		out = append(out, map[string]any{"id": id, "name": name})
	}
	return out
}

func (f *fakeGitea) renderMilestones() []any {
	out := []any{}
	for title, id := range f.milestone {
		out = append(out, map[string]any{"id": id, "title": title, "state": "open"})
	}
	return out
}

func (f *fakeGitea) labelName(id int64) string {
	for name, x := range f.labels {
		if x == id {
			return name
		}
	}
	return ""
}

func (f *fakeGitea) milestoneName(id int64) string {
	for title, x := range f.milestone {
		if x == id {
			return title
		}
	}
	return ""
}

// helpers -------------------------------------------------------------------

func num(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }

func str(v any) string { s, _ := v.(string); return s }

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

func toInts(v any) []int64 {
	raw, _ := v.([]any)
	out := make([]int64, 0, len(raw))
	for _, x := range raw {
		out = append(out, int64(toFloat(x)))
	}
	return out
}

func toStrings(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func appendUnique(list []string, v string) []string {
	for _, have := range list {
		if have == v {
			return list
		}
	}
	return append(list, v)
}

func removeStr(list []string, v string) []string {
	var out []string
	for _, have := range list {
		if have != v {
			out = append(out, have)
		}
	}
	return out
}

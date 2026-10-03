package ghreview

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ghapi "github.com/hdweiss/git-issue/internal/bridge/github/api"
)

// fakeGitHub is a GitHub that remembers what was written to it.
//
// The recorded fixture in testdata covers reading; this covers the round trip,
// which is the different and load-bearing question: what a push sends has to
// read back as the same folded state, or a mirror never settles.
type fakeGitHub struct {
	t     *testing.T
	pulls map[string]*fakePull
	next  int
	clock time.Time
}

type fakePull struct {
	ID        string
	Number    int
	Title     string
	Body      string
	State     string
	Base      string
	Head      string
	HeadOID   string
	Milestone string
	Draft     bool
	Labels    []string
	Reviewers []string
	Comments  []*fakeComment
	Reviews   []*fakeReview
	Threads   []*fakeThread
	CreatedAt time.Time
}

type fakeComment struct {
	ID        string
	Body      string
	CreatedAt time.Time
}

type fakeReview struct {
	ID        string
	State     string
	Body      string
	CommitOID string
	CreatedAt time.Time
}

type fakeThread struct {
	ID        string
	Path      string
	Line      int
	StartLine int
	Side      string
	Resolved  bool
	Comments  []*fakeThreadComment
}

type fakeThreadComment struct {
	ID        string
	Body      string
	CreatedAt time.Time
	Path      string
	Side      string
	Line      int
	Commit    string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	return &fakeGitHub{
		t:     t,
		pulls: map[string]*fakePull{},
		clock: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

// tick advances the fake's clock, so every upstream object gets a distinct
// timestamp the way a real server's would.
func (f *fakeGitHub) tick() time.Time {
	f.clock = f.clock.Add(time.Minute)
	return f.clock
}

func (f *fakeGitHub) id(prefix string) string {
	f.next++
	return fmt.Sprintf("%s_%d", prefix, f.next)
}

func (f *fakeGitHub) client() *ghapi.Client {
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	return ghapi.New(srv.URL, "test-token")
}

func fakeTarget() ghapi.Target {
	t, err := ghapi.ParseTarget("https://github.com/hdweiss/git-issue")
	if err != nil {
		panic(err)
	}
	return t
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Error(err)
		return
	}

	data, err := f.dispatch(req.Query, req.Variables)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{{"message": err.Error()}},
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// dispatch matches on the mutation name, longest first where one name is a
// prefix of another — addPullRequestReviewThreadReply contains
// addPullRequestReviewThread, which contains addPullRequestReview.
func (f *fakeGitHub) dispatch(query string, vars map[string]any) (any, error) {
	input, _ := vars["input"].(map[string]any)

	switch {
	case strings.Contains(query, "nodes(ids:"):
		return f.nodes(vars), nil
	case strings.Contains(query, "defaultBranchRef"):
		return map[string]any{"repository": map[string]any{
			"defaultBranchRef": map[string]any{"name": "main"}}}, nil
	case strings.Contains(query, "repository(owner:") && strings.Contains(query, "{ id }"):
		return map[string]any{"repository": map[string]any{"id": "R_repo"}}, nil
	case strings.Contains(query, "labels(first:") && strings.Contains(query, "nodes { id name }"):
		return named("labels", fakeLabels, "LA_", "name"), nil
	case strings.Contains(query, "milestones(first:"):
		return named("milestones", fakeMilestones, "MI_", "title"), nil
	case strings.Contains(query, "user(login:"):
		return f.users(vars), nil

	case strings.Contains(query, "createPullRequest"):
		return f.createPull(input)
	case strings.Contains(query, "updatePullRequestReviewComment"):
		return f.updateThreadComment(input)
	case strings.Contains(query, "deletePullRequestReviewComment"):
		return f.deleteThreadComment(input)
	case strings.Contains(query, "updatePullRequestReview"):
		return f.updateReview(input)
	case strings.Contains(query, "updatePullRequest"):
		return f.updatePull(input)
	case strings.Contains(query, "closePullRequest"):
		return f.setState(input, "CLOSED")
	case strings.Contains(query, "reopenPullRequest"):
		return f.setState(input, "OPEN")
	case strings.Contains(query, "markPullRequestReadyForReview"):
		return f.setDraft(input, false)
	case strings.Contains(query, "convertPullRequestToDraft"):
		return f.setDraft(input, true)
	case strings.Contains(query, "requestReviews"):
		return f.requestReviews(input)
	case strings.Contains(query, "addPullRequestReviewThreadReply"):
		return f.addThreadReply(input)
	case strings.Contains(query, "addPullRequestReviewThread"):
		return f.addThread(input)
	case strings.Contains(query, "addPullRequestReview"):
		return f.addReview(input)
	case strings.Contains(query, "dismissPullRequestReview"):
		return f.dismissReview(input)
	case strings.Contains(query, "resolveReviewThread"):
		return f.setResolved(input, true)
	case strings.Contains(query, "unresolveReviewThread"):
		return f.setResolved(input, false)
	case strings.Contains(query, "addLabelsToLabelable"):
		return f.labels(input, true)
	case strings.Contains(query, "removeLabelsFromLabelable"):
		return f.labels(input, false)
	case strings.Contains(query, "addComment"):
		return f.addComment(input)
	case strings.Contains(query, "updateIssueComment"):
		return f.updateComment(input)
	case strings.Contains(query, "deleteIssueComment"):
		return f.deleteComment(input)
	}
	return nil, fmt.Errorf("fake github: unhandled query %.90s", query)
}

// fakeHead is the commit the fake reports as every pull request's head.
//
// A real createPullRequest answers with the tip of the branch it was given, and
// the tracker is authoritative for that: `head.sha` is not pushable, so whatever
// comes back on the next import wins. The fake has no branches, so it states one
// commit and the tests use the same constant.
const fakeHead = "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d"

// The repository's fixed vocabulary. A push resolves names against these, and a
// name that is not here is an error rather than something to invent.
var (
	fakeLabels     = []string{"bug", "area", "urgent"}
	fakeMilestones = []string{"v1", "v2"}
)

func named(field string, names []string, prefix, key string) any {
	nodes := []any{}
	for _, name := range names {
		nodes = append(nodes, map[string]any{"id": prefix + name, key: name})
	}
	return map[string]any{"repository": map[string]any{
		field: map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			"nodes":    nodes,
		}}}
}

func (f *fakeGitHub) users(vars map[string]any) any {
	out := map[string]any{}
	i := 0
	for {
		login, ok := vars[fmt.Sprintf("l%d", i)].(string)
		if !ok {
			break
		}
		// A word that is not a known person resolves to nothing, which is what a
		// team slug does — the case the push has to survive rather than fail on.
		if login != "nobody" {
			out[fmt.Sprintf("u%d", i)] = map[string]any{"id": "U_" + login, "login": login}
		}
		i++
	}
	if i == 0 {
		if login, ok := vars["login"].(string); ok {
			out["user"] = map[string]any{"id": "U_" + login}
		}
	}
	return out
}

func (f *fakeGitHub) pull(input map[string]any, key string) (*fakePull, error) {
	id, _ := input[key].(string)
	pr, ok := f.pulls[id]
	if !ok {
		return nil, fmt.Errorf("no such pull request: %q", id)
	}
	return pr, nil
}

func (f *fakeGitHub) createPull(input map[string]any) (any, error) {
	head, _ := input["headRefName"].(string)
	if head == "" {
		return nil, fmt.Errorf("createPullRequest: no head branch")
	}
	f.next++
	pr := &fakePull{
		ID:        fmt.Sprintf("PR_%d", f.next),
		Number:    f.next,
		Title:     str(input["title"]),
		Body:      str(input["body"]),
		State:     "OPEN",
		Base:      str(input["baseRefName"]),
		Head:      head,
		HeadOID:   fakeHead,
		CreatedAt: f.tick(),
	}
	if draft, ok := input["draft"].(bool); ok {
		pr.Draft = draft
	}
	f.pulls[pr.ID] = pr
	return map[string]any{"createPullRequest": map[string]any{
		"pullRequest": map[string]any{
			"id":     pr.ID,
			"url":    fmt.Sprintf("https://github.com/hdweiss/git-issue/pull/%d", pr.Number),
			"number": pr.Number,
		}}}, nil
}

func (f *fakeGitHub) updatePull(input map[string]any) (any, error) {
	pr, err := f.pull(input, "pullRequestId")
	if err != nil {
		return nil, err
	}
	if v, ok := input["title"]; ok {
		pr.Title = str(v)
	}
	if v, ok := input["body"]; ok {
		pr.Body = str(v)
	}
	if v, ok := input["baseRefName"]; ok {
		pr.Base = str(v)
	}
	if v, ok := input["milestoneId"]; ok {
		pr.Milestone = strings.TrimPrefix(str(v), "MI_")
	}
	return map[string]any{"updatePullRequest": map[string]any{"pullRequest": map[string]any{"id": pr.ID}}}, nil
}

func (f *fakeGitHub) setState(input map[string]any, state string) (any, error) {
	pr, err := f.pull(input, "pullRequestId")
	if err != nil {
		return nil, err
	}
	pr.State = state
	return map[string]any{"pullRequest": map[string]any{"id": pr.ID}}, nil
}

func (f *fakeGitHub) setDraft(input map[string]any, draft bool) (any, error) {
	pr, err := f.pull(input, "pullRequestId")
	if err != nil {
		return nil, err
	}
	pr.Draft = draft
	return map[string]any{"pullRequest": map[string]any{"id": pr.ID}}, nil
}

func (f *fakeGitHub) requestReviews(input map[string]any) (any, error) {
	pr, err := f.pull(input, "pullRequestId")
	if err != nil {
		return nil, err
	}
	pr.Reviewers = nil
	for _, id := range list(input["userIds"]) {
		pr.Reviewers = append(pr.Reviewers, strings.TrimPrefix(id, "U_"))
	}
	return map[string]any{"requestReviews": map[string]any{"pullRequest": map[string]any{"id": pr.ID}}}, nil
}

func (f *fakeGitHub) labels(input map[string]any, add bool) (any, error) {
	pr, err := f.pull(input, "labelableId")
	if err != nil {
		return nil, err
	}
	for _, id := range list(input["labelIds"]) {
		name := strings.TrimPrefix(id, "LA_")
		pr.Labels = remove(pr.Labels, name)
		if add {
			pr.Labels = append(pr.Labels, name)
		}
	}
	return map[string]any{"labelable": map[string]any{"id": pr.ID}}, nil
}

func (f *fakeGitHub) addComment(input map[string]any) (any, error) {
	pr, err := f.pull(input, "subjectId")
	if err != nil {
		return nil, err
	}
	c := &fakeComment{ID: f.id("IC"), Body: str(input["body"]), CreatedAt: f.tick()}
	pr.Comments = append(pr.Comments, c)
	return map[string]any{"addComment": map[string]any{
		"commentEdge": map[string]any{"node": map[string]any{"id": c.ID}}}}, nil
}

func (f *fakeGitHub) updateComment(input map[string]any) (any, error) {
	for _, pr := range f.pulls {
		for _, c := range pr.Comments {
			if c.ID == str(input["id"]) {
				c.Body = str(input["body"])
				return map[string]any{"updateIssueComment": map[string]any{"clientMutationId": ""}}, nil
			}
		}
	}
	return nil, fmt.Errorf("no such comment: %q", str(input["id"]))
}

func (f *fakeGitHub) deleteComment(input map[string]any) (any, error) {
	for _, pr := range f.pulls {
		for i, c := range pr.Comments {
			if c.ID == str(input["id"]) {
				pr.Comments = append(pr.Comments[:i], pr.Comments[i+1:]...)
				return map[string]any{"deleteIssueComment": map[string]any{"clientMutationId": ""}}, nil
			}
		}
	}
	return nil, fmt.Errorf("no such comment: %q", str(input["id"]))
}

func (f *fakeGitHub) addReview(input map[string]any) (any, error) {
	pr, err := f.pull(input, "pullRequestId")
	if err != nil {
		return nil, err
	}
	state := map[string]string{
		ghapi.EventApprove:        ghapi.ReviewApproved,
		ghapi.EventRequestChanges: ghapi.ReviewChangesRequested,
		ghapi.EventComment:        ghapi.ReviewCommented,
	}[str(input["event"])]
	if state == "" {
		return nil, fmt.Errorf("addPullRequestReview: unknown event %q", str(input["event"]))
	}
	r := &fakeReview{
		ID:        f.id("PRR"),
		State:     state,
		Body:      str(input["body"]),
		CommitOID: str(input["commitOID"]),
		CreatedAt: f.tick(),
	}
	pr.Reviews = append(pr.Reviews, r)
	return map[string]any{"addPullRequestReview": map[string]any{
		"pullRequestReview": map[string]any{"id": r.ID}}}, nil
}

func (f *fakeGitHub) review(id string) (*fakeReview, error) {
	for _, pr := range f.pulls {
		for _, r := range pr.Reviews {
			if r.ID == id {
				return r, nil
			}
		}
	}
	return nil, fmt.Errorf("no such review: %q", id)
}

func (f *fakeGitHub) dismissReview(input map[string]any) (any, error) {
	r, err := f.review(str(input["pullRequestReviewId"]))
	if err != nil {
		return nil, err
	}
	if str(input["message"]) == "" {
		return nil, fmt.Errorf("dismissPullRequestReview: a message is required")
	}
	r.State = ghapi.ReviewDismissed
	return map[string]any{"dismissPullRequestReview": map[string]any{
		"pullRequestReview": map[string]any{"id": r.ID}}}, nil
}

func (f *fakeGitHub) updateReview(input map[string]any) (any, error) {
	r, err := f.review(str(input["pullRequestReviewId"]))
	if err != nil {
		return nil, err
	}
	r.Body = str(input["body"])
	return map[string]any{"updatePullRequestReview": map[string]any{
		"pullRequestReview": map[string]any{"id": r.ID}}}, nil
}

func (f *fakeGitHub) addThread(input map[string]any) (any, error) {
	pr, err := f.pull(input, "pullRequestId")
	if err != nil {
		return nil, err
	}
	line := num(input["line"])
	if line == 0 || str(input["path"]) == "" {
		return nil, fmt.Errorf("addPullRequestReviewThread: a thread needs a path and a line")
	}
	t := &fakeThread{
		ID:        f.id("PRRT"),
		Path:      str(input["path"]),
		Line:      line,
		StartLine: num(input["startLine"]),
		Side:      str(input["side"]),
	}
	c := &fakeThreadComment{
		ID: f.id("PRRC"), Body: str(input["body"]), CreatedAt: f.tick(),
		Path: t.Path, Side: t.Side, Line: line, Commit: pr.HeadOID,
	}
	t.Comments = append(t.Comments, c)
	pr.Threads = append(pr.Threads, t)
	return map[string]any{"addPullRequestReviewThread": map[string]any{
		"thread": map[string]any{
			"id":       t.ID,
			"comments": map[string]any{"nodes": []any{map[string]any{"id": c.ID}}},
		}}}, nil
}

func (f *fakeGitHub) thread(id string) (*fakePull, *fakeThread, error) {
	for _, pr := range f.pulls {
		for _, t := range pr.Threads {
			if t.ID == id {
				return pr, t, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("no such review thread: %q", id)
}

func (f *fakeGitHub) addThreadReply(input map[string]any) (any, error) {
	pr, t, err := f.thread(str(input["pullRequestReviewThreadId"]))
	if err != nil {
		return nil, err
	}
	c := &fakeThreadComment{
		ID: f.id("PRRC"), Body: str(input["body"]), CreatedAt: f.tick(),
		Path: t.Path, Side: t.Side, Line: t.Line, Commit: pr.HeadOID,
	}
	t.Comments = append(t.Comments, c)
	return map[string]any{"addPullRequestReviewThreadReply": map[string]any{
		"comment": map[string]any{"id": c.ID}}}, nil
}

func (f *fakeGitHub) setResolved(input map[string]any, resolved bool) (any, error) {
	_, t, err := f.thread(str(input["threadId"]))
	if err != nil {
		return nil, err
	}
	t.Resolved = resolved
	return map[string]any{"thread": map[string]any{"id": t.ID}}, nil
}

func (f *fakeGitHub) updateThreadComment(input map[string]any) (any, error) {
	for _, pr := range f.pulls {
		for _, t := range pr.Threads {
			for _, c := range t.Comments {
				if c.ID == str(input["pullRequestReviewCommentId"]) {
					c.Body = str(input["body"])
					return map[string]any{"updatePullRequestReviewComment": map[string]any{
						"pullRequestReviewComment": map[string]any{"id": c.ID}}}, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("no such review comment: %q", str(input["pullRequestReviewCommentId"]))
}

func (f *fakeGitHub) deleteThreadComment(input map[string]any) (any, error) {
	for _, pr := range f.pulls {
		for _, t := range pr.Threads {
			for i, c := range t.Comments {
				if c.ID == str(input["id"]) {
					t.Comments = append(t.Comments[:i], t.Comments[i+1:]...)
					return map[string]any{"deletePullRequestReviewComment": map[string]any{
						"clientMutationId": ""}}, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("no such review comment: %q", str(input["id"]))
}

// nodes answers the nodes(ids:) query a plan and a read-back both make.
func (f *fakeGitHub) nodes(vars map[string]any) any {
	var out []any
	for _, id := range list(vars["ids"]) {
		pr, ok := f.pulls[id]
		if !ok {
			out = append(out, nil)
			continue
		}
		out = append(out, f.render(pr))
	}
	return map[string]any{"nodes": out}
}

// render serialises one pull request into the shape the read path parses. It is
// the fake's whole contract with internal/bridge/github/api: everything else here is
// bookkeeping, and this is the part that has to match the query.
func (f *fakeGitHub) render(pr *fakePull) any {
	labels := []any{}
	for _, l := range pr.Labels {
		labels = append(labels, map[string]any{"name": l})
	}
	reviewers := []any{}
	for _, r := range pr.Reviewers {
		reviewers = append(reviewers, map[string]any{
			"requestedReviewer": map[string]any{"login": r}})
	}
	comments := []any{}
	for _, c := range pr.Comments {
		comments = append(comments, map[string]any{
			"id": c.ID, "body": c.Body, "createdAt": c.CreatedAt,
			"author": map[string]any{"login": "pusher"},
		})
	}
	reviews := []any{}
	for _, r := range pr.Reviews {
		node := map[string]any{
			"id": r.ID, "state": r.State, "body": r.Body,
			"createdAt": r.CreatedAt, "author": map[string]any{"login": "pusher"},
		}
		if r.CommitOID != "" {
			node["commit"] = map[string]any{"oid": r.CommitOID}
		}
		reviews = append(reviews, node)
	}
	threads := []any{}
	for _, t := range pr.Threads {
		entries := []any{}
		for _, c := range t.Comments {
			entry := map[string]any{
				"id": c.ID, "body": c.Body, "createdAt": c.CreatedAt,
				"author": map[string]any{"login": "pusher"},
				"path":   c.Path, "originalLine": c.Line,
			}
			if c.Commit != "" {
				entry["originalCommit"] = map[string]any{"oid": c.Commit}
			}
			entries = append(entries, entry)
		}
		threads = append(threads, map[string]any{
			"id": t.ID, "isResolved": t.Resolved, "isOutdated": false,
			"path": t.Path, "diffSide": t.Side, "line": t.Line,
			"comments": map[string]any{"nodes": entries},
		})
	}

	node := map[string]any{
		"id": pr.ID, "number": pr.Number,
		"url":       fmt.Sprintf("https://github.com/hdweiss/git-issue/pull/%d", pr.Number),
		"title":     pr.Title,
		"body":      pr.Body,
		"createdAt": pr.CreatedAt,
		"updatedAt": f.clock,
		"state":     pr.State,
		"isDraft":   pr.Draft,
		"locked":    false,

		"baseRefName": pr.Base,
		"headRefName": strings.TrimPrefix(pr.Head, "contributor:"),
		"headRefOid":  pr.HeadOID,
		"author":      map[string]any{"login": "pusher"},

		"labels":         map[string]any{"nodes": labels},
		"reviewRequests": map[string]any{"nodes": reviewers},
		"comments": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
			"nodes":    comments,
		},
		"reviews": map[string]any{"nodes": reviews},
		"reviewThreads": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
			"nodes":    threads,
		},
		"closingIssuesReferences": map[string]any{"nodes": []any{}},
		"commits":                 map[string]any{"nodes": []any{}},
	}
	if pr.Milestone != "" {
		node["milestone"] = map[string]any{"title": pr.Milestone}
	}
	if owner, branch, ok := strings.Cut(pr.Head, ":"); ok {
		node["headRepository"] = map[string]any{
			"name": "git-issue", "owner": map[string]any{"login": owner}}
		node["headRefName"] = branch
	}
	return node
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func list(v any) []string {
	var out []string
	switch vs := v.(type) {
	case []string:
		return vs
	case []any:
		for _, e := range vs {
			out = append(out, str(e))
		}
	}
	return out
}

func remove(values []string, want string) []string {
	out := values[:0]
	for _, v := range values {
		if v != want {
			out = append(out, v)
		}
	}
	return out
}

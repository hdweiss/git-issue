package ghissue

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
// The recorded-response fixtures cover reading; this covers the round trip,
// which is a different question and the one write-back rests on: what a push
// sends must read back as the same folded state, or a mirror never settles.
type fakeGitHub struct {
	t      *testing.T
	issues map[string]*fakeIssue
	next   int
	clock  time.Time
}

type fakeIssue struct {
	ID          string
	Parent      string
	BlockedBy   []string
	Number      int
	Title       string
	Body        string
	State       string
	StateReason string
	Milestone   string
	Labels      []string
	Assignees   []string
	Comments    []*fakeComment
	CreatedAt   time.Time
	// Timeline entries the fake records for the changes it is asked to make,
	// so an import reads back a history rather than only current state.
	Timeline []fakeEvent
}

type fakeComment struct {
	ID        string
	Body      string
	CreatedAt time.Time
}

type fakeEvent struct {
	Typename  string
	ID        string
	CreatedAt time.Time
	Label     string
	Assignee  string
	Prev      string
	Curr      string
	Reason    string
	Milestone string
	Target    string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	return &fakeGitHub{
		t:      t,
		issues: map[string]*fakeIssue{},
		clock:  time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

// tick advances the fake's clock, so every upstream object gets a distinct
// timestamp the way a real server's would.
func (f *fakeGitHub) tick() time.Time {
	f.clock = f.clock.Add(time.Minute)
	return f.clock
}

func (f *fakeGitHub) client() *ghapi.Client {
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	return ghapi.New(srv.URL, "test-token")
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

func (f *fakeGitHub) dispatch(query string, vars map[string]any) (any, error) {
	input, _ := vars["input"].(map[string]any)

	switch {
	case strings.Contains(query, "nodes(ids:"):
		return f.nodes(vars), nil
	case strings.Contains(query, "repository(owner:") && strings.Contains(query, "{ id }"):
		return map[string]any{"repository": map[string]any{"id": "R_repo"}}, nil
	case strings.Contains(query, "labels(first:") && strings.Contains(query, "nodes { id name }"):
		return f.repoLabels(), nil
	case strings.Contains(query, "milestones(first:"):
		return f.repoMilestones(), nil
	case strings.Contains(query, "user(login:"):
		return f.users(vars), nil
	case strings.Contains(query, "createIssue"):
		return f.createIssue(input), nil
	case strings.Contains(query, "updateIssue"):
		return f.updateIssue(input)
	case strings.Contains(query, "closeIssue"):
		return f.setState(input, ghapi.StateClosed)
	case strings.Contains(query, "reopenIssue"):
		return f.setState(input, ghapi.StateOpen)
	case strings.Contains(query, "addLabelsToLabelable"):
		return f.labels(input, true)
	case strings.Contains(query, "removeLabelsFromLabelable"):
		return f.labels(input, false)
	case strings.Contains(query, "addAssigneesToAssignable"):
		return f.assignees(input, true)
	case strings.Contains(query, "removeAssigneesFromAssignable"):
		return f.assignees(input, false)
	case strings.Contains(query, "addSubIssue"):
		return f.subIssue(input, true)
	case strings.Contains(query, "removeSubIssue"):
		return f.subIssue(input, false)
	case strings.Contains(query, "addBlockedBy"):
		return f.blockedBy(input, true)
	case strings.Contains(query, "removeBlockedBy"):
		return f.blockedBy(input, false)
	case strings.Contains(query, "addComment"):
		return f.addComment(input)
	case strings.Contains(query, "updateIssueComment"):
		return f.updateComment(input)
	case strings.Contains(query, "deleteIssueComment"):
		return f.deleteComment(input)
	}
	return nil, fmt.Errorf("fake github: unhandled query %.80s", query)
}

// The repository's fixed vocabulary. A push resolves names against these, and a
// name that is not here is an error rather than something to invent.
var (
	fakeLabels     = []string{"bug", "storage", "confirmed", "urgent"}
	fakeMilestones = []string{"v1", "v2"}
)

func (f *fakeGitHub) repoLabels() any {
	nodes := []any{}
	for _, name := range fakeLabels {
		nodes = append(nodes, map[string]any{"id": "LA_" + name, "name": name})
	}
	return map[string]any{"repository": map[string]any{
		"labels": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			"nodes":    nodes,
		}}}
}

func (f *fakeGitHub) repoMilestones() any {
	nodes := []any{}
	for _, title := range fakeMilestones {
		nodes = append(nodes, map[string]any{"id": "MI_" + title, "title": title})
	}
	return map[string]any{"repository": map[string]any{
		"milestones": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			"nodes":    nodes,
		}}}
}

func (f *fakeGitHub) users(vars map[string]any) any {
	out := map[string]any{}
	i := 0
	for {
		key := fmt.Sprintf("l%d", i)
		login, ok := vars[key].(string)
		if !ok {
			break
		}
		out[fmt.Sprintf("u%d", i)] = map[string]any{"id": "U_" + login, "login": login}
		i++
	}
	if i == 0 {
		if login, ok := vars["login"].(string); ok {
			out["user"] = map[string]any{"id": "U_" + login}
		}
	}
	return out
}

func (f *fakeGitHub) createIssue(input map[string]any) any {
	f.next++
	now := f.tick()
	issue := &fakeIssue{
		ID:        fmt.Sprintf("I_fake%d", f.next),
		Number:    f.next,
		Title:     str(input["title"]),
		Body:      str(input["body"]),
		State:     ghapi.StateOpen,
		CreatedAt: now,
	}
	for _, id := range strs(input["labelIds"]) {
		issue.Labels = append(issue.Labels, strings.TrimPrefix(id, "LA_"))
	}
	for _, id := range strs(input["assigneeIds"]) {
		issue.Assignees = append(issue.Assignees, strings.TrimPrefix(id, "U_"))
	}
	if m := str(input["milestoneId"]); m != "" {
		issue.Milestone = strings.TrimPrefix(m, "MI_")
	}
	// A parent given at creation is a link the issue was born with, so it
	// raises no timeline event — which is exactly why the pusher sends it here
	// rather than as a sub-issue added afterwards.
	issue.Parent = str(input["parentIssueId"])
	f.issues[issue.ID] = issue
	return map[string]any{"createIssue": map[string]any{"issue": map[string]any{
		"id":  issue.ID,
		"url": fmt.Sprintf("https://github.com/acme/git-issue/issues/%d", issue.Number),
	}}}
}

func (f *fakeGitHub) find(id string) (*fakeIssue, error) {
	issue, ok := f.issues[id]
	if !ok {
		return nil, fmt.Errorf("no such issue %s", id)
	}
	return issue, nil
}

func (f *fakeGitHub) updateIssue(input map[string]any) (any, error) {
	issue, err := f.find(str(input["id"]))
	if err != nil {
		return nil, err
	}
	if title, ok := input["title"]; ok {
		// A rename carries the previous title, which is the one field GitHub
		// keeps complete history for and the import relies on.
		issue.Timeline = append(issue.Timeline, fakeEvent{
			Typename: ghapi.Renamed, ID: f.eventID(), CreatedAt: f.tick(),
			Prev: issue.Title, Curr: str(title),
		})
		issue.Title = str(title)
	}
	if body, ok := input["body"]; ok {
		issue.Body = str(body)
	}
	if m, ok := input["milestoneId"]; ok {
		if m == nil {
			issue.Milestone = ""
			issue.Timeline = append(issue.Timeline, fakeEvent{
				Typename: ghapi.Demilestoned, ID: f.eventID(), CreatedAt: f.tick(),
			})
		} else {
			issue.Milestone = strings.TrimPrefix(str(m), "MI_")
			issue.Timeline = append(issue.Timeline, fakeEvent{
				Typename: ghapi.Milestoned, ID: f.eventID(), CreatedAt: f.tick(),
				Milestone: issue.Milestone,
			})
		}
	}
	return map[string]any{"updateIssue": map[string]any{"issue": map[string]any{"id": issue.ID}}}, nil
}

func (f *fakeGitHub) setState(input map[string]any, state string) (any, error) {
	issue, err := f.find(str(input["issueId"]))
	if err != nil {
		return nil, err
	}
	issue.State = state
	kind := ghapi.Reopened
	if state == ghapi.StateClosed {
		kind = ghapi.Closed
		issue.StateReason = str(input["stateReason"])
	} else {
		issue.StateReason = ""
	}
	issue.Timeline = append(issue.Timeline, fakeEvent{
		Typename: kind, ID: f.eventID(), CreatedAt: f.tick(), Reason: issue.StateReason,
	})
	return map[string]any{"issue": map[string]any{"id": issue.ID}}, nil
}

func (f *fakeGitHub) labels(input map[string]any, add bool) (any, error) {
	issue, err := f.find(str(input["labelableId"]))
	if err != nil {
		return nil, err
	}
	for _, id := range strs(input["labelIds"]) {
		name := strings.TrimPrefix(id, "LA_")
		kind := ghapi.Unlabeled
		if add {
			issue.Labels = appendUnique(issue.Labels, name)
			kind = ghapi.Labeled
		} else {
			issue.Labels = remove(issue.Labels, name)
		}
		issue.Timeline = append(issue.Timeline, fakeEvent{
			Typename: kind, ID: f.eventID(), CreatedAt: f.tick(), Label: name,
		})
	}
	return map[string]any{"clientMutationId": nil}, nil
}

func (f *fakeGitHub) assignees(input map[string]any, add bool) (any, error) {
	issue, err := f.find(str(input["assignableId"]))
	if err != nil {
		return nil, err
	}
	for _, id := range strs(input["assigneeIds"]) {
		login := strings.TrimPrefix(id, "U_")
		kind := ghapi.Unassigned
		if add {
			issue.Assignees = appendUnique(issue.Assignees, login)
			kind = ghapi.Assigned
		} else {
			issue.Assignees = remove(issue.Assignees, login)
		}
		issue.Timeline = append(issue.Timeline, fakeEvent{
			Typename: kind, ID: f.eventID(), CreatedAt: f.tick(), Assignee: login,
		})
	}
	return map[string]any{"clientMutationId": nil}, nil
}

// subIssue moves a parent link. GitHub addresses these from the parent, so the
// event lands on the child — which is the end that stores the link here.
func (f *fakeGitHub) subIssue(input map[string]any, add bool) (any, error) {
	parent, err := f.find(str(input["issueId"]))
	if err != nil {
		return nil, err
	}
	child, err := f.find(str(input["subIssueId"]))
	if err != nil {
		return nil, err
	}
	kind := ghapi.ParentRemoved
	if add {
		if child.Parent != "" && child.Parent != parent.ID {
			return nil, fmt.Errorf("%s already has a parent", child.ID)
		}
		child.Parent, kind = parent.ID, ghapi.ParentAdded
	} else {
		child.Parent = ""
	}
	child.Timeline = append(child.Timeline, fakeEvent{
		Typename: kind, ID: f.eventID(), CreatedAt: f.tick(), Target: parent.ID,
	})
	return map[string]any{"clientMutationId": nil}, nil
}

// blockedBy moves a dependency, addressed from the blocked issue.
func (f *fakeGitHub) blockedBy(input map[string]any, add bool) (any, error) {
	blocked, err := f.find(str(input["issueId"]))
	if err != nil {
		return nil, err
	}
	blocking, err := f.find(str(input["blockingIssueId"]))
	if err != nil {
		return nil, err
	}
	kind := ghapi.BlockedByRemoved
	if add {
		blocked.BlockedBy, kind = appendUnique(blocked.BlockedBy, blocking.ID), ghapi.BlockedByAdded
	} else {
		blocked.BlockedBy = remove(blocked.BlockedBy, blocking.ID)
	}
	blocked.Timeline = append(blocked.Timeline, fakeEvent{
		Typename: kind, ID: f.eventID(), CreatedAt: f.tick(), Target: blocking.ID,
	})
	return map[string]any{"clientMutationId": nil}, nil
}

func (f *fakeGitHub) addComment(input map[string]any) (any, error) {
	issue, err := f.find(str(input["subjectId"]))
	if err != nil {
		return nil, err
	}
	c := &fakeComment{
		ID:        fmt.Sprintf("IC_fake%d", len(issue.Comments)+1) + issue.ID,
		Body:      str(input["body"]),
		CreatedAt: f.tick(),
	}
	issue.Comments = append(issue.Comments, c)
	return map[string]any{"addComment": map[string]any{
		"commentEdge": map[string]any{"node": map[string]any{"id": c.ID}},
	}}, nil
}

func (f *fakeGitHub) updateComment(input map[string]any) (any, error) {
	id := str(input["id"])
	for _, issue := range f.issues {
		for _, c := range issue.Comments {
			if c.ID == id {
				c.Body = str(input["body"])
				return map[string]any{"clientMutationId": nil}, nil
			}
		}
	}
	return nil, fmt.Errorf("no such comment %s", id)
}

func (f *fakeGitHub) deleteComment(input map[string]any) (any, error) {
	id := str(input["id"])
	for _, issue := range f.issues {
		for i, c := range issue.Comments {
			if c.ID == id {
				issue.Comments = append(issue.Comments[:i], issue.Comments[i+1:]...)
				return map[string]any{"clientMutationId": nil}, nil
			}
		}
	}
	return nil, fmt.Errorf("no such comment %s", id)
}

// nodes answers the query a push uses to read the tracker's current state.
func (f *fakeGitHub) nodes(vars map[string]any) any {
	var out []any
	for _, raw := range strs(vars["ids"]) {
		issue, ok := f.issues[raw]
		if !ok {
			out = append(out, nil)
			continue
		}
		out = append(out, f.render(issue))
	}
	return map[string]any{"nodes": out}
}

func (f *fakeGitHub) render(issue *fakeIssue) map[string]any {
	labels := []any{}
	for _, name := range issue.Labels {
		labels = append(labels, map[string]any{"name": name})
	}
	assignees := []any{}
	for _, login := range issue.Assignees {
		assignees = append(assignees, map[string]any{"login": login})
	}
	comments := []any{}
	for _, c := range issue.Comments {
		comments = append(comments, map[string]any{
			"id": c.ID, "body": c.Body,
			"createdAt": c.CreatedAt.Format(time.RFC3339),
			"author":    map[string]any{"login": "pusher"},
		})
	}
	timeline := []any{}
	for _, e := range issue.Timeline {
		item := map[string]any{
			"__typename": e.Typename,
			"id":         e.ID,
			"createdAt":  e.CreatedAt.Format(time.RFC3339),
			"actor":      map[string]any{"login": "pusher"},
		}
		if e.Label != "" {
			item["label"] = map[string]any{"name": e.Label}
		}
		if e.Assignee != "" {
			item["assignee"] = map[string]any{"login": e.Assignee}
		}
		if e.Prev != "" {
			item["previousTitle"], item["currentTitle"] = e.Prev, e.Curr
		}
		if e.Reason != "" {
			item["stateReason"] = e.Reason
		}
		if e.Milestone != "" {
			item["milestoneTitle"] = e.Milestone
		}
		// The two relation ends never appear on one event, so the fake fills in
		// whichever field the type carries.
		if e.Target != "" {
			switch e.Typename {
			case ghapi.ParentAdded, ghapi.ParentRemoved:
				item["parent"] = map[string]any{"id": e.Target}
			default:
				item["blockingIssue"] = map[string]any{"id": e.Target}
			}
		}
		timeline = append(timeline, item)
	}

	var milestone, parent any
	if issue.Milestone != "" {
		milestone = map[string]any{"title": issue.Milestone}
	}
	if issue.Parent != "" {
		parent = map[string]any{"id": issue.Parent}
	}
	return map[string]any{
		"id": issue.ID, "number": issue.Number,
		"url":       fmt.Sprintf("https://github.com/acme/git-issue/issues/%d", issue.Number),
		"title":     issue.Title,
		"body":      issue.Body,
		"createdAt": issue.CreatedAt.Format(time.RFC3339),
		"updatedAt": f.clock.Format(time.RFC3339),
		"state":     issue.State, "stateReason": issue.StateReason,
		"locked": false, "activeLockReason": "",
		"author":      map[string]any{"login": "pusher"},
		"milestone":   milestone,
		"parent":      parent,
		"duplicateOf": nil,
		"labels":      conn(labels),
		"assignees":   conn(assignees),
		"comments":    conn(comments),
		"timelineItems": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			"nodes":    timeline,
		},
	}
}

func conn(nodes []any) map[string]any {
	return map[string]any{
		"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
		"nodes":    nodes,
	}
}

func (f *fakeGitHub) eventID() string {
	f.next++
	return fmt.Sprintf("TE_fake%d", f.next)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strs(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	if out == nil {
		if typed, ok := v.([]string); ok {
			return typed
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

func remove(list []string, v string) []string {
	out := list[:0]
	for _, have := range list {
		if have != v {
			out = append(out, have)
		}
	}
	return out
}

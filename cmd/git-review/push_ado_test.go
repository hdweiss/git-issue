package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// statefulADO is a small in-memory Azure DevOps repository: one pull request,
// the read path a pull needs, and the write path a push exercises. It is
// stateful because a push test's question is what the tracker says *after* the
// write — the same reason internal/bridge/ado/review's fake is.
type statefulADO struct {
	t       *testing.T
	srv     *httptest.Server
	nextTID int
	nextCID int
	pr      *sadoPR
}

type sadoPR struct {
	id                         int
	title, description, status string
	base, source, headSHA      string
	draft                      bool
	labels                     []string
	vote                       int
	threads                    []*sadoThread
}

type sadoThread struct {
	id       int
	status   string
	path     string
	first    int
	comments []*sadoComment
}

type sadoComment struct {
	id      int
	content string
	deleted bool
}

var (
	saPR       = regexp.MustCompile(`/pullrequests/(\d+)$`)
	saThreads  = regexp.MustCompile(`/pullRequests/(\d+)/threads$`)
	saThread   = regexp.MustCompile(`/pullRequests/(\d+)/threads/(\d+)$`)
	saComments = regexp.MustCompile(`/pullRequests/(\d+)/threads/(\d+)/comments$`)
	saComment  = regexp.MustCompile(`/pullRequests/(\d+)/threads/(\d+)/comments/(\d+)$`)
	saReviewer = regexp.MustCompile(`/pullRequests/(\d+)/reviewers/([^/]+)$`)
	saLabels   = regexp.MustCompile(`/pullRequests/(\d+)/labels$`)
	saLabel    = regexp.MustCompile(`/pullRequests/(\d+)/labels/([^/]+)$`)
	saSub      = regexp.MustCompile(`/pullRequests/(\d+)/([a-z]+)$`)
)

func newStatefulADO(t *testing.T) *statefulADO {
	f := &statefulADO{
		t: t, nextTID: 10, nextCID: 100,
		pr: &sadoPR{
			id: 42, title: "Fix the area subtree walk", description: "The picker dropped the subtree.",
			status: "active", base: "main", source: "feature/walk",
			headSHA: "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d",
		},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *statefulADO) target() string {
	return "ado:" + f.srv.URL + "/testorg/MyProj/_git/repo"
}

func (f *statefulADO) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	lower := strings.ToLower(path)

	switch {
	case strings.HasSuffix(lower, "/_apis/connectiondata"):
		w.Write([]byte(`{"authenticatedUser":{"id":"self-guid","providerDisplayName":"Push Bot","properties":{"Account":{"$value":"bot@example.com"}}}}`))
	case strings.Contains(lower, "/_apis/projects/"):
		w.Write([]byte(`{"name":"MyProj"}`))
	case strings.HasSuffix(lower, "/repositories/repo"):
		w.Write([]byte(`{"defaultBranch":"refs/heads/main"}`))

	case strings.HasSuffix(lower, "/pullrequests") && r.Method == http.MethodGet:
		fmt.Fprintf(w, `{"count":1,"value":[%s]}`, f.prJSON())
	case saPR.MatchString(lower) && r.Method == http.MethodGet:
		w.Write([]byte(f.prJSON()))
	case saPR.MatchString(lower) && r.Method == http.MethodPatch:
		f.patchPR(body)
		w.Write([]byte(f.prJSON()))
	case strings.HasSuffix(lower, "/pullrequests") && r.Method == http.MethodPost:
		f.t.Fatalf("stateful ado: unexpected pull request creation")

	case saComment.MatchString(path):
		m := saComment.FindStringSubmatch(path)
		f.commentWrite(r.Method, atoiSA(m[2]), atoiSA(m[3]), body)
		w.Write([]byte(`{}`))
	case saComments.MatchString(path) && r.Method == http.MethodPost:
		m := saComments.FindStringSubmatch(path)
		fmt.Fprintf(w, `{"id":%d}`, f.addComment(atoiSA(m[2]), body))
	case saThread.MatchString(path) && r.Method == http.MethodPatch:
		m := saThread.FindStringSubmatch(path)
		f.setThreadStatus(atoiSA(m[2]), body)
		w.Write([]byte(`{}`))
	case saThreads.MatchString(path) && r.Method == http.MethodPost:
		tid, cid := f.addThread(body)
		fmt.Fprintf(w, `{"id":%d,"comments":[{"id":%d}]}`, tid, cid)
	case saThreads.MatchString(path) && r.Method == http.MethodGet:
		w.Write([]byte(f.threadsJSON()))

	case saReviewer.MatchString(path) && r.Method == http.MethodPut:
		var in struct {
			Vote int `json:"vote"`
		}
		json.Unmarshal(body, &in)
		f.pr.vote = in.Vote
		fmt.Fprintf(w, `{"vote":%d}`, in.Vote)

	case saLabels.MatchString(path) && r.Method == http.MethodPost:
		var in struct {
			Name string `json:"name"`
		}
		json.Unmarshal(body, &in)
		f.pr.labels = append(f.pr.labels, in.Name)
		w.Write([]byte(`{}`))
	case saLabel.MatchString(path) && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)

	case saSub.MatchString(path) && r.Method == http.MethodGet:
		w.Write([]byte(`{"value":[]}`))

	default:
		f.t.Errorf("stateful ado: unexpected %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *statefulADO) patchPR(body []byte) {
	var in map[string]any
	json.Unmarshal(body, &in)
	if v, ok := in["title"].(string); ok {
		f.pr.title = v
	}
	if v, ok := in["description"].(string); ok {
		f.pr.description = v
	}
	if v, ok := in["status"].(string); ok {
		f.pr.status = v
	}
}

func (f *statefulADO) addThread(body []byte) (int, int) {
	var in struct {
		Comments []struct {
			Content string `json:"content"`
		} `json:"comments"`
		ThreadContext *struct {
			FilePath       string `json:"filePath"`
			RightFileStart *struct {
				Line int `json:"line"`
			} `json:"rightFileStart"`
		} `json:"threadContext"`
	}
	json.Unmarshal(body, &in)
	f.nextTID++
	th := &sadoThread{id: f.nextTID, status: "active"}
	if in.ThreadContext != nil {
		th.path = in.ThreadContext.FilePath
		if in.ThreadContext.RightFileStart != nil {
			th.first = in.ThreadContext.RightFileStart.Line
		}
	}
	f.nextCID++
	c := &sadoComment{id: f.nextCID}
	if len(in.Comments) > 0 {
		c.content = in.Comments[0].Content
	}
	th.comments = append(th.comments, c)
	f.pr.threads = append(f.pr.threads, th)
	return th.id, c.id
}

func (f *statefulADO) addComment(tid int, body []byte) int {
	var in struct {
		Content string `json:"content"`
	}
	json.Unmarshal(body, &in)
	for _, th := range f.pr.threads {
		if th.id == tid {
			f.nextCID++
			c := &sadoComment{id: f.nextCID, content: in.Content}
			th.comments = append(th.comments, c)
			return c.id
		}
	}
	f.t.Fatalf("stateful ado: reply to unknown thread %d", tid)
	return 0
}

func (f *statefulADO) commentWrite(method string, tid, cid int, body []byte) {
	for _, th := range f.pr.threads {
		if th.id != tid {
			continue
		}
		for _, c := range th.comments {
			if c.id != cid {
				continue
			}
			if method == http.MethodPatch {
				var in struct {
					Content string `json:"content"`
				}
				json.Unmarshal(body, &in)
				c.content = in.Content
			} else {
				c.deleted = true
			}
		}
	}
}

func (f *statefulADO) setThreadStatus(tid int, body []byte) {
	var in struct {
		Status string `json:"status"`
	}
	json.Unmarshal(body, &in)
	for _, th := range f.pr.threads {
		if th.id == tid {
			th.status = in.Status
		}
	}
}

func (f *statefulADO) prJSON() string {
	var labels []string
	for _, l := range f.pr.labels {
		labels = append(labels, fmt.Sprintf(`{"name":%q,"active":true}`, l))
	}
	reviewers := ""
	if f.pr.vote != 0 {
		reviewers = fmt.Sprintf(`{"displayName":"Push Bot","uniqueName":"bot@example.com","id":"self-guid","vote":%d}`, f.pr.vote)
	}
	return fmt.Sprintf(`{
		"pullRequestId":%d,"status":%q,"title":%q,"description":%q,"isDraft":%t,
		"createdBy":{"displayName":"Author","uniqueName":"author@example.com","id":"author-guid"},
		"creationDate":"2026-01-01T00:00:00Z",
		"sourceRefName":"refs/heads/%s","targetRefName":"refs/heads/%s",
		"lastMergeSourceCommit":{"commitId":%q},
		"labels":[%s],"reviewers":[%s],
		"_links":{"web":{"href":"https://example/pr/%d"}}
	}`, f.pr.id, f.pr.status, f.pr.title, f.pr.description, f.pr.draft,
		f.pr.source, f.pr.base, f.pr.headSHA, strings.Join(labels, ","), reviewers, f.pr.id)
}

func (f *statefulADO) threadsJSON() string {
	var threads []string
	for _, th := range f.pr.threads {
		var comments []string
		for _, c := range th.comments {
			comments = append(comments, fmt.Sprintf(
				`{"id":%d,"parentCommentId":0,"content":%q,"commentType":"text",`+
					`"author":{"displayName":"Push Bot","uniqueName":"bot@example.com","id":"self-guid"},`+
					`"publishedDate":"2026-01-02T00:00:00Z","isDeleted":%t}`,
				c.id, c.content, c.deleted))
		}
		ctx := ""
		if th.path != "" {
			ctx = fmt.Sprintf(`,"threadContext":{"filePath":%q,"rightFileStart":{"line":%d,"offset":1},"rightFileEnd":{"line":%d,"offset":1}}`,
				th.path, th.first, th.first)
		}
		threads = append(threads, fmt.Sprintf(
			`{"id":%d,"status":%q,"isDeleted":false,"publishedDate":"2026-01-02T00:00:00Z","comments":[%s]%s}`,
			th.id, th.status, strings.Join(comments, ","), ctx))
	}
	return fmt.Sprintf(`{"value":[%s]}`, strings.Join(threads, ","))
}

func atoiSA(s string) int { n, _ := strconv.Atoi(s); return n }

// A pushed edit lands on the pull request, and importing the result back
// converges: the second push has nothing to do.
func TestPushADORoundTrips(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	f := newStatefulADO(t)

	mustRun(t, bin, dir, "pull", "--token", "test-token", f.target())
	id := listID(t, bin, dir, "Fix the area subtree walk")

	mustRun(t, bin, dir, "edit", id, "-t", "Fix the area subtree walk, properly")
	mustRun(t, bin, dir, "comment", id, "-m", "Ready for a read.")

	r := gitReview(t, bin, dir, "push", "-y", "--token", "test-token", f.target())
	if r.code != 0 {
		t.Fatalf("push: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "pushed to") {
		t.Errorf("push did not report success:\n%s%s", r.stdout, r.stderr)
	}
	if f.pr.title != "Fix the area subtree walk, properly" {
		t.Errorf("the title edit did not land: %q", f.pr.title)
	}
	var posted bool
	for _, th := range f.pr.threads {
		for _, c := range th.comments {
			if c.content == "Ready for a read." {
				posted = true
			}
		}
	}
	if !posted {
		t.Errorf("the comment did not land: %+v", f.pr.threads)
	}

	// Import the result and push again: nothing left to do.
	mustRun(t, bin, dir, "pull", "--token", "test-token", f.target())
	r = gitReview(t, bin, dir, "push", "-y", "--token", "test-token", f.target())
	if r.code != 0 {
		t.Fatalf("second push: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "Everything up-to-date") {
		t.Errorf("the mirror did not converge:\n%s%s", r.stdout, r.stderr)
	}
}

// --dry-run reads the tracker, prints the plan, and writes nothing.
func TestPushADODryRun(t *testing.T) {
	bin := build(t)
	dir, _, _ := repo(t)
	f := newStatefulADO(t)

	mustRun(t, bin, dir, "pull", "--token", "test-token", f.target())
	id := listID(t, bin, dir, "Fix the area subtree walk")
	mustRun(t, bin, dir, "edit", id, "-t", "Renamed in the plan only")

	r := gitReview(t, bin, dir, "push", "--dry-run", "--token", "test-token", f.target())
	if r.code != 0 {
		t.Fatalf("dry run: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "title") {
		t.Errorf("the plan should name the changed field:\n%s", r.stdout)
	}
	if f.pr.title != "Fix the area subtree walk" {
		t.Errorf("a dry run wrote to the tracker: %q", f.pr.title)
	}
}

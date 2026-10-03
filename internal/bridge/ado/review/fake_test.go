package adoreview

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
)

// fakeADO is an in-memory Azure DevOps repository: enough of the pull request
// API to write to and read back from.
//
// A stateful fake rather than recorded responses, for the reason the work-item
// bridge gives: the write path is about what the tracker says *after* a write,
// and only replaying a real read against real state proves that pushing a change
// and importing the result converges.
type fakeADO struct {
	t          *testing.T
	srv        *httptest.Server
	self       adoapi.Identity
	nextPR     int
	nextThread int
	nextCID    int
	clock      time.Time
	pulls      map[int]*fakePR
}

type fakePR struct {
	id                         int
	title, description, status string
	base, source, headSHA      string
	draft                      bool
	labels                     []string
	reviewers                  map[string]*fakeReviewer
	threads                    []*fakeThread
}

type fakeReviewer struct {
	id   adoapi.Identity
	vote int
}

type fakeThread struct {
	id          int
	status      string
	path        string
	first, last int
	side        string
	comments    []*fakeThreadComment
}

type fakeThreadComment struct {
	id      int
	parent  int
	content string
	deleted bool
	author  adoapi.Identity
}

func newFakeADO(t *testing.T) *fakeADO {
	f := &fakeADO{
		t:      t,
		self:   adoapi.Identity{ID: "self-guid", DisplayName: "Push Bot", UniqueName: "bot@example.com"},
		nextPR: 100, nextThread: 500, nextCID: 900,
		clock: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		pulls: map[int]*fakePR{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeADO) client() *adoapi.Client { return adoapi.New("test-token") }

func (f *fakeADO) target() adoapi.Target {
	target, err := adoapi.ParseTarget(f.srv.URL + "/DefaultCollection/MyProj/_git/repo")
	if err != nil {
		f.t.Fatal(err)
	}
	return target
}

// seed files a pull request as though somebody had opened it upstream.
func (f *fakeADO) seed(title, description string) *fakePR {
	f.nextPR++
	pr := &fakePR{
		id: f.nextPR, title: title, description: description, status: "active",
		base: "main", source: "feature/" + fmt.Sprint(f.nextPR),
		headSHA:   "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d",
		reviewers: map[string]*fakeReviewer{},
	}
	f.pulls[pr.id] = pr
	return pr
}

var (
	rePR       = regexp.MustCompile(`/pullrequests/(\d+)$`)
	reThreads  = regexp.MustCompile(`/pullRequests/(\d+)/threads$`)
	reThread   = regexp.MustCompile(`/pullRequests/(\d+)/threads/(\d+)$`)
	reComments = regexp.MustCompile(`/pullRequests/(\d+)/threads/(\d+)/comments$`)
	reComment  = regexp.MustCompile(`/pullRequests/(\d+)/threads/(\d+)/comments/(\d+)$`)
	reReviewer = regexp.MustCompile(`/pullRequests/(\d+)/reviewers/([^/]+)$`)
	reLabels   = regexp.MustCompile(`/pullRequests/(\d+)/labels$`)
	reLabel    = regexp.MustCompile(`/pullRequests/(\d+)/labels/([^/]+)$`)
	reSub      = regexp.MustCompile(`/pullRequests/(\d+)/([a-z]+)$`)
)

func (f *fakeADO) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	lower := strings.ToLower(path)

	switch {
	case strings.HasSuffix(lower, "/_apis/connectiondata"):
		fmt.Fprintf(w, `{"authenticatedUser":{"id":%q,"providerDisplayName":%q,"properties":{"Account":{"$value":%q}}}}`,
			f.self.ID, f.self.DisplayName, f.self.UniqueName)

	case strings.HasSuffix(lower, "/_git/repo") || strings.HasSuffix(lower, "/repositories/repo"):
		w.Write([]byte(`{"defaultBranch":"refs/heads/main"}`))

	case rePR.MatchString(lower) && r.Method == http.MethodGet:
		f.renderPR(w, atoi(rePR.FindStringSubmatch(lower)[1]))
	case rePR.MatchString(lower) && r.Method == http.MethodPatch:
		f.patchPR(w, atoi(rePR.FindStringSubmatch(lower)[1]), body)
	case strings.HasSuffix(lower, "/pullrequests") && r.Method == http.MethodPost:
		f.createPR(w, body)

	case reComment.MatchString(path):
		m := reComment.FindStringSubmatch(path)
		f.commentWrite(w, r.Method, atoi(m[1]), atoi(m[2]), atoi(m[3]), body)
	case reComments.MatchString(path) && r.Method == http.MethodPost:
		m := reComments.FindStringSubmatch(path)
		f.addComment(w, atoi(m[1]), atoi(m[2]), body)
	case reThread.MatchString(path) && r.Method == http.MethodPatch:
		m := reThread.FindStringSubmatch(path)
		f.patchThread(w, atoi(m[1]), atoi(m[2]), body)
	case reThreads.MatchString(path) && r.Method == http.MethodPost:
		f.addThread(w, atoi(reThreads.FindStringSubmatch(path)[1]), body)
	case reThreads.MatchString(path) && r.Method == http.MethodGet:
		f.renderThreads(w, atoi(reThreads.FindStringSubmatch(path)[1]))

	case reReviewer.MatchString(path) && r.Method == http.MethodPut:
		m := reReviewer.FindStringSubmatch(path)
		f.setVote(w, atoi(m[1]), m[2], body)

	case reLabels.MatchString(path) && r.Method == http.MethodPost:
		f.addLabel(w, atoi(reLabels.FindStringSubmatch(path)[1]), body)
	case reLabel.MatchString(path) && r.Method == http.MethodDelete:
		m := reLabel.FindStringSubmatch(path)
		f.removeLabel(w, atoi(m[1]), m[2])

	case reSub.MatchString(path) && r.Method == http.MethodGet:
		// iterations, workitems, statuses — nothing this fake models.
		w.Write([]byte(`{"value":[]}`))

	default:
		f.t.Errorf("fake ado: unexpected %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeADO) pr(w http.ResponseWriter, id int) *fakePR {
	pr, ok := f.pulls[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}
	return pr
}

func (f *fakeADO) createPR(w http.ResponseWriter, body []byte) {
	var in struct {
		SourceRefName string `json:"sourceRefName"`
		TargetRefName string `json:"targetRefName"`
		Title         string `json:"title"`
		Description   string `json:"description"`
		IsDraft       bool   `json:"isDraft"`
	}
	json.Unmarshal(body, &in)
	f.nextPR++
	pr := &fakePR{
		id: f.nextPR, title: in.Title, description: in.Description, status: "active",
		base:    strings.TrimPrefix(in.TargetRefName, "refs/heads/"),
		source:  strings.TrimPrefix(in.SourceRefName, "refs/heads/"),
		headSHA: "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d", draft: in.IsDraft,
		reviewers: map[string]*fakeReviewer{},
	}
	f.pulls[pr.id] = pr
	fmt.Fprintf(w, `{"pullRequestId":%d,"_links":{"web":{"href":"https://example/pr/%d"}}}`, pr.id, pr.id)
}

func (f *fakeADO) patchPR(w http.ResponseWriter, id int, body []byte) {
	pr := f.pr(w, id)
	if pr == nil {
		return
	}
	var in map[string]any
	json.Unmarshal(body, &in)
	if v, ok := in["title"].(string); ok {
		pr.title = v
	}
	if v, ok := in["description"].(string); ok {
		pr.description = v
	}
	if v, ok := in["targetRefName"].(string); ok {
		pr.base = strings.TrimPrefix(v, "refs/heads/")
	}
	if v, ok := in["isDraft"].(bool); ok {
		pr.draft = v
	}
	if v, ok := in["status"].(string); ok {
		pr.status = v
	}
	f.renderPR(w, id)
}

func (f *fakeADO) addLabel(w http.ResponseWriter, id int, body []byte) {
	pr := f.pr(w, id)
	if pr == nil {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	json.Unmarshal(body, &in)
	for _, l := range pr.labels {
		if l == in.Name {
			w.Write([]byte(`{}`))
			return
		}
	}
	pr.labels = append(pr.labels, in.Name)
	w.Write([]byte(`{}`))
}

func (f *fakeADO) removeLabel(w http.ResponseWriter, id int, name string) {
	pr := f.pr(w, id)
	if pr == nil {
		return
	}
	out := pr.labels[:0]
	for _, l := range pr.labels {
		if l != name {
			out = append(out, l)
		}
	}
	pr.labels = out
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeADO) addThread(w http.ResponseWriter, id int, body []byte) {
	pr := f.pr(w, id)
	if pr == nil {
		return
	}
	var in struct {
		Comments []struct {
			ParentCommentID int    `json:"parentCommentId"`
			Content         string `json:"content"`
		} `json:"comments"`
		Status        string `json:"status"`
		ThreadContext *struct {
			FilePath       string `json:"filePath"`
			RightFileStart *struct {
				Line int `json:"line"`
			} `json:"rightFileStart"`
			RightFileEnd *struct {
				Line int `json:"line"`
			} `json:"rightFileEnd"`
			LeftFileStart *struct {
				Line int `json:"line"`
			} `json:"leftFileStart"`
			LeftFileEnd *struct {
				Line int `json:"line"`
			} `json:"leftFileEnd"`
		} `json:"threadContext"`
	}
	json.Unmarshal(body, &in)

	f.nextThread++
	th := &fakeThread{id: f.nextThread, status: "active"}
	if in.ThreadContext != nil {
		th.path = in.ThreadContext.FilePath
		if in.ThreadContext.RightFileStart != nil {
			th.first, th.side = in.ThreadContext.RightFileStart.Line, "right"
			if in.ThreadContext.RightFileEnd != nil {
				th.last = in.ThreadContext.RightFileEnd.Line
			}
		}
		if in.ThreadContext.LeftFileStart != nil {
			th.first, th.side = in.ThreadContext.LeftFileStart.Line, "left"
			if in.ThreadContext.LeftFileEnd != nil {
				th.last = in.ThreadContext.LeftFileEnd.Line
			}
		}
	}
	for _, c := range in.Comments {
		f.nextCID++
		th.comments = append(th.comments, &fakeThreadComment{
			id: f.nextCID, parent: c.ParentCommentID, content: c.Content, author: f.self,
		})
	}
	pr.threads = append(pr.threads, th)

	var comments []string
	for _, c := range th.comments {
		comments = append(comments, fmt.Sprintf(`{"id":%d}`, c.id))
	}
	fmt.Fprintf(w, `{"id":%d,"comments":[%s]}`, th.id, strings.Join(comments, ","))
}

func (f *fakeADO) patchThread(w http.ResponseWriter, id, tid int, body []byte) {
	pr := f.pr(w, id)
	if pr == nil {
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	json.Unmarshal(body, &in)
	for _, th := range pr.threads {
		if th.id == tid {
			th.status = in.Status
			w.Write([]byte(`{}`))
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func (f *fakeADO) addComment(w http.ResponseWriter, id, tid int, body []byte) {
	pr := f.pr(w, id)
	if pr == nil {
		return
	}
	var in struct {
		ParentCommentID int    `json:"parentCommentId"`
		Content         string `json:"content"`
	}
	json.Unmarshal(body, &in)
	for _, th := range pr.threads {
		if th.id != tid {
			continue
		}
		f.nextCID++
		c := &fakeThreadComment{id: f.nextCID, parent: in.ParentCommentID, content: in.Content, author: f.self}
		th.comments = append(th.comments, c)
		fmt.Fprintf(w, `{"id":%d}`, c.id)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (f *fakeADO) commentWrite(w http.ResponseWriter, method string, id, tid, cid int, body []byte) {
	pr := f.pr(w, id)
	if pr == nil {
		return
	}
	for _, th := range pr.threads {
		if th.id != tid {
			continue
		}
		for _, c := range th.comments {
			if c.id != cid {
				continue
			}
			switch method {
			case http.MethodPatch:
				var in struct {
					Content string `json:"content"`
				}
				json.Unmarshal(body, &in)
				c.content = in.Content
			case http.MethodDelete:
				c.deleted = true
			}
			w.Write([]byte(`{}`))
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func (f *fakeADO) setVote(w http.ResponseWriter, id int, reviewerID string, body []byte) {
	pr := f.pr(w, id)
	if pr == nil {
		return
	}
	var in struct {
		Vote int `json:"vote"`
	}
	json.Unmarshal(body, &in)
	r, ok := pr.reviewers[reviewerID]
	if !ok {
		who := f.self
		if reviewerID != f.self.ID {
			who = adoapi.Identity{ID: reviewerID, UniqueName: reviewerID + "@example.com"}
		}
		r = &fakeReviewer{id: who}
		pr.reviewers[reviewerID] = r
	}
	r.vote = in.Vote
	fmt.Fprintf(w, `{"id":%q,"vote":%d}`, reviewerID, in.Vote)
}

func (f *fakeADO) renderPR(w http.ResponseWriter, id int) {
	pr := f.pr(w, id)
	if pr == nil {
		return
	}
	var labels []string
	for _, l := range pr.labels {
		labels = append(labels, fmt.Sprintf(`{"name":%q,"active":true}`, l))
	}
	var reviewers []string
	for _, r := range pr.reviewers {
		reviewers = append(reviewers, fmt.Sprintf(
			`{"displayName":%q,"uniqueName":%q,"id":%q,"vote":%d}`,
			r.id.DisplayName, r.id.UniqueName, r.id.ID, r.vote))
	}
	fmt.Fprintf(w, `{
		"pullRequestId":%d,"status":%q,"title":%q,"description":%q,"isDraft":%t,
		"createdBy":{"displayName":"Author","uniqueName":"author@example.com","id":"author-guid"},
		"creationDate":"2026-01-01T00:00:00Z",
		"sourceRefName":"refs/heads/%s","targetRefName":"refs/heads/%s",
		"lastMergeSourceCommit":{"commitId":%q},
		"labels":[%s],"reviewers":[%s],
		"_links":{"web":{"href":"https://example/pr/%d"}}
	}`, pr.id, pr.status, pr.title, pr.description, pr.draft,
		pr.source, pr.base, pr.headSHA, strings.Join(labels, ","), strings.Join(reviewers, ","), pr.id)
}

func (f *fakeADO) renderThreads(w http.ResponseWriter, id int) {
	pr := f.pr(w, id)
	if pr == nil {
		return
	}
	var threads []string
	for _, th := range pr.threads {
		var comments []string
		for _, c := range th.comments {
			comments = append(comments, fmt.Sprintf(
				`{"id":%d,"parentCommentId":%d,"content":%q,"commentType":"text",`+
					`"author":{"displayName":%q,"uniqueName":%q,"id":%q},`+
					`"publishedDate":"2026-01-02T00:00:00Z","isDeleted":%t}`,
				c.id, c.parent, c.content, c.author.DisplayName, c.author.UniqueName, c.author.ID, c.deleted))
		}
		ctx := ""
		if th.path != "" {
			key := "rightFileStart"
			keyEnd := "rightFileEnd"
			if th.side == "left" {
				key, keyEnd = "leftFileStart", "leftFileEnd"
			}
			ctx = fmt.Sprintf(`,"threadContext":{"filePath":%q,%q:{"line":%d,"offset":1},%q:{"line":%d,"offset":1}}`,
				th.path, key, th.first, keyEnd, th.last)
		}
		threads = append(threads, fmt.Sprintf(
			`{"id":%d,"status":%q,"isDeleted":false,"publishedDate":"2026-01-02T00:00:00Z","comments":[%s]%s}`,
			th.id, th.status, strings.Join(comments, ","), ctx))
	}
	fmt.Fprintf(w, `{"value":[%s]}`, strings.Join(threads, ","))
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

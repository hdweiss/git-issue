// Reading pull requests: the search that finds them, and the per-request feeds
// that carry their threads, iterations, linked work items and statuses.
//
// Azure DevOps has no "updated since" filter for pull requests — searchCriteria
// takes Created and Closed time ranges and nothing else — so an incremental
// pull is best-effort: the closed ones can be skipped by age, but an active one
// is re-read in full every time. The active set is small, so this costs little;
// see FetchPulls.

package adoapi

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Pull request states, as Azure DevOps spells them in `status`.
const (
	PRActive    = "active"
	PRAbandoned = "abandoned"
	PRCompleted = "completed"
)

// Reviewer votes, as Azure DevOps numbers them.
const (
	VoteApproved            = 10
	VoteApprovedWithSuggest = 5
	VoteNone                = 0
	VoteWaitingForAuthor    = -5
	VoteRejected            = -10
)

// PullRequest is one pull request with everything an import needs, as Azure
// DevOps reports it. Nothing here is translated onto the review vocabulary —
// that is internal/bridge/ado/review's job.
type PullRequest struct {
	ID          int
	URL         string // the browser URL, from _links.web
	Title       string
	Description string
	Status      string // active | abandoned | completed
	IsDraft     bool

	CreatedBy Identity
	CreatedAt time.Time
	ClosedAt  time.Time // zero while active

	// UpdatedAt is the newest moment anything about the pull request changed,
	// synthesised from the creation, the close and every comment: Azure DevOps
	// reports no single "last activity" field. It is what a sync watermark is
	// taken from.
	UpdatedAt time.Time

	// TargetRef and SourceRef are bare branch names, the refs/heads/ prefix
	// stripped. ForkRepo is the name of the repository the source branch lives
	// in when it is a fork, and "" otherwise.
	TargetRef string
	SourceRef string
	ForkRepo  string
	// HeadCommit is the tip of the source branch as last merged/previewed —
	// lastMergeSourceCommit. It is the sha anchors, verdicts and checks address.
	HeadCommit string

	Labels     []string
	Reviewers  []Reviewer
	Threads    []PRThread
	Iterations []PRIteration
	WorkItems  []int
	Statuses   []PRStatus
}

// Reviewer is one person asked to read the pull request, and their current
// vote. Azure DevOps keeps no vote history, so this is the whole of what a
// verdict import has to work from.
type Reviewer struct {
	Identity   Identity
	Vote       int
	IsRequired bool
}

// PRThread is one conversation on a pull request: the whole-PR discussion when
// it has no file context, or a review thread anchored to a place in the diff
// when it does.
type PRThread struct {
	ID          int
	Status      string // active | fixed | wontFix | closed | byDesign | pending | ""
	IsDeleted   bool
	PublishedAt time.Time

	// FilePath is the anchored file, "/"-rooted as Azure DevOps returns it, or
	// "" for a thread with no code context.
	FilePath   string
	RightStart int
	RightEnd   int
	LeftStart  int
	LeftEnd    int
	// IterationID is the iteration the thread was left against, resolved to a
	// commit through Iterations. 0 when Azure DevOps recorded none.
	IterationID int

	Comments []PRComment
}

// PRComment is one entry of a thread.
type PRComment struct {
	ID              int
	ParentCommentID int
	Content         string
	// CommentType is "text" for something a person wrote and "system" for a
	// vote or a ref update the server logged. Only "text" is imported.
	CommentType string
	Author      Identity
	PublishedAt time.Time
	UpdatedAt   time.Time
	IsDeleted   bool
}

// PRIteration is one push to the source branch during the review's life. Its
// source commit is what a thread left against that iteration is anchored to.
type PRIteration struct {
	ID           int
	SourceCommit string
}

// PRStatus is one status posted against the pull request — a build, a scan, an
// external gate. It maps onto a check run.
type PRStatus struct {
	Genre     string
	Name      string
	State     string // notSet | pending | succeeded | failed | error | notApplicable
	TargetURL string
	CreatedAt time.Time
}

// PullFilter narrows what FetchPulls reads.
type PullFilter struct {
	// All lifts the active-only default: abandoned and completed pull requests
	// too.
	All bool
	// Since drops pull requests whose last activity is older than this. It can
	// only skip closed ones — an active pull request keeps an old creation date
	// however recently it changed — so it bounds a first --all import rather
	// than making a routine one cheap.
	Since time.Time
	// Limit stops after this many, 0 for no limit.
	Limit int
}

// prPage is how many pull request summaries one search request asks for.
const prPage = 100

// FetchPulls reads every pull request in scope, with its threads, iterations,
// linked work items and statuses.
//
// Two phases, like the work item fetch: one search for the summaries, then a
// handful of requests per pull request for the feeds the summary does not
// carry. Progress is reported from the second phase, which is where the time
// goes.
func (c *Client) FetchPulls(t Target, f PullFilter, progress func(fetched, total int)) ([]PullRequest, error) {
	summaries, err := c.searchPulls(t, f)
	if err != nil {
		return nil, err
	}

	// The age filter, applied before the feeds are fetched so an old closed
	// pull request costs nothing.
	if !f.Since.IsZero() {
		kept := summaries[:0]
		for _, pr := range summaries {
			at := pr.CreatedAt
			if pr.ClosedAt.After(at) {
				at = pr.ClosedAt
			}
			if !at.Before(f.Since) {
				kept = append(kept, pr)
			}
		}
		summaries = kept
	}
	if f.Limit > 0 && len(summaries) > f.Limit {
		summaries = summaries[:f.Limit]
	}

	for i := range summaries {
		if err := c.completePull(t, &summaries[i]); err != nil {
			return nil, fmt.Errorf("pull request %d: %w", summaries[i].ID, err)
		}
		if progress != nil {
			progress(i+1, len(summaries))
		}
	}
	return summaries, nil
}

// FetchPull reads one pull request by number, with the same feeds FetchPulls
// completes each summary with. A push uses it to read back exactly the pull
// requests the origin ledger names, rather than searching the whole repository.
func (c *Client) FetchPull(t Target, id int) (PullRequest, error) {
	var p pullJSON
	endpoint := fmt.Sprintf("%s/pullrequests/%d", t.PullRequestAPI(), id)
	if err := c.get(endpoint, nil, &p); err != nil {
		return PullRequest{}, err
	}
	pr := p.pullRequest()
	if err := c.completePull(t, &pr); err != nil {
		return PullRequest{}, fmt.Errorf("pull request %d: %w", id, err)
	}
	return pr, nil
}

// searchPulls reads the pull request summaries in scope, following the paging
// Azure DevOps applies with $top / $skip.
func (c *Client) searchPulls(t Target, f PullFilter) ([]PullRequest, error) {
	status := "active"
	if f.All {
		status = "all"
	}

	var out []PullRequest
	for skip := 0; ; skip += prPage {
		params := url.Values{
			"searchCriteria.status": {status},
			"$top":                  {strconv.Itoa(prPage)},
			"$skip":                 {strconv.Itoa(skip)},
		}
		var resp struct {
			Value []pullJSON `json:"value"`
			Count int        `json:"count"`
		}
		if err := c.get(t.PullRequestAPI()+"/pullrequests", params, &resp); err != nil {
			return nil, err
		}
		for _, p := range resp.Value {
			out = append(out, p.pullRequest())
		}
		if len(resp.Value) < prPage {
			return out, nil
		}
	}
}

// completePull fills in the feeds the search does not carry: threads, and — only
// when a thread needs one — the iteration commits, plus the linked work items
// and the statuses.
func (c *Client) completePull(t Target, pr *PullRequest) error {
	threads, err := c.prThreads(t, pr.ID)
	if err != nil {
		return err
	}
	pr.Threads = threads

	needsIterations := false
	for _, th := range threads {
		if th.IterationID > 0 {
			needsIterations = true
			break
		}
	}
	if needsIterations {
		its, err := c.prIterations(t, pr.ID)
		if err != nil {
			return err
		}
		pr.Iterations = its
	}

	items, err := c.prWorkItems(t, pr.ID)
	if err != nil {
		return err
	}
	pr.WorkItems = items

	statuses, err := c.prStatuses(t, pr.ID)
	if err != nil {
		return err
	}
	pr.Statuses = statuses

	pr.UpdatedAt = pullActivity(*pr)
	return nil
}

// pullActivity is the newest moment anything about the pull request changed.
func pullActivity(pr PullRequest) time.Time {
	at := pr.CreatedAt
	bump := func(t time.Time) {
		if t.After(at) {
			at = t
		}
	}
	bump(pr.ClosedAt)
	for _, th := range pr.Threads {
		bump(th.PublishedAt)
		for _, c := range th.Comments {
			bump(c.PublishedAt)
			bump(c.UpdatedAt)
		}
	}
	for _, s := range pr.Statuses {
		bump(s.CreatedAt)
	}
	return at
}

func (c *Client) prThreads(t Target, id int) ([]PRThread, error) {
	var resp struct {
		Value []threadJSON `json:"value"`
	}
	endpoint := fmt.Sprintf("%s/pullRequests/%d/threads", t.PullRequestAPI(), id)
	if err := c.get(endpoint, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]PRThread, 0, len(resp.Value))
	for _, th := range resp.Value {
		out = append(out, th.thread())
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (c *Client) prIterations(t Target, id int) ([]PRIteration, error) {
	var resp struct {
		Value []struct {
			ID              int `json:"id"`
			SourceRefCommit struct {
				CommitID string `json:"commitId"`
			} `json:"sourceRefCommit"`
		} `json:"value"`
	}
	endpoint := fmt.Sprintf("%s/pullRequests/%d/iterations", t.PullRequestAPI(), id)
	if err := c.get(endpoint, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]PRIteration, 0, len(resp.Value))
	for _, it := range resp.Value {
		out = append(out, PRIteration{ID: it.ID, SourceCommit: it.SourceRefCommit.CommitID})
	}
	return out, nil
}

func (c *Client) prWorkItems(t Target, id int) ([]int, error) {
	var resp struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	endpoint := fmt.Sprintf("%s/pullRequests/%d/workitems", t.PullRequestAPI(), id)
	if err := c.get(endpoint, nil, &resp); err != nil {
		return nil, err
	}
	var out []int
	for _, w := range resp.Value {
		if n, err := strconv.Atoi(w.ID); err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out, nil
}

func (c *Client) prStatuses(t Target, id int) ([]PRStatus, error) {
	var resp struct {
		Value []struct {
			State   string `json:"state"`
			Context struct {
				Genre string `json:"genre"`
				Name  string `json:"name"`
			} `json:"context"`
			TargetURL    string    `json:"targetUrl"`
			CreationDate timestamp `json:"creationDate"`
		} `json:"value"`
	}
	endpoint := fmt.Sprintf("%s/pullRequests/%d/statuses", t.PullRequestAPI(), id)
	if err := c.get(endpoint, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]PRStatus, 0, len(resp.Value))
	for _, s := range resp.Value {
		out = append(out, PRStatus{
			Genre:     s.Context.Genre,
			Name:      s.Context.Name,
			State:     s.State,
			TargetURL: s.TargetURL,
			CreatedAt: s.CreationDate.Time,
		})
	}
	return out, nil
}

// Iteration resolves an iteration id to its source commit, which is what a
// thread left against that iteration is anchored to.
func (pr PullRequest) Iteration(id int) (string, bool) {
	for _, it := range pr.Iterations {
		if it.ID == id {
			return it.SourceCommit, it.SourceCommit != ""
		}
	}
	return "", false
}

// The wire shapes. Kept beside the consumed types so the two read against each
// other, the way decode.go sits beside workitems.go.

type pullJSON struct {
	PullRequestID         int       `json:"pullRequestId"`
	Status                string    `json:"status"`
	Title                 string    `json:"title"`
	Description           string    `json:"description"`
	IsDraft               bool      `json:"isDraft"`
	CreatedBy             Identity  `json:"createdBy"`
	CreationDate          timestamp `json:"creationDate"`
	ClosedDate            timestamp `json:"closedDate"`
	SourceRefName         string    `json:"sourceRefName"`
	TargetRefName         string    `json:"targetRefName"`
	LastMergeSourceCommit struct {
		CommitID string `json:"commitId"`
	} `json:"lastMergeSourceCommit"`
	Reviewers []struct {
		DisplayName string `json:"displayName"`
		UniqueName  string `json:"uniqueName"`
		ID          string `json:"id"`
		Vote        int    `json:"vote"`
		IsRequired  bool   `json:"isRequired"`
	} `json:"reviewers"`
	Labels []struct {
		Name   string `json:"name"`
		Active bool   `json:"active"`
	} `json:"labels"`
	ForkSource *struct {
		Repository struct {
			Name string `json:"name"`
		} `json:"repository"`
	} `json:"forkSource"`
	Links struct {
		Web struct {
			Href string `json:"href"`
		} `json:"web"`
	} `json:"_links"`
}

func (p pullJSON) pullRequest() PullRequest {
	pr := PullRequest{
		ID:          p.PullRequestID,
		URL:         p.Links.Web.Href,
		Title:       p.Title,
		Description: p.Description,
		Status:      p.Status,
		IsDraft:     p.IsDraft,
		CreatedBy:   p.CreatedBy,
		CreatedAt:   p.CreationDate.Time,
		ClosedAt:    p.ClosedDate.Time,
		TargetRef:   branchName(p.TargetRefName),
		SourceRef:   branchName(p.SourceRefName),
		HeadCommit:  p.LastMergeSourceCommit.CommitID,
	}
	if p.ForkSource != nil {
		pr.ForkRepo = p.ForkSource.Repository.Name
	}
	for _, r := range p.Reviewers {
		pr.Reviewers = append(pr.Reviewers, Reviewer{
			Identity:   Identity{DisplayName: r.DisplayName, UniqueName: r.UniqueName, ID: r.ID},
			Vote:       r.Vote,
			IsRequired: r.IsRequired,
		})
	}
	for _, l := range p.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	return pr
}

type threadJSON struct {
	ID            int       `json:"id"`
	Status        string    `json:"status"`
	IsDeleted     bool      `json:"isDeleted"`
	PublishedDate timestamp `json:"publishedDate"`
	Comments      []struct {
		ID                     int       `json:"id"`
		ParentCommentID        int       `json:"parentCommentId"`
		Content                string    `json:"content"`
		CommentType            string    `json:"commentType"`
		Author                 Identity  `json:"author"`
		PublishedDate          timestamp `json:"publishedDate"`
		LastContentUpdatedDate timestamp `json:"lastContentUpdatedDate"`
		IsDeleted              bool      `json:"isDeleted"`
	} `json:"comments"`
	ThreadContext *struct {
		FilePath       string   `json:"filePath"`
		LeftFileStart  *linePos `json:"leftFileStart"`
		LeftFileEnd    *linePos `json:"leftFileEnd"`
		RightFileStart *linePos `json:"rightFileStart"`
		RightFileEnd   *linePos `json:"rightFileEnd"`
	} `json:"threadContext"`
	PullRequestThreadContext *struct {
		IterationContext *struct {
			SecondComparingIteration int `json:"secondComparingIteration"`
			FirstComparingIteration  int `json:"firstComparingIteration"`
		} `json:"iterationContext"`
	} `json:"pullRequestThreadContext"`
}

type linePos struct {
	Line   int `json:"line"`
	Offset int `json:"offset"`
}

func (t threadJSON) thread() PRThread {
	th := PRThread{
		ID:          t.ID,
		Status:      t.Status,
		IsDeleted:   t.IsDeleted,
		PublishedAt: t.PublishedDate.Time,
	}
	for _, c := range t.Comments {
		th.Comments = append(th.Comments, PRComment{
			ID:              c.ID,
			ParentCommentID: c.ParentCommentID,
			Content:         c.Content,
			CommentType:     c.CommentType,
			Author:          c.Author,
			PublishedAt:     c.PublishedDate.Time,
			UpdatedAt:       c.LastContentUpdatedDate.Time,
			IsDeleted:       c.IsDeleted,
		})
	}
	if t.ThreadContext != nil {
		th.FilePath = t.ThreadContext.FilePath
		if p := t.ThreadContext.RightFileStart; p != nil {
			th.RightStart = p.Line
		}
		if p := t.ThreadContext.RightFileEnd; p != nil {
			th.RightEnd = p.Line
		}
		if p := t.ThreadContext.LeftFileStart; p != nil {
			th.LeftStart = p.Line
		}
		if p := t.ThreadContext.LeftFileEnd; p != nil {
			th.LeftEnd = p.Line
		}
	}
	if c := t.PullRequestThreadContext; c != nil && c.IterationContext != nil {
		th.IterationID = c.IterationContext.SecondComparingIteration
	}
	return th
}

// branchName drops the refs/heads/ prefix a pull request spells its ends with.
// A ref that is not under refs/heads/ — a tag, a note — is left whole rather
// than mangled.
func branchName(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

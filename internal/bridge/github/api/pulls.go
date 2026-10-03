package ghapi

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// PullRequest is one GitHub pull request with everything an import needs, as
// GitHub reports it. Nothing here is translated: `State` is GitHub's
// OPEN/CLOSED/MERGED, `Author` is a bare login, and mapping those onto the
// review vocabulary happens a layer up.
type PullRequest struct {
	ID        string // the node id, which is the stable identity
	Number    int
	URL       string
	Title     string
	Body      string
	CreatedAt time.Time
	// UpdatedAt is what the `since` filter compares against, so it is also what
	// a sync watermark has to be taken from.
	UpdatedAt time.Time
	State     string
	Draft     bool
	Author    string
	Milestone string
	Locked    bool

	// BaseRef is the branch being merged into, a bare name. HeadRef is the
	// branch being merged from and HeadOwner the login of the repository it
	// lives in — separate, because a fork's head branch is only meaningful with
	// the fork named alongside it, and the two come from different fields.
	BaseRef   string
	HeadRef   string
	HeadOwner string
	HeadRepo  string
	// HeadOID is the commit the pull request currently proposes.
	HeadOID string

	Labels    []string
	Reviewers []string
	Comments  []Comment
	Reviews   []Review
	Threads   []ReviewThread
	Checks    []CheckRun

	// Closes are the node ids of the issues GitHub says this pull request would
	// close. Current state rather than history: GitHub raises no timeline event
	// a bridge can replay for these.
	Closes []string

	// Cursors for connections that did not fit in the page that named this pull
	// request. Empty means complete.
	commentCursor string
	threadCursor  string
}

// Review is one submitted review: somebody's position on one revision.
type Review struct {
	ID        string
	State     string // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	Body      string
	CreatedAt time.Time
	Author    string
	// CommitOID is the revision the review was submitted against, which is what
	// makes staleness derivable later. GitHub sometimes reports none — a review
	// on a pull request whose commits were force-pushed away — and an empty one
	// is carried through rather than guessed at.
	CommitOID string
}

// ReviewThread is one conversation anchored to a place in the diff.
type ReviewThread struct {
	ID        string
	Resolved  bool
	Outdated  bool
	Path      string
	DiffSide  string // LEFT or RIGHT
	Line      int
	StartLine int
	Comments  []ReviewComment
}

// ReviewComment is one entry of a review thread.
//
// OriginalCommitOID is the revision the comment was written against, and it is
// the field an anchor needs: `commit` moves as GitHub re-anchors a thread onto
// later revisions, while `originalCommit` is where the person was actually
// looking. Re-anchoring is exactly what this bridge must not import — an anchor
// is part of what somebody said.
type ReviewComment struct {
	ID                string
	Body              string
	CreatedAt         time.Time
	Author            string
	OriginalCommitOID string
	OriginalLine      int
	OriginalStartLine int
	Path              string
}

// CheckRun is one check against the pull request's head commit.
//
// Flattened from GitHub's two shapes — CheckRun, from the Checks API, and
// StatusContext, from the older commit-status API — because a consumer needs a
// name, an outcome and a link, and both supply all three under different field
// names. Which one it came from is not information anybody acts on.
type CheckRun struct {
	Name       string
	Conclusion string
	URL        string
	// CommitOID is the commit the run belongs to, which is the key it is stored
	// under: a check result is a fact about a commit rather than about the pull
	// request that happens to contain it.
	CommitOID string
	StartedAt time.Time
}

// GitHub's pull request states.
const (
	StateMerged = "MERGED"
)

// How a pull request import is paged, in two sizes, because it happens in two
// passes that want opposite things.
//
// pullListPage is the identity walk. It asks for an id and a timestamp and
// nothing else, so it is bounded by GraphQL's own 1-100 limit rather than by
// anything about cost: a hundred at a time is two requests for a repository with
// two hundred open pull requests, and neither can time out.
//
// pullDetailBatch is the detail fetch, and it is small on purpose. Every pull
// request in a batch is read in full — issue comments, reviews, review threads
// with their comments, the head commit's checks — so the batch is what GitHub
// has to execute inside its time budget. Ten keeps a batch quick enough to come
// back, and with fetchWorkers in flight it is forty pull requests being read at
// any moment, which is more than a single page of any size ever was.
//
// The node limit bounds it from the other side. GitHub caps a query at 500,000
// *possible* nodes, counted from the declared `first:` values rather than from
// what exists, and a pull request carries a connection nested two deep that an
// issue has no counterpart for: reviewThreads(first:100) { comments(first:100) }
// declares 10,000 on its own. Ten pull requests declare about 107,000 in total,
// well inside the cap; TestQueriesFitTheNodeLimit does the arithmetic.
const (
	pullListPage    = 100
	pullDetailBatch = 10
)

// fetchWorkers is how many detail queries are in flight at once.
//
// The number is a compromise with GitHub's secondary rate limits, which count
// concurrent requests as well as their rate, and with the fact that nothing here
// is waiting on this program: four keeps the connection busy without looking
// like an attack, and a run that trips a limit anyway is already handled — the
// retry waits the Retry-After out.
const fetchWorkers = 4

// Review states, as GitHub spells them.
const (
	ReviewApproved         = "APPROVED"
	ReviewChangesRequested = "CHANGES_REQUESTED"
	ReviewCommented        = "COMMENTED"
	ReviewDismissed        = "DISMISSED"
	ReviewPending          = "PENDING"
)

// FetchPulls reads every pull request in the target, calling progress as they
// arrive.
//
// Two passes, and the split is the whole performance story of this bridge. A
// cursor walk is strictly serial — the next request's cursor is inside the
// previous request's answer — so paging the *detail* of two hundred pull
// requests means two hundred pull requests' worth of comments, reviews and
// review threads through one connection, one page at a time, with GitHub's
// per-query time budget forcing the page small and the pages numerous.
//
// So the serial walk asks for identity alone: an id and a timestamp, a hundred
// at a time, in a query too light to time out. Detail is then fetched by id, in
// small batches, several at once — and batches addressed by id have no cursors
// between them, which is exactly what makes them parallel.
func (c *Client) FetchPulls(t Target, f Filter, progress func(fetched, total int)) ([]PullRequest, error) {
	ids, total, err := c.pullIDs(t, f)
	if err != nil {
		return nil, err
	}
	if f.Limit > 0 && (total == 0 || total > f.Limit) {
		total = f.Limit
	}
	return c.pullDetails(ids, total, progress)
}

// FetchPullNodes reads pull requests by node id — the ids the origin ledger
// holds — which is what a push uses to find out what the tracker currently
// says.
//
// Ids that no longer resolve come back as nulls rather than as errors.
func (c *Client) FetchPullNodes(ids []string) ([]PullRequest, error) {
	return c.pullDetails(ids, 0, nil)
}

// pullIDs walks the pull request connection for identity alone, newest first.
func (c *Client) pullIDs(t Target, f Filter) ([]string, int, error) {
	var (
		ids    []string
		cursor *string
		total  int
	)
	for {
		vars := map[string]any{
			"owner":  t.Owner,
			"name":   t.Name,
			"cursor": cursor,
			// A null states argument means "no restriction"; [] would match
			// nothing.
			"states": nil,
		}
		if len(f.States) > 0 {
			vars["states"] = pullStates(f.States)
		}

		var resp struct {
			Repository struct {
				PullRequests struct {
					TotalCount int      `json:"totalCount"`
					PageInfo   pageInfo `json:"pageInfo"`
					Nodes      []struct {
						ID        string    `json:"id"`
						UpdatedAt time.Time `json:"updatedAt"`
					} `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		}
		if err := c.query(pullIDsQuery, vars, &resp); err != nil {
			return nil, 0, err
		}

		conn := resp.Repository.PullRequests
		total = conn.TotalCount
		for _, n := range conn.Nodes {
			// `since` is applied here rather than in the query: the pull request
			// connection has no filterBy argument, unlike the issue one.
			// Ordering is by update time descending, so the walk stops at the
			// first one older than the watermark — which is what keeps a repeat
			// import cheap despite the filter being client-side, and now also
			// means an unchanged pull request costs no detail request at all.
			if !f.Since.IsZero() && n.UpdatedAt.Before(f.Since) {
				return ids, total, nil
			}
			ids = append(ids, n.ID)
			if f.Limit > 0 && len(ids) >= f.Limit {
				return ids, total, nil
			}
		}
		if !conn.PageInfo.HasNextPage {
			return ids, total, nil
		}
		next := conn.PageInfo.EndCursor
		cursor = &next
	}
}

// pullDetails reads the pull requests named by ids, in batches, several at a
// time, and returns them in the order the ids were given.
//
// The first batch is fetched alone. queryDegrading learns from a rejection which
// fields this server does not have and caches the verdict on the client, so a
// fan-out that started cold would spend one wasted request per worker learning
// the same thing — and against a GitHub Enterprise install missing two fields,
// twice that.
func (c *Client) pullDetails(ids []string, total int, progress func(fetched, total int)) ([]PullRequest, error) {
	batches := chunk(ids, pullDetailBatch)
	if len(batches) == 0 {
		return nil, nil
	}
	out := make([][]PullRequest, len(batches))

	var (
		mu   sync.Mutex
		done int
	)
	report := func(n int) {
		mu.Lock()
		done += n
		fetched := done
		mu.Unlock()
		c.report(progress, fetched, total)
	}

	got, err := c.pullBatch(batches[0])
	if err != nil {
		return nil, err
	}
	out[0] = got
	report(len(got))

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
		stop     = make(chan struct{})
		work     = make(chan int)
	)
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			// Closed rather than signalled, so both the feeder and every other
			// worker see it: one failed batch ends the fetch, and nothing is
			// left blocked on a send nobody will receive.
			close(stop)
		})
	}

	for w := 0; w < min(fetchWorkers, len(batches)-1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				got, err := c.pullBatch(batches[i])
				if err != nil {
					fail(err)
					return
				}
				out[i] = got
				report(len(got))
			}
		}()
	}
	for i := 1; i < len(batches); i++ {
		select {
		case work <- i:
		case <-stop:
			i = len(batches)
		}
	}
	close(work)
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	pulls := make([]PullRequest, 0, len(ids))
	for _, b := range out {
		pulls = append(pulls, b...)
	}
	return pulls, nil
}

// pullBatch reads one batch of pull requests in full, halving the batch whenever
// GitHub declines to finish it.
//
// Halving rather than retrying, because a batch GitHub could not execute inside
// its time budget will not become executable by being asked for again: the
// remedy for too much work in one query is less work in one query. It costs
// round trips and loses nothing — the same pull requests arrive, in more
// requests — and it bottoms out at one, which is where there is nothing left to
// divide and the failure is the caller's to hear about.
func (c *Client) pullBatch(ids []string) ([]PullRequest, error) {
	var resp struct {
		Nodes []*pullNode `json:"nodes"`
	}
	if err := c.queryDegrading(pullNodesQuery, map[string]any{"ids": ids}, &resp); err != nil {
		var e *Error
		if !errors.As(err, &e) || !e.TooHeavy() || len(ids) < 2 {
			return nil, err
		}
		half := len(ids) / 2
		c.notice("GitHub could not finish %d pull requests at once; asking for %d", len(ids), half)
		left, err := c.pullBatch(ids[:half])
		if err != nil {
			return nil, err
		}
		right, err := c.pullBatch(ids[half:])
		if err != nil {
			return nil, err
		}
		return append(left, right...), nil
	}

	out := make([]PullRequest, 0, len(resp.Nodes))
	for _, n := range resp.Nodes {
		// A null is an id that no longer resolves — a pull request deleted, or
		// in a repository this token has lost sight of.
		if n == nil {
			continue
		}
		pr := convertPull(*n)
		if err := c.completePull(&pr); err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, nil
}

// chunk splits ids into batches of at most size.
func chunk(ids []string, size int) [][]string {
	var out [][]string
	for start := 0; start < len(ids); start += size {
		out = append(out, ids[start:min(start+size, len(ids))])
	}
	return out
}

// pullStates maps the issue states a caller passes onto the pull request enum,
// which has a third value.
//
// A caller asking for open work means OPEN. A caller asking for everything
// passes nothing and gets everything, MERGED included; asking for CLOSED alone
// would silently omit every merged pull request, which is the majority of the
// closed ones and never what anybody means.
func pullStates(states []string) []string {
	var out []string
	for _, s := range states {
		switch strings.ToUpper(s) {
		case StateOpen:
			out = append(out, StateOpen)
		case StateClosed:
			out = append(out, StateClosed, StateMerged)
		default:
			out = append(out, strings.ToUpper(s))
		}
	}
	return out
}

// completePull pages the connections that did not fit in the page that named
// the pull request.
func (c *Client) completePull(pr *PullRequest) error {
	for pr.commentCursor != "" {
		var resp struct {
			Node struct {
				Comments commentConn `json:"comments"`
			} `json:"node"`
		}
		vars := map[string]any{"id": pr.ID, "cursor": pr.commentCursor}
		if err := c.query(morePullCommentsQuery, vars, &resp); err != nil {
			return err
		}
		for _, n := range resp.Node.Comments.Nodes {
			pr.Comments = append(pr.Comments, convertComment(n))
		}
		pr.commentCursor = nextCursor(resp.Node.Comments.PageInfo)
	}

	for pr.threadCursor != "" {
		var resp struct {
			Node struct {
				ReviewThreads threadConn `json:"reviewThreads"`
			} `json:"node"`
		}
		vars := map[string]any{"id": pr.ID, "cursor": pr.threadCursor}
		if err := c.query(moreThreadsQuery, vars, &resp); err != nil {
			return err
		}
		for _, n := range resp.Node.ReviewThreads.Nodes {
			pr.Threads = append(pr.Threads, convertThread(n))
		}
		pr.threadCursor = nextCursor(resp.Node.ReviewThreads.PageInfo)
	}
	return nil
}

type pullResponse struct {
	Repository struct {
		PullRequests struct {
			TotalCount int        `json:"totalCount"`
			PageInfo   pageInfo   `json:"pageInfo"`
			Nodes      []pullNode `json:"nodes"`
		} `json:"pullRequests"`
	} `json:"repository"`
}

type pullNode struct {
	ID          string    `json:"id"`
	Number      int       `json:"number"`
	URL         string    `json:"url"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	State       string    `json:"state"`
	IsDraft     bool      `json:"isDraft"`
	Locked      bool      `json:"locked"`
	BaseRefName string    `json:"baseRefName"`
	HeadRefName string    `json:"headRefName"`
	HeadRefOid  string    `json:"headRefOid"`
	Author      *actor    `json:"author"`
	Milestone   *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	HeadRepository *struct {
		Name  string `json:"name"`
		Owner *actor `json:"owner"`
	} `json:"headRepository"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer *struct {
				Login string `json:"login"`
				Name  string `json:"name"`
				Slug  string `json:"slug"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	Comments      commentConn `json:"comments"`
	ReviewThreads threadConn  `json:"reviewThreads"`
	Reviews       struct {
		Nodes []reviewNode `json:"nodes"`
	} `json:"reviews"`
	ClosingIssuesReferences *struct {
		Nodes []idNode `json:"nodes"`
	} `json:"closingIssuesReferences"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				OID               string `json:"oid"`
				StatusCheckRollup *struct {
					Contexts struct {
						Nodes []contextNode `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

type reviewNode struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	Author    *actor    `json:"author"`
	Commit    *struct {
		OID string `json:"oid"`
	} `json:"commit"`
}

type threadConn struct {
	PageInfo pageInfo     `json:"pageInfo"`
	Nodes    []threadNode `json:"nodes"`
}

type threadNode struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	DiffSide   string `json:"diffSide"`
	Line       *int   `json:"line"`
	StartLine  *int   `json:"startLine"`
	Comments   struct {
		Nodes []reviewCommentNode `json:"nodes"`
	} `json:"comments"`
}

type reviewCommentNode struct {
	ID                string    `json:"id"`
	Body              string    `json:"body"`
	CreatedAt         time.Time `json:"createdAt"`
	Author            *actor    `json:"author"`
	Path              string    `json:"path"`
	OriginalLine      *int      `json:"originalLine"`
	OriginalStartLine *int      `json:"originalStartLine"`
	OriginalCommit    *struct {
		OID string `json:"oid"`
	} `json:"originalCommit"`
}

// contextNode is the merged selection over GitHub's two check shapes. Only one
// set of fields is ever populated, and convertContext picks whichever it is.
type contextNode struct {
	Typename string `json:"__typename"`

	// CheckRun
	Name       string     `json:"name"`
	Conclusion string     `json:"conclusion"`
	Status     string     `json:"status"`
	DetailsURL string     `json:"detailsUrl"`
	StartedAt  *time.Time `json:"startedAt"`
	CheckSuite *struct {
		App *struct {
			Name string `json:"name"`
		} `json:"app"`
	} `json:"checkSuite"`

	// StatusContext
	Context   string     `json:"context"`
	State     string     `json:"state"`
	TargetURL string     `json:"targetUrl"`
	CreatedAt *time.Time `json:"createdAt"`
}

func convertPull(n pullNode) PullRequest {
	pr := PullRequest{
		ID:        n.ID,
		Number:    n.Number,
		URL:       n.URL,
		Title:     n.Title,
		Body:      n.Body,
		CreatedAt: n.CreatedAt,
		UpdatedAt: n.UpdatedAt,
		State:     n.State,
		Draft:     n.IsDraft,
		Locked:    n.Locked,
		BaseRef:   n.BaseRefName,
		HeadRef:   n.HeadRefName,
		HeadOID:   n.HeadRefOid,
		Author:    login(n.Author),
	}
	if n.Milestone != nil {
		pr.Milestone = n.Milestone.Title
	}
	if n.HeadRepository != nil {
		pr.HeadRepo = n.HeadRepository.Name
		pr.HeadOwner = login(n.HeadRepository.Owner)
	}
	for _, l := range n.Labels.Nodes {
		pr.Labels = append(pr.Labels, l.Name)
	}
	// A requested reviewer is a User, a Team or a Bot, and the three name
	// themselves with different fields. A team has no login, so its slug stands
	// in — which is what the review request actually names.
	for _, r := range n.ReviewRequests.Nodes {
		if r.RequestedReviewer == nil {
			continue
		}
		switch {
		case r.RequestedReviewer.Login != "":
			pr.Reviewers = append(pr.Reviewers, r.RequestedReviewer.Login)
		case r.RequestedReviewer.Slug != "":
			pr.Reviewers = append(pr.Reviewers, r.RequestedReviewer.Slug)
		}
	}
	for _, c := range n.Comments.Nodes {
		pr.Comments = append(pr.Comments, convertComment(c))
	}
	pr.commentCursor = nextCursor(n.Comments.PageInfo)

	for _, t := range n.ReviewThreads.Nodes {
		pr.Threads = append(pr.Threads, convertThread(t))
	}
	pr.threadCursor = nextCursor(n.ReviewThreads.PageInfo)

	for _, r := range n.Reviews.Nodes {
		// A pending review has not been submitted and is visible only to its
		// author. Importing one would publish a draft somebody has not sent.
		if strings.EqualFold(r.State, ReviewPending) {
			continue
		}
		v := Review{
			ID:        r.ID,
			State:     r.State,
			Body:      r.Body,
			CreatedAt: r.CreatedAt,
			Author:    login(r.Author),
		}
		if r.Commit != nil {
			v.CommitOID = r.Commit.OID
		}
		pr.Reviews = append(pr.Reviews, v)
	}

	if n.ClosingIssuesReferences != nil {
		for _, i := range n.ClosingIssuesReferences.Nodes {
			if i.ID != "" {
				pr.Closes = append(pr.Closes, i.ID)
			}
		}
	}

	for _, cn := range n.Commits.Nodes {
		if cn.Commit.StatusCheckRollup == nil {
			continue
		}
		for _, ctx := range cn.Commit.StatusCheckRollup.Contexts.Nodes {
			if run, ok := convertContext(ctx, cn.Commit.OID); ok {
				pr.Checks = append(pr.Checks, run)
			}
		}
	}
	return pr
}

func convertThread(n threadNode) ReviewThread {
	t := ReviewThread{
		ID:       n.ID,
		Resolved: n.IsResolved,
		Outdated: n.IsOutdated,
		Path:     n.Path,
		DiffSide: n.DiffSide,
	}
	if n.Line != nil {
		t.Line = *n.Line
	}
	if n.StartLine != nil {
		t.StartLine = *n.StartLine
	}
	for _, c := range n.Comments.Nodes {
		rc := ReviewComment{
			ID:        c.ID,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
			Author:    login(c.Author),
			Path:      c.Path,
		}
		if c.OriginalCommit != nil {
			rc.OriginalCommitOID = c.OriginalCommit.OID
		}
		if c.OriginalLine != nil {
			rc.OriginalLine = *c.OriginalLine
		}
		if c.OriginalStartLine != nil {
			rc.OriginalStartLine = *c.OriginalStartLine
		}
		t.Comments = append(t.Comments, rc)
	}
	return t
}

// convertContext flattens one check, whichever of GitHub's two shapes it is.
//
// A run with neither a name nor a context is dropped: the name is the key a
// check is stored under, and one without it could not be superseded by its own
// re-run.
func convertContext(n contextNode, commit string) (CheckRun, bool) {
	run := CheckRun{CommitOID: commit}
	switch {
	case n.Name != "":
		run.Name = n.Name
		// A check that has not finished has no conclusion, and its status is
		// the only thing that distinguishes queued from running from done.
		run.Conclusion = n.Conclusion
		if run.Conclusion == "" {
			run.Conclusion = n.Status
		}
		run.URL = n.DetailsURL
		// Two apps can register checks under the same name, so the app
		// qualifies it — which is also how the name reads on GitHub's own UI.
		if n.CheckSuite != nil && n.CheckSuite.App != nil && n.CheckSuite.App.Name != "" {
			run.Name = n.CheckSuite.App.Name + "/" + n.Name
		}
		if n.StartedAt != nil {
			run.StartedAt = *n.StartedAt
		}
	case n.Context != "":
		run.Name = n.Context
		run.Conclusion = n.State
		run.URL = n.TargetURL
		if n.CreatedAt != nil {
			run.StartedAt = *n.CreatedAt
		}
	default:
		return CheckRun{}, false
	}
	// A name with whitespace in it would break the wire form, which is
	// whitespace-separated. Squashed rather than refused: a check nobody can
	// name is worse than one whose name reads slightly differently.
	run.Name = strings.Join(strings.Fields(run.Name), "-")
	run.Conclusion = strings.Join(strings.Fields(run.Conclusion), "-")
	return run, true
}

// The GraphQL selections. Kept beside the structs they unmarshal into, so the
// two can be read against each other.

const reviewThreadSelection = `
  pageInfo { hasNextPage endCursor }
  nodes {
    id isResolved isOutdated path diffSide line startLine
    comments(first:%d) {
      nodes {
        id body createdAt author { login } path
        originalLine originalStartLine originalCommit { oid }
      }
    }
  }`

// pullSelection is everything an import reads from one pull request.
//
// Shared between the repository-wide query and the nodes(ids:) one so the two
// cannot drift: a field added to a listing but not to a refetch would make a
// pull request import differently depending on which door it came in by, and
// the re-import convergence the whole bridge rests on would stop holding.
//
// `commits(last:1)` is the head commit, and the only one whose checks are read.
// A pull request's earlier commits have checks of their own, and they are not
// imported: the head is what a review is about, and reading every commit's
// rollup would multiply the query's cost by the length of the branch for
// results nothing displays.
func pullSelection(s schema) string {
	closing := ""
	if s.pullExtras {
		closing = fmt.Sprintf("closingIssuesReferences(first:%d) { nodes { id } }", nestedPage)
	}
	checks := ""
	if s.pullExtras {
		checks = fmt.Sprintf(`
        commits(last:1) {
          nodes {
            commit {
              oid
              statusCheckRollup {
                contexts(first:%d) {
                  nodes {
                    __typename
                    ... on CheckRun     { name conclusion status detailsUrl startedAt checkSuite { app { name } } }
                    ... on StatusContext { context state targetUrl createdAt }
                  }
                }
              }
            }
          }
        }`, nestedPage)
	}

	return fmt.Sprintf(`
        id number url title body createdAt updatedAt state isDraft locked
        baseRefName headRefName headRefOid
        author { login }
        milestone { title }
        headRepository { name owner { login } }
        labels(first:%d) { nodes { name } }
        reviewRequests(first:%d) { nodes { requestedReviewer { ... on User { login } ... on Bot { login } ... on Team { slug } } } }
        comments(first:%d) {%s}
        reviews(first:%d, states:[APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED]) {
          nodes { id state body createdAt author { login } commit { oid } }
        }
        reviewThreads(first:%d) {%s}
        %s
        %s`,
		nestedPage, nestedPage,
		nestedPage, commentSelection,
		nestedPage,
		nestedPage, fmt.Sprintf(reviewThreadSelection, nestedPage),
		closing, checks)
}

// pullIDsQuery is the identity walk: which pull requests there are and when each
// last changed, and not one field more.
//
// Ordering is by update time descending, unlike the issue query's ascending
// walk, because the pull request connection has no `since` argument: the filter
// is applied client-side, and only a newest-first walk lets it stop early rather
// than reading the whole history to discard it.
var pullIDsQuery = fmt.Sprintf(`
query($owner:String!, $name:String!, $cursor:String, $states:[PullRequestState!]) {
  repository(owner:$owner, name:$name) {
    pullRequests(first:%d, after:$cursor, states:$states, orderBy:{field:UPDATED_AT, direction:DESC}) {
      totalCount
      pageInfo { hasNextPage endCursor }
      nodes { id updatedAt }
    }
  }
}`, pullListPage)

func pullNodesQuery(s schema) string {
	return fmt.Sprintf(`
query($ids:[ID!]!) {
  nodes(ids:$ids) { ... on PullRequest {%s} }
}`, pullSelection(s))
}

var morePullCommentsQuery = fmt.Sprintf(`
query($id:ID!, $cursor:String!) {
  node(id:$id) { ... on PullRequest { comments(first:%d, after:$cursor) {%s} } }
}`, nestedPage, commentSelection)

var moreThreadsQuery = fmt.Sprintf(`
query($id:ID!, $cursor:String!) {
  node(id:$id) { ... on PullRequest { reviewThreads(first:%d, after:$cursor) {%s} } }
}`, nestedPage, fmt.Sprintf(reviewThreadSelection, nestedPage))

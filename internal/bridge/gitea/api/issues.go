// Reading issues: the list that finds them, and the per-issue feeds that carry
// their comments, their timeline, and what they wait on.

package giteaapi

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RepoInfo is the little a push needs to know about the target repository
// before it writes anything: that it is there, and that it takes issues.
type RepoInfo struct {
	FullName  string `json:"full_name"`
	HasIssues bool   `json:"has_issues"`
	// Permissions is present on an authenticated view and reports what this
	// token may do. A pointer so its absence is distinguishable from a token
	// that genuinely has no push access.
	Permissions *struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

// Repo reads the target repository. A 404 here is the difference between a push
// that fails cleanly before it starts and one that dies half-way through with a
// per-issue error.
func (c *Client) Repo(t Target) (RepoInfo, error) {
	var out RepoInfo
	err := c.get("/repos/"+t.Owner+"/"+t.Repo, nil, &out)
	return out, err
}

// Version is the Gitea/Forgejo version string the host reports, and the probe a
// bare `git issue pull` uses to recognise a Gitea remote it was not told about.
// A host that is not Gitea answers 404, or with something that is not this
// envelope, and either way this returns an error rather than a version.
func (c *Client) Version() (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := c.do("GET", c.API+"/version", nil, &out); err != nil {
		return "", err
	}
	if out.Version == "" {
		return "", fmt.Errorf("no version in response")
	}
	return out.Version, nil
}

// User is a person as Gitea reports one. Login is the stable handle; the rest
// is display sugar and may be absent depending on the token's permissions.
type User struct {
	ID       int64  `json:"id"`
	Login    string `json:"login"`
	UserName string `json:"username"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
}

// Name is the login, falling back to username for the versions that only fill
// one of the two.
func (u User) Name() string {
	if u.Login != "" {
		return u.Login
	}
	return u.UserName
}

// Label is one repository label.
type Label struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Milestone is one repository milestone.
type Milestone struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	State string `json:"state"`
}

// Comment is one issue comment.
type Comment struct {
	ID        int64     `json:"id"`
	HTMLURL   string    `json:"html_url"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Issue is one issue with everything an import needs.
type Issue struct {
	ID        int64      `json:"id"`
	Number    int64      `json:"number"`
	HTMLURL   string     `json:"html_url"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	User      User       `json:"user"`
	Labels    []Label    `json:"labels"`
	Milestone *Milestone `json:"milestone"`
	Assignees []User     `json:"assignees"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`

	// NumComments is the comment count from the list response — 0 means the
	// comments feed can be skipped entirely.
	NumComments int `json:"comments"`

	// PullRequest is set on entries the issues endpoint returns that are really
	// pull requests. An import drops them.
	PullRequest *struct{} `json:"pull_request"`

	// Repository names the repository this issue belongs to, which the
	// dependencies feed needs so a cross-repository dependency can be told apart
	// from a local one.
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`

	// Filled by the per-issue feeds, not by the list.
	Comments []Comment       `json:"-"`
	Timeline []TimelineEntry `json:"-"`
	// Dependencies is the repo-local numbers of the issues that block this one —
	// what it is "blocked by". Cross-repository dependencies are left out,
	// because a link's target has to be an issue this tracker can name.
	Dependencies []int64 `json:"-"`
}

// IsPull reports whether this entry is really a pull request.
func (i Issue) IsPull() bool { return i.PullRequest != nil }

// TimelineEntry is one entry of an issue's timeline.
//
// Gitea's timeline is a union of a few dozen comment types that differ only in
// which payload fields they carry. A flat struct with a Type discriminator
// unmarshals straight out of the response and reads better at the mapping site
// than a type switch over a dozen near-empty structs would.
type TimelineEntry struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	User      User      `json:"user"`

	Label           *Label     `json:"label"`
	Assignee        *User      `json:"assignee"`
	RemovedAssignee bool       `json:"removed_assignee"`
	OldTitle        string     `json:"old_title"`
	NewTitle        string     `json:"new_title"`
	OldMilestone    *Milestone `json:"old_milestone"`
	Milestone       *Milestone `json:"milestone"`
}

// Timeline entry types this bridge understands. Everything else Gitea returns —
// tracked time, project moves, branch deletions, review machinery — is not part
// of a portable issue and is ignored.
const (
	TLLabel     = "label"
	TLMilestone = "milestone"
	TLAssignees = "assignees"
	TLClose     = "close"
	TLReopen    = "reopen"
	TLTitle     = "change_title"
	TLLock      = "lock"
	TLUnlock    = "unlock"
	TLPin       = "pin"
	TLUnpin     = "unpin"
)

// Filter narrows what an import reads.
type Filter struct {
	// Open restricts to open issues. The consequence is worth knowing: an issue
	// closed upstream since the last import is not in the result set, so its
	// local copy keeps saying open until an --all run.
	Open bool
	// Since restricts to issues updated at or after a time. This is what makes a
	// second import cheap; it cannot see deletions, which is why it is a filter
	// rather than the only mode.
	Since time.Time
	// Limit stops after this many issues, 0 for no limit.
	Limit int
}

// Fetch reads every issue in scope, with its comments, its timeline and its
// dependencies.
//
// Two phases: one paged list for the issues, then three calls per issue for the
// feeds the list cannot carry. The second phase is what makes a first import of
// a large repository slow, which is why the caller passes a progress callback.
func (c *Client) Fetch(t Target, f Filter, progress func(fetched, total int)) ([]Issue, error) {
	params := url.Values{}
	params.Set("type", "issues") // exclude pull requests where the server supports it
	params.Set("sort", "leastupdate")
	if f.Open {
		params.Set("state", "open")
	} else {
		params.Set("state", "all")
	}
	if !f.Since.IsZero() {
		params.Set("since", f.Since.UTC().Format(time.RFC3339))
	}

	var issues []Issue
	total := 0
	for page := 1; ; page++ {
		var batch []Issue
		pageTotal, last, err := c.page("/repos/"+t.Owner+"/"+t.Repo+"/issues", cloneValues(params), page, &batch)
		if err != nil {
			return nil, err
		}
		if page == 1 {
			total = pageTotal
		}
		for _, iss := range batch {
			if iss.IsPull() {
				continue
			}
			issues = append(issues, iss)
			if f.Limit > 0 && len(issues) >= f.Limit {
				last = true
				break
			}
		}
		if last {
			break
		}
	}
	if f.Limit > 0 && total > f.Limit {
		total = f.Limit
	}

	if err := c.hydrate(t, issues, max(total, len(issues)), progress); err != nil {
		return nil, err
	}
	return issues, nil
}

// hydrate fills in every issue's per-issue feeds, several at a time.
//
// Gitea has no batch endpoint, so this is where a large import spends its time:
// three small requests per issue, and one at a time they add up to minutes. A
// bounded pool of workers turns that into seconds without hammering the server.
func (c *Client) hydrate(t Target, issues []Issue, total int, progress func(fetched, total int)) error {
	workers := c.Workers
	if workers <= 0 {
		workers = 12
	}
	if workers > len(issues) {
		workers = len(issues)
	}
	if workers < 1 {
		return nil
	}

	var (
		mu       sync.Mutex
		done     int
		firstErr error
		next     int32 = -1
	)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt32(&next, 1))
				if i >= len(issues) {
					return
				}
				mu.Lock()
				stop := firstErr != nil
				mu.Unlock()
				if stop {
					return
				}
				err := c.complete(t, &issues[i])
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("issue %d: %w", issues[i].Number, err)
				}
				done++
				if progress != nil {
					progress(done, total)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// Issue reads one issue by its repo-local number, with the same feeds Fetch
// hydrates. This is how a push reads the tracker's current state — Gitea has no
// batch endpoint.
func (c *Client) Issue(t Target, number int64) (Issue, error) {
	iss, err := c.IssueBare(t, number)
	if err != nil {
		return Issue{}, err
	}
	if err := c.complete(t, &iss); err != nil {
		return Issue{}, err
	}
	return iss, nil
}

// IssueBare reads one issue without its feeds. A push's Apply needs only the
// current scalar fields and the assignee list, so paying for comments, timeline
// and dependencies again would triple the cost of every update.
func (c *Client) IssueBare(t Target, number int64) (Issue, error) {
	var iss Issue
	if err := c.get("/repos/"+t.Owner+"/"+t.Repo+"/issues/"+strconv.FormatInt(number, 10), nil, &iss); err != nil {
		return Issue{}, err
	}
	return iss, nil
}

// Issues reads several issues by number with their feeds, skipping any the
// server will not return — deleted, or moved somewhere this token cannot see.
//
// The order is completion order, not the order asked for: this is a thin
// collector over IssuesEach, and every caller that needs a lookup keys by
// number anyway.
func (c *Client) Issues(t Target, numbers []int64) ([]Issue, error) {
	out := make([]Issue, 0, len(numbers))
	if err := c.IssuesEach(t, numbers, func(iss Issue) error {
		out = append(out, iss)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// IssuesEach reads several issues by number, each with the comment, timeline
// and dependency feeds complete fills, and hands every one to fn as it arrives
// instead of returning them together.
//
// A push planning against a large mirror diffs thousands of mapped issues.
// Materialising them all — every comment and timeline entry of each — at once is
// what drove the process into an OOM kill on a small host; streaming keeps only
// a page's worth live at a time. Issues the server will not return (deleted, or
// moved out of this token's sight) are skipped silently, the same as Fetch.
//
// Two ways to read them, and listedEach picks: the paged list where it is
// cheaper, a request per issue otherwise.
//
// fn is called from a single goroutine and needs no locking of its own. The
// first error fn returns, and the first fetch error, stop the read; workers
// already in flight are let go, not waited on for more work.
func (c *Client) IssuesEach(t Target, numbers []int64, fn func(Issue) error) error {
	if len(numbers) == 0 {
		return nil
	}
	if handled, err := c.listedEach(t, numbers, fn); handled {
		return err
	}
	return c.eachByNumber(t, numbers, fn)
}

// listedEach reads the wanted issues out of the repository's issue list instead
// of asking for each one, reporting whether it handled the read at all — false
// means the caller should fall back to a request per issue.
//
// Gitea has no endpoint that takes a set of numbers, but the paged list is a
// batch read by another name: 50 issues per request against one per issue, and
// the entries it returns carry every field the per-issue read does that an
// import looks at. That is the whole cost of planning a push against a mirror
// where nothing changed — measured on a 1002-issue repository at 0.9s through
// the list against 12.6s one at a time, with the per-request cost on the server
// rather than the wire, so more workers do not help and fewer requests do.
//
// It is not always cheaper. The list cannot be narrowed to a set of numbers, so
// it costs a page per 50 issues in the repository whether they were asked for
// or not, and a push naming one issue would pay for all of them. The first page
// carries X-Total-Count, so the size that decides it is read from the server
// after one request — and that request is the first page of the walk whenever
// the list wins.
func (c *Client) listedEach(t Target, numbers []int64, fn func(Issue) error) (bool, error) {
	params := url.Values{}
	params.Set("type", "issues") // exclude pull requests where the server supports it
	params.Set("state", "all")
	base := "/repos/" + t.Owner + "/" + t.Repo + "/issues"

	var batch []Issue
	total, last, err := c.page(base, cloneValues(params), 1, &batch)
	if err != nil {
		// Not fatal here. The per-issue path is asking the same server for the
		// same issues, and it reports what it finds against a number rather than
		// against the whole repository.
		return false, nil
	}
	// A server that gave no total says nothing about how long the walk is, and a
	// walk that could be longer than the set it serves is not worth guessing at.
	if total <= 0 || pagesFor(total) > len(numbers) {
		return false, nil
	}

	want := make(map[int64]bool, len(numbers))
	for _, n := range numbers {
		want[n] = true
	}

	remaining := len(want)
	for page := 1; ; page++ {
		if page > 1 {
			batch = nil
			if _, last, err = c.page(base, cloneValues(params), page, &batch); err != nil {
				return true, err
			}
		}

		keep := make([]Issue, 0, len(batch))
		for _, iss := range batch {
			if !iss.IsPull() && want[iss.Number] {
				keep = append(keep, iss)
			}
		}
		if err := c.hydrate(t, keep, len(keep), nil); err != nil {
			return true, err
		}
		for _, iss := range keep {
			if err := fn(iss); err != nil {
				return true, err
			}
		}

		remaining -= len(keep)
		if last || remaining <= 0 {
			return true, nil
		}
	}
}

// pagesFor is how many requests a list walk over total entries takes.
func pagesFor(total int) int { return (total + pageSize - 1) / pageSize }

// eachByNumber is IssuesEach's per-issue read: one request per number, a
// bounded pool of them at once.
func (c *Client) eachByNumber(t Target, numbers []int64, fn func(Issue) error) error {
	workers := c.Workers
	if workers <= 0 {
		workers = 12
	}
	if workers > len(numbers) {
		workers = len(numbers)
	}
	if workers < 1 {
		return nil
	}

	type fetched struct {
		iss Issue
		ok  bool
		err error
	}
	out := make(chan fetched, workers)
	stop := make(chan struct{})
	var next int32 = -1
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			send := func(m fetched) bool {
				select {
				case out <- m:
					return true
				case <-stop:
					return false
				}
			}
			for {
				i := int(atomic.AddInt32(&next, 1))
				if i >= len(numbers) {
					return
				}
				select {
				case <-stop:
					return
				default:
				}
				iss, err := c.IssueBare(t, numbers[i])
				if err != nil {
					if e, is := err.(*Error); is && e.Status == 404 {
						if !send(fetched{}) {
							return
						}
						continue
					}
					send(fetched{err: err})
					return
				}
				if err := c.complete(t, &iss); err != nil {
					send(fetched{err: err})
					return
				}
				if !send(fetched{iss: iss, ok: true}) {
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()

	var firstErr error
	for m := range out {
		if firstErr != nil {
			continue // drain until the workers notice stop and the channel closes
		}
		switch {
		case m.err != nil:
			firstErr = m.err
			close(stop)
		case m.ok:
			if err := fn(m.iss); err != nil {
				firstErr = err
				close(stop)
			}
		}
	}
	return firstErr
}

// complete fills in the per-issue feeds, skipping the calls it can prove will
// come back empty: an issue never touched since it was filed has no comments,
// no timeline and no dependencies, and one with a zero comment count needs no
// comments call.
func (c *Client) complete(t Target, iss *Issue) error {
	if iss.UpdatedAt.Unix() <= iss.CreatedAt.Unix() && iss.NumComments == 0 {
		return nil
	}

	if iss.NumComments > 0 {
		comments, err := c.comments(t, iss.Number)
		if err != nil {
			return err
		}
		iss.Comments = comments
	}

	timeline, err := c.timeline(t, iss.Number)
	if err != nil {
		return err
	}
	iss.Timeline = timeline

	if !c.depsOff.Load() {
		deps, err := c.dependencies(t, iss.Number)
		if err != nil {
			return err
		}
		iss.Dependencies = deps
	}
	return nil
}

func (c *Client) comments(t Target, number int64) ([]Comment, error) {
	var all []Comment
	base := "/repos/" + t.Owner + "/" + t.Repo + "/issues/" + strconv.FormatInt(number, 10) + "/comments"
	for page := 1; ; page++ {
		var batch []Comment
		_, last, err := c.page(base, nil, page, &batch)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if last {
			return all, nil
		}
	}
}

func (c *Client) timeline(t Target, number int64) ([]TimelineEntry, error) {
	var all []TimelineEntry
	base := "/repos/" + t.Owner + "/" + t.Repo + "/issues/" + strconv.FormatInt(number, 10) + "/timeline"
	for page := 1; ; page++ {
		var batch []TimelineEntry
		_, last, err := c.page(base, nil, page, &batch)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if last {
			return all, nil
		}
	}
}

// dependencies reads the issues that block this one, keeping only those in the
// same repository — a link's target has to be something this tracker can name.
func (c *Client) dependencies(t Target, number int64) ([]int64, error) {
	want := strings.ToLower(t.Owner + "/" + t.Repo)
	var out []int64
	base := "/repos/" + t.Owner + "/" + t.Repo + "/issues/" + strconv.FormatInt(number, 10) + "/dependencies"
	for page := 1; ; page++ {
		var batch []Issue
		_, last, err := c.page(base, nil, page, &batch)
		if err != nil {
			if e, ok := err.(*Error); ok && (e.Status == 404 || e.Status == 403) {
				// Dependencies can be disabled per repository; that is not a
				// failure of the import. Remember it, so the other few thousand
				// issues in the same import do not each ask again.
				c.depsOff.Store(true)
				return nil, nil
			}
			return nil, err
		}
		for _, dep := range batch {
			if dep.Repository == nil || strings.ToLower(dep.Repository.FullName) == want {
				out = append(out, dep.Number)
			}
		}
		if last {
			return out, nil
		}
	}
}

// Labels maps the label names an issue in this repository can carry to their
// ids, which the create and label-mutation calls address a label by.
//
// This is the repository's own labels plus the owning organization's, since an
// issue can be tagged with either. The org call is skipped for a user-owned
// repository, where it would 404.
func (c *Client) Labels(t Target) (map[string]int64, error) {
	out := map[string]int64{}
	if err := c.pageLabels("/repos/"+t.Owner+"/"+t.Repo+"/labels", out); err != nil {
		return nil, err
	}
	if err := c.pageLabels("/orgs/"+t.Owner+"/labels", out); err != nil {
		if e, ok := err.(*Error); !ok || (e.Status != 404 && e.Status != 403) {
			return nil, err
		}
	}
	return out, nil
}

func (c *Client) pageLabels(base string, out map[string]int64) error {
	for page := 1; ; page++ {
		var batch []Label
		_, last, err := c.page(base, nil, page, &batch)
		if err != nil {
			return err
		}
		for _, l := range batch {
			if _, seen := out[l.Name]; !seen {
				out[l.Name] = l.ID
			}
		}
		if last {
			return nil
		}
	}
}

// Assignees is the set of logins that can be assigned to an issue in this
// repository — its collaborators and the org members with access — lowercased
// for case-insensitive matching. A push checks a would-be assignee against this
// rather than letting Gitea reject the whole write with a 422.
func (c *Client) Assignees(t Target) (map[string]bool, error) {
	out := map[string]bool{}
	base := "/repos/" + t.Owner + "/" + t.Repo + "/assignees"
	for page := 1; ; page++ {
		var batch []User
		_, last, err := c.page(base, nil, page, &batch)
		if err != nil {
			return nil, err
		}
		for _, u := range batch {
			if n := u.Name(); n != "" {
				out[strings.ToLower(n)] = true
			}
		}
		if last {
			return out, nil
		}
	}
}

// Milestones maps the repository's milestone titles to their ids. Every state
// is read: a closed milestone can still hold issues.
func (c *Client) Milestones(t Target) (map[string]int64, error) {
	out := map[string]int64{}
	base := "/repos/" + t.Owner + "/" + t.Repo + "/milestones"
	params := url.Values{"state": {"all"}}
	for page := 1; ; page++ {
		var batch []Milestone
		_, last, err := c.page(base, cloneValues(params), page, &batch)
		if err != nil {
			return nil, err
		}
		for _, m := range batch {
			out[m.Title] = m.ID
		}
		if last {
			return out, nil
		}
	}
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

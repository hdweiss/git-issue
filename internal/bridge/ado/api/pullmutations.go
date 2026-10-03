// The write half of the pull request client.
//
// Separate from mutations.go for the reason ghapi splits the same way: almost
// nothing is shared. A work item is patched as a JSON document; a pull request
// has its own update call, its own thread and comment endpoints, and three state
// axes — draft, reviewer votes, thread resolution — a work item does not have.
//
// Every call goes through c.do with the api-version the endpoint needs. The
// thread and comment endpoints are still behind a preview version at 7.1.

package adoapi

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// NewPullInput is everything a pull request is filed with. The branch names are
// bare — refs/heads/ is added here.
type NewPullInput struct {
	SourceRef   string
	TargetRef   string
	Title       string
	Description string
	Draft       bool
}

// NewPull is what CreatePull returns: the number the ledger files it under and
// the browser URL when the response carried one.
type NewPull struct {
	ID  int
	URL string
}

// DefaultBranch is the repository's default branch, bare — the base a pull
// request gets when the review records none. Asked of the server rather than
// resolved from the clone, whose HEAD is whatever this checkout is on.
func (c *Client) DefaultBranch(t Target) (string, error) {
	var resp struct {
		DefaultBranch string `json:"defaultBranch"`
	}
	if err := c.get(t.PullRequestAPI(), nil, &resp); err != nil {
		return "", err
	}
	if resp.DefaultBranch == "" {
		return "", fmt.Errorf("%s has no default branch; name a base with 'git review edit --base'", t.Repo)
	}
	return strings.TrimPrefix(resp.DefaultBranch, "refs/heads/"), nil
}

// CreatePull files a new pull request. Labels and reviewers are not accepted
// here, so the caller applies them straight afterwards.
func (c *Client) CreatePull(t Target, in NewPullInput) (NewPull, error) {
	body := map[string]any{
		"sourceRefName": headRef(in.SourceRef),
		"targetRefName": headRef(in.TargetRef),
		"title":         in.Title,
	}
	if in.Description != "" {
		body["description"] = in.Description
	}
	if in.Draft {
		body["isDraft"] = true
	}

	var resp struct {
		ID    int `json:"pullRequestId"`
		Links struct {
			Web struct {
				Href string `json:"href"`
			} `json:"web"`
		} `json:"_links"`
	}
	endpoint := withVersion(t.PullRequestAPI()+"/pullrequests", nil, APIVersion)
	if err := c.do(http.MethodPost, endpoint, body, &resp); err != nil {
		return NewPull{}, err
	}
	if resp.ID == 0 {
		return NewPull{}, fmt.Errorf("create pull request: the response carried no id")
	}
	return NewPull{ID: resp.ID, URL: resp.Links.Web.Href}, nil
}

// UpdatePull writes the scalar fields the update call covers. fields carries
// Azure DevOps' own keys: title, description, targetRefName, status, isDraft.
func (c *Client) UpdatePull(t Target, id int, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	endpoint := withVersion(fmt.Sprintf("%s/pullrequests/%d", t.PullRequestAPI(), id), nil, APIVersion)
	return c.do(http.MethodPatch, endpoint, fields, nil)
}

// AddPullLabel attaches a label, creating the definition if the project has
// none by that name — which is Azure DevOps' own behaviour and unlike GitHub's.
func (c *Client) AddPullLabel(t Target, id int, name string) error {
	endpoint := withVersion(fmt.Sprintf("%s/pullRequests/%d/labels", t.PullRequestAPI(), id), nil, APIVersion+"-preview.1")
	return c.do(http.MethodPost, endpoint, map[string]string{"name": name}, nil)
}

// RemovePullLabel detaches a label. Azure DevOps addresses it by name or id in
// the path; the name is what a delta carries.
func (c *Client) RemovePullLabel(t Target, id int, name string) error {
	endpoint := withVersion(fmt.Sprintf("%s/pullRequests/%d/labels/%s", t.PullRequestAPI(), id, url.PathEscape(name)), nil, APIVersion+"-preview.1")
	return c.do(http.MethodDelete, endpoint, nil, nil)
}

// NewThreadInput opens one thread. An empty Path is the pull request's general
// discussion; a Path with a line range is a review thread anchored to the diff.
type NewThreadInput struct {
	Body string

	Path  string
	First int
	Last  int
	// Side is "left" for a thread on the base side of the diff, "" for the right.
	Side string
}

// AddPullThread opens a thread and returns its id alongside its first comment's.
//
// Both ids matter and they are different objects: the thread is what a
// resolution names, the comment is what an edit names.
func (c *Client) AddPullThread(t Target, prID int, in NewThreadInput) (threadID, commentID int, err error) {
	body := map[string]any{
		"comments": []map[string]any{{
			"parentCommentId": 0,
			"content":         in.Body,
			"commentType":     "text",
		}},
		"status": "active",
	}
	if in.Path != "" && in.First > 0 {
		last := in.Last
		if last < in.First {
			last = in.First
		}
		start := map[string]int{"line": in.First, "offset": 1}
		end := map[string]int{"line": last, "offset": 1}
		ctx := map[string]any{"filePath": "/" + in.Path}
		if in.Side == "left" {
			ctx["leftFileStart"], ctx["leftFileEnd"] = start, end
		} else {
			ctx["rightFileStart"], ctx["rightFileEnd"] = start, end
		}
		body["threadContext"] = ctx
	}

	var resp threadWriteJSON
	endpoint := withVersion(fmt.Sprintf("%s/pullRequests/%d/threads", t.PullRequestAPI(), prID), nil, APIVersion)
	if err := c.do(http.MethodPost, endpoint, body, &resp); err != nil {
		return 0, 0, err
	}
	if resp.ID == 0 {
		return 0, 0, fmt.Errorf("create thread: the response carried no id")
	}
	if len(resp.Comments) > 0 {
		commentID = resp.Comments[0].ID
	}
	return resp.ID, commentID, nil
}

// AddPullComment appends a comment to an existing thread and returns its id.
// parentID 0 posts at the top of the thread — a locally authored reply is
// flattened, matching the import's own "comments do not thread" note.
func (c *Client) AddPullComment(t Target, prID, threadID, parentID int, content string) (int, error) {
	body := map[string]any{
		"parentCommentId": parentID,
		"content":         content,
		"commentType":     "text",
	}
	var resp struct {
		ID int `json:"id"`
	}
	endpoint := withVersion(fmt.Sprintf("%s/pullRequests/%d/threads/%d/comments", t.PullRequestAPI(), prID, threadID), nil, APIVersion)
	if err := c.do(http.MethodPost, endpoint, body, &resp); err != nil {
		return 0, err
	}
	return resp.ID, nil
}

// UpdatePullComment rewrites a comment's text. Azure DevOps keeps the previous
// version, which is what lets the next import read the edit back as history.
func (c *Client) UpdatePullComment(t Target, prID, threadID, commentID int, content string) error {
	endpoint := withVersion(fmt.Sprintf("%s/pullRequests/%d/threads/%d/comments/%d", t.PullRequestAPI(), prID, threadID, commentID), nil, APIVersion)
	return c.do(http.MethodPatch, endpoint, map[string]string{"content": content}, nil)
}

// DeletePullComment removes a comment. It stays readable with the deleted flag,
// so the next import still sees the tombstone rather than a gap.
func (c *Client) DeletePullComment(t Target, prID, threadID, commentID int) error {
	endpoint := withVersion(fmt.Sprintf("%s/pullRequests/%d/threads/%d/comments/%d", t.PullRequestAPI(), prID, threadID, commentID), nil, APIVersion)
	return c.do(http.MethodDelete, endpoint, nil, nil)
}

// SetPullThreadStatus resolves or reopens a thread. "closed" is the resolved
// state a push writes; "active" reopens.
func (c *Client) SetPullThreadStatus(t Target, prID, threadID int, status string) error {
	endpoint := withVersion(fmt.Sprintf("%s/pullRequests/%d/threads/%d", t.PullRequestAPI(), prID, threadID), nil, APIVersion)
	return c.do(http.MethodPatch, endpoint, map[string]string{"status": status}, nil)
}

// SetReviewerVote casts the authenticated user's vote. The PUT adds the caller
// as a reviewer if they are not one already, which is what casting a verdict on
// a pull request nobody assigned you to does in the web UI too.
func (c *Client) SetReviewerVote(t Target, prID int, reviewerID string, vote int) error {
	if reviewerID == "" {
		return fmt.Errorf("cast a vote: the authenticated identity is unknown")
	}
	endpoint := withVersion(fmt.Sprintf("%s/pullRequests/%d/reviewers/%s", t.PullRequestAPI(), prID, reviewerID), nil, APIVersion)
	return c.do(http.MethodPut, endpoint, map[string]any{"vote": vote}, nil)
}

// ConnectionData is who the token authenticates as, which is the identity a vote
// is cast under and the one a pushed verdict is recorded against on the ledger.
func (c *Client) ConnectionData(t Target) (Identity, error) {
	var resp struct {
		AuthenticatedUser struct {
			ID                  string `json:"id"`
			ProviderDisplayName string `json:"providerDisplayName"`
			Properties          struct {
				Account struct {
					Value string `json:"$value"`
				} `json:"Account"`
			} `json:"properties"`
		} `json:"authenticatedUser"`
	}
	endpoint := withVersion(t.Base+"/"+t.Collection+"/_apis/connectionData", nil, APIVersion)
	if err := c.do(http.MethodGet, endpoint, nil, &resp); err != nil {
		return Identity{}, err
	}
	u := resp.AuthenticatedUser
	if u.ID == "" {
		return Identity{}, fmt.Errorf("connectionData carried no authenticated user")
	}
	return Identity{ID: u.ID, DisplayName: u.ProviderDisplayName, UniqueName: u.Properties.Account.Value}, nil
}

type threadWriteJSON struct {
	ID       int `json:"id"`
	Comments []struct {
		ID int `json:"id"`
	} `json:"comments"`
}

// headRef qualifies a bare branch name as a ref, leaving an already-qualified
// one alone.
func headRef(branch string) string {
	if branch == "" || strings.HasPrefix(branch, "refs/") {
		return branch
	}
	return "refs/heads/" + branch
}

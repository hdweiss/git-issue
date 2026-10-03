// Writing: the issue, label and comment mutations a push needs.
//
// Gitea addresses almost everything by a small integer — an issue by its
// repo-local number, a label and a milestone by id, a comment by a
// repo-global id — so unlike ghapi there is little to resolve first. What
// resolution there is (a label name to its id, a milestone title to its id)
// the bridge does through Labels and Milestones.

package giteaapi

import (
	"net/http"
	"strconv"
)

// CreateIssueOption is everything an issue can be filed with in one call. A
// nil/zero field is left out of the request body.
type CreateIssueOption struct {
	Title     string
	Body      string
	Assignees []string
	Milestone int64
	Labels    []int64
	Closed    bool
}

// EditIssueOption carries the scalar fields updateIssue covers. Each is a
// pointer so that "not changing" and "setting to empty" stay distinct — a nil
// Milestone leaves it alone, a *0 detaches it.
type EditIssueOption struct {
	Title     *string
	Body      *string
	Assignees []string
	Milestone *int64
	State     *string
}

// CreateIssue files a new issue.
//
// Everything the issue is filed with goes in this one call rather than a create
// followed by edits: an issue created bare and then edited generates timeline
// entries for changes that never happened, and the next import would read that
// history back.
func (c *Client) CreateIssue(t Target, opt CreateIssueOption) (Issue, error) {
	body := map[string]any{"title": opt.Title}
	if opt.Body != "" {
		body["body"] = opt.Body
	}
	if len(opt.Assignees) > 0 {
		body["assignees"] = opt.Assignees
	}
	if opt.Milestone != 0 {
		body["milestone"] = opt.Milestone
	}
	if len(opt.Labels) > 0 {
		body["labels"] = opt.Labels
	}
	if opt.Closed {
		body["closed"] = true
	}

	var created Issue
	if err := c.do(http.MethodPost, c.API+"/repos/"+t.Owner+"/"+t.Repo+"/issues", body, &created); err != nil {
		return Issue{}, err
	}
	return created, nil
}

// EditIssue writes the scalar fields of an existing issue.
func (c *Client) EditIssue(t Target, number int64, opt EditIssueOption) error {
	body := map[string]any{}
	if opt.Title != nil {
		body["title"] = *opt.Title
	}
	if opt.Body != nil {
		body["body"] = *opt.Body
	}
	if opt.Assignees != nil {
		body["assignees"] = opt.Assignees
	}
	if opt.Milestone != nil {
		body["milestone"] = *opt.Milestone
	}
	if opt.State != nil {
		body["state"] = *opt.State
	}
	if len(body) == 0 {
		return nil
	}
	return c.do(http.MethodPatch, c.issuePath(t, number), body, nil)
}

// AddLabels adds labels to an issue by id.
func (c *Client) AddLabels(t Target, number int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return c.do(http.MethodPost, c.issuePath(t, number)+"/labels", map[string]any{"labels": ids}, nil)
}

// RemoveLabel removes one label from an issue by id.
func (c *Client) RemoveLabel(t Target, number, labelID int64) error {
	return c.do(http.MethodDelete, c.issuePath(t, number)+"/labels/"+strconv.FormatInt(labelID, 10), nil, nil)
}

// AddComment posts a comment and returns it, so the caller can record its id in
// the origin ledger — without it, the next push would post the same comment
// again.
func (c *Client) AddComment(t Target, number int64, body string) (Comment, error) {
	var created Comment
	if err := c.do(http.MethodPost, c.issuePath(t, number)+"/comments", map[string]string{"body": body}, &created); err != nil {
		return Comment{}, err
	}
	return created, nil
}

// EditComment rewrites a comment's body. The path carries no issue number:
// Gitea comment ids are unique per repository.
func (c *Client) EditComment(t Target, commentID int64, body string) error {
	return c.do(http.MethodPatch, c.commentPath(t, commentID), map[string]string{"body": body}, nil)
}

// DeleteComment removes a comment outright.
//
// This is the one irreversible thing a push does, and it is reached only from a
// local comment.remove naming an entry this repository posted — never from an
// entity removal.
func (c *Client) DeleteComment(t Target, commentID int64) error {
	return c.do(http.MethodDelete, c.commentPath(t, commentID), nil, nil)
}

// AddDependency makes this issue blocked by another in the same repository.
func (c *Client) AddDependency(t Target, number, blocking int64) error {
	return c.do(http.MethodPost, c.issuePath(t, number)+"/dependencies", dependencyRef(t, blocking), nil)
}

// RemoveDependency drops a blocked-by link.
//
// A dependency that upstream has already lost comes back as a 404, which is not
// a failure: the ordinary case for the second end of the link is that dropping
// it here retracts both ends, both are pushed, and one finds it already gone.
func (c *Client) RemoveDependency(t Target, number, blocking int64) error {
	err := c.do(http.MethodDelete, c.issuePath(t, number)+"/dependencies", dependencyRef(t, blocking), nil)
	if e, ok := err.(*Error); ok && e.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// dependencyRef is the body the issue-dependencies API wants: the blocking
// issue's number, and the repository it lives in.
//
// The owner and repo are always sent, and are always this repository's. Gitea
// compares them against the issue named in the path, and on any mismatch — an
// omitted field counts, arriving as "" — switches to its cross-repository
// branch, which then fails to resolve the empty repository and answers
// `404 repository does not exist [id: 0, uid: 0, owner_name: , name: ]`. Naming
// the repository keeps the call on the same-repo path that actually works.
//
// The repo name goes under the key "repo", not "name": Gitea's IssueMeta binds
// that field as `json:"repo"`, and sending "name" leaves it empty — which is
// the same mismatch by another route (owner_name set, name still blank).
func dependencyRef(t Target, blocking int64) map[string]any {
	return map[string]any{"index": blocking, "owner": t.Owner, "repo": t.Repo}
}

func (c *Client) issuePath(t Target, number int64) string {
	return c.API + "/repos/" + t.Owner + "/" + t.Repo + "/issues/" + strconv.FormatInt(number, 10)
}

func (c *Client) commentPath(t Target, commentID int64) string {
	return c.API + "/repos/" + t.Owner + "/" + t.Repo + "/issues/comments/" + strconv.FormatInt(commentID, 10)
}

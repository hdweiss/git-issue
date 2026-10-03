// The write half of the pull request client.
//
// Separate from mutations.go because almost nothing is shared. An issue is
// edited through updateIssue and commented on through addComment; a pull request
// has its own update mutation, its own comment types — a review-thread entry is
// not an issue comment and is not edited by the same call — and three state
// axes an issue does not have at all: draft, review requests, and verdicts.
//
// The one genuine overlap is labels: addLabelsToLabelable takes any labelable,
// so a pull request reuses AddLabels and RemoveLabels unchanged.

package ghapi

import (
	"fmt"
	"strings"
)

// DefaultBranch is the repository's default branch, which is the base a pull
// request gets when the review records none.
//
// Asked of the tracker rather than resolved from the clone: the local HEAD is
// whatever this checkout happens to be on, and a fork's default branch is not
// necessarily the upstream's.
func (c *Client) DefaultBranch(t Target) (string, error) {
	var resp struct {
		Repository struct {
			DefaultBranchRef struct {
				Name string `json:"name"`
			} `json:"defaultBranchRef"`
		} `json:"repository"`
	}
	q := `query($owner:String!, $name:String!) {
	  repository(owner:$owner, name:$name) { defaultBranchRef { name } }
	}`
	if err := c.query(q, map[string]any{"owner": t.Owner, "name": t.Name}, &resp); err != nil {
		return "", err
	}
	if name := resp.Repository.DefaultBranchRef.Name; name != "" {
		return name, nil
	}
	return "", fmt.Errorf("%s has no default branch; name a base with 'git review edit --base'", t)
}

// NewPull is the pull request createPullRequest returns.
type NewPull struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Number int    `json:"number"`
}

// NewPullInput is everything a pull request is filed with.
//
// HeadRefName carries the cross-repository spelling where the head lives in a
// fork: `owner:branch`, which is what GitHub reads for a head outside the target
// repository. A bare name is a branch in the target itself.
type NewPullInput struct {
	RepoID      string
	BaseRefName string
	HeadRefName string
	Title       string
	Body        string
	Draft       bool
}

// CreatePull files a new pull request.
//
// Only the fields createPullRequest actually takes go in this call — labels,
// reviewers and milestone are not among them, so unlike CreateIssue this cannot
// avoid the follow-up writes. The caller applies them straight afterwards.
func (c *Client) CreatePull(in NewPullInput) (NewPull, error) {
	input := map[string]any{
		"repositoryId": in.RepoID,
		"baseRefName":  in.BaseRefName,
		"headRefName":  in.HeadRefName,
		"title":        in.Title,
	}
	if in.Body != "" {
		input["body"] = in.Body
	}
	if in.Draft {
		input["draft"] = true
	}

	var resp struct {
		CreatePullRequest struct {
			PullRequest NewPull `json:"pullRequest"`
		} `json:"createPullRequest"`
	}
	m := `mutation($input:CreatePullRequestInput!) { createPullRequest(input:$input) { pullRequest { id url number } } }`
	if err := c.mutate(m, map[string]any{"input": input}, &resp); err != nil {
		return NewPull{}, err
	}
	if resp.CreatePullRequest.PullRequest.ID == "" {
		return NewPull{}, fmt.Errorf("createPullRequest returned no pull request")
	}
	return resp.CreatePullRequest.PullRequest, nil
}

// UpdatePull writes the scalar fields updatePullRequest covers. Fields carries
// GitHub's own input keys, and a nil value clears.
func (c *Client) UpdatePull(pullID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	input := map[string]any{"pullRequestId": pullID}
	for k, v := range fields {
		input[k] = v
	}
	m := `mutation($input:UpdatePullRequestInput!) { updatePullRequest(input:$input) { pullRequest { id } } }`
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

// ClosePull closes a pull request without merging it.
//
// GitHub has no close reason for a pull request — `not_planned` and the rest are
// issue vocabulary — so nothing is carried alongside. The bridge says so rather
// than dropping it silently.
func (c *Client) ClosePull(pullID string) error {
	m := `mutation($input:ClosePullRequestInput!) { closePullRequest(input:$input) { pullRequest { id } } }`
	return c.mutate(m, map[string]any{"input": map[string]any{"pullRequestId": pullID}}, &struct{}{})
}

// ReopenPull returns a closed pull request to open. A merged one cannot be
// reopened, and GitHub reports that itself.
func (c *Client) ReopenPull(pullID string) error {
	m := `mutation($input:ReopenPullRequestInput!) { reopenPullRequest(input:$input) { pullRequest { id } } }`
	return c.mutate(m, map[string]any{"input": map[string]any{"pullRequestId": pullID}}, &struct{}{})
}

// SetPullDraft moves a pull request between draft and ready.
//
// Two mutations rather than a field on updatePullRequest, because that is what
// GitHub offers: `draft` is settable at creation only.
func (c *Client) SetPullDraft(pullID string, draft bool) error {
	m := `mutation($input:MarkPullRequestReadyForReviewInput!) { markPullRequestReadyForReview(input:$input) { pullRequest { id } } }`
	if draft {
		m = `mutation($input:ConvertPullRequestToDraftInput!) { convertPullRequestToDraft(input:$input) { pullRequest { id } } }`
	}
	return c.mutate(m, map[string]any{"input": map[string]any{"pullRequestId": pullID}}, &struct{}{})
}

// RequestReviews sets who has been asked to read a pull request.
//
// The whole set, not a difference: requestReviews replaces what is there unless
// `union` is set, and there is no remove mutation at all. The caller therefore
// works out the set it wants and states it, which also means a reviewer added
// upstream since the last pull is preserved only because the caller folded it in.
func (c *Client) RequestReviews(pullID string, userIDs, teamIDs []string) error {
	input := map[string]any{"pullRequestId": pullID, "userIds": userIDs, "teamIds": teamIDs}
	m := `mutation($input:RequestReviewsInput!) { requestReviews(input:$input) { pullRequest { id } } }`
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

// The review events addPullRequestReview takes.
const (
	EventApprove        = "APPROVE"
	EventRequestChanges = "REQUEST_CHANGES"
	EventComment        = "COMMENT"
)

// AddPullReview submits one verdict and returns the review's node id.
//
// commitOID is the revision the verdict is cast against, which is the field that
// makes staleness derivable on the way back in. GitHub accepts none and pins the
// review to the head at submission time; passing the local `head.sha` says what
// the person was actually looking at.
func (c *Client) AddPullReview(pullID, event, body, commitOID string) (string, error) {
	input := map[string]any{"pullRequestId": pullID, "event": event}
	if body != "" {
		input["body"] = body
	}
	if commitOID != "" {
		input["commitOID"] = commitOID
	}

	var resp struct {
		AddPullRequestReview struct {
			PullRequestReview struct {
				ID string `json:"id"`
			} `json:"pullRequestReview"`
		} `json:"addPullRequestReview"`
	}
	m := `mutation($input:AddPullRequestReviewInput!) { addPullRequestReview(input:$input) { pullRequestReview { id } } }`
	if err := c.mutate(m, map[string]any{"input": input}, &resp); err != nil {
		return "", err
	}
	id := resp.AddPullRequestReview.PullRequestReview.ID
	if id == "" {
		return "", fmt.Errorf("addPullRequestReview returned no review id")
	}
	return id, nil
}

// DismissPullReview withdraws a submitted review.
//
// GitHub requires a message, so an empty one is filled in rather than refused:
// the local dismissal is the fact being mirrored, and failing the push over a
// missing sentence would lose it.
func (c *Client) DismissPullReview(reviewID, message string) error {
	if strings.TrimSpace(message) == "" {
		message = "Dismissed."
	}
	input := map[string]any{"pullRequestReviewId": reviewID, "message": message}
	m := `mutation($input:DismissPullRequestReviewInput!) { dismissPullRequestReview(input:$input) { pullRequestReview { id } } }`
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

// UpdatePullReview rewrites a submitted review's body.
//
// A review body is a third comment shape, addressed by neither
// updateIssueComment nor updatePullRequestReviewComment: the id names a
// PullRequestReview. See review.CommentVerdict for how a delta says which shape
// it is holding.
func (c *Client) UpdatePullReview(reviewID, body string) error {
	m := `mutation($input:UpdatePullRequestReviewInput!) { updatePullRequestReview(input:$input) { pullRequestReview { id } } }`
	input := map[string]any{"pullRequestReviewId": reviewID, "body": body}
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

// NewThreadInput is where a review thread is opened and what it says.
type NewThreadInput struct {
	Path string
	// Line is the last line of the anchored range, StartLine the first. GitHub
	// takes only `line` for a single-line thread, and rejects a startLine equal
	// to it.
	Line      int
	StartLine int
	// Side is LEFT or RIGHT. Empty means RIGHT, which is GitHub's own default.
	Side string
	Body string
}

// AddReviewThread opens a review thread and returns the thread's node id
// alongside its first comment's.
//
// Both ids matter and they are different objects: the thread is what a
// resolution names, the comment is what an edit names. Recording only one would
// leave the push unable to do the other.
func (c *Client) AddReviewThread(pullID string, in NewThreadInput) (threadID, commentID string, err error) {
	input := map[string]any{
		"pullRequestId": pullID,
		"path":          in.Path,
		"line":          in.Line,
		"body":          in.Body,
	}
	if in.StartLine > 0 && in.StartLine < in.Line {
		input["startLine"] = in.StartLine
		input["startSide"] = side(in.Side)
	}
	if s := side(in.Side); s != "" {
		input["side"] = s
	}

	var resp struct {
		AddPullRequestReviewThread struct {
			Thread struct {
				ID       string `json:"id"`
				Comments struct {
					Nodes []struct {
						ID string `json:"id"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"thread"`
		} `json:"addPullRequestReviewThread"`
	}
	m := `mutation($input:AddPullRequestReviewThreadInput!) {
	  addPullRequestReviewThread(input:$input) {
	    thread { id comments(first:1) { nodes { id } } }
	  }
	}`
	if err := c.mutate(m, map[string]any{"input": input}, &resp); err != nil {
		return "", "", err
	}
	t := resp.AddPullRequestReviewThread.Thread
	if t.ID == "" {
		return "", "", fmt.Errorf("addPullRequestReviewThread returned no thread")
	}
	if len(t.Comments.Nodes) > 0 {
		commentID = t.Comments.Nodes[0].ID
	}
	return t.ID, commentID, nil
}

func side(s string) string {
	if strings.EqualFold(s, "left") {
		return "LEFT"
	}
	return "RIGHT"
}

// AddThreadReply appends to an existing review thread and returns the new
// comment's node id.
func (c *Client) AddThreadReply(threadID, body string) (string, error) {
	var resp struct {
		AddPullRequestReviewThreadReply struct {
			Comment struct {
				ID string `json:"id"`
			} `json:"comment"`
		} `json:"addPullRequestReviewThreadReply"`
	}
	m := `mutation($input:AddPullRequestReviewThreadReplyInput!) {
	  addPullRequestReviewThreadReply(input:$input) { comment { id } }
	}`
	input := map[string]any{"pullRequestReviewThreadId": threadID, "body": body}
	if err := c.mutate(m, map[string]any{"input": input}, &resp); err != nil {
		return "", err
	}
	id := resp.AddPullRequestReviewThreadReply.Comment.ID
	if id == "" {
		return "", fmt.Errorf("addPullRequestReviewThreadReply returned no comment id")
	}
	return id, nil
}

// SetThreadResolved resolves or reopens a review thread.
func (c *Client) SetThreadResolved(threadID string, resolved bool) error {
	m := `mutation($input:UnresolveReviewThreadInput!) { unresolveReviewThread(input:$input) { thread { id } } }`
	if resolved {
		m = `mutation($input:ResolveReviewThreadInput!) { resolveReviewThread(input:$input) { thread { id } } }`
	}
	return c.mutate(m, map[string]any{"input": map[string]any{"threadId": threadID}}, &struct{}{})
}

// UpdateReviewComment rewrites a review-thread entry's body.
//
// Not UpdateComment: a review comment is a PullRequestReviewComment, and
// updateIssueComment does not address one. Sending the wrong mutation for the id
// is a type error GitHub reports, so the caller has to know which shape it
// holds — which is why CommentPush carries the anchor.
func (c *Client) UpdateReviewComment(commentID, body string) error {
	m := `mutation($input:UpdatePullRequestReviewCommentInput!) { updatePullRequestReviewComment(input:$input) { pullRequestReviewComment { id } } }`
	input := map[string]any{"pullRequestReviewCommentId": commentID, "body": body}
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

// DeleteReviewComment removes a review-thread entry outright, the counterpart of
// DeleteComment for the anchored kind.
func (c *Client) DeleteReviewComment(commentID string) error {
	m := `mutation($input:DeletePullRequestReviewCommentInput!) { deletePullRequestReviewComment(input:$input) { clientMutationId } }`
	return c.mutate(m, map[string]any{"input": map[string]any{"id": commentID}}, &struct{}{})
}

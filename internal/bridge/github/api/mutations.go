// The write half of the GitHub client: the mutations a push needs, and the id
// lookups they depend on.
//
// GraphQL mutations address everything by node id, and almost nothing a local
// issue holds is one. A label is a name, an assignee is a login, a milestone is
// a title — so each of those has to be resolved against the repository before it
// can be written. That is what most of this file is.

package ghapi

import (
	"fmt"
	"strings"
)

// mutate runs one mutation. GraphQL carries mutations in the same request field
// as queries, so this is query with a name that says what it does at the call
// site.
func (c *Client) mutate(m string, vars map[string]any, out any) error {
	return c.query(m, vars, out)
}

// RepoID is the repository's node id, which createIssue needs and nothing else
// supplies.
func (c *Client) RepoID(t Target) (string, error) {
	var resp struct {
		Repository struct {
			ID string `json:"id"`
		} `json:"repository"`
	}
	q := `query($owner:String!, $name:String!) { repository(owner:$owner, name:$name) { id } }`
	if err := c.query(q, map[string]any{"owner": t.Owner, "name": t.Name}, &resp); err != nil {
		return "", err
	}
	if resp.Repository.ID == "" {
		return "", fmt.Errorf("%s: no such repository", t)
	}
	return resp.Repository.ID, nil
}

// Labels maps a repository's label names to their node ids.
//
// The whole set at once rather than one lookup per label: a push touching fifty
// issues would otherwise spend fifty requests rediscovering the same handful of
// labels, and repositories rarely have more than a few hundred.
func (c *Client) Labels(t Target) (map[string]string, error) {
	return c.namedNodes(t, "labels", `nodes { id name }`, func(n namedNode) string { return n.Name })
}

// Milestones maps a repository's milestone titles to their node ids. Only open
// milestones are offered by GitHub's own UI, but a closed one can still hold
// issues, so every state is read.
func (c *Client) Milestones(t Target) (map[string]string, error) {
	return c.namedNodes(t, "milestones", `nodes { id title }`, func(n namedNode) string { return n.Title })
}

type namedNode struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Title string `json:"title"`
}

// namedNodes pages one of the repository's name-to-id collections.
func (c *Client) namedNodes(t Target, field, selection string, name func(namedNode) string) (map[string]string, error) {
	out := map[string]string{}
	cursor := ""
	for {
		var resp struct {
			Repository map[string]struct {
				PageInfo pageInfo    `json:"pageInfo"`
				Nodes    []namedNode `json:"nodes"`
			} `json:"repository"`
		}
		q := fmt.Sprintf(`
query($owner:String!, $name:String!, $cursor:String) {
  repository(owner:$owner, name:$name) {
    %s(first:%d, after:$cursor) { pageInfo { hasNextPage endCursor } %s }
  }
}`, field, nestedPage, selection)

		vars := map[string]any{"owner": t.Owner, "name": t.Name, "cursor": nil}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if err := c.query(q, vars, &resp); err != nil {
			return nil, err
		}
		conn := resp.Repository[field]
		for _, n := range conn.Nodes {
			if key := name(n); key != "" {
				out[key] = n.ID
			}
		}
		if !conn.PageInfo.HasNextPage {
			return out, nil
		}
		cursor = conn.PageInfo.EndCursor
	}
}

// UserIDs maps logins to node ids, for the assignee mutations.
//
// One aliased query rather than one request per login, and a login that does not
// resolve is simply absent from the result: an assignee who has since deleted
// their account, or who was never a GitHub user at all, must not take down a
// push that has other things to say.
func (c *Client) UserIDs(logins []string) (map[string]string, error) {
	out := map[string]string{}
	if len(logins) == 0 {
		return out, nil
	}

	var b strings.Builder
	b.WriteString("query(")
	for i := range logins {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "$l%d:String!", i)
	}
	b.WriteString(") {\n")
	for i := range logins {
		fmt.Fprintf(&b, "  u%d: user(login:$l%d) { id login }\n", i, i)
	}
	b.WriteString("}")

	vars := map[string]any{}
	for i, login := range logins {
		vars[fmt.Sprintf("l%d", i)] = login
	}

	var resp map[string]*struct {
		ID    string `json:"id"`
		Login string `json:"login"`
	}
	if err := c.query(b.String(), vars, &resp); err != nil {
		// A login that does not exist makes the whole aliased query report an
		// error alongside partial data. Fall back to asking one at a time so the
		// ones that do resolve still come back.
		return c.userIDsOneByOne(logins)
	}
	for _, u := range resp {
		if u != nil && u.Login != "" {
			out[u.Login] = u.ID
		}
	}
	return out, nil
}

func (c *Client) userIDsOneByOne(logins []string) (map[string]string, error) {
	out := map[string]string{}
	for _, login := range logins {
		var resp struct {
			User *struct {
				ID string `json:"id"`
			} `json:"user"`
		}
		q := `query($login:String!) { user(login:$login) { id } }`
		if err := c.query(q, map[string]any{"login": login}, &resp); err != nil {
			continue
		}
		if resp.User != nil {
			out[login] = resp.User.ID
		}
	}
	return out, nil
}

// NewIssue is what createIssue gives back: the identity to record, and the
// locator to show.
type NewIssue struct {
	ID  string
	URL string
}

// NewIssueInput is everything an issue can be filed with in one call. A field
// left empty is left out of the mutation entirely, which is not the same as
// sending it null.
type NewIssueInput struct {
	RepoID      string
	Title       string
	Body        string
	LabelIDs    []string
	AssigneeIDs []string
	MilestoneID string
	// ParentID files the new issue under another one as it is created. GitHub
	// takes it here as well as through addSubIssue, and here is better: a
	// sub-issue added afterwards raises a ParentIssueAddedEvent for a link the
	// issue was born with.
	ParentID string
}

// CreateIssue files a new issue.
//
// Everything the issue is filed with goes in this one call rather than as a
// create followed by edits. That is not only cheaper: an issue created bare and
// then labelled generates timeline events for each change, so the next import
// would read back a history of edits that never happened.
func (c *Client) CreateIssue(in NewIssueInput) (NewIssue, error) {
	input := map[string]any{"repositoryId": in.RepoID, "title": in.Title}
	if in.Body != "" {
		input["body"] = in.Body
	}
	if len(in.LabelIDs) > 0 {
		input["labelIds"] = in.LabelIDs
	}
	if len(in.AssigneeIDs) > 0 {
		input["assigneeIds"] = in.AssigneeIDs
	}
	if in.MilestoneID != "" {
		input["milestoneId"] = in.MilestoneID
	}
	if in.ParentID != "" {
		input["parentIssueId"] = in.ParentID
	}

	var resp struct {
		CreateIssue struct {
			Issue NewIssue `json:"issue"`
		} `json:"createIssue"`
	}
	m := `mutation($input:CreateIssueInput!) { createIssue(input:$input) { issue { id url } } }`
	if err := c.mutate(m, map[string]any{"input": input}, &resp); err != nil {
		return NewIssue{}, err
	}
	if resp.CreateIssue.Issue.ID == "" {
		return NewIssue{}, fmt.Errorf("createIssue returned no issue")
	}
	return resp.CreateIssue.Issue, nil
}

// UpdateIssue writes the scalar fields that updateIssue covers. Fields carries
// GitHub's own input keys, and a nil value clears — which is how a milestone is
// removed.
func (c *Client) UpdateIssue(issueID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	input := map[string]any{"id": issueID}
	for k, v := range fields {
		input[k] = v
	}
	m := `mutation($input:UpdateIssueInput!) { updateIssue(input:$input) { issue { id } } }`
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

// CloseIssue closes an issue, with a reason when there is one to give.
//
// closeIssue rather than updateIssue(state:) because the dedicated mutation is
// what produces a ClosedEvent carrying the reason, which is what the timeline
// replay reads back on the next import.
func (c *Client) CloseIssue(issueID, reason string) error {
	input := map[string]any{"issueId": issueID}
	if reason != "" {
		input["stateReason"] = strings.ToUpper(reason)
	}
	m := `mutation($input:CloseIssueInput!) { closeIssue(input:$input) { issue { id } } }`
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

// ReopenIssue returns an issue to open.
func (c *Client) ReopenIssue(issueID string) error {
	m := `mutation($input:ReopenIssueInput!) { reopenIssue(input:$input) { issue { id } } }`
	input := map[string]any{"issueId": issueID}
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

// AddLabels and RemoveLabels move label membership. Both take node ids, which
// is what Labels resolves.
func (c *Client) AddLabels(issueID string, labelIDs []string) error {
	return c.labelable(issueID, labelIDs, "addLabelsToLabelable", "AddLabelsToLabelableInput")
}

func (c *Client) RemoveLabels(issueID string, labelIDs []string) error {
	return c.labelable(issueID, labelIDs, "removeLabelsFromLabelable", "RemoveLabelsFromLabelableInput")
}

func (c *Client) labelable(issueID string, labelIDs []string, mutation, inputType string) error {
	if len(labelIDs) == 0 {
		return nil
	}
	m := fmt.Sprintf(`mutation($input:%s!) { %s(input:$input) { clientMutationId } }`, inputType, mutation)
	input := map[string]any{"labelableId": issueID, "labelIds": labelIDs}
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

// AddAssignees and RemoveAssignees move assignee membership.
func (c *Client) AddAssignees(issueID string, userIDs []string) error {
	return c.assignable(issueID, userIDs, "addAssigneesToAssignable", "AddAssigneesToAssignableInput")
}

func (c *Client) RemoveAssignees(issueID string, userIDs []string) error {
	return c.assignable(issueID, userIDs, "removeAssigneesFromAssignable", "RemoveAssigneesFromAssignableInput")
}

func (c *Client) assignable(issueID string, userIDs []string, mutation, inputType string) error {
	if len(userIDs) == 0 {
		return nil
	}
	m := fmt.Sprintf(`mutation($input:%s!) { %s(input:$input) { clientMutationId } }`, inputType, mutation)
	input := map[string]any{"assignableId": issueID, "assigneeIds": userIDs}
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

// AddSubIssue and RemoveSubIssue move a parent link.
//
// Both are addressed from the **parent**: `issueId` is the issue that gains or
// loses a child. That is the opposite end from the one the link is stored on
// here (docs/issues.md), which is a property of GitHub's API rather than a
// disagreement about direction — the same edge, named from the other side.
//
// GitHub allows an issue one parent, so re-filing is a removal and then an
// addition rather than an overwrite. `replaceParent` is deliberately not sent:
// an issue that turns out to have a parent upstream that this clone never saw
// is a concurrent change, and taking it silently is exactly what a push must
// not do.
func (c *Client) AddSubIssue(parentID, childID string) error {
	m := `mutation($input:AddSubIssueInput!) { addSubIssue(input:$input) { clientMutationId } }`
	input := map[string]any{"issueId": parentID, "subIssueId": childID}
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

func (c *Client) RemoveSubIssue(parentID, childID string) error {
	m := `mutation($input:RemoveSubIssueInput!) { removeSubIssue(input:$input) { clientMutationId } }`
	input := map[string]any{"issueId": parentID, "subIssueId": childID}
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

// AddBlockedBy and RemoveBlockedBy move a dependency. These are addressed from
// the blocked issue, which is the end the link is stored on here too.
func (c *Client) AddBlockedBy(issueID, blockingID string) error {
	m := `mutation($input:AddBlockedByInput!) { addBlockedBy(input:$input) { clientMutationId } }`
	input := map[string]any{"issueId": issueID, "blockingIssueId": blockingID}
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

func (c *Client) RemoveBlockedBy(issueID, blockingID string) error {
	m := `mutation($input:RemoveBlockedByInput!) { removeBlockedBy(input:$input) { clientMutationId } }`
	input := map[string]any{"issueId": issueID, "blockingIssueId": blockingID}
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

// AddComment posts a comment and returns its node id, which the caller records
// in the origin ledger — without it, the next push would post the same comment
// again.
func (c *Client) AddComment(issueID, body string) (string, error) {
	var resp struct {
		AddComment struct {
			CommentEdge struct {
				Node struct {
					ID string `json:"id"`
				} `json:"node"`
			} `json:"commentEdge"`
		} `json:"addComment"`
	}
	m := `mutation($input:AddCommentInput!) { addComment(input:$input) { commentEdge { node { id } } } }`
	input := map[string]any{"subjectId": issueID, "body": body}
	if err := c.mutate(m, map[string]any{"input": input}, &resp); err != nil {
		return "", err
	}
	id := resp.AddComment.CommentEdge.Node.ID
	if id == "" {
		return "", fmt.Errorf("addComment returned no comment id")
	}
	return id, nil
}

// UpdateComment rewrites a comment's body.
func (c *Client) UpdateComment(commentID, body string) error {
	m := `mutation($input:UpdateIssueCommentInput!) { updateIssueComment(input:$input) { clientMutationId } }`
	input := map[string]any{"id": commentID, "body": body}
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

// DeleteComment removes a comment outright.
//
// This is the one irreversible thing a push does, and it is reached only from a
// local comment.remove naming an entry this repository posted — never from an
// entity removal, which unlists locally and must not delete anything upstream.
func (c *Client) DeleteComment(commentID string) error {
	m := `mutation($input:DeleteIssueCommentInput!) { deleteIssueComment(input:$input) { clientMutationId } }`
	input := map[string]any{"id": commentID}
	return c.mutate(m, map[string]any{"input": input}, &struct{}{})
}

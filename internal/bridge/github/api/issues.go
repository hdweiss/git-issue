package ghapi

import (
	"fmt"
	"strings"
	"time"
)

// Issue is one GitHub issue with everything an import needs, as GitHub reports
// it. Nothing here is translated: `State` is GitHub's OPEN/CLOSED, `Author` is
// a bare login, and mapping those onto the tracker's vocabulary happens a
// layer up.
type Issue struct {
	ID        string // the node id, which is the stable identity
	Number    int
	URL       string
	Title     string
	Body      string
	CreatedAt time.Time
	// UpdatedAt is what the `since` filter compares against, so it is also
	// what a sync watermark has to be taken from — any other clock would
	// compare two different notions of time.
	UpdatedAt  time.Time
	State      string
	Reason     string
	Author     string
	Milestone  string
	Type       string
	Locked     bool
	LockReason string
	Labels     []string
	Assignees  []string
	Comments   []Comment
	Timeline   []TimelineItem

	// Parent and DuplicateOf are the node ids of the issues this one is filed
	// under and duplicates, as GitHub currently reports them. Both are current
	// state rather than history: an import reconciles from them what the
	// timeline did not already say.
	Parent      string
	DuplicateOf string

	// Cursors for connections that did not fit in the page that named this
	// issue. Empty means complete.
	commentCursor  string
	timelineCursor string
}

// Comment is one issue comment.
type Comment struct {
	ID        string
	Body      string
	CreatedAt time.Time
	Author    string
}

// TimelineItem is one entry of an issue's timeline, flattened.
//
// GitHub's timeline is a union of a few dozen types, and the handful this
// imports differ only in which one or two payload fields they carry. A flat
// struct with a Kind discriminator unmarshals straight out of the merged
// fragment selection, and reads better at the mapping site than a type switch
// over a dozen near-empty structs would.
type TimelineItem struct {
	Kind      string
	ID        string
	CreatedAt time.Time
	Actor     string

	Label         string
	Assignee      string
	PreviousTitle string
	CurrentTitle  string
	Reason        string // close reason, or lock reason
	Milestone     string
	// Target is the node id of the other issue a relation event names: the
	// parent an issue was filed under, the issue that blocks it. One field,
	// because no event carries two of them.
	Target string
}

// Timeline item kinds this bridge understands. Everything else GitHub might
// return is not requested at all — subscribed/unsubscribed above all, which is
// the highest-volume type in sampled data and is personal state that does not
// belong in a shared tracker (docs/bridge-github.md).
const (
	Labeled      = "LabeledEvent"
	Unlabeled    = "UnlabeledEvent"
	Assigned     = "AssignedEvent"
	Unassigned   = "UnassignedEvent"
	Renamed      = "RenamedTitleEvent"
	Closed       = "ClosedEvent"
	Reopened     = "ReopenedEvent"
	Milestoned   = "MilestonedEvent"
	Demilestoned = "DemilestonedEvent"
	Locked       = "LockedEvent"
	Unlocked     = "UnlockedEvent"
	Pinned       = "PinnedEvent"
	Unpinned     = "UnpinnedEvent"
)

// Relation timeline kinds, which are the recent half of GitHub's schema and are
// requested only where the server has them.
//
// Only the dependent end of each pair is here. `SubIssueAddedEvent` and
// `BlockingAddedEvent` are the same links seen from the other issue, and that
// issue is the one they would have to be written on — so requesting them would
// buy an event this import cannot use. The inverse is derived by a reader
// (docs/issues.md), never stored.
//
// There is no duplicate event in this list on purpose: GitHub raises
// `MarkedAsDuplicateEvent` on the *canonical* issue, naming the duplicate,
// while the duplicate's own timeline says nothing. Confirmed against
// nodejs/node#56645 and #58091. The duplicate end therefore has no history to
// replay and imports from `duplicateOf` instead.
const (
	ParentAdded      = "ParentIssueAddedEvent"
	ParentRemoved    = "ParentIssueRemovedEvent"
	BlockedByAdded   = "BlockedByAddedEvent"
	BlockedByRemoved = "BlockedByRemovedEvent"
)

var timelineKinds = []string{
	Labeled, Unlabeled, Assigned, Unassigned, Renamed, Closed, Reopened,
	Milestoned, Demilestoned, Locked, Unlocked, Pinned, Unpinned,
}

var relationKinds = []string{ParentAdded, ParentRemoved, BlockedByAdded, BlockedByRemoved}

// timelineItemTypes is the same list in the spelling the itemTypes argument
// wants, for the features one query is asking for.
//
// GraphQL uses two names for one thing here: the response's __typename is
// `LabeledEvent`, while the enum that selects it is `LABELED_EVENT`. Deriving
// the second from the first keeps a single list, so a kind cannot be requested
// under one name and then go unhandled under the other.
//
// It also keeps the argument and the fragments in step. A server without the
// relationship types rejects `PARENT_ISSUE_ADDED_EVENT` as an enum value just
// as it rejects the fragment, so both have to be dropped by the same decision.
func timelineItemTypes(s schema) []string {
	kinds := timelineKinds
	if s.relations {
		kinds = append(append([]string{}, kinds...), relationKinds...)
	}
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, enumName(k))
	}
	return out
}

// enumName converts a GraphQL type name to its SCREAMING_SNAKE enum spelling.
func enumName(typename string) string {
	var b strings.Builder
	for i, r := range typename {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String())
}

// GitHub's issue states, as the states argument spells them.
const (
	StateOpen   = "OPEN"
	StateClosed = "CLOSED"
)

// Filter narrows what an import reads.
type Filter struct {
	// States restricts to issues in these GitHub states; empty means every
	// state. StateOpen is the default a command should pass, because the
	// common case is wanting the work that is still live rather than a decade
	// of resolved tickets.
	States []string
	// Since restricts to issues updated at or after a time. This is what makes
	// a second import cheap; it cannot see deletions, which is why it is a
	// filter rather than the only mode.
	Since time.Time
	// Limit stops after this many issues, 0 for no limit. Pages arrive whole,
	// so a limit inside a page is honoured by discarding the remainder rather
	// than by asking for a smaller page.
	Limit int
}

// The page sizes are the maximum GraphQL allows, which is also what the two
// budgets that could argue for less turn out to permit.
//
// The node budget is not the binding constraint it looks like. A full page
// costs issues × (comments + timeline + labels + assignees) nodes — 100 × 400,
// plus the issues themselves, so 40,100 against a documented ceiling of
// 500,000. Twelve times the headroom.
//
// Nor is the hourly points budget, because the cost does not depend on the page
// size at all. Points count one request per connection per parent node, so a
// page costs 1 + 4 × issues, rounded down by a hundred: 1 point for 25 issues,
// 4 points for 100. Either way an issue costs 0.04 points, and the 5,000-point
// hourly budget buys around 125,000 of them.
//
// What the page size does buy is round trips, and those are what an import
// actually spends its time on: a cursor-paged walk cannot overlap them, so 100
// per page is four times less waiting than 25. The cost is a heavier query,
// which on issues with large comment threads can run into GitHub's own
// execution timeout; that comes back as an error rather than as silent
// truncation, so it is visible if it ever bites.
//
// The pull request connection is smaller for a reason that is not about speed
// at all — see pullsPerPage.
const (
	issuePage  = 100
	nestedPage = 100
)

// Fetch reads every issue in the target, calling progress as pages arrive so a
// long import can say what it is doing. The second progress argument is how
// many issues GitHub says match the filter — its own connection totalCount, or
// 0 when the server did not report one — capped at any --limit the caller set.
func (c *Client) Fetch(t Target, f Filter, progress func(fetched, total int)) ([]Issue, error) {
	var (
		issues []Issue
		cursor *string
	)
	for {
		page, next, total, err := c.issuePage(t, f, cursor)
		if err != nil {
			return nil, err
		}
		for i := range page {
			if err := c.completeIssue(&page[i]); err != nil {
				return nil, err
			}
		}
		issues = append(issues, page...)
		if progress != nil {
			if f.Limit > 0 && (total == 0 || total > f.Limit) {
				total = f.Limit
			}
			progress(len(issues), total)
		}
		if next == nil || (f.Limit > 0 && len(issues) >= f.Limit) {
			if f.Limit > 0 && len(issues) > f.Limit {
				issues = issues[:f.Limit]
			}
			return issues, nil
		}
		cursor = next
	}
}

// FetchNodes reads issues by node id — the ids the origin ledger holds.
//
// This is what a push uses to find out what the tracker currently says, and it
// is also the cheap answer to refreshing issues already held: an open-only
// import cannot see that an issue closed upstream, but naming the ids directly
// re-reads exactly the ones this clone knows about, without a search.
//
// Ids that no longer resolve come back as nulls rather than as errors — an issue
// deleted or transferred out is a fact to skip over, not a reason to abandon the
// run.
func (c *Client) FetchNodes(ids []string) ([]Issue, error) {
	var out []Issue
	for start := 0; start < len(ids); start += issuePage {
		end := min(start+issuePage, len(ids))

		var resp struct {
			Nodes []*issueNode `json:"nodes"`
		}
		vars := map[string]any{"ids": ids[start:end], "kinds": nil}
		if err := c.queryDegrading(nodesQuery, vars, &resp); err != nil {
			return nil, err
		}
		for _, n := range resp.Nodes {
			if n == nil {
				continue
			}
			issue := convertIssue(*n)
			if err := c.completeIssue(&issue); err != nil {
				return nil, err
			}
			out = append(out, issue)
		}
	}
	return out, nil
}

// issueResponse mirrors the shape of the issues query. Field names are
// GitHub's, so the query and the struct can be read against each other.
type issueResponse struct {
	Repository struct {
		Issues struct {
			TotalCount int         `json:"totalCount"`
			PageInfo   pageInfo    `json:"pageInfo"`
			Nodes      []issueNode `json:"nodes"`
		} `json:"issues"`
	} `json:"repository"`
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type actor struct {
	Login string `json:"login"`
}

type issueNode struct {
	ID          string    `json:"id"`
	Number      int       `json:"number"`
	URL         string    `json:"url"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	State       string    `json:"state"`
	StateReason string    `json:"stateReason"`
	Locked      bool      `json:"locked"`
	LockReason  string    `json:"activeLockReason"`
	Author      *actor    `json:"author"`
	Milestone   *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	IssueType *struct {
		Name string `json:"name"`
	} `json:"issueType"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Assignees struct {
		Nodes []actor `json:"nodes"`
	} `json:"assignees"`
	Parent        *idNode      `json:"parent"`
	DuplicateOf   *idNode      `json:"duplicateOf"`
	Comments      commentConn  `json:"comments"`
	TimelineItems timelineConn `json:"timelineItems"`
}

// idNode is another issue named by identity alone, which is all a relation
// needs: everything else about it belongs to that issue's own import.
type idNode struct {
	ID string `json:"id"`
}

type commentConn struct {
	PageInfo pageInfo      `json:"pageInfo"`
	Nodes    []commentNode `json:"nodes"`
}

type commentNode struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	Author    *actor    `json:"author"`
}

type timelineConn struct {
	PageInfo pageInfo       `json:"pageInfo"`
	Nodes    []timelineNode `json:"nodes"`
}

type timelineNode struct {
	Typename  string    `json:"__typename"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Actor     *actor    `json:"actor"`
	Label     *struct {
		Name string `json:"name"`
	} `json:"label"`
	Assignee       *actor  `json:"assignee"`
	PreviousTitle  string  `json:"previousTitle"`
	CurrentTitle   string  `json:"currentTitle"`
	StateReason    string  `json:"stateReason"`
	LockReason     string  `json:"lockReason"`
	MilestoneTitle string  `json:"milestoneTitle"`
	Parent         *idNode `json:"parent"`
	BlockingIssue  *idNode `json:"blockingIssue"`
}

func (c *Client) issuePage(t Target, f Filter, cursor *string) ([]Issue, *string, int, error) {
	vars := map[string]any{
		"owner":  t.Owner,
		"name":   t.Name,
		"cursor": cursor,
		// Filled in per attempt by queryDegrading, which is the only place that
		// knows which types this server will accept.
		"kinds": nil,
		"since": nil,
		// A null states argument is not the same as an empty list: null means
		// "no restriction", while [] would match nothing at all.
		"states": nil,
	}
	if len(f.States) > 0 {
		vars["states"] = f.States
	}
	if !f.Since.IsZero() {
		vars["since"] = f.Since.UTC().Format(time.RFC3339)
	}

	var resp issueResponse
	if err := c.queryDegrading(issuesQuery, vars, &resp); err != nil {
		return nil, nil, 0, err
	}

	conn := resp.Repository.Issues
	issues := make([]Issue, 0, len(conn.Nodes))
	for _, n := range conn.Nodes {
		issues = append(issues, convertIssue(n))
	}
	if conn.PageInfo.HasNextPage {
		next := conn.PageInfo.EndCursor
		return issues, &next, conn.TotalCount, nil
	}
	return issues, nil, conn.TotalCount, nil
}

// schema is which optional parts of GitHub's schema a query may name.
//
// GitHub Enterprise runs older schemas than github.com, and both issue types
// and issue relationships are recent enough that a query naming them fails
// outright against a server that has never heard of them. They are two flags
// rather than one because a server can have either without the other, and
// giving up a field nobody asked us to give up loses data for no reason.
type schema struct {
	issueTypes bool
	relations  bool
	// pullExtras covers the two parts of the pull request query a server can
	// lack: closingIssuesReferences and the status check rollup. One flag rather
	// than two because both landed in the same era and no deployment has been
	// seen with one and not the other — and because losing the pair costs a
	// review its `closes` links and its checks, neither of which is core state.
	pullExtras bool
}

// relationMarkers are the strings whose appearance in a GraphQL error names the
// relationship half of the schema as the thing the server rejected — the type
// names as the fragments spell them, and as the itemTypes enum does.
//
// `parent` and `duplicateOf` are deliberately not markers. They are short
// enough to appear in a message about something else entirely, and the catch-all
// below already covers a schema error that implicates nothing specific.
var relationMarkers = func() []string {
	out := []string{}
	for _, k := range relationKinds {
		out = append(out, k, enumName(k))
	}
	return out
}()

// queryDegrading retries without the newest schema fields when the server
// rejects them, and remembers that it had to.
//
// The verdict is cached because a schema does not change underneath a run.
// Probing per call would spend a guaranteed failed request on every page of
// every import against such a server — doubling the request count to learn the
// same thing the first page already established.
func (c *Client) queryDegrading(build func(schema) string, vars map[string]any, out any) error {
	for {
		s := c.schema()
		// The itemTypes argument selects the same types the fragments name, so
		// it is filled in per attempt rather than by the caller.
		if _, ok := vars["kinds"]; ok {
			vars["kinds"] = timelineItemTypes(s)
		}
		err := c.query(build(s), vars, out)
		if err == nil {
			return nil
		}
		e, ok := err.(*Error)
		if !ok || !c.degrade(s, e.Messages) {
			return err
		}
	}
}

func (c *Client) schema() schema {
	return schema{
		issueTypes: !c.noIssueTypes.Load(),
		relations:  !c.noRelations.Load(),
		pullExtras: !c.noPullExtras.Load(),
	}
}

// degrade switches off whichever optional features an error implicates, and
// reports whether anything was switched off — which is also the signal to retry.
//
// A message that implicates nothing in particular but is plainly about the
// schema costs every remaining feature at once. That is the crude case on
// purpose: matching error text is a guess, so the fallback has to be the one
// that cannot loop, and losing an optional field is always better than losing
// the import.
func (c *Client) degrade(s schema, messages []string) bool {
	switch {
	case s.issueTypes && mentions(messages, "issueType"):
		c.noIssueTypes.Store(true)
	case s.relations && mentions(messages, relationMarkers...):
		c.noRelations.Store(true)
	case s.pullExtras && mentions(messages, "closingIssuesReferences", "statusCheckRollup", "CheckRun", "StatusContext"):
		c.noPullExtras.Store(true)
	case !mentionsUnknownField(messages) || !(s.issueTypes || s.relations || s.pullExtras):
		return false
	default:
		c.noIssueTypes.Store(true)
		c.noRelations.Store(true)
		c.noPullExtras.Store(true)
	}
	return true
}

func mentions(messages []string, tokens ...string) bool {
	for _, m := range messages {
		for _, token := range tokens {
			if strings.Contains(m, token) {
				return true
			}
		}
	}
	return false
}

// mentionsUnknownField reports that an error is about the shape of the query
// rather than about the data it asked for. An unknown enum value counts: that
// is how a server without the relationship types rejects the itemTypes
// argument.
func mentionsUnknownField(messages []string) bool {
	return mentions(messages,
		"doesn't exist on type", "Unknown field", "Unknown type",
		"is not a valid enum value", "provided invalid value")
}

// completeIssue pages the connections that did not fit in the issue page.
//
// Most issues need none of this: an issue with under a hundred comments and
// under a hundred timeline entries arrives complete in the page that named it.
func (c *Client) completeIssue(issue *Issue) error {
	for _, more := range []struct {
		cursor string
		fetch  func(*Issue, string) (string, error)
	}{
		{issue.commentCursor, c.moreComments},
		{issue.timelineCursor, c.moreTimeline},
	} {
		for cursor := more.cursor; cursor != ""; {
			next, err := more.fetch(issue, cursor)
			if err != nil {
				return err
			}
			cursor = next
		}
	}
	return nil
}

func (c *Client) moreComments(issue *Issue, cursor string) (string, error) {
	var resp struct {
		Node struct {
			Comments commentConn `json:"comments"`
		} `json:"node"`
	}
	vars := map[string]any{"id": issue.ID, "cursor": cursor}
	if err := c.query(moreCommentsQuery, vars, &resp); err != nil {
		return "", err
	}
	for _, n := range resp.Node.Comments.Nodes {
		issue.Comments = append(issue.Comments, convertComment(n))
	}
	return nextCursor(resp.Node.Comments.PageInfo), nil
}

func (c *Client) moreTimeline(issue *Issue, cursor string) (string, error) {
	var resp struct {
		Node struct {
			TimelineItems timelineConn `json:"timelineItems"`
		} `json:"node"`
	}
	vars := map[string]any{"id": issue.ID, "cursor": cursor, "kinds": nil}
	if err := c.queryDegrading(moreTimelineQuery, vars, &resp); err != nil {
		return "", err
	}
	for _, n := range resp.Node.TimelineItems.Nodes {
		issue.Timeline = append(issue.Timeline, convertTimelineItem(n))
	}
	return nextCursor(resp.Node.TimelineItems.PageInfo), nil
}

// nextCursor is the cursor to continue from, or "" when a connection is
// exhausted.
func nextCursor(p pageInfo) string {
	if p.HasNextPage {
		return p.EndCursor
	}
	return ""
}

func convertIssue(n issueNode) Issue {
	issue := Issue{
		ID:         n.ID,
		Number:     n.Number,
		URL:        n.URL,
		Title:      n.Title,
		Body:       n.Body,
		CreatedAt:  n.CreatedAt,
		UpdatedAt:  n.UpdatedAt,
		State:      n.State,
		Reason:     n.StateReason,
		Locked:     n.Locked,
		LockReason: n.LockReason,
		Author:     login(n.Author),
	}
	if n.Milestone != nil {
		issue.Milestone = n.Milestone.Title
	}
	if n.IssueType != nil {
		issue.Type = n.IssueType.Name
	}
	issue.Parent = nodeID(n.Parent)
	issue.DuplicateOf = nodeID(n.DuplicateOf)
	for _, l := range n.Labels.Nodes {
		issue.Labels = append(issue.Labels, l.Name)
	}
	for _, a := range n.Assignees.Nodes {
		issue.Assignees = append(issue.Assignees, a.Login)
	}
	for _, cn := range n.Comments.Nodes {
		issue.Comments = append(issue.Comments, convertComment(cn))
	}
	for _, tn := range n.TimelineItems.Nodes {
		issue.Timeline = append(issue.Timeline, convertTimelineItem(tn))
	}
	if n.Comments.PageInfo.HasNextPage {
		issue.commentCursor = n.Comments.PageInfo.EndCursor
	}
	if n.TimelineItems.PageInfo.HasNextPage {
		issue.timelineCursor = n.TimelineItems.PageInfo.EndCursor
	}
	return issue
}

func convertComment(n commentNode) Comment {
	return Comment{ID: n.ID, Body: n.Body, CreatedAt: n.CreatedAt, Author: login(n.Author)}
}

func convertTimelineItem(n timelineNode) TimelineItem {
	item := TimelineItem{
		Kind:          n.Typename,
		ID:            n.ID,
		CreatedAt:     n.CreatedAt,
		Actor:         login(n.Actor),
		PreviousTitle: n.PreviousTitle,
		CurrentTitle:  n.CurrentTitle,
		Milestone:     n.MilestoneTitle,
	}
	if n.Label != nil {
		item.Label = n.Label.Name
	}
	if n.Assignee != nil {
		item.Assignee = n.Assignee.Login
	}
	// Close reason and lock reason never appear on the same type, so one field
	// carries both without ambiguity. Nor do the two relation ends: a parent
	// event names a parent and a blocking event names a blocker.
	item.Reason = n.StateReason
	if item.Reason == "" {
		item.Reason = n.LockReason
	}
	item.Target = nodeID(n.Parent)
	if item.Target == "" {
		item.Target = nodeID(n.BlockingIssue)
	}
	return item
}

func nodeID(n *idNode) string {
	if n == nil {
		return ""
	}
	return n.ID
}

// login is "" for a deleted account, which GitHub reports as a null author.
func login(a *actor) string {
	if a == nil {
		return ""
	}
	return a.Login
}

// timelineSelection is the fragments for the item types one query asked for.
// A fragment on a type the server does not have is an error even when nothing
// selects it, so the relationship ones appear only where they exist.
func timelineSelection(s schema) string {
	return timelineBase + relationFragments(s) + `
  }`
}

func relationFragments(s schema) string {
	if !s.relations {
		return ""
	}
	return `
    ... on ParentIssueAddedEvent   { createdAt actor { login } parent { id } }
    ... on ParentIssueRemovedEvent { createdAt actor { login } parent { id } }
    ... on BlockedByAddedEvent     { createdAt actor { login } blockingIssue { id } }
    ... on BlockedByRemovedEvent   { createdAt actor { login } blockingIssue { id } }`
}

const timelineBase = `
  pageInfo { hasNextPage endCursor }
  nodes {
    __typename
    ... on Node { id }
    ... on LabeledEvent            { createdAt actor { login } label { name } }
    ... on UnlabeledEvent          { createdAt actor { login } label { name } }
    ... on AssignedEvent           { createdAt actor { login } assignee { ... on User { login } ... on Bot { login } ... on Organization { login } } }
    ... on UnassignedEvent         { createdAt actor { login } assignee { ... on User { login } ... on Bot { login } ... on Organization { login } } }
    ... on RenamedTitleEvent       { createdAt actor { login } previousTitle currentTitle }
    ... on ClosedEvent             { createdAt actor { login } stateReason }
    ... on ReopenedEvent           { createdAt actor { login } }
    ... on MilestonedEvent         { createdAt actor { login } milestoneTitle }
    ... on DemilestonedEvent       { createdAt actor { login } milestoneTitle }
    ... on LockedEvent             { createdAt actor { login } lockReason }
    ... on UnlockedEvent           { createdAt actor { login } }
    ... on PinnedEvent             { createdAt actor { login } }
    ... on UnpinnedEvent           { createdAt actor { login } }`

const commentSelection = `
  pageInfo { hasNextPage endCursor }
  nodes { id body createdAt author { login } }`

// issuesQuery is one page of issues with their comments and timelines.
//
// Ordering is by update time ascending so that a `since` filter and the cursor
// walk in the same direction, and a run interrupted halfway has still imported
// a prefix rather than an arbitrary scattering.
func issuesQuery(s schema) string {
	return fmt.Sprintf(`
query($owner:String!, $name:String!, $cursor:String, $since:DateTime, $states:[IssueState!], $kinds:[IssueTimelineItemsItemType!]) {
  repository(owner:$owner, name:$name) {
    issues(first:%d, after:$cursor, states:$states, filterBy:{since:$since}, orderBy:{field:UPDATED_AT, direction:ASC}) {
      totalCount
      pageInfo { hasNextPage endCursor }
      nodes {%s}
    }
  }
}`, issuePage, issueSelection(s))
}

// issueSelection is everything an import reads from one issue.
//
// Shared between the repository-wide query and the nodes(ids:) one so the two
// cannot drift: a field added to a listing but not to a refetch would make an
// issue import differently depending on which door it came in by, and the
// re-import convergence that the whole bridge rests on would stop holding.
func issueSelection(s schema) string {
	issueType := ""
	if s.issueTypes {
		issueType = "issueType { name }"
	}
	// Current state for the two relations the timeline cannot supply on this
	// end: a parent whose event predates the sub-issue feature or was lost to a
	// transfer, and a duplicate, which GitHub records only on the canonical.
	//
	// Both are single-valued fields, which is why they are here and `blockedBy`
	// is not: a connection cannot be reconciled without paging it, and a
	// half-read page would silently drop links. Blocking pairs have complete
	// timeline history, so nothing is lost by reading them from it alone.
	relations := ""
	if s.relations {
		relations = "parent { id } duplicateOf { id }"
	}
	return fmt.Sprintf(`
        id number url title body createdAt updatedAt state stateReason locked activeLockReason
        author { login }
        milestone { title }
        %s
        %s
        labels(first:%d) { nodes { name } }
        assignees(first:%d) { nodes { login } }
        comments(first:%d) {%s}
        timelineItems(first:%d, itemTypes:$kinds) {%s}`,
		issueType, relations, nestedPage, nestedPage, nestedPage, commentSelection, nestedPage, timelineSelection(s))
}

// nodesQuery reads issues by node id, which is what the origin ledger records
// and therefore how a push finds out what upstream currently says.
func nodesQuery(s schema) string {
	return fmt.Sprintf(`
query($ids:[ID!]!, $kinds:[IssueTimelineItemsItemType!]) {
  nodes(ids:$ids) { ... on Issue {%s} }
}`, issueSelection(s))
}

var moreCommentsQuery = fmt.Sprintf(`
query($id:ID!, $cursor:String!) {
  node(id:$id) { ... on Issue { comments(first:%d, after:$cursor) {%s} } }
}`, nestedPage, commentSelection)

// moreTimelineQuery names the same fragments the first page did, so it degrades
// the same way: a long timeline must not fail on page two against a server the
// first page already accommodated.
func moreTimelineQuery(s schema) string {
	return fmt.Sprintf(`
query($id:ID!, $cursor:String!, $kinds:[IssueTimelineItemsItemType!]) {
  node(id:$id) { ... on Issue { timelineItems(first:%d, after:$cursor, itemTypes:$kinds) {%s} } }
}`, nestedPage, timelineSelection(s))
}

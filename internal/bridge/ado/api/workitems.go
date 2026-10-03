// Reading work items: the query that finds them, the batch that fetches them,
// and the two per-item feeds that carry their history.

package adoapi

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Field reference names. Azure DevOps identifies fields by these rather than
// by their display names, which are localised and renameable.
const (
	FieldTitle       = "System.Title"
	FieldDescription = "System.Description"
	FieldReproSteps  = "Microsoft.VSTS.TCM.ReproSteps"
	FieldState       = "System.State"
	FieldReason      = "System.Reason"
	FieldType        = "System.WorkItemType"
	FieldTags        = "System.Tags"
	FieldAssignedTo  = "System.AssignedTo"
	FieldAreaPath    = "System.AreaPath"
	FieldIteration   = "System.IterationPath"
	FieldCreatedBy   = "System.CreatedBy"
	FieldCreatedDate = "System.CreatedDate"
	FieldChangedDate = "System.ChangedDate"
	FieldHistory     = "System.History"
)

// The link types this package names.
//
// Every directional family is a Forward/Reverse pair, and one rule fixes which
// half is which: Azure DevOps spells the link from the *dependent* end, and
// "Reverse" is always the way up. Hierarchy-Reverse is the parent,
// Dependency-Reverse is the predecessor — the work item that has to come first
// — and Duplicate-Reverse is the item this one duplicates. The Forward halves
// are those same three edges seen from the other end, which is why they have
// names here at all: something has to be able to recognise and ignore them.
//
// Related is symmetric, sits on both work items under one reference name, and
// so has no direction to get wrong.
const (
	RelParent      = "System.LinkTypes.Hierarchy-Reverse"
	RelChild       = "System.LinkTypes.Hierarchy-Forward"
	RelPredecessor = "System.LinkTypes.Dependency-Reverse"
	RelSuccessor   = "System.LinkTypes.Dependency-Forward"
	RelDuplicateOf = "System.LinkTypes.Duplicate-Reverse"
	RelDuplicate   = "System.LinkTypes.Duplicate-Forward"
	RelRelated     = "System.LinkTypes.Related"
)

// batchSize is the hard limit workitemsbatch imposes, not a tuning choice.
const batchSize = 200

// Identity is a person as Azure DevOps reports one.
//
// All three parts are kept because none is reliably present. UniqueName is
// usually a UPN or email but is DOMAIN\user on older on-prem installs, and a
// service account may have only an id. Which of them becomes an event's author
// is the bridge's decision, not this package's.
type Identity struct {
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName"`
	ID          string `json:"id"`
}

// UnmarshalJSON accepts both spellings Azure DevOps uses for a person.
//
// Identity fields come back as an object from the work item endpoints, but as
// a "Display Name <unique.name>" string from some server versions and from
// revision payloads. Failing on the string form would make the bridge's output
// depend on which server answered, so both are read.
func (i *Identity) UnmarshalJSON(data []byte) error {
	type raw Identity
	var obj raw
	if err := json.Unmarshal(data, &obj); err == nil {
		*i = Identity(obj)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	i.DisplayName = strings.TrimSpace(s)
	if open := strings.LastIndex(s, "<"); open >= 0 && strings.HasSuffix(strings.TrimSpace(s), ">") {
		i.DisplayName = strings.TrimSpace(s[:open])
		i.UniqueName = strings.TrimSuffix(strings.TrimSpace(s[open+1:]), ">")
	}
	return nil
}

// Empty reports whether the identity names nobody, which is how Azure DevOps
// spells an unassigned work item.
func (i Identity) Empty() bool {
	return i.UniqueName == "" && i.DisplayName == "" && i.ID == ""
}

// WorkItem is one work item with everything an import needs.
type WorkItem struct {
	ID  int
	Rev int

	Type        string
	Title       string
	Description string
	State       string
	Reason      string
	Tags        []string
	AssignedTo  Identity
	// Iteration is the iteration path below the project root, "/"-separated,
	// or "" when the work item sits at the root.
	Iteration string
	// AreaPath is carried so a caller can see it; nothing imports it. See
	// "The area is scope, and only scope" in docs/bridge-ado.md.
	AreaPath string

	CreatedBy Identity
	CreatedAt time.Time
	// ChangedAt is what the Since filter compares against, so it is also what a
	// sync watermark has to be taken from — any other clock would compare two
	// different notions of time.
	ChangedAt time.Time

	// Relations is every link the work item currently holds, in the order Azure
	// DevOps returned them — which is also the order a JSON-Patch removal has to
	// address them by, since it names one by index and by nothing else.
	//
	// This is *current state*, and the updates feed is the same links as
	// history. Both are read: the feed is what attributes a link to whoever made
	// it, and this is what makes an import right anyway when the feed is
	// unavailable or does not go back far enough.
	Relations []Relation

	Comments []Comment
	Updates  []Update
}

// Comment is one work item comment, with its edit history.
type Comment struct {
	ID int
	// Text is the comment's current text, which for an edited comment is its
	// latest version rather than what was originally posted. Versions is where
	// the original is.
	Text      string
	CreatedBy Identity
	CreatedAt time.Time
	Deleted   bool
	// Versions holds every version, oldest first, and is empty for a comment
	// that was never edited. Azure DevOps keeps them all, so an edited comment
	// reconstructs as real history rather than collapsing to its latest text —
	// which means the first version, not Text, is what was posted.
	Versions []CommentVersion
}

// CommentVersion is one edit of a comment.
type CommentVersion struct {
	Version   int
	Text      string
	CreatedBy Identity
	CreatedAt time.Time
}

// Update is one revision of a work item: what changed, who changed it, when.
//
// This is Azure DevOps' timeline, and it is richer than GitHub's — every
// revision carries the old and the new value of each field it touched, so most
// fields reconstruct as attributed history rather than collapsing to a single
// event at import time.
type Update struct {
	Rev    int
	By     Identity
	At     time.Time
	Fields map[string]FieldChange

	RelationsAdded   []Relation
	RelationsRemoved []Relation
}

// FieldChange is one field's before and after within a revision.
type FieldChange struct{ Old, New string }

// Relation is one link: its type, and the API address of the work item at the
// other end. It is what a revision reports as added or removed and what a work
// item reports as currently held, because Azure DevOps spells a link the same
// way in both places.
type Relation struct {
	Rel string
	URL string
}

// Filter narrows what an import reads.
type Filter struct {
	// Types restricts to these work item types. Empty means every type Azure
	// DevOps does not itself keep off backlogs and boards — see Open.
	Types []string
	// Open restricts to work items whose state is not in a terminal category.
	// This is asked of the server as a state-category group rather than by
	// naming states, so it holds on a customised process too.
	Open bool
	// Since restricts to work items changed at or after a time. This is what
	// makes a second import cheap; it cannot see deletions, which is why it is
	// a filter rather than the only mode.
	Since time.Time
	// Limit stops after this many work items, 0 for no limit.
	Limit int
}

// Fetch reads every work item in scope, with its history and its comments.
//
// Three phases, because that is what the API offers: one query for the ids,
// then batches of 200 for the fields, then two calls per work item for the
// revisions and the comments. The last phase is what makes an import cost
// roughly two requests per work item, and is why the caller passes a progress
// callback — a first import of a large project is not quick.
func (c *Client) Fetch(t Target, f Filter, progress func(fetched, total int)) ([]WorkItem, error) {
	ids, err := c.queryIDs(t, f)
	if err != nil {
		return nil, err
	}
	if f.Limit > 0 && len(ids) > f.Limit {
		ids = ids[:f.Limit]
	}
	return c.fetchByID(t, ids, progress)
}

// FetchIDs reads work items named outright, ignoring every filter.
//
// This is how the staleness window of a scoped or open-only import is closed:
// the origin ledger records every work item id this repository holds, so they
// can be re-read without a query that would, by construction, exclude the ones
// that have since moved out of scope.
func (c *Client) FetchIDs(t Target, ids []int, progress func(fetched, total int)) ([]WorkItem, error) {
	return c.fetchByID(t, ids, progress)
}

// fetchByID reads work items by id, in the two phases the API forces.
//
// Progress is reported from the second phase only, and counted against the
// same total throughout. Reporting from both would run the count to the end
// during batching and then restart it, so the line would reach 100%, drop back
// and climb again — and a progress indicator that goes backwards is worse than
// none. The second phase is also where the time goes: batching is one request
// per 200 items, while completing them is two requests each.
func (c *Client) fetchByID(t Target, ids []int, progress func(fetched, total int)) ([]WorkItem, error) {
	items := make([]WorkItem, 0, len(ids))
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		batch, err := c.batch(t, ids[start:end])
		if err != nil {
			return nil, err
		}
		items = append(items, batch...)
	}

	for i := range items {
		if err := c.complete(t, &items[i]); err != nil {
			return nil, fmt.Errorf("work item %d: %w", items[i].ID, err)
		}
		if progress != nil {
			progress(i+1, len(items))
		}
	}
	return items, nil
}

// queryIDs runs the WIQL query that decides what is in scope. It returns ids
// only — that is all WIQL can return — which is why the fields come separately.
func (c *Client) queryIDs(t Target, f Filter) ([]int, error) {
	var response struct {
		WorkItems []struct {
			ID int `json:"id"`
		} `json:"workItems"`
	}
	body := map[string]string{"query": wiql(t, f)}
	endpoint := withVersion(t.API()+"/wit/wiql", nil, APIVersion)
	if err := c.do("POST", endpoint, body, &response); err != nil {
		return nil, err
	}
	ids := make([]int, len(response.WorkItems))
	for i, w := range response.WorkItems {
		ids[i] = w.ID
	}
	return ids, nil
}

// wiql builds the query.
//
// Two of the three predicates are expressed as category groups rather than as
// lists of names, which is what keeps them correct on a customised process:
// Microsoft.HiddenCategory is Azure DevOps' own record of the types it keeps
// off backlogs and boards, and the Completed and Removed state categories are
// its own record of which states end a work item's life. Naming states or
// types outright would be this tracker guessing at another project's process.
//
// The ordering is not cosmetic. Ascending ChangedDate means a run cut short by
// a limit has read a prefix, so every work item it did not reach has a
// ChangedDate at or after the watermark and the next run asks for exactly
// those.
func wiql(t Target, f Filter) string {
	var b strings.Builder
	b.WriteString("SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = ")
	b.WriteString(quote(t.Project))
	b.WriteString(" AND [System.AreaPath] UNDER ")
	b.WriteString(quote(t.AreaPath()))

	if len(f.Types) > 0 {
		b.WriteString(" AND [System.WorkItemType] IN (")
		for i, typ := range f.Types {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(quote(typ))
		}
		b.WriteString(")")
	} else {
		b.WriteString(" AND [System.WorkItemType] NOT IN GROUP 'Microsoft.HiddenCategory'")
	}

	if f.Open {
		b.WriteString(" AND [System.State] NOT IN GROUP 'Completed'")
		b.WriteString(" AND [System.State] NOT IN GROUP 'Removed'")
	}
	if !f.Since.IsZero() {
		b.WriteString(" AND [System.ChangedDate] >= ")
		b.WriteString(quote(f.Since.UTC().Format("2006-01-02T15:04:05Z")))
	}
	b.WriteString(" ORDER BY [System.ChangedDate] ASC")
	return b.String()
}

// quote renders a WIQL string literal. Azure DevOps escapes a single quote by
// doubling it, as SQL does.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// batch reads the fields and relations of up to batchSize work items.
func (c *Client) batch(t Target, ids []int) ([]WorkItem, error) {
	var response struct {
		Value []workItemJSON `json:"value"`
	}
	body := map[string]any{
		"ids":     ids,
		"$expand": "relations",
		// A work item deleted between the query and the batch would otherwise
		// fail the whole batch; omitting it loses nothing, since it is gone.
		"errorPolicy": "omit",
	}
	endpoint := withVersion(t.API()+"/wit/workitemsbatch", nil, APIVersion)
	if err := c.do("POST", endpoint, body, &response); err != nil {
		return nil, err
	}

	items := make([]WorkItem, 0, len(response.Value))
	for _, w := range response.Value {
		items = append(items, w.workItem(t))
	}
	return items, nil
}

// complete fills in the two per-item feeds the batch cannot carry.
func (c *Client) complete(t Target, w *WorkItem) error {
	updates, err := c.updates(t, w.ID)
	if err != nil {
		return err
	}
	w.Updates = updates

	comments, err := c.comments(t, w.ID)
	if err != nil {
		return err
	}
	w.Comments = comments
	return nil
}

// updates reads a work item's revision history, following the paging Azure
// DevOps applies to it.
func (c *Client) updates(t Target, id int) ([]Update, error) {
	const page = 200
	var all []Update
	for skip := 0; ; skip += page {
		var response struct {
			Count int          `json:"count"`
			Value []updateJSON `json:"value"`
		}
		params := url.Values{"$top": {strconv.Itoa(page)}, "$skip": {strconv.Itoa(skip)}}
		endpoint := fmt.Sprintf("%s/wit/workItems/%d/updates", t.API(), id)
		if err := c.get(endpoint, params, &response); err != nil {
			return nil, err
		}
		for _, u := range response.Value {
			all = append(all, u.update())
		}
		if len(response.Value) < page {
			return all, nil
		}
	}
}

// comments reads a work item's comments, including the deleted ones — a
// deletion is a fact the thread has to carry, not an absence.
func (c *Client) comments(t Target, id int) ([]Comment, error) {
	var response struct {
		Comments []commentJSON `json:"comments"`
	}
	params := url.Values{"includeDeleted": {"true"}}
	endpoint := fmt.Sprintf("%s/wit/workItems/%d/comments", t.API(), id)
	// The comments API is still behind a preview version even at 7.1.
	full := withVersion(endpoint, params, APIVersion+"-preview.4")
	if err := c.do("GET", full, nil, &response); err != nil {
		return nil, err
	}

	comments := make([]Comment, 0, len(response.Comments))
	for _, cj := range response.Comments {
		comment := cj.comment()
		// Version 1 is the comment itself; anything beyond it is an edit, and
		// only then is the extra request worth making.
		if cj.Version > 1 {
			versions, err := c.commentVersions(t, id, comment.ID)
			if err != nil {
				return nil, err
			}
			comment.Versions = versions
		}
		comments = append(comments, comment)
	}
	return comments, nil
}

// commentVersions reads every version of a comment, oldest first. The first is
// what was posted; each one after it is an edit.
func (c *Client) commentVersions(t Target, workItem, comment int) ([]CommentVersion, error) {
	var response struct {
		Value []commentVersionJSON `json:"value"`
	}
	endpoint := fmt.Sprintf("%s/wit/workItems/%d/comments/%d/versions", t.API(), workItem, comment)
	full := withVersion(endpoint, nil, APIVersion+"-preview.3")
	if err := c.do("GET", full, nil, &response); err != nil {
		return nil, err
	}

	var versions []CommentVersion
	for _, v := range response.Value {
		versions = append(versions, CommentVersion{
			Version:   v.Version,
			Text:      v.Text,
			CreatedBy: v.ModifiedBy.or(v.CreatedBy),
			CreatedAt: v.ModifiedDate.or(v.CreatedDate),
		})
	}
	return versions, nil
}

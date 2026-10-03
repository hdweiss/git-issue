// Writing: JSON-Patch for work items, and the comments API for the thread.

package adoapi

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
)

// Field is one field to write, by its reference name.
type Field struct {
	Name  string
	Value any
}

// LinkOps is the link changes one patch carries.
//
// A removal names the link it drops the way the caller thinks of it — by type
// and target — and is matched against the work item's relations here. That
// matching cannot be pushed up to the caller, because JSON-Patch addresses a
// relation by its *index* in the array and by nothing else.
type LinkOps struct {
	Add    []Relation
	Remove []Relation
}

// Empty reports whether there is nothing to write.
func (l LinkOps) Empty() bool { return len(l.Add) == 0 && len(l.Remove) == 0 }

// CreateWorkItem files a new work item of the given type, with its links.
//
// The type is part of the path, after a literal '$' — Azure DevOps spells it
// that way, and it is not a placeholder.
//
// The links go in this document rather than in a patch after it, for the reason
// the fields do: a work item created bare and then linked gains a revision for
// a link it was born with, and the next import reads that back as history that
// never happened.
func (c *Client) CreateWorkItem(t Target, workItemType string, fields []Field, links []Relation) (WorkItem, error) {
	patch := make(patchDocument, 0, len(fields)+len(links))
	for _, f := range fields {
		patch = append(patch, patchOp{Op: "add", Path: "/fields/" + f.Name, Value: f.Value})
	}
	patch = append(patch, addLinkOps(links)...)

	endpoint := withVersion(fmt.Sprintf("%s/wit/workitems/$%s", t.API(), workItemType), nil, APIVersion)
	var created workItemJSON
	if err := c.do(http.MethodPost, endpoint, patch, &created); err != nil {
		return WorkItem{}, err
	}
	return created.workItem(t), nil
}

// UpdateWorkItem writes fields and links to an existing work item.
//
// It takes the work item rather than its id because both halves of the document
// are built against what was read: the leading test names the revision, and a
// link removal names an index into the relations array as that read reported it.
//
// The document opens with a test on /rev, so Azure DevOps rejects the whole
// patch if the work item has moved since it was read. That turns a concurrent
// upstream edit into a failed write rather than a silent overwrite, which is
// the one place this API is stronger than GitHub's — there is no equivalent
// there, and a push has to hope instead. It also makes the indexes safe: a
// relations array that has changed under the patch is a revision that has
// changed, and the test refuses it.
func (c *Client) UpdateWorkItem(t Target, w WorkItem, fields []Field, links LinkOps) error {
	if len(fields) == 0 && links.Empty() {
		return nil
	}
	patch := make(patchDocument, 0, len(fields)+len(links.Add)+len(links.Remove)+1)
	patch = append(patch, patchOp{Op: "test", Path: "/rev", Value: w.Rev})
	for _, f := range fields {
		patch = append(patch, patchOp{Op: "add", Path: "/fields/" + f.Name, Value: f.Value})
	}
	// Removals before additions, and descending within themselves, so that no
	// operation renumbers the array an operation after it addresses.
	patch = append(patch, removeLinkOps(w.Relations, links.Remove)...)
	patch = append(patch, addLinkOps(links.Add)...)

	// A removal whose link upstream has already gone produces no operation, and
	// a document that is nothing but its own precondition is not a write. This
	// is the ordinary case for the second end of a symmetric link: dropping it
	// here retracts both ends locally, both are pushed, and one of them finds
	// the work item already unlinked.
	if len(patch) == 1 {
		return nil
	}

	endpoint := withVersion(fmt.Sprintf("%s/wit/workitems/%d", t.API(), w.ID), nil, APIVersion)
	return c.do(http.MethodPatch, endpoint, patch, nil)
}

// addLinkOps appends each link to the relations array.
func addLinkOps(links []Relation) []patchOp {
	ops := make([]patchOp, 0, len(links))
	for _, l := range links {
		ops = append(ops, patchOp{
			Op:    "add",
			Path:  "/relations/-",
			Value: map[string]string{"rel": l.Rel, "url": l.URL},
		})
	}
	return ops
}

// removeLinkOps turns links into the indexes that name them.
//
// Descending, because JSON-Patch applies operations in order and removing an
// element shifts every one after it — ascending would delete the wrong links
// from the second removal on.
//
// A link that is not in the array produces no operation. Upstream has already
// dropped it, so there is nothing to do, and failing here would turn "somebody
// else got there first" into a failed push.
func removeLinkOps(current, remove []Relation) []patchOp {
	taken := make(map[int]bool, len(remove))
	var indexes []int
	for _, l := range remove {
		for i, r := range current {
			// By id rather than by the URL as a string: Azure DevOps returns a
			// link spelled against the collection and this client writes one
			// spelled against the project, so the two never compare equal.
			if taken[i] || r.Rel != l.Rel || workItemID(r.URL) != workItemID(l.URL) {
				continue
			}
			taken[i] = true
			indexes = append(indexes, i)
			break
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(indexes)))

	ops := make([]patchOp, 0, len(indexes))
	for _, i := range indexes {
		ops = append(ops, patchOp{Op: "remove", Path: "/relations/" + strconv.Itoa(i)})
	}
	return ops
}

// AddComment posts a comment and returns its id.
func (c *Client) AddComment(t Target, workItem int, text string) (int, error) {
	var created struct {
		ID int `json:"id"`
	}
	endpoint := commentsEndpoint(t, workItem, 0)
	if err := c.do(http.MethodPost, endpoint, map[string]string{"text": text}, &created); err != nil {
		return 0, err
	}
	return created.ID, nil
}

// UpdateComment rewrites a comment's text. Azure DevOps keeps the previous
// version, which is what lets the next import read the edit back as history
// rather than as a body that silently changed.
func (c *Client) UpdateComment(t Target, workItem, comment int, text string) error {
	endpoint := commentsEndpoint(t, workItem, comment)
	return c.do(http.MethodPatch, endpoint, map[string]string{"text": text}, nil)
}

// DeleteComment removes a comment. It stays readable with includeDeleted, so
// the next import still sees the tombstone rather than a gap.
func (c *Client) DeleteComment(t Target, workItem, comment int) error {
	return c.do(http.MethodDelete, commentsEndpoint(t, workItem, comment), nil, nil)
}

// commentsEndpoint addresses the thread, or one entry in it when comment is
// non-zero. The comments API is still behind a preview version at 7.1.
func commentsEndpoint(t Target, workItem, comment int) string {
	endpoint := fmt.Sprintf("%s/wit/workItems/%d/comments", t.API(), workItem)
	if comment != 0 {
		endpoint += fmt.Sprintf("/%d", comment)
	}
	return withVersion(endpoint, nil, APIVersion+"-preview.3")
}

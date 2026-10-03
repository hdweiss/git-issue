// Package bridge is the contract between the issue vocabulary and an external
// tracker.
//
// It sits above internal/issue and below each concrete bridge, and names no
// platform. Adding one — Azure DevOps, Jira, GitLab — is an API leaf plus a
// package implementing Bridge, plus one case in the command's dispatch. Nothing
// else moves, which is the layering AGENTS.md describes and the reason this
// package exists at all rather than the command talking to a bridge directly.
//
// The push mechanics themselves are specified in "Pushing to an external
// tracker" in docs/storage-model.md; what a Bridge supplies is the two ends of
// it, reading the tracker's current state and applying a delta.
package bridge

import (
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

// Candidate is one local issue a push is considering.
//
// Events comes alongside State because the two answer different questions: the
// state is what this clone says, while the raw events are what the base is
// reconstructed from — an event is identified by its bytes, and folding loses
// them.
type Candidate struct {
	ID     string
	Events []entity.Event
	State  entity.State
	// Upstream is the object this entity corresponds to in the tracker, from the
	// origin ledger. Empty means the tracker has no counterpart, so a push
	// creates one.
	Upstream string
}

// Plan is what a push intends to do to one tracker.
type Plan struct {
	// Deltas are the issues with something to send, one entry each.
	Deltas []issue.Delta
	// Conflicts are fields both sides moved. The issues they name are not in
	// Deltas: a conflict skips its issue and leaves every other one alone.
	Conflicts []issue.Conflict
	// Skipped is what this bridge cannot write, said out loud rather than
	// dropped. A field the tracker has no mutation for is a real limitation and
	// belongs in the report.
	Skipped []Skip
}

// Skip is one change a bridge declined to send, and why.
type Skip struct{ ID, Field, Reason string }

// CommentOrigin ties one local thread-entry event to the upstream comment it
// corresponds to.
type CommentOrigin struct{ EventID, Upstream string }

// Result is what applying one delta produced, in mappings for the origin ledger.
//
// Everything here has to reach the ledger before the run ends, because a mapping
// earned and then lost costs a duplicate upstream that nothing undoes.
type Result struct {
	ID string
	// Upstream and URL are set when the delta created the issue.
	Upstream string
	URL      string
	Comments []CommentOrigin
}

// Claimed reports which upstream objects are already represented locally, so an
// import does not file a second copy of something this clone pushed.
type Claimed interface {
	ClaimedComment(upstream string) bool
}

// Lookup is everything the origin ledger can answer while a bridge runs.
//
// It is the small interface AGENTS.md asks for in place of the ledger itself:
// a bridge is handed the questions it may ask, and never the store, so
// internal/origins stays reachable from cmd alone.
//
// A bridge that resolves links needs the second question, because a relation's
// target is an entity id and cannot be filled in from an upstream id without
// knowing what that upstream object is filed as here. It must be answered the
// same way on a pull and inside a push's plan: an import that resolved fewer
// links than the pull did would report a difference that is not there, and push
// it forever.
// Pushing a link needs the same question asked backwards, because a relation
// names an entity and a mutation names an upstream object.
type Lookup interface {
	Claimed
	// Entity is the local entity an upstream object is already filed as.
	Entity(upstream string) (string, bool)
	// Upstream is the object an entity is filed as on this tracker, empty when
	// the tracker has no counterpart for it yet.
	Upstream(entity string) (string, bool)
}

// LinksOnly narrows a ledger to the half a push's plan may use: where an
// upstream object is filed here, while claiming no comment.
//
// A plan needs the two halves to differ, and the reason is worth stating. Links
// must resolve exactly as they did on the pull, per the paragraph above.
// Comments must *not* be suppressed, because a plan reads the tracker's state
// in full rather than a blob to write, and hiding the comments this clone
// posted would understate what is already there.
//
// A ledger that is absent narrows to absent: nothing to resolve, nothing to
// claim.
func LinksOnly(l Lookup) Lookup {
	if l == nil {
		return nil
	}
	return linksOnly{l}
}

type linksOnly struct{ Lookup }

func (linksOnly) ClaimedComment(string) bool { return false }

// Bridge is one upstream tracker, writable.
type Bridge interface {
	// Tracker names the repository, spelled as the origin ledger spells it.
	Tracker() string

	// Plan reads the tracker's current state for every candidate and works out
	// what to send. It performs no writes, which is what makes --dry-run
	// honest rather than approximate.
	Plan(candidates []Candidate, mapped issue.Mapped) (Plan, error)

	// Apply sends one delta and reports the mappings it earned. It is called
	// once per delta rather than once per run so that a failure part-way through
	// still leaves everything before it recorded.
	Apply(c Candidate, d issue.Delta) (Result, error)
}

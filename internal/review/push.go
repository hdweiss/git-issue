// The push pipeline's vocabulary: the candidate a push considers, the plan it
// draws up, and the result one delta leaves behind.
//
// These types name a review and an upstream tracker and nothing platform
// specific — the same reason internal/review/delta.go carries review.Diff. Every
// review bridge builds the same Candidate, returns the same Plan, and hands back
// the same Result, so cmd/git-review drives all of them through one code path.
// A bridge that needs more per-object detail than CommentOrigin carries is where
// this grows.

package review

import "github.com/hdweiss/git-issue/internal/entity"

// CommentOrigin ties one local event to the upstream object it corresponds to —
// a comment, the thread it opened, or the review a verdict was cast as.
type CommentOrigin struct{ EventID, Upstream string }

// Lookup answers the questions an API response cannot: which upstream objects
// this repository already holds, and as what.
//
// Small and passed in, so a bridge cannot grow a dependency on where the origin
// ledger is stored.
type Lookup interface {
	// Entity is the local entity an upstream object is filed as, on any ref. A
	// `closes` target is an issue, so this spans both namespaces — which is the
	// ledger's job rather than a bridge's.
	Entity(upstream string) (string, bool)
	// ClaimedComment reports that an upstream comment is already present here as
	// an entry this repository authored and pushed. Skipping those is what stops
	// a pushed comment coming back as a second one.
	ClaimedComment(upstream string) bool
	// ClaimedVerdict is the local verdict an upstream object was recorded as, for
	// a review this repository pushed. It answers with the event id rather than a
	// bare yes because a dismissal has to name the add it undoes.
	ClaimedVerdict(upstream string) (string, bool)
}

// Ledger is everything the origin ledger answers while a push runs. It is the
// small interface a bridge asks for in place of the ledger itself.
type Ledger interface {
	Lookup
	Mapped
	// Upstream is the object an entity is filed as on this tracker.
	Upstream(entity string) (string, bool)
}

// Candidate is one local review a push is considering.
//
// Events comes alongside State because the two answer different questions: the
// state is what this clone says, while the raw events are what the common base
// is reconstructed from — an event is identified by its bytes, and folding loses
// them.
type Candidate struct {
	ID     string
	Events []entity.Event
	State  entity.State
	// Upstream is the object this review corresponds to on the tracker, from the
	// origin ledger. Empty means the tracker has no counterpart, so a push files
	// one.
	Upstream string
}

// Plan is what a push intends to do to one tracker.
type Plan struct {
	// Deltas are the reviews with something to send, one entry each.
	Deltas []Delta
	// Conflicts are fields both sides moved. The reviews they name are not in
	// Deltas: a conflict skips its review and leaves every other one alone.
	Conflicts []Conflict
	// Skipped is what the bridge cannot write, said out loud rather than dropped.
	Skipped []Skip
}

// Skip is one change a bridge declined to send, and why.
type Skip struct{ ID, Field, Reason string }

// Result is what applying one delta produced, in mappings for the origin ledger.
//
// Everything here has to reach the ledger before the run ends: a mapping earned
// and then lost costs a duplicate upstream that nothing undoes.
type Result struct {
	ID string
	// Upstream and URL are set when the delta filed the pull request.
	Upstream string
	URL      string

	// Comments, Threads and Verdicts are the three mappings a review earns, and
	// they are three because the objects are: an entry, the thread it opened, and
	// the submitted review a verdict was cast as.
	Comments []CommentOrigin
	Threads  []CommentOrigin
	Verdicts []CommentOrigin
}

// Verdicts: one person's position on a review, and whether it still describes
// the revision in front of them.

package review

import (
	"slices"
	"strings"

	"github.com/hdweiss/git-issue/internal/entity"
)

// The ops a verdict is written with. An ordinary OR-Set, needing no new field
// kind: a member is the pair (val, ref), so an approval of one revision and a
// request for changes on the next are two distinct members rather than a
// collision, and two clones recording the same verdict against the same
// revision are one.
const (
	VerdictField  = "verdict"
	VerdictAdd    = "verdict.add"
	VerdictRemove = "verdict.remove"
)

// The verdict vocabulary. Open, like every other value in this system.
const (
	VerdictApprove        = "approve"
	VerdictRequestChanges = "request-changes"
	VerdictComment        = "comment"
)

// Verdicts are the verdict values this client has a phrase for, in the order a
// rendering lists them.
var Verdicts = []string{VerdictApprove, VerdictRequestChanges, VerdictComment}

// KnownVerdict reports whether a value is one docs/reviews.md defines.
func KnownVerdict(v string) bool { return slices.Contains(Verdicts, v) }

// Verdict is one surviving verdict member.
type Verdict struct {
	// ID is the `verdict.add` that put it there, which is what a dismissal must
	// name.
	ID     string
	Author string
	Value  string
	// Revision is the head.sha the verdict was cast against, as the event's ref
	// carried it. Empty where a writer recorded none.
	Revision string
	TS       int64
	// Stale is whether Revision is something other than the review's current
	// head. A stale verdict is shown as stale and never dropped, and its
	// revision is never rewritten to the current head.
	Stale bool
}

// Verdicts returns the surviving verdicts, one per author: the highest (c, id)
// among each author's un-retracted members.
//
// A reader rule over ordinary storage, not a resolution mechanism of its own —
// the same shape internal/issue applies to parent cardinality. The alternative,
// a scalar per author, would make a reviewer who approves and then reconsiders
// lose the record that they ever approved, which is the thing a review most
// needs to keep. Superseded members stay in the blob and stay foldable; they
// are simply not this author's current position.
//
// Ordered by the winning member's (c, id), so a rendering is stable.
func VerdictsOf(st entity.State) []Verdict {
	head := Head(st)

	// Members are already in (c, id) order, so the last one seen for an author
	// is that author's current position by plain assignment.
	latest := map[string]Verdict{}
	var order []string
	for _, m := range st.Members(VerdictField) {
		ev, ok := eventByID(st, m.ID)
		if !ok {
			continue
		}
		if _, seen := latest[ev.A]; !seen {
			order = append(order, ev.A)
		}
		latest[ev.A] = Verdict{
			ID:       m.ID,
			Author:   ev.A,
			Value:    m.Val.Display(),
			Revision: m.Ref,
			TS:       ev.TS,
			Stale:    head != "" && m.Ref != "" && m.Ref != head,
		}
	}

	out := make([]Verdict, 0, len(order))
	for _, author := range order {
		out = append(out, latest[author])
	}
	slices.SortFunc(out, func(a, b Verdict) int {
		if c := strings.Compare(a.Author, b.Author); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// VerdictBy is one author's current verdict, and whether they have one.
func VerdictBy(st entity.State, author string) (Verdict, bool) {
	for _, v := range VerdictsOf(st) {
		if strings.EqualFold(v.Author, author) {
			return v, true
		}
	}
	return Verdict{}, false
}

// Approvals are the current approvals that are not stale — the ones that
// describe the revision in front of the reader.
//
// Whether a stale approval still counts is policy, not format: upstream it is
// decided by branch protection, and locally a client reports it and stops
// there. This is the count `status` reports and nothing enforces.
func Approvals(st entity.State) []Verdict {
	var out []Verdict
	for _, v := range VerdictsOf(st) {
		if v.Value == VerdictApprove && !v.Stale {
			out = append(out, v)
		}
	}
	return out
}

// ChangesRequested are the current request-changes verdicts that are not stale.
func ChangesRequested(st entity.State) []Verdict {
	var out []Verdict
	for _, v := range VerdictsOf(st) {
		if v.Value == VerdictRequestChanges && !v.Stale {
			out = append(out, v)
		}
	}
	return out
}

// eventByID finds one event of this entity by its id. Linear, over one blob's
// events, which is the same scale everything else here walks.
func eventByID(st entity.State, id string) (entity.Event, bool) {
	for _, ev := range st.Events {
		if ev.ID == id {
			return ev, true
		}
	}
	return entity.Event{}, false
}

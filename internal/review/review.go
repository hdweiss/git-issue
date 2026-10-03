// Package review is the review entity type: its vocabulary and its refs.
//
// A review is a change proposal — a base, a head, commentary anchored to the
// code, verdicts, and a terminal state of merged or abandoned. A pull request
// is a review with an entry in the origin ledger; a review produced locally, by
// a person or by an agent reading a branch, is one that has none. There is no
// second type for that case and no flag telling them apart: the ledger already
// answers "does this correspond to something on a forge", per tracker, so a
// review opened against a fork and later against the upstream is two ledger
// lines and one entity.
//
// Most of the vocabulary is internal/issue's, reused unchanged — a generic
// renderer or indexer that knows issues displays most of a review without being
// taught anything. docs/reviews.md is the specification. Like internal/issue,
// this package is type-specific: internal/entity and internal/render must never
// import it.
package review

import (
	"strings"

	"github.com/hdweiss/git-issue/internal/entity"
)

// Type is the val of a review's create event. The blob, not the ref it is filed
// under, is the authority on what an entity is.
const Type = "review"

// RefOpen holds reviews in any non-terminal status; reviews that reach a
// terminal status move to RefArchived.
//
// States are siblings, never parent/child, for the same reason issues' are.
const (
	RefOpen     = "reviews/open"
	RefArchived = "reviews/archived"
)

// The status values docs/reviews.md defines. StatusOpen is the implied status
// of a review with no status event.
//
// StatusMerged carries a rule the others do not: it is a claim about the code
// rather than about the review, so a writer may only write it having observed
// the merge — an import from the platform that performed it, or a merge this
// client performed itself. A review its author is done with but nobody merged
// is StatusClosed.
const (
	StatusOpen   = "open"
	StatusMerged = "merged"
	StatusClosed = "closed"
)

// StatusInfo is what this client knows about one status value: whether reaching
// it ends the review's life. The vocabulary stays open — a value absent from
// Statuses is preserved and displayed verbatim — so this is the set that gets
// an opinion about terminality, not the set that is allowed.
type StatusInfo struct {
	Name     string
	Terminal bool
}

// Statuses are the status values this client classifies: the three native ones,
// plus the spellings Azure DevOps uses for a pull request, so a bridged review
// dots the same way a local close does rather than always reading as
// non-terminal.
//
// GitHub needs no entry — its OPEN / MERGED / CLOSED are mapped onto these in
// the bridge. A platform whose vocabulary is not here still folds and still
// renders; it simply reads as non-terminal until it is added.
var Statuses = []StatusInfo{
	{Name: StatusOpen},
	{Name: StatusMerged, Terminal: true},
	{Name: StatusClosed, Terminal: true},

	{Name: "Active"},
	{Name: "Completed", Terminal: true},
	{Name: "Abandoned", Terminal: true},
}

// Terminal reports whether a status ends a review's life. An unrecognised
// status is not terminal: leaving something listed is the failure that can be
// noticed, and archiving it wrongly is the one that cannot.
func Terminal(status string) bool {
	for _, s := range Statuses {
		if strings.EqualFold(s.Name, status) {
			return s.Terminal
		}
	}
	return false
}

// The status.reason vocabulary. `completed` is deliberately absent: `merged`
// already says it, and writing both would be two fields saying one thing.
const (
	ReasonNotPlanned = "not_planned"
	ReasonSuperseded = "superseded"
	ReasonDuplicate  = "duplicate"
)

// Reasons are the close reasons `close --as` accepts, in the order a chooser
// lists them.
var Reasons = []string{ReasonNotPlanned, ReasonSuperseded, ReasonDuplicate}

// NormalizeReason maps what a person types onto the blob's own spelling, the
// way internal/issue does: a command line carries a hyphen or a space where the
// blob carries an underscore.
func NormalizeReason(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

// KnownReason reports whether a normalised reason is one docs/reviews.md
// defines.
func KnownReason(reason string) bool {
	for _, r := range Reasons {
		if r == reason {
			return true
		}
	}
	return false
}

// ReasonTakesTarget reports whether a close reason names another entity, which
// is what `--as <reason>:<id>` spells. Both of these close a review in favour
// of a specific other one, and neither means anything without it.
func ReasonTakesTarget(reason string) (kind string, ok bool) {
	switch reason {
	case ReasonSuperseded:
		return KindSupersededBy, true
	case ReasonDuplicate:
		return KindDuplicate, true
	}
	return "", false
}

// Vocabulary is the review type's contribution to the fold: which op names are
// scalars. Everything else — create, the comment family and its annotations,
// react, and the .add/.remove/.note list pattern that carries relations,
// verdicts and checks — is structural and folds generically.
//
// Note what is absent. `type` is the issue type's bug/feature/task axis and has
// no meaning here, so a bridge must not synthesise one; `pinned` is a property
// of a repository's view rather than of an entity, and no platform pins a pull
// request.
var Vocabulary = entity.Vocabulary{
	Scalars: []string{
		"title",
		"description",
		"status",
		"status.reason",
		"milestone",
		"base",
		"head",
		"head.sha",
		"locked",
		"lock.reason",
		"draft",
	},
}

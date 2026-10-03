// Package issue is the issue entity type: its vocabulary and its refs.
//
// This is the only package in the tree that knows what an issue is. Everything
// it depends on — canonical bytes, event ids, the fold, the renderers — is
// entity-agnostic, so a pull request type is this file again with a different
// vocabulary, not a second implementation. Keep it that way: internal/entity
// and internal/render must never import this package.
package issue

import (
	"strings"

	"github.com/hdweiss/git-issue/internal/entity"
)

// Type is the val of an issue's create event. The blob, not the ref it is
// filed under, is the authority on what an entity is.
const Type = "issue"

// RefOpen holds issues in any non-terminal status; issues that reach a
// terminal status move to RefArchived.
//
// States are siblings, never parent/child: a git ref cannot be both a leaf and
// a path prefix, so "issues" + "issues/archived" would fail to lock. Placement
// is an enumeration optimization — the in-blob status field is the truth.
const (
	RefOpen     = "issues/open"
	RefArchived = "issues/archived"
)

// StatusOpen is the implied status of an issue with no status event;
// StatusClosed is git-issue's own terminal status and what `git issue close`
// writes.
const (
	StatusOpen   = "open"
	StatusClosed = "closed"
)

// StatusInfo is what this client knows about one status value: whether reaching
// it ends the issue's life. The vocabulary stays open — a value absent from
// Statuses is preserved and displayed verbatim — so this is the set that gets
// an opinion about terminality, not the set that is allowed.
type StatusInfo struct {
	Name     string
	Terminal bool
}

// Statuses are the status values this client classifies. The two native ones,
// plus the Azure DevOps process states, so a bridged work item dots the same
// way a local close does rather than always reading as non-terminal.
//
// GitHub needs no entry: its OPEN / CLOSED are mapped onto open / closed in the
// bridge. The Azure DevOps names span the Agile, Scrum, CMMI and Basic
// processes, split on that platform's own state categories — Proposed,
// InProgress and Resolved are live; Completed and Removed end a work item — the
// same grouping its scope query already uses (internal/bridge/ado/api). A customised
// process with a state named something else entirely still folds and still
// renders; it simply reads as non-terminal until it is added here or a
// cross-bridge mapping lands (docs/issues.md, TODO.md).
var Statuses = []StatusInfo{
	{Name: StatusOpen},
	{Name: StatusClosed, Terminal: true},

	{Name: "New"},
	{Name: "Proposed"},
	{Name: "Approved"},
	{Name: "To Do"},
	{Name: "Active"},
	{Name: "Committed"},
	{Name: "Doing"},
	{Name: "Resolved"},
	{Name: "Done", Terminal: true},
	{Name: "Closed", Terminal: true},
	{Name: "Removed", Terminal: true},
}

// Terminal reports whether a status ends an issue's life — it becomes eligible
// for the archived ref, and a listing dots it closed.
//
// An unrecognised status is not terminal. docs/issues.md requires a client that
// does not know a value to leave the issue listed rather than archive it, which
// is the same call Status makes when it refuses to coerce. The lookup folds
// case because a bridge passes its platform's own spelling through, and
// platforms differ on it.
func Terminal(status string) bool {
	for _, s := range Statuses {
		if strings.EqualFold(s.Name, status) {
			return s.Terminal
		}
	}
	return false
}

// The status.reason vocabulary — why an issue reached its current terminal
// status, when the status alone does not say (docs/issues.md). Open vocabulary:
// `edit --status-reason` writes an unrecognised value verbatim, `close --as`
// only takes these three.
const (
	ReasonCompleted  = "completed"
	ReasonNotPlanned = "not_planned"
	ReasonDuplicate  = "duplicate"
)

// Reasons are the close reasons `close --as` accepts, in the order a chooser
// lists them.
var Reasons = []string{ReasonCompleted, ReasonNotPlanned, ReasonDuplicate}

// NormalizeReason maps what a person types onto the blob's own spelling: the
// two-word reason is `not_planned` in the blob (docs/issues.md), but a command
// line more naturally carries a hyphen or a space. An unrecognised reason is
// lowercased and underscored and otherwise left alone, for the raw
// `edit --status-reason` path; `close --as` checks it against KnownReason.
func NormalizeReason(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

// KnownReason reports whether a normalised reason is one of the three
// docs/issues.md defines.
func KnownReason(reason string) bool {
	for _, r := range Reasons {
		if r == reason {
			return true
		}
	}
	return false
}

// Vocabulary is the issue type's contribution to the fold: which op names are
// scalars. Everything else — create, comment, comment.edit, comment.remove,
// react, react.remove, and the .add/.remove/.note list pattern that carries
// relations — is structural and handled generically.
var Vocabulary = entity.Vocabulary{
	Scalars: []string{
		"title",
		"description",
		"status",
		"status.reason",
		"milestone",
		"type",
		"locked",
		"lock.reason",
		"draft",
		"pinned",
	},
}

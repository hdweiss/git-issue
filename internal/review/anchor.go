// Anchors: where in the code a review comment sits, and whether it still
// describes the head.

package review

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hdweiss/git-issue/internal/entity"
)

// The two thread annotations this type adds. Both address a comment by id and
// resolve the way an edit does — the annotation rule internal/entity already
// folds, used rather than extended.
//
// AnchorName says where a comment sits; ResolveName whether its thread is
// resolved. Neither is a field kind of its own.
const (
	AnchorName  = "anchor"
	ResolveName = "resolve"

	AnchorOp  = "comment." + AnchorName
	ResolveOp = "comment." + ResolveName
)

// The two halves of a split diff. `right` is the default and is never written.
const (
	SideRight = "right"
	SideLeft  = "left"
)

// Anchor is where a review comment sits in the code.
//
// The wire form is whitespace-separated and additive, in the spirit of the
// origin ledger's lines:
//
//	<sha> <path> <line>[-<line>] [key=value …]
//
// The leading three fields are required and positional; anything after them is
// key=value pairs. Unrecognised pairs are preserved byte for byte, which is
// what lets a pair be added without a format change.
type Anchor struct {
	// Revision is the commit the comment was written against. It need not name
	// an object this repository holds: a fork's commits are absent until
	// fetched, and a rewritten one may be gone for good.
	Revision string
	Path     string
	// First and Last are the anchored line range, 1-based and inclusive. A
	// single line has First == Last.
	First, Last int
	// Side distinguishes the two halves of a split diff; "" means right.
	Side string
	// Context is a hash of the anchored lines' content, for offering a
	// relocation. Empty where the writer recorded none.
	Context string
	// Extra holds key=value pairs this build does not recognise, in the order
	// they arrived, so a round trip preserves them.
	Extra []string
}

// ParseAnchor reads an anchor's wire form. A malformed one is an error rather
// than a silently dropped annotation: the event stays in the blob regardless,
// and a reader that cannot place a comment should say so.
func ParseAnchor(s string) (Anchor, error) {
	fields := strings.Fields(s)
	if len(fields) < 3 {
		return Anchor{}, fmt.Errorf("an anchor is spelled '<sha> <path> <line>[-<line>]'")
	}

	a := Anchor{Revision: fields[0], Path: fields[1]}
	first, last, err := parseLines(fields[2])
	if err != nil {
		return Anchor{}, err
	}
	a.First, a.Last = first, last

	for _, f := range fields[3:] {
		key, value, ok := strings.Cut(f, "=")
		if !ok {
			a.Extra = append(a.Extra, f)
			continue
		}
		switch key {
		case "side":
			a.Side = value
		case "ctx":
			a.Context = value
		default:
			a.Extra = append(a.Extra, f)
		}
	}
	return a, nil
}

func parseLines(s string) (first, last int, err error) {
	lo, hi, ranged := strings.Cut(s, "-")
	first, err = strconv.Atoi(lo)
	if err != nil || first < 1 {
		return 0, 0, fmt.Errorf("'%s' is not a line number", lo)
	}
	if !ranged {
		return first, first, nil
	}
	last, err = strconv.Atoi(hi)
	if err != nil || last < first {
		return 0, 0, fmt.Errorf("'%s' is not a line range", s)
	}
	return first, last, nil
}

// String is the anchor's wire form. Round-trips ParseAnchor, unrecognised
// pairs included.
func (a Anchor) String() string {
	out := a.Revision + " " + a.Path + " " + a.Lines()
	if a.Side != "" && a.Side != SideRight {
		out += " side=" + a.Side
	}
	if a.Context != "" {
		out += " ctx=" + a.Context
	}
	for _, e := range a.Extra {
		out += " " + e
	}
	return out
}

// Lines is the line range as it is written and displayed.
func (a Anchor) Lines() string {
	if a.Last > a.First {
		return strconv.Itoa(a.First) + "-" + strconv.Itoa(a.Last)
	}
	return strconv.Itoa(a.First)
}

// Where is the anchor as a person reads it: path, then line range.
func (a Anchor) Where() string { return a.Path + ":" + a.Lines() }

// ParseWhere reads the `<path>:<line>` and `<path>:<line>-<line>` spelling the
// `--on` flag uses. The revision is not part of it — a person names a place in
// the code, and the command fills in the head they are commenting against.
func ParseWhere(arg string) (path string, first, last int, err error) {
	i := strings.LastIndex(arg, ":")
	if i <= 0 || i == len(arg)-1 {
		return "", 0, 0, fmt.Errorf("--on is spelled <path>:<line>, as in 'internal/review/anchor.go:42'")
	}
	first, last, err = parseLines(arg[i+1:])
	if err != nil {
		return "", 0, 0, err
	}
	return arg[:i], first, last, nil
}

// AnchorOf is the anchor of one thread entry, and whether it has one.
//
// Only a root entry's anchor is read; replies inherit their thread's. A reply
// carrying one of its own is preserved on disk and ignored here — a thread is
// attached to one place in the code, and letting replies wander produces a
// thread whose entries disagree about what they are discussing.
func AnchorOf(st entity.State, comment string) (Anchor, bool) {
	v := st.Annotation(comment, AnchorName)
	if !v.IsString() || v.Str == "" {
		return Anchor{}, false
	}
	a, err := ParseAnchor(v.Str)
	if err != nil {
		return Anchor{}, false
	}
	return a, true
}

// Resolved reports whether a thread is resolved, addressed by its root entry.
func Resolved(st entity.State, root string) bool {
	v := st.Annotation(root, ResolveName)
	switch {
	case v.Kind == entity.KindBool:
		return v.Bool
	case v.IsString():
		return v.Str == "true"
	}
	return false
}

// Currency is how well an anchor still describes the review's head.
//
// Two distinct things make an anchor stale, and a client that reports them as
// one mislabels the common case — so they are separate values rather than a
// boolean.
type Currency int

const (
	// Current: the anchor's commit is reachable from the head and the file has
	// not changed since. Only here may a client render the thread inline
	// against the head.
	Current Currency = iota
	// Outdated: the commit is reachable, but the anchored path changed between
	// it and the head. The ordinary case after a plain push.
	//
	// Note that ancestry alone is not the test. An anchor's commit stays an
	// ancestor across every non-rewriting push, so a client testing
	// reachability would call a thread current long after the lines under it
	// were replaced.
	Outdated
	// Detached: the commit is not reachable from the head at all. The revision
	// was rewritten away by a rebase or an amend, and it may not be in this
	// object store.
	Detached
	// Unknown: this repository does not hold enough of the history to say.
	Unknown
)

func (c Currency) String() string {
	switch c {
	case Current:
		return "current"
	case Outdated:
		return "outdated"
	case Detached:
		return "detached"
	default:
		return "unknown"
	}
}

// Label is the word a rendering puts on a thread that is not current, or "".
func (c Currency) Label() string {
	if c == Current {
		return ""
	}
	return c.String()
}

// Repo is the little of git an anchor needs to place itself. Passed in rather
// than reached for, so this package keeps knowing nothing about how a
// repository is opened.
type Repo interface {
	// Reachable reports whether commit is an ancestor of, or equal to, head.
	Reachable(commit, head string) (bool, error)
	// Changed reports whether path differs between two commits.
	Changed(from, to, path string) (bool, error)
}

// Currency classifies an anchor against the review's head.
//
// An anchor with no head to compare against, or a repository that does not hold
// the commits, is Unknown rather than an error: the thread is rendered where it
// was written either way, and a missing commit is ordinary rather than a
// failure.
func (a Anchor) Currency(repo Repo, head string) Currency {
	if repo == nil || head == "" || a.Revision == "" {
		return Unknown
	}
	if a.Revision == head {
		return Current
	}
	reachable, err := repo.Reachable(a.Revision, head)
	if err != nil {
		return Unknown
	}
	if !reachable {
		return Detached
	}
	changed, err := repo.Changed(a.Revision, head, a.Path)
	if err != nil {
		return Unknown
	}
	if changed {
		return Outdated
	}
	return Current
}

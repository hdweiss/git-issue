// Checks: what a machine asserts about a commit — a build, a quality gate, a
// scan.
//
// Checks do not live in the review blob. They live on their own ref, keyed by
// commit sha, and docs/reviews.md gives the argument in full. In short: a check
// result is a fact about a commit rather than about a review, so it is true
// whichever review contains that commit, shared by every review and branch that
// contains it, and it outlives the review by as long as the commit does.
// Keying by commit also makes rebases correct with no logic — old commits keep
// their old results — and keeps machine-written churn off the review ref.

package review

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hdweiss/git-issue/internal/entity"
)

// RefChecks holds check runs, keyed by commit sha.
//
// One state, named explicitly: even a type with a single state names it, so a
// second can be added later without a ref that is both a leaf and a path
// prefix.
const RefChecks = "checks/runs"

// The ops a check is written with. An ordinary OR-Set, like verdicts.
const (
	CheckField  = "check"
	CheckAdd    = "check.add"
	CheckRemove = "check.remove"
)

// The conclusions docs/reviews.md defines. The vocabulary is open: a platform's
// own spelling is carried through rather than coerced, and an unrecognised one
// renders verbatim.
const (
	ConclusionPass      = "pass"
	ConclusionFail      = "fail"
	ConclusionPending   = "pending"
	ConclusionSkipped   = "skipped"
	ConclusionCancelled = "cancelled"
)

// ChecksVocabulary is what the checks ref contributes to the fold.
//
// Empty, and that is the point: a checks blob holds nothing but list members,
// so it needs no scalar named. It is declared rather than left implicit because
// opening the ref needs a vocabulary to pass, and passing the review type's
// would suggest a checks blob might hold a title.
var ChecksVocabulary = entity.Vocabulary{}

// Check is one check's current result.
type Check struct {
	// ID is the `check.add` that put it there, which is what a removal must
	// name.
	ID   string
	Name string
	// Conclusion is the check's outcome. Compare with Failed and Pending rather
	// than against the constants, so a platform's own spelling still classifies.
	Conclusion string
	URL        string
	// Extra holds key=value pairs this build does not recognise, in the order
	// they arrived, so a round trip preserves them.
	Extra []string
	// TS is when the result was written.
	TS int64
}

// Failed reports whether a conclusion is one that should stop a merge. An
// unrecognised conclusion does not: this client classifies what it knows, and
// treating an unknown word as a failure would block on a platform's vocabulary
// rather than on its verdict.
func (c Check) Failed() bool {
	switch strings.ToLower(c.Conclusion) {
	case ConclusionFail, "failure", "failed", "error", "broken":
		return true
	}
	return false
}

// Pending reports whether a check has not finished.
func (c Check) Pending() bool {
	switch strings.ToLower(c.Conclusion) {
	case ConclusionPending, "queued", "running", "in_progress", "waiting":
		return true
	}
	return false
}

// String is the check's wire form, which is what a `check.add` carries in val:
//
//	<name> <conclusion> [key=value …]
func (c Check) String() string {
	out := c.Name + " " + c.Conclusion
	if c.URL != "" {
		out += " url=" + c.URL
	}
	for _, e := range c.Extra {
		out += " " + e
	}
	return out
}

// ParseCheck reads a check's wire form.
func ParseCheck(s string) (Check, error) {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return Check{}, fmt.Errorf("a check is spelled '<name> <conclusion> [key=value …]'")
	}
	c := Check{Name: fields[0], Conclusion: fields[1]}
	for _, f := range fields[2:] {
		key, value, ok := strings.Cut(f, "=")
		if ok && key == "url" {
			c.URL = value
			continue
		}
		c.Extra = append(c.Extra, f)
	}
	return c, nil
}

// ChecksOf returns the current result of each check on one commit: the survivor
// with the highest (c, id) among the members sharing its name.
//
// The same reader rule verdicts use, over the same ordinary OR-Set. A re-run is
// a new member; a writer replacing a result should retract the members it
// supersedes, but a reader must not depend on that having happened, because a
// bridge writing from two machines will sometimes not have seen the member it
// is superseding.
//
// Sorted by name, so a rendering is stable however the events arrived.
func ChecksOf(st entity.State) []Check {
	latest := map[string]Check{}
	for _, m := range st.Members(CheckField) {
		c, err := ParseCheck(m.Val.Display())
		if err != nil {
			continue
		}
		c.ID = m.ID
		if ev, ok := eventByID(st, m.ID); ok {
			c.TS = ev.TS
		}
		latest[c.Name] = c
	}

	out := make([]Check, 0, len(latest))
	for _, c := range latest {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ChecksSummary counts a commit's checks by outcome.
type ChecksSummary struct {
	Passed  int
	Failed  int
	Pending int
	Other   int
}

// Total is how many checks the commit has at all.
func (s ChecksSummary) Total() int { return s.Passed + s.Failed + s.Pending + s.Other }

// Summarize counts checks by outcome.
func Summarize(checks []Check) ChecksSummary {
	var s ChecksSummary
	for _, c := range checks {
		switch {
		case c.Failed():
			s.Failed++
		case c.Pending():
			s.Pending++
		case strings.EqualFold(c.Conclusion, ConclusionPass), strings.EqualFold(c.Conclusion, "success"):
			s.Passed++
		default:
			s.Other++
		}
	}
	return s
}

// String renders a summary the way a listing's check column does: ✓3 ✗1 ·2.
func (s ChecksSummary) String() string {
	if s.Total() == 0 {
		return ""
	}
	var parts []string
	if s.Passed > 0 {
		parts = append(parts, fmt.Sprintf("✓%d", s.Passed))
	}
	if s.Failed > 0 {
		parts = append(parts, fmt.Sprintf("✗%d", s.Failed))
	}
	if s.Pending > 0 {
		parts = append(parts, fmt.Sprintf("·%d", s.Pending))
	}
	if s.Other > 0 {
		parts = append(parts, fmt.Sprintf("?%d", s.Other))
	}
	return strings.Join(parts, " ")
}

// Plain renders a summary without the glyphs, for a piped listing.
func (s ChecksSummary) Plain() string {
	if s.Total() == 0 {
		return ""
	}
	var parts []string
	if s.Passed > 0 {
		parts = append(parts, fmt.Sprintf("%d passed", s.Passed))
	}
	if s.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", s.Failed))
	}
	if s.Pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", s.Pending))
	}
	if s.Other > 0 {
		parts = append(parts, fmt.Sprintf("%d other", s.Other))
	}
	return strings.Join(parts, ", ")
}

// LoadChecks folds the checks recorded against one commit.
//
// A commit with no checks — or a repository with no checks ref at all — is an
// empty result and not an error. Core state may never depend on this ref: a
// clone without it folds complete, correct reviews, and every caller here has
// to be built for that.
func LoadChecks(store *entity.Store, commit string) ([]Check, error) {
	if commit == "" {
		return nil, nil
	}
	note, err := store.Resolve(commit, "commit")
	if err != nil {
		return nil, nil
	}
	st, err := store.Load(note)
	if err != nil {
		return nil, nil
	}
	return ChecksOf(st), nil
}

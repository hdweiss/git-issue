// What is blocking a review from merging.
//
// This is the read surface an agent loops on: pull, ask what stands in the way,
// act, push. Assembling it from `show` output would mean parsing prose, so it
// is a value with a shape instead, and the command that prints it is a thin
// rendering of this.
//
// Everything here is a *report*. Nothing in a grow-only set can prevent a
// merge — a verdict is a statement, a check is a machine's opinion, and a
// client that ignores both is not corrupting anything. Upstream, branch
// protection decides; locally, this says what upstream would probably say.

package review

import (
	"fmt"
	"strings"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/render"
)

// BlockerKind classifies why a review is not ready.
type BlockerKind string

const (
	BlockerStatus     BlockerKind = "status"
	BlockerDraft      BlockerKind = "draft"
	BlockerChecks     BlockerKind = "checks"
	BlockerPending    BlockerKind = "pending"
	BlockerChanges    BlockerKind = "changes-requested"
	BlockerThreads    BlockerKind = "unresolved"
	BlockerNoApproval BlockerKind = "unapproved"
	BlockerNoRevision BlockerKind = "no-revision"
)

// Blocker is one reason a review is not ready to merge.
type Blocker struct {
	Kind BlockerKind
	// Summary is the one line a person reads.
	Summary string
	// Detail is what the summary could not carry: the failing checks by name
	// and url, the unresolved threads by anchor. One entry per line.
	Detail []string
}

// Report is everything `status` answers.
type Report struct {
	ID       string
	Title    string
	Status   string
	Draft    bool
	Base     string
	Head     string
	Revision string

	Checks    []Check
	Summary   ChecksSummary
	Approvals []Verdict
	Changes   []Verdict
	Stale     []Verdict
	// Threads is every conversation on the review; Resolvable the anchored
	// subset resolution is a meaningful question about; Unresolved the ones of
	// those still open, which is what blocks.
	Threads    []Thread
	Resolvable []Thread
	Unresolved []Thread

	Blockers []Blocker
}

// Ready reports whether nothing stands in the way.
func (r Report) Ready() bool { return len(r.Blockers) == 0 }

// Requirements are the local policy a Report is measured against.
//
// It is policy and says so: upstream the same questions are answered by branch
// protection, which this client cannot read and must not pretend to. The zero
// value asks for what almost everyone wants — checks passing, threads resolved,
// nobody blocking — and leaves the approval count at zero, because requiring an
// approval on a review nobody has been asked to read would report every fresh
// review as blocked.
type Requirements struct {
	// MinApprovals is how many non-stale approvals are wanted. Zero asks for
	// none.
	MinApprovals int
	// IgnoreChecks skips the check ref entirely, for a repository that runs
	// none.
	IgnoreChecks bool
	// AllowUnresolved stops open threads counting as blockers.
	AllowUnresolved bool
}

// Status assembles the report.
//
// checks are the head commit's check runs, read from the checks ref by the
// caller — this package never reaches for a ref of its own. A caller that
// passes none gets a report with no check blockers, which is the correct
// reading of "this clone does not hold that ref": core state may never depend
// on it.
func StatusOf(id string, st entity.State, checks []Check, repo Repo, req Requirements) Report {
	r := Report{
		ID:        id,
		Title:     title(st),
		Status:    Status(st),
		Draft:     Draft(st),
		Base:      Base(st),
		Head:      HeadRef(st),
		Revision:  Head(st),
		Checks:    checks,
		Summary:   Summarize(checks),
		Approvals: Approvals(st),
		Changes:   ChangesRequested(st),
		Threads:   Threads(st, repo),
	}
	r.Resolvable = Resolvable(r.Threads)
	r.Unresolved = OpenQuestions(r.Threads)
	for _, v := range VerdictsOf(st) {
		if v.Stale {
			r.Stale = append(r.Stale, v)
		}
	}

	// A terminal review is not blocked, it is over. Reporting an abandoned
	// review's failing build as something standing in the way of a merge would
	// send an agent to fix a branch nobody wants.
	if Terminal(r.Status) {
		r.Blockers = append(r.Blockers, Blocker{
			Kind:    BlockerStatus,
			Summary: "the review is " + r.Status,
		})
		return r
	}

	if r.Draft {
		r.Blockers = append(r.Blockers, Blocker{
			Kind:    BlockerDraft,
			Summary: "the review is a draft",
		})
	}

	if r.Revision == "" {
		r.Blockers = append(r.Blockers, Blocker{
			Kind:    BlockerNoRevision,
			Summary: "no revision recorded: nothing says which commit this proposes",
		})
	}

	if !req.IgnoreChecks {
		var failed, pending []string
		for _, c := range checks {
			switch {
			case c.Failed():
				failed = append(failed, checkLine(c))
			case c.Pending():
				pending = append(pending, checkLine(c))
			}
		}
		if len(failed) > 0 {
			r.Blockers = append(r.Blockers, Blocker{
				Kind:    BlockerChecks,
				Summary: fmt.Sprintf("%s failing", several(len(failed), "check")),
				Detail:  failed,
			})
		}
		if len(pending) > 0 {
			r.Blockers = append(r.Blockers, Blocker{
				Kind:    BlockerPending,
				Summary: fmt.Sprintf("%s still running", several(len(pending), "check")),
				Detail:  pending,
			})
		}
	}

	if len(r.Changes) > 0 {
		var who []string
		for _, v := range r.Changes {
			who = append(who, v.Author)
		}
		r.Blockers = append(r.Blockers, Blocker{
			Kind:    BlockerChanges,
			Summary: "changes requested by " + strings.Join(who, ", "),
		})
	}

	if !req.AllowUnresolved && len(r.Unresolved) > 0 {
		detail := make([]string, 0, len(r.Unresolved))
		for _, t := range r.Unresolved {
			detail = append(detail, threadLine(t))
		}
		r.Blockers = append(r.Blockers, Blocker{
			Kind:    BlockerThreads,
			Summary: fmt.Sprintf("%s unresolved", several(len(r.Unresolved), "thread")),
			Detail:  detail,
		})
	}

	if n := req.MinApprovals; n > 0 && len(r.Approvals) < n {
		summary := fmt.Sprintf("%d of %d approvals", len(r.Approvals), n)
		b := Blocker{Kind: BlockerNoApproval, Summary: summary}
		// A stale approval is the confusing case — it looks like an approval
		// and does not count — so it is named rather than silently omitted.
		for _, v := range r.Stale {
			if v.Value == VerdictApprove {
				b.Detail = append(b.Detail, v.Author+" approved "+render.Abbrev(v.Revision)+", which is not the current head")
			}
		}
		r.Blockers = append(r.Blockers, b)
	}

	return r
}

func checkLine(c Check) string {
	line := c.Name + "  " + c.Conclusion
	if c.URL != "" {
		line += "  " + c.URL
	}
	return line
}

func threadLine(t Thread) string {
	where := "(no anchor)"
	if t.Anchored {
		where = t.Anchor.Where()
		if label := t.Currency.Label(); label != "" {
			where += "  (" + label + ")"
		}
	}
	line := render.Abbrev(t.Root.ID()) + "  " + where + "  " + t.Root.Event.A
	if body := strings.Join(strings.Fields(t.Root.Body.Display()), " "); body != "" {
		line += "  " + render.Trim(body, 60)
	}
	return line
}

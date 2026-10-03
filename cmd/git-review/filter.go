package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/review"
)

// listFilter narrows a listing to the reviews that match every criterion. A
// zero listFilter matches everything.
//
// Every comparison folds case. --label and --reviewer match a value in full and
// AND when repeated; --author matches any substring of the identity that opened
// the review. Passing 'none' to a repeatable filter selects the reviews that
// have none at all.
type listFilter struct {
	state    string // "", "open", "merged", "closed", or a forge's own state
	base     string
	author   string
	checks   string
	draft    bool
	noDraft  bool
	labels   []string
	who      []string
	noLabels bool
	noWho    bool
}

const filterNone = "none"

// The --checks vocabulary. These classify a review by its head commit's runs
// rather than by any one check.
const (
	checksPassing = "passing"
	checksFailing = "failing"
	checksPending = "pending"
	checksNone    = "none"
)

func (f listFilter) active() bool {
	return f.state != "" || f.base != "" || f.author != "" || f.checks != "" ||
		f.draft || f.noDraft || len(f.labels) > 0 || len(f.who) > 0 || f.noLabels || f.noWho
}

func bindFilterFlags(fs *flag.FlagSet, f *listFilter, labels, who *listFlag) {
	fs.StringVar(&f.state, "state", "", "open, merged, closed, all, or a forge's own state")
	fs.StringVar(&f.base, "base", "", "reviews targeting this branch")
	fs.StringVar(&f.author, "author", "", "reviews opened by an identity containing this")
	fs.StringVar(&f.checks, "checks", "", "passing, failing, pending, or none")
	fs.BoolVar(&f.draft, "draft", false, "only drafts")
	fs.BoolVar(&f.noDraft, "no-draft", false, "exclude drafts")
	for _, name := range []string{"l", "label"} {
		fs.Var(labels, name, "reviews carrying this label; repeatable, or comma-separated")
	}
	for _, name := range []string{"a", "reviewer"} {
		fs.Var(who, name, "reviews this person was asked to read; repeatable")
	}
}

// resolveState normalises --state, folding "" and "all" to no filter.
//
// `open`, `merged` and `closed` are the three docs/reviews.md defines. Every
// other word is matched against the status verbatim, which is what makes a
// forge's own vocabulary filterable: the value is an open vocabulary, so a
// filter over it cannot be a closed list.
func resolveState(s string) (string, error) {
	switch trimmed := strings.TrimSpace(s); strings.ToLower(trimmed) {
	case "", "all":
		return "", nil
	case "open":
		return "open", nil
	case "merged":
		return "merged", nil
	case "closed":
		return "closed", nil
	default:
		return trimmed, nil
	}
}

func resolveChecks(s string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "", checksPassing, checksFailing, checksPending, checksNone:
		return v, nil
	default:
		return "", fmt.Errorf("unknown --checks '%s'; try passing, failing, pending or none", s)
	}
}

func resolveFilter(f *listFilter, labels, who listFlag) error {
	var err error
	if f.labels, f.noLabels, err = splitNone("label", labels.values); err != nil {
		return err
	}
	if f.who, f.noWho, err = splitNone("reviewer", who.values); err != nil {
		return err
	}
	if f.draft && f.noDraft {
		return fmt.Errorf("--draft and --no-draft both say which reviews to keep; use one")
	}
	if f.checks, err = resolveChecks(f.checks); err != nil {
		return err
	}
	f.state, err = resolveState(f.state)
	return err
}

func splitNone(name string, values []string) (want []string, none bool, err error) {
	for _, v := range values {
		if strings.EqualFold(v, filterNone) {
			none = true
			continue
		}
		want = append(want, v)
	}
	if none && len(want) > 0 {
		return nil, false, fmt.Errorf("--%s takes a name or '%s', not both", name, filterNone)
	}
	return want, none, nil
}

// matches reports whether st passes every criterion set on f.
//
// checks are the head commit's runs, handed in because they live on another ref
// and this package reads that ref once for the whole listing rather than per
// row.
func (f listFilter) matches(st entity.State, checks []review.Check) bool {
	switch f.state {
	case "":
	case "open":
		if review.Terminal(review.Status(st)) {
			return false
		}
	case "merged":
		if !strings.EqualFold(review.Status(st), review.StatusMerged) {
			return false
		}
	case "closed":
		// The two terminal endings are different things and a filter must not
		// merge them: a merged review succeeded, a closed one was abandoned,
		// and "show me the closed ones" means the second.
		if !strings.EqualFold(review.Status(st), review.StatusClosed) {
			return false
		}
	default:
		if !strings.EqualFold(review.Status(st), f.state) {
			return false
		}
	}

	if f.base != "" && !strings.EqualFold(review.Base(st), f.base) {
		return false
	}
	if f.draft && !review.Draft(st) {
		return false
	}
	if f.noDraft && review.Draft(st) {
		return false
	}
	if f.author != "" {
		if st.Create == nil || !strings.Contains(strings.ToLower(st.Create.A), strings.ToLower(f.author)) {
			return false
		}
	}
	if !matchesChecks(f.checks, checks) {
		return false
	}
	if f.noLabels && len(st.List("label")) > 0 {
		return false
	}
	if f.noWho && len(st.List("assignee")) > 0 {
		return false
	}
	for _, want := range f.labels {
		if !containsFold(st.List("label"), want) {
			return false
		}
	}
	for _, want := range f.who {
		if !containsFold(st.List("assignee"), want) {
			return false
		}
	}
	return true
}

// matchesChecks classifies a review by its head commit's runs.
//
// A review with no runs answers `none`, whether that is because nothing has run
// or because this clone has never fetched the checks ref. The two are genuinely
// indistinguishable from here, and reporting the second as an error would make
// a filter over optional state fail on the clones that need it least.
func matchesChecks(want string, checks []review.Check) bool {
	if want == "" {
		return true
	}
	s := review.Summarize(checks)
	switch want {
	case checksNone:
		return s.Total() == 0
	case checksFailing:
		return s.Failed > 0
	case checksPending:
		return s.Pending > 0
	case checksPassing:
		// Passing means nothing is failing or still running, and something ran.
		return s.Total() > 0 && s.Failed == 0 && s.Pending == 0
	}
	return true
}

func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

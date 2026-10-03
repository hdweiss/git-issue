package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

// listFilter narrows a listing to the issues that match every criterion set on
// it. A zero listFilter matches everything.
//
// The flags are add's, so the same word selects an issue here as would have set
// it there: --type, -l/--label and -a/--assignee are spelled and split exactly
// as `git issue add` spells and splits them. --state and --author have no add
// equivalent — an issue is born open, by whoever runs the command.
//
// Every comparison folds case. --type, --label and --assignee match a value in
// full; --author matches any substring of the identity that opened the issue,
// which is usually an address nobody wants to type whole. Repeated --label and
// --assignee are AND: the issue must carry every one. Passing 'none' to either
// selects issues that have no label / no assignee at all.
type listFilter struct {
	state       string // "", "open", "closed", or a tracker's own state
	typ         string
	labels      []string
	assignees   []string
	author      string
	noLabels    bool
	noAssignees bool
}

// filterNone is the value that turns a repeatable filter into "has none".
const filterNone = "none"

// active reports whether any criterion is set.
func (f listFilter) active() bool {
	return f.state != "" || f.typ != "" || f.author != "" ||
		len(f.labels) > 0 || len(f.assignees) > 0 || f.noLabels || f.noAssignees
}

// bindFilterFlags registers the filter flags on fs. labels and assignees bind
// to the same repeatable, comma-splitting listFlag that add uses.
func bindFilterFlags(fs *flag.FlagSet, f *listFilter, labels, assignees *listFlag) {
	fs.StringVar(&f.state, "state", "", "open, closed, all, or a tracker's own state")
	fs.StringVar(&f.typ, "type", "", "issues whose type matches")
	fs.StringVar(&f.author, "author", "", "issues opened by an identity containing this")
	for _, name := range []string{"l", "label"} {
		fs.Var(labels, name, "issues carrying this label; repeatable, or comma-separated")
	}
	for _, name := range []string{"a", "assignee"} {
		fs.Var(assignees, name, "issues assigned to this person; repeatable, or comma-separated")
	}
}

// resolveState normalises --state, folding "" and "all" to no filter.
//
// Anything else passes through as itself. `open` and `closed` are the two
// values docs/issues.md defines and are answered by whether the status is
// terminal; every other word is matched against the status verbatim, which is
// what makes `--state Resolved` and `--state Done` work. That is the
// affordance of storing an upstream state as the platform spells it: the
// vocabulary is open, so a filter over it cannot be a closed list.
func resolveState(s string) (string, error) {
	switch trimmed := strings.TrimSpace(s); strings.ToLower(trimmed) {
	case "", "all":
		return "", nil
	case "open":
		return "open", nil
	case "closed":
		return "closed", nil
	default:
		return trimmed, nil
	}
}

// resolveFilter finalises a parsed filter: the repeatable flags become value
// lists, the 'none' sentinel is lifted out of each, and --state is normalised.
func resolveFilter(f *listFilter, labels, assignees listFlag) error {
	var err error
	if f.labels, f.noLabels, err = splitNone("label", labels.values); err != nil {
		return err
	}
	if f.assignees, f.noAssignees, err = splitNone("assignee", assignees.values); err != nil {
		return err
	}
	f.state, err = resolveState(f.state)
	return err
}

// splitNone pulls the 'none' sentinel out of a repeatable filter's values. It
// means "the issue has none of these at all", so it cannot sit alongside a
// concrete name.
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
func (f listFilter) matches(st entity.State) bool {
	switch f.state {
	case "":
	case "open":
		if issue.Terminal(issue.Status(st)) {
			return false
		}
	case "closed":
		if !issue.Terminal(issue.Status(st)) {
			return false
		}
	default:
		// A platform's own state, matched verbatim. Case is folded because
		// nothing guarantees the caller typed it the way the tracker spells
		// it, and it is a name rather than an identifier.
		if !strings.EqualFold(issue.Status(st), f.state) {
			return false
		}
	}
	if f.typ != "" && !strings.EqualFold(f.typ, st.Scalar("type").Display()) {
		return false
	}
	if f.author != "" {
		if st.Create == nil || !strings.Contains(strings.ToLower(st.Create.A), strings.ToLower(f.author)) {
			return false
		}
	}
	if f.noLabels && len(st.List("label")) > 0 {
		return false
	}
	if f.noAssignees && len(st.List("assignee")) > 0 {
		return false
	}
	for _, want := range f.labels {
		if !containsFold(st.List("label"), want) {
			return false
		}
	}
	for _, want := range f.assignees {
		if !containsFold(st.List("assignee"), want) {
			return false
		}
	}
	return true
}

// containsFold reports whether values holds want, comparing case-insensitively.
func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

package ghapi

import (
	"strings"
	"testing"
)

// `diffSide` is a field of PullRequestReviewThread and of nothing inside it.
// Asking for it on a PullRequestReviewComment is not a field GitHub ignores —
// it fails the whole query, so every import against a real server dies with
// "Field 'diffSide' doesn't exist on type 'PullRequestReviewComment'".
//
// Which is fine, because a thread is attached to one place in the code: its
// entries cannot disagree about which half of a split diff they are on, so
// there is nothing the per-comment field would have said.
func TestReviewCommentsDoNotAskForDiffSide(t *testing.T) {
	thread, comments, ok := strings.Cut(reviewThreadSelection, "comments(first:%d)")
	if !ok {
		t.Fatal("the review-thread selection no longer has a comments block")
	}
	if !strings.Contains(thread, "diffSide") {
		t.Error("the thread itself should still ask for diffSide")
	}
	if strings.Contains(comments, "diffSide") {
		t.Errorf("a review comment has no diffSide:\n%s", comments)
	}
}

// GitHub refuses a query whose declared connection sizes could reach more than
// 500,000 nodes. The bound comes from the `first:` values in the query text, not
// from what the repository holds, so a query that is too big is too big
// everywhere — and the failure is a hard error on every import, not a slow path.
//
// This is the arithmetic that caught us: a pull request has a connection nested
// two deep (reviewThreads → comments) that an issue has no counterpart for, so
// the issue query's page size does not carry over.
const githubNodeLimit = 500_000

func TestQueriesFitTheNodeLimit(t *testing.T) {
	// Every optional half on: the widest form of each query, which is what a
	// modern github.com is sent. A server that lacks one gets a smaller query.
	full := schema{issueTypes: true, relations: true, pullExtras: true}

	for _, q := range []struct {
		name  string
		query string
		// roots is the multiplier the outermost selection carries. A connection
		// declares its own with `first:`; nodes(ids:) declares none, and its
		// bound is however many ids one request chunks into.
		roots int
	}{
		{"pullIDsQuery", pullIDsQuery, 1},
		// The detail query's multiplier is however many ids one batch names,
		// since nodes(ids:) declares no `first:` of its own.
		{"pullNodesQuery", pullNodesQuery(full), pullDetailBatch},
		{"moreThreadsQuery", moreThreadsQuery, 1},
		{"morePullCommentsQuery", morePullCommentsQuery, 1},

		// The issue queries have never been near the limit — nothing under an
		// issue nests two connections deep — and they are here so that stays a
		// measured fact rather than an assumption.
		{"issuesQuery", issuesQuery(full), 1},
		{"nodesQuery", nodesQuery(full), issuePage},
		{"moreCommentsQuery", moreCommentsQuery, 1},
	} {
		if got := nodeBudget(q.query, q.roots); got > githubNodeLimit {
			t.Errorf("%s declares up to %d nodes, over GitHub's limit of %d",
				q.name, got, githubNodeLimit)
		}
	}
}

// nodeBudget is GitHub's own node-count arithmetic over a query's text: every
// connection multiplies its enclosing one, and the total is the sum over every
// branch.
//
// Braces inside parentheses — orderBy:{field:…} — are arguments rather than
// selections, so the scan ignores them; counting one as a block would let the
// `first:` before it apply to the wrong thing and quietly undercount.
func nodeBudget(query string, roots int) int {
	stack := []int{roots}
	total, pending, parens := 0, 0, 0

	for i := 0; i < len(query); i++ {
		switch query[i] {
		case '(':
			parens++
		case ')':
			parens--
		case '{':
			if parens > 0 {
				continue
			}
			m := stack[len(stack)-1]
			if pending > 0 {
				m *= pending
				total += m
				pending = 0
			}
			stack = append(stack, m)
		case '}':
			if parens > 0 || len(stack) == 1 {
				continue
			}
			stack = stack[:len(stack)-1]
		default:
			for _, arg := range []string{"first:", "last:"} {
				if !strings.HasPrefix(query[i:], arg) {
					continue
				}
				j := i + len(arg)
				n := 0
				for ; j < len(query) && query[j] >= '0' && query[j] <= '9'; j++ {
					n = n*10 + int(query[j]-'0')
				}
				if n > 0 {
					pending = n
					i = j - 1
				}
			}
		}
	}
	return total
}

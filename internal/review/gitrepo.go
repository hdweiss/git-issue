package review

import "github.com/hdweiss/git-issue/internal/gitx"

// GitRepo answers an anchor's two questions against a real repository.
//
// anchor.go states them as an interface so the classification can be tested and
// reasoned about without a repository; this is the one implementation, kept
// beside it so no caller has to write the same two lines.
type GitRepo struct{ Repo *gitx.Repo }

// Reachable reports whether commit is an ancestor of, or equal to, head.
//
// A commit this clone does not hold reads as unreachable, which classifies the
// anchor as detached. That is the right answer for the case that produces it —
// a rewritten revision this clone never fetched — and it is honest about the
// case that does not, a fork's commits before a fetch, because in both the
// thread genuinely cannot be placed against the head here.
func (g GitRepo) Reachable(commit, head string) (bool, error) {
	if !g.Repo.HasCommit(commit) || !g.Repo.HasCommit(head) {
		return false, nil
	}
	return g.Repo.IsAncestor(commit, head), nil
}

// Changed reports whether path differs between two commits.
//
// Compared by the path's own object name rather than by running a diff: two
// commits naming the same blob at the same path are identical there, whatever
// happened in between, and a file that is absent at one end and present at the
// other differs — which is what a rename or a delete under a review comment
// should read as.
func (g GitRepo) Changed(from, to, path string) (bool, error) {
	return g.Repo.PathSHA(from, path) != g.Repo.PathSHA(to, path), nil
}

package main

import (
	"fmt"
	"strings"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/review"
)

// The refs a relation target may be resolved against, in the order they are
// tried: this command's own first, then the issue type's.
//
// Named as ref strings rather than reached for through internal/issue, which
// this command must not import — resolving an id is a prefix match over a
// notes tree's keys and needs no vocabulary at all. That is what keeps
// `--closes 4b0755a3` usable without git-review learning what an issue is.
var targetRefs = []string{"issues/open", "issues/archived"}

// resolveTarget expands an abbreviated relation target to a full entity id.
//
// A `ref` names an entity, and an abbreviation only means something relative to
// the ref it was measured against — so storing one would write a link that no
// other clone, and no other ref, can follow. Every target is therefore expanded
// before it is written, or refused.
//
// Three outcomes, in order:
//
//   - It resolves here, on this command's ref or the issue type's: the full id.
//   - It is already a full object name: kept as given. The target may live in
//     another repository, or on a ref this clone has never fetched, and
//     docs/issues.md is explicit that a relation is not promised to name
//     something local.
//   - Neither: refused, because a short id nothing here can expand is a link
//     nobody will ever be able to follow.
func resolveTarget(s *entity.Store, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", fmt.Errorf("a relation needs a target")
	}

	if id, err := s.Resolve(arg, review.Type); err == nil {
		return id.Entity, nil
	} else if isAmbiguous(err) {
		return "", err
	}

	for _, ref := range targetRefs {
		other := entity.NewStore(s.Repo, ref, entity.Vocabulary{})
		if note, err := other.Resolve(arg, "entity"); err == nil {
			return note.Entity, nil
		} else if isAmbiguous(err) {
			return "", err
		}
	}

	if isFullID(s, arg) {
		return arg, nil
	}
	return "", fmt.Errorf("unknown entity '%s'; give the id in full if it lives in another repository", arg)
}

// isAmbiguous reports whether a resolution failed because the prefix named
// several entities rather than none. An ambiguous prefix is refused rather
// than resolved arbitrarily — the same rule that governs every other id here —
// while an unknown one is simply the next ref's turn to try.
func isAmbiguous(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "ambiguous")
}

// isFullID reports whether a target is already a full object name for this
// repository's hash algorithm, which is the width every entity id has.
func isFullID(s *entity.Store, arg string) bool {
	width := 40
	if s.Format() == entity.SHA256 {
		width = 64
	}
	if len(arg) != width {
		return false
	}
	for _, r := range arg {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// resolveRelations expands every target in a set of links.
func resolveRelations(s *entity.Store, rels []review.Relation) ([]review.Relation, error) {
	out := make([]review.Relation, 0, len(rels))
	for _, r := range rels {
		target, err := resolveTarget(s, r.Target)
		if err != nil {
			return nil, err
		}
		r.Target = target
		out = append(out, r)
	}
	return out, nil
}

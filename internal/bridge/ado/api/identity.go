// How an Azure DevOps identity becomes an event author and a commit byline.
//
// Both bridges that read this API — internal/bridge/ado/issue for work items and
// internal/bridge/ado/review for pull requests — spell an author the same way,
// and they must: `a` is part of an event's hashed bytes, so two spellings of
// one person would produce different ids for the same event and stop the
// re-import convergence the whole design rests on. The rule lives here, once,
// rather than in each bridge.

package adoapi

import (
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/gitx"
)

// EventAuthor maps an identity onto an event's `a` field.
//
// The scheme prefix matters as much as the name. The unique name is preferred
// because it is the most portable of the three parts — usually a UPN or an
// email address — and it is lowercased because Azure DevOps is not consistent
// about its case.
func (i Identity) EventAuthor() string { return "ado:" + i.name() }

func (i Identity) name() string {
	switch {
	case i.UniqueName != "":
		return strings.ToLower(i.UniqueName)
	case i.ID != "":
		return strings.ToLower(i.ID)
	case i.DisplayName != "":
		return strings.ToLower(i.DisplayName)
	}
	return "unknown"
}

// CommitIdentity maps an identity onto the author of a commit.
//
// Unlike EventAuthor, nothing hashes this: two bridges spelling it differently
// disagree about a commit's byline and about nothing else. The unique name is
// used as the address when it looks like one, which is what makes
// `git shortlog`, `git log --author` and a repository's .mailmap work on the
// tracker's history. On an older on-prem install it can be DOMAIN\user, which
// is not an address, so that case gets a reserved one instead of a malformed
// one.
func (i Identity) CommitIdentity(at time.Time) gitx.Identity {
	display := i.DisplayName
	if display == "" {
		display = i.name()
	}
	email := i.UniqueName
	if !strings.Contains(email, "@") {
		email = sanitizeLocalPart(i.name()) + "@azure-devops.invalid"
	}
	return gitx.Identity{Name: display, Email: email, When: at}
}

// sanitizeLocalPart makes a local part out of something that is not one.
func sanitizeLocalPart(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}

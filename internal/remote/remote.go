// Package remote parses the target of a pull or a push.
//
// The syntax is `[scheme:]target[#scope]`, where the scheme names where the
// issues are to be read from — `git` for another copy of the notes refs,
// `github` for the bridge in docs/bridge-github.md, `ado` for the one in
// docs/bridge-ado.md — and the target names which repository.
//
// A bare target means `git`. That default is deliberate: the git path is the
// native one and needs no credentials and no API, so a scheme is never needed
// just to fetch another clone's notes ref. An unqualified `git issue pull`
// with no target at all reads a bridge too, but only once it has resolved the
// target and found it is a tracker's — see LooksLikeGitHub, LooksLikeADO and
// cmd/git-issue. Naming a target explicitly always means exactly the scheme
// given.
//
// The scope after `#` is scheme-defined and only `ado` has one, where it is an
// area path. It is cut before the target is resolved, so a scope can never be
// mistaken for part of a remote name or a URL.
package remote

import (
	"strings"

	"github.com/hdweiss/git-issue/internal/gitx"
)

// Schemes are the sources a target can name, in the order they are offered in
// error messages.
const (
	SchemeGit    = "git"
	SchemeGitHub = "github"
	SchemeADO    = "ado"
	SchemeGitea  = "gitea"
)

var Schemes = []string{SchemeGit, SchemeGitHub, SchemeADO, SchemeGitea}

// Default is the target used when none is given and the current branch names
// no remote of its own — the same fallback `git clone` itself configures.
const Default = "origin"

// Spec is a resolved target.
type Spec struct {
	Scheme string // SchemeGit, SchemeGitHub, SchemeADO, SchemeGitea
	Target string // the target as written, without the scheme or the scope
	Name   string // the git remote it names, or "" if it named a URL or slug
	URL    string // the remote's fetch URL, or the target itself

	// Scope is the scheme-defined refinement written after `#`: for ado, an
	// area path. Empty for every other scheme, which have none.
	Scope string
	// ScopeSet records that a `#` was written even when nothing followed it.
	// The two are different requests: no `#` means "whatever is configured",
	// while a bare `#` means "forget that and ask me again".
	ScopeSet bool
}

// Parse resolves a target against the repository's configured remotes.
//
// Only a known scheme is treated as one. That keeps URLs unambiguous: the
// `https:` in an https URL and the `git@host` of an scp-style one are part of
// the target, not schemes, and no amount of URL syntax can be mistaken for a
// bridge name.
func Parse(repo *gitx.Repo, arg string) Spec {
	s := Spec{Scheme: SchemeGit, Target: arg}
	if scheme, rest, ok := strings.Cut(arg, ":"); ok && isScheme(scheme) {
		s.Scheme, s.Target = scheme, rest
	}
	// The scope comes off before anything resolves the target, so a `#` can
	// never end up inside a remote name or a URL. Only a scheme that defines a
	// scope splits on it: for the others `#` is an ordinary character, and
	// treating it otherwise would change what an existing target means.
	if hasScope(s.Scheme) {
		if target, scope, ok := strings.Cut(s.Target, "#"); ok {
			s.Target, s.Scope, s.ScopeSet = target, scope, true
		}
	}
	// The target defaults independently of the scheme, so `github:` reads the
	// same remote a bare invocation would fetch from. One rule, stated once:
	// the current branch's own remote, the way plain `git pull` resolves one,
	// falling back to origin when the branch has none configured.
	if s.Target == "" {
		s.Target = Default
		if r := repo.BranchRemote(); r != "" {
			s.Target = r
		}
	}

	if url := repo.RemoteURL(s.Target); url != "" {
		s.Name, s.URL = s.Target, url
	} else {
		s.URL = s.Target
	}
	return s
}

func isScheme(s string) bool {
	for _, known := range Schemes {
		if s == known {
			return true
		}
	}
	return false
}

// hasScope reports whether a scheme defines a `#` scope. Only ado does, where
// it is an area path; see docs/bridge-ado.md.
func hasScope(scheme string) bool { return scheme == SchemeADO }

// IsGitRemote reports whether the target named a remote git already knows,
// rather than a bare URL or an owner/repo slug.
func (s Spec) IsGitRemote() bool { return s.Name != "" }

// LooksLikeGitHub reports whether the target's URL points at github.com.
//
// Guessing a bridge from a URL is ordinarily exactly the surprise an explicit
// scheme exists to avoid, and everywhere a target was named this only
// improves an error message. A fully bare `git issue pull` is the one
// exception: there was nothing to name a scheme on, so cmd/git-issue reads
// this to decide whether the resolved remote is worth also asking the bridge
// about.
func (s Spec) LooksLikeGitHub() bool {
	url := strings.ToLower(s.URL)
	return strings.Contains(url, "github.com/") || strings.Contains(url, "github.com:")
}

// LooksLikeADO reports whether the target's URL points at Azure DevOps, for
// the same fully-bare-invocation case LooksLikeGitHub serves.
//
// The hosted services are named outright. An on-prem Azure DevOps Server is on
// somebody's own hostname and cannot be, so it is recognised by the `_git`
// path segment its clone URLs carry — the same segment ParseTarget splits on
// to find the collection and the project, and one that appears in essentially
// no other tracker's URLs.
func (s Spec) LooksLikeADO() bool {
	url := strings.ToLower(s.URL)
	for _, host := range []string{"dev.azure.com/", "dev.azure.com:", ".visualstudio.com/", ".visualstudio.com:"} {
		if strings.Contains(url, host) {
			return true
		}
	}
	return strings.Contains(url, "/_git/")
}

// LooksLikeGitea reports whether the target's URL points at a host that is
// recognisably Gitea or Forgejo.
//
// Gitea is self-hosted on arbitrary hostnames, so unlike GitHub and ADO there
// is no definitive URL marker. This is the cheap half of the recognition: the
// two big public instances by name, and a hostname that carries "gitea" or
// "forgejo". The other half — a `/api/v1/version` probe for a bare invocation
// against an unrecognised host — lives in cmd/git-issue, since it costs a
// request.
func (s Spec) LooksLikeGitea() bool {
	url := strings.ToLower(s.URL)
	for _, host := range []string{"codeberg.org/", "codeberg.org:", "gitea.com/", "gitea.com:"} {
		if strings.Contains(url, host) {
			return true
		}
	}
	host := hostOf(url)
	return strings.Contains(host, "gitea") || strings.Contains(host, "forgejo")
}

// hostOf pulls the host out of a lowercased remote URL, https and scp-style
// alike, for a substring test.
func hostOf(url string) string {
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
	}
	if i := strings.Index(url, "@"); i >= 0 {
		url = url[i+1:]
	}
	if i := strings.IndexAny(url, "/:"); i >= 0 {
		url = url[:i]
	}
	return url
}

package remote

import (
	"os/exec"
	"testing"

	"github.com/hdweiss/git-issue/internal/gitx"
)

// testRepo is a repository with two remotes: one plain, one that looks like a
// forge. Resolution asks git what it knows, so the test has to give it
// something to know.
func testRepo(t *testing.T) *gitx.Repo {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://github.com/hdweiss/git-issue.git"},
		{"remote", "add", "backup", "/srv/mirrors/git-issue.git"},
		{"remote", "add", "ado", "https://dev.azure.com/contoso/MyProj/_git/repo"},
		{"remote", "add", "onprem", "https://corp.local/tfs/DefaultCollection/MyProj/_git/repo"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	repo, err := gitx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestParse(t *testing.T) {
	repo := testRepo(t)

	for _, tc := range []struct {
		arg      string
		scheme   string
		target   string
		isRemote bool
		github   bool
		ado      bool
		scope    string
		scopeSet bool
	}{
		// No target is origin, and no scheme is git. Nothing reaches a forge
		// unless it was asked for by name.
		{arg: "", scheme: SchemeGit, target: "origin", isRemote: true, github: true},
		{arg: "origin", scheme: SchemeGit, target: "origin", isRemote: true, github: true},
		{arg: "git:origin", scheme: SchemeGit, target: "origin", isRemote: true, github: true},
		{arg: "github:origin", scheme: SchemeGitHub, target: "origin", isRemote: true, github: true},
		{arg: "github:", scheme: SchemeGitHub, target: "origin", isRemote: true, github: true},
		{arg: "backup", scheme: SchemeGit, target: "backup", isRemote: true},

		// A target git does not recognise is a URL or a slug, not an error:
		// the bridge can import a repository this clone has no remote for.
		{arg: "github:hdweiss/git-issue", scheme: SchemeGitHub, target: "hdweiss/git-issue"},
		{arg: "github:https://github.com/o/r", scheme: SchemeGitHub, target: "https://github.com/o/r", github: true},

		// Only a known scheme is a scheme. URL punctuation never is, however
		// much it looks like one.
		{arg: "https://github.com/o/r", scheme: SchemeGit, target: "https://github.com/o/r", github: true},
		{arg: "git@github.com:o/r.git", scheme: SchemeGit, target: "git@github.com:o/r.git", github: true},
		{arg: "ssh://host/x.git", scheme: SchemeGit, target: "ssh://host/x.git"},

		// A remote named after a scheme is still just a remote: a scheme needs
		// the colon.
		{arg: "ado", scheme: SchemeGit, target: "ado", isRemote: true, ado: true},
		{arg: "ado:ado", scheme: SchemeADO, target: "ado", isRemote: true, ado: true},
		{arg: "ado:onprem", scheme: SchemeADO, target: "onprem", isRemote: true, ado: true},

		// The target defaults independently of the scheme, so `ado:` resolves
		// the same remote a bare invocation would — which here is GitHub's,
		// and the point is that Parse says so rather than guessing.
		{arg: "ado:", scheme: SchemeADO, target: "origin", isRemote: true, github: true},

		// The scope comes off before the target resolves, so the remote is
		// still found and the area never reaches the URL.
		{arg: "ado:ado#Web/Auth", scheme: SchemeADO, target: "ado", isRemote: true, ado: true, scope: "Web/Auth", scopeSet: true},
		{arg: "ado:ado#", scheme: SchemeADO, target: "ado", isRemote: true, ado: true, scopeSet: true},
		{arg: "ado:contoso/MyProj#Web", scheme: SchemeADO, target: "contoso/MyProj", scope: "Web", scopeSet: true},
		{
			arg:    "ado:https://corp.local/tfs/DefaultCollection/MyProj/_git/repo#Web",
			scheme: SchemeADO, target: "https://corp.local/tfs/DefaultCollection/MyProj/_git/repo",
			ado: true, scope: "Web", scopeSet: true,
		},

		// Only a scheme that defines a scope splits on '#'. For every other
		// one it is an ordinary character in the target.
		{arg: "github:o/r#x", scheme: SchemeGitHub, target: "o/r#x"},
		{arg: "https://host/r#x", scheme: SchemeGit, target: "https://host/r#x"},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			s := Parse(repo, tc.arg)
			if s.Scheme != tc.scheme || s.Target != tc.target {
				t.Errorf("Parse(%q) = %s:%s, want %s:%s", tc.arg, s.Scheme, s.Target, tc.scheme, tc.target)
			}
			if s.IsGitRemote() != tc.isRemote {
				t.Errorf("Parse(%q).IsGitRemote() = %v, want %v", tc.arg, s.IsGitRemote(), tc.isRemote)
			}
			if s.LooksLikeGitHub() != tc.github {
				t.Errorf("Parse(%q).LooksLikeGitHub() = %v, want %v", tc.arg, s.LooksLikeGitHub(), tc.github)
			}
			if s.LooksLikeADO() != tc.ado {
				t.Errorf("Parse(%q).LooksLikeADO() = %v, want %v", tc.arg, s.LooksLikeADO(), tc.ado)
			}
			if s.Scope != tc.scope || s.ScopeSet != tc.scopeSet {
				t.Errorf("Parse(%q) scope = %q (set %v), want %q (set %v)",
					tc.arg, s.Scope, s.ScopeSet, tc.scope, tc.scopeSet)
			}
		})
	}
}

// LooksLikeGitea recognises the two public instances and any host that names
// itself, and nothing else — the probe in cmd/git-issue covers the rest.
func TestLooksLikeGitea(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://codeberg.org/o/r", true},
		{"https://gitea.com/o/r", true},
		{"git@gitea.example.com:o/r.git", true},
		{"https://forgejo.example.org/o/r", true},
		{"https://git.example.com/o/r", false},
		{"https://github.com/o/r", false},
		{"https://dev.azure.com/org/proj/_git/repo", false},
	} {
		if got := (Spec{URL: tc.url}).LooksLikeGitea(); got != tc.want {
			t.Errorf("LooksLikeGitea(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

// A named remote resolves to its URL; anything else stands for itself.
func TestParseResolvesURL(t *testing.T) {
	repo := testRepo(t)

	s := Parse(repo, "github:origin")
	if want := "https://github.com/hdweiss/git-issue.git"; s.URL != want {
		t.Errorf("URL = %q, want %q", s.URL, want)
	}

	s = Parse(repo, "github:hdweiss/git-issue")
	if want := "hdweiss/git-issue"; s.URL != want {
		t.Errorf("URL = %q, want %q", s.URL, want)
	}
}

// A scheme with nothing after it defaults its target like any other missing
// one, so `git:` and a bare invocation mean the same thing.
func TestParseDefaultsTargetUnderAScheme(t *testing.T) {
	repo := testRepo(t)
	s := Parse(repo, "git:")
	if s.Scheme != SchemeGit || s.Target != Default {
		t.Errorf(`Parse("git:") = %s:%s, want %s:%s`, s.Scheme, s.Target, SchemeGit, Default)
	}
}

// A missing target reads the current branch's own remote — the same key
// plain `git pull` resolves — rather than assuming origin unconditionally.
// `github:` defaults the same way, since the two share one rule.
func TestParseDefaultsToBranchRemote(t *testing.T) {
	repo := testRepo(t)
	for _, args := range [][]string{
		{"checkout", "-q", "-b", "work"},
		{"config", "branch.work.remote", "backup"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo.Dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	for _, arg := range []string{"", "github:"} {
		s := Parse(repo, arg)
		if s.Target != "backup" || s.Name != "backup" {
			t.Errorf("Parse(%q) on a branch tracking backup = target %q, name %q, want backup", arg, s.Target, s.Name)
		}
	}
}

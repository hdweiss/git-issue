package adoapi

import "testing"

// Every form a git remote can actually hold, plus the slug. The point of the
// "/_git/" anchor is that one rule covers the hosted service, the legacy
// visualstudio.com spelling and an on-prem collection under a virtual
// directory, so the table is the specification of that claim.
func TestParseTarget(t *testing.T) {
	for _, tc := range []struct {
		in         string
		base       string
		collection string
		project    string
		repo       string
	}{
		{
			in:   "https://dev.azure.com/contoso/MyProj/_git/repo",
			base: "https://dev.azure.com", collection: "contoso", project: "MyProj", repo: "repo",
		},
		{
			in:   "https://dev.azure.com/contoso/MyProj/_git/repo.git",
			base: "https://dev.azure.com", collection: "contoso", project: "MyProj", repo: "repo",
		},
		// A repository name with a space survives, spelled as the URL escapes it.
		{
			in:   "https://dev.azure.com/contoso/MyProj/_git/My%20Repo",
			base: "https://dev.azure.com", collection: "contoso", project: "MyProj", repo: "My%20Repo",
		},
		// https clone URLs frequently carry the organization as userinfo.
		{
			in:   "https://contoso@dev.azure.com/contoso/MyProj/_git/repo",
			base: "https://dev.azure.com", collection: "contoso", project: "MyProj", repo: "repo",
		},
		// On-prem: the virtual directory belongs to the base, and the last two
		// segments before /_git/ are the collection and the project.
		{
			in:   "https://corp.local/tfs/DefaultCollection/MyProj/_git/repo",
			base: "https://corp.local/tfs", collection: "DefaultCollection", project: "MyProj", repo: "repo",
		},
		// On-prem with no virtual directory at all.
		{
			in:   "https://corp.local/DefaultCollection/MyProj/_git/repo",
			base: "https://corp.local", collection: "DefaultCollection", project: "MyProj", repo: "repo",
		},
		// On-prem behind a deeper path, and over plain http, which some
		// installs are.
		{
			in:   "http://corp.local:8080/tfs/inner/DefaultCollection/MyProj/_git/repo",
			base: "http://corp.local:8080/tfs/inner", collection: "DefaultCollection", project: "MyProj", repo: "repo",
		},
		// The legacy form carries its collection in the hostname.
		{
			in:   "https://contoso.visualstudio.com/MyProj/_git/repo",
			base: "https://contoso.visualstudio.com", collection: "contoso", project: "MyProj", repo: "repo",
		},
		// ...and its DefaultCollection spelling is the ordinary path form.
		{
			in:   "https://contoso.visualstudio.com/DefaultCollection/MyProj/_git/repo",
			base: "https://contoso.visualstudio.com", collection: "DefaultCollection", project: "MyProj", repo: "repo",
		},
		// SSH, which is the one form with no /_git/ segment.
		{
			in:   "git@ssh.dev.azure.com:v3/contoso/MyProj/repo",
			base: "https://dev.azure.com", collection: "contoso", project: "MyProj", repo: "repo",
		},
		{
			in:   "ssh://git@ssh.dev.azure.com/v3/contoso/MyProj/repo",
			base: "https://dev.azure.com", collection: "contoso", project: "MyProj", repo: "repo",
		},
		// A slug names the hosted service, since it carries no host to say
		// otherwise — and no repository.
		{
			in:   "contoso/MyProj",
			base: "https://dev.azure.com", collection: "contoso", project: "MyProj", repo: "",
		},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseTarget(tc.in)
			if err != nil {
				t.Fatalf("ParseTarget(%q): %v", tc.in, err)
			}
			if got.Base != tc.base || got.Collection != tc.collection || got.Project != tc.project || got.Repo != tc.repo {
				t.Errorf("ParseTarget(%q) = %s | %s | %s | %s, want %s | %s | %s | %s",
					tc.in, got.Base, got.Collection, got.Project, got.Repo, tc.base, tc.collection, tc.project, tc.repo)
			}
		})
	}
}

func TestParseTargetRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"contoso",
		"https://github.com/hdweiss/git-issue.git",
		"https://corp.local/_git/repo",
	} {
		t.Run(in, func(t *testing.T) {
			if got, err := ParseTarget(in); err == nil {
				t.Errorf("ParseTarget(%q) = %+v, want an error", in, got)
			}
		})
	}
}

// String is the tracker name, so it is the origin ledger's path and the
// watermark's key. It drops the scheme — the same project reached over http in
// a test and https in life is one tracker — and keeps the virtual directory,
// since two collections can differ by nothing else.
func TestTargetString(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://dev.azure.com/contoso/MyProj/_git/repo", "dev.azure.com/contoso/MyProj"},
		{"http://corp.local:8080/tfs/DefaultCollection/MyProj/_git/repo", "corp.local:8080/tfs/DefaultCollection/MyProj"},
		{"git@ssh.dev.azure.com:v3/contoso/MyProj/repo", "dev.azure.com/contoso/MyProj"},
	} {
		got, err := ParseTarget(tc.in)
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", tc.in, err)
		}
		if got.String() != tc.want {
			t.Errorf("ParseTarget(%q).String() = %q, want %q", tc.in, got.String(), tc.want)
		}
	}
}

// Key names the collection, not the project. A work item id is unique per
// collection and survives being moved between projects, so keying identity on
// the project would fork such an item into two entities on the next import.
func TestTargetKeyIsTheCollection(t *testing.T) {
	a, err := ParseTarget("https://dev.azure.com/contoso/MyProj/_git/repo")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseTarget("https://dev.azure.com/contoso/OtherProj/_git/other")
	if err != nil {
		t.Fatal(err)
	}
	if a.Key() != b.Key() {
		t.Errorf("two projects of one collection have keys %q and %q; want one", a.Key(), b.Key())
	}
	if want := "dev.azure.com/contoso"; a.Key() != want {
		t.Errorf("Key() = %q, want %q", a.Key(), want)
	}
}

// The area is rooted at the project and separated the way Azure DevOps spells
// it, whatever the target syntax used to write it.
func TestTargetAreaPath(t *testing.T) {
	base, err := ParseTarget("https://dev.azure.com/contoso/MyProj/_git/repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ area, want string }{
		{"", `MyProj`},
		{"Web", `MyProj\Web`},
		{"Web/Auth", `MyProj\Web\Auth`},
		{"/Web/Auth/", `MyProj\Web\Auth`},
	} {
		target := base
		target.Area = tc.area
		if got := target.AreaPath(); got != tc.want {
			t.Errorf("Area %q -> AreaPath() = %q, want %q", tc.area, got, tc.want)
		}
	}
}

// A personal access token is basic auth with an empty username; an Entra
// access token is a bearer. Sending a PAT as a bearer fails with a 203 and a
// sign-in page, which is the confusing failure this distinction avoids.
func TestAuthorizationTellsTokensApart(t *testing.T) {
	// A minimal JWT: {"alg":"none"} . {} . signature
	const jwt = "eyJhbGciOiJub25lIn0.e30.sig"
	if got := authorization(jwt); got != "Bearer "+jwt {
		t.Errorf("authorization(jwt) = %q, want a bearer", got)
	}
	// Base64 of ":pat".
	if got, want := authorization("pat"), "Basic OnBhdA=="; got != want {
		t.Errorf("authorization(pat) = %q, want %q", got, want)
	}
	if got := authorization(""); got != "" {
		t.Errorf("authorization(\"\") = %q, want empty", got)
	}
	// A PAT that happens to contain dots is still not a JWT.
	if got := authorization("abc.def.ghi"); !hasPrefix(got, "Basic ") {
		t.Errorf("authorization(non-jwt with dots) = %q, want basic", got)
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

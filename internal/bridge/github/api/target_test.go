package ghapi

import "testing"

func TestParseTarget(t *testing.T) {
	for _, tc := range []struct{ in, host, owner, name, endpoint string }{
		{"hdweiss/git-issue", "github.com", "hdweiss", "git-issue", "https://api.github.com/graphql"},
		{"https://github.com/hdweiss/git-issue.git", "github.com", "hdweiss", "git-issue", "https://api.github.com/graphql"},
		{"git@github.com:hdweiss/git-issue.git", "github.com", "hdweiss", "git-issue", "https://api.github.com/graphql"},
		{"ssh://git@github.com/hdweiss/git-issue", "github.com", "hdweiss", "git-issue", "https://api.github.com/graphql"},
		{"https://git.corp.example/team/tracker.git", "git.corp.example", "team", "tracker", "https://git.corp.example/api/graphql"},
		{"git@git.corp.example:team/tracker.git", "git.corp.example", "team", "tracker", "https://git.corp.example/api/graphql"},
		{"http://127.0.0.1:8080/o/n", "127.0.0.1:8080", "o", "n", "http://127.0.0.1:8080/api/graphql"},
	} {
		got, err := ParseTarget(tc.in)
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", tc.in, err)
			continue
		}
		if got.Host != tc.host || got.Owner != tc.owner || got.Name != tc.name {
			t.Errorf("ParseTarget(%q) = %+v, want %s %s/%s", tc.in, got, tc.host, tc.owner, tc.name)
		}
		if got.Endpoint() != tc.endpoint {
			t.Errorf("ParseTarget(%q).Endpoint() = %q, want %q", tc.in, got.Endpoint(), tc.endpoint)
		}
	}

	for _, bad := range []string{"", "justaname", "https://github.com/onlyowner", "a/b/c"} {
		if got, err := ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) = %+v, want an error", bad, got)
		}
	}
}

package issue

import "testing"

func TestTypeIcon(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"bug", "\U0001f41e"},
		{"Bug", "\U0001f41e"},
		{"  BUG  ", "\U0001f41e"},
		{"backend bug", "\U0001f41e"}, // alias found on a word boundary
		{"task", "✅"},
		{"todo", "✅"},
		{"feature", "\U0001f3c6"},
		{"feat", "\U0001f3c6"},
		{"User Story", "\U0001f4d6"},
		{"user-story", "\U0001f4d6"},
		{"user_stories", "\U0001f4d6"},
		{"epic", "\U0001f451"},
		{"enhancement", "\U0001f4c8"},
		{"CI/CD", "\U0001f527"},
		{"", "✅"},                        // blank falls back to the task icon
		{"   ", "✅"},                     // whitespace only is still blank
		{"docker", "docker"},             // "doc" must not match inside a word
		{"security", "\U0001f512"},       // whole-word alias
		{"mystery type", "mystery type"}, // no match keeps the original text
	} {
		if got := typeIcon(tc.in); got != tc.want {
			t.Errorf("typeIcon(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The first matching group wins, so a type that could read as two categories
// resolves to the one listed higher: "epic" precedes "feature".
func TestTypeIconOrder(t *testing.T) {
	if got := typeIcon("epic feature"); got != "\U0001f451" {
		t.Errorf("typeIcon(%q) = %q, want the epic icon", "epic feature", got)
	}
}

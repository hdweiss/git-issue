package issue

import "testing"

func TestTerminal(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{
		{"", false},
		{"open", false},
		{"closed", true},
		{"CLOSED", true}, // a bridge's own case
		{"New", false},
		{"Active", false},
		{"Resolved", false}, // fixed, not verified — non-terminal in ADO
		{"Done", true},      // Basic / Scrum terminal state
		{"Removed", true},
		{"In Review", false}, // a customised process's state: preserved, non-terminal
	} {
		if got := Terminal(tc.status); got != tc.want {
			t.Errorf("Terminal(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestNormalizeReason(t *testing.T) {
	for in, want := range map[string]string{
		"completed":    "completed",
		"Completed":    "completed",
		"not-planned":  "not_planned",
		"not planned":  "not_planned",
		"not_planned":  "not_planned",
		"duplicate":    "duplicate",
		"  Duplicate ": "duplicate",
		"wont-fix":     "wont_fix", // unknown: normalised but not mapped
	} {
		if got := NormalizeReason(in); got != want {
			t.Errorf("NormalizeReason(%q) = %q, want %q", in, got, want)
		}
	}

	for _, r := range []string{"completed", "not_planned", "duplicate"} {
		if !KnownReason(r) {
			t.Errorf("KnownReason(%q) = false", r)
		}
	}
	for _, r := range []string{"", "wont_fix", "reopened"} {
		if KnownReason(r) {
			t.Errorf("KnownReason(%q) = true", r)
		}
	}
}

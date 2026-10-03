package issue

import "strings"

// typeIcon renders an issue's type as an emoji for a listing.
//
// The type field is free text — imported from whatever tracker the issue came
// from, or typed by hand — so the match is deliberately loose: case and the
// usual separators are normalised away, then the type is searched for any known
// alias on a word boundary, top to bottom. The first hit wins, so a more
// specific category is listed before one whose alias it contains.
//
// A blank type gets the task icon, since an untyped issue is a task by default.
// A type that matches nothing is shown as it was written, on the theory that an
// unknown word says more than a wrong icon.
func typeIcon(t string) string {
	norm := normalizeType(t)
	if norm == "" {
		return typeIconBlank
	}
	padded := " " + norm + " "
	for _, g := range typeGroups {
		for _, alias := range g.aliases {
			if strings.Contains(padded, " "+alias+" ") {
				return g.icon
			}
		}
	}
	return strings.TrimSpace(t)
}

// typeIconBlank stands in for an issue with no type at all: the same icon a
// task gets, since an issue with nothing said about it is a task by default.
const typeIconBlank = "✅" // ✅, kept in step with the task group

// normalizeType lowercases, turns -, _, /, and . into spaces, and collapses
// runs of whitespace, so "User_Story", "user-story" and "User Story" all reduce
// to the same thing.
func normalizeType(t string) string {
	t = strings.ToLower(t)
	t = strings.Map(func(r rune) rune {
		switch r {
		case '-', '_', '/', '.':
			return ' '
		default:
			return r
		}
	}, t)
	return strings.Join(strings.Fields(t), " ")
}

// typeGroup is one category: the emoji it renders as and the normalised
// spellings that select it. Aliases are matched on word boundaries, so short
// ones like "ci" or "qa" only match as whole words, never inside another.
type typeGroup struct {
	icon    string
	aliases []string
}

// typeGroups is scanned in order, so anything whose alias is a word inside
// another category's name must come first. Every icon here is a single code
// point that internal/render treats as double-width — the emoji planes, plus
// U+2705 (✅), which render's width table covers as a one-off for this file.
var typeGroups = []typeGroup{
	{"\U0001f41e", []string{"bug", "bugs", "defect", "fault", "regression"}},                          // 🐞
	{"\U0001f4d6", []string{"user story", "user stories", "story", "stories"}},                        // 📖
	{"\U0001f451", []string{"epic", "epics", "initiative", "theme"}},                                  // 👑, Azure DevOps' crown
	{"\U0001f3c6", []string{"feature", "features", "feat", "new feature"}},                            // 🏆, Azure DevOps' trophy
	{"\U0001f4c8", []string{"improvement", "improvements", "enhancement", "enhancements", "enhance"}}, // 📈
	{"✅", []string{"task", "tasks", "todo", "to do", "chore task"}},                                   // ✅
	{"\U0001f9f9", []string{"chore", "chores", "cleanup", "housekeeping"}},                            // 🧹
	{"\U0001f504", []string{"refactor", "refactoring", "tech debt", "debt"}},                          // 🔄
	{"\U0001f4da", []string{"documentation", "docs", "doc"}},                                          // 📚
	{"\U0001f9ea", []string{"test", "tests", "testing", "qa"}},                                        // 🧪
	{"\U0001f52c", []string{"spike", "research", "investigation", "discovery", "poc"}},                // 🔬
	{"\U0001f914", []string{"question", "questions", "discussion", "query"}},                          // 🤔
	{"\U0001f6a8", []string{"incident", "incidents", "outage", "sev"}},                                // 🚨
	{"\U0001f512", []string{"security", "vulnerability", "vuln", "cve"}},                              // 🔒
	{"\U0001f3c3", []string{"performance", "perf", "optimization", "latency"}},                        // 🏃
	{"\U0001f680", []string{"release", "releases", "deployment", "rollout"}},                          // 🚀
	{"\U0001f691", []string{"hotfix", "hotfixes", "patch", "urgent fix"}},                             // 🚑
	{"\U0001f3a8", []string{"design", "ux", "ui", "mockup"}},                                          // 🎨
	{"\U0001f3a7", []string{"support", "help", "customer"}},                                           // 🎧
	{"\U0001f4e6", []string{"dependencies", "dependency", "deps", "bump"}},                            // 📦
	{"\U0001f527", []string{"build", "ci", "cd", "ci cd", "pipeline", "infra"}},                       // 🔧
	{"\U0001f4a1", []string{"proposal", "proposals", "rfc", "idea", "suggestion"}},                    // 💡
}

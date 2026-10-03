package issue

import (
	"strings"
	"testing"

	"github.com/hdweiss/git-issue/internal/entity"
)

// ev builds an event whose bytes do not matter — the message grammar reads the
// parsed fields, never the line.
func ev(op string, val entity.Value, ref string) entity.Event {
	return entity.Event{V: entity.FormatVersion, C: 1, TS: 1, A: "a@example.com", Op: op, Val: val, Ref: ref}
}

func str(op, val string) entity.Event { return ev(op, entity.Str(val), "") }
func null(op string) entity.Event     { return ev(op, entity.Null(), "") }
func boolean(op string, b bool) entity.Event {
	return ev(op, entity.Bool(b), "")
}

func subjectOf(t *testing.T, a Action) string {
	t.Helper()
	return strings.SplitN(a.Message(), "\n", 2)[0]
}

// One phrase per op, in the words the issue vocabulary already uses. These are
// the lines `git log --oneline` shows, so they are worth pinning.
func TestSubjects(t *testing.T) {
	const title = "cat_sort_uniq reorders the blob"

	for _, tc := range []struct {
		name   string
		action Action
		want   string
	}{
		{"create", Action{Events: []entity.Event{
			str("create", "issue"), str("title", title), str("description", "body"),
		}}, `Create issue "cat_sort_uniq reorders the blob"`},

		{"comment", Action{Title: title, Events: []entity.Event{str("comment", "hi")}},
			`Comment on "cat_sort_uniq reorders the blob"`},

		// An edit and a retraction address an entry by id, so the count is
		// all a subject can honestly say about which comment moved.
		{"comment edit", Action{Title: title, Events: []entity.Event{
			ev("comment.edit", entity.Str("said better"), "be9e96b43964"),
		}}, `Edit a comment on "cat_sort_uniq reorders the blob"`},

		{"comment removal", Action{Title: title, Events: []entity.Event{
			ev("comment.remove", entity.Value{}, "be9e96b43964"),
		}}, `Retract a comment on "cat_sort_uniq reorders the blob"`},

		{"comment removals", Action{Title: title, Events: []entity.Event{
			ev("comment.remove", entity.Value{}, "be9e96b43964"),
			ev("comment.remove", entity.Value{}, "5c07be2d3199"),
		}}, `Retract 2 comments on "cat_sort_uniq reorders the blob"`},

		{"rename", Action{Title: title, Events: []entity.Event{str("title", "Something else")}},
			`Rename to "Something else"`},

		{"close", Action{Title: title, Events: []entity.Event{str("status", "closed")}},
			`Close "cat_sort_uniq reorders the blob"`},

		// The reason follows the issue, not the verb: "Close X as completed"
		// is the sentence, "Close as completed X" is not.
		{"close with reason", Action{Title: title, Events: []entity.Event{
			str("status", "closed"), str("status.reason", "completed"),
		}}, `Close "cat_sort_uniq reorders the blob" as completed`},

		{"reopen", Action{Title: title, Events: []entity.Event{str("status", "open")}},
			`Reopen "cat_sort_uniq reorders the blob"`},

		// docs/issues.md: a status this client does not know is not terminal,
		// so it must not be reported as an ending.
		{"unknown status", Action{Title: title, Events: []entity.Event{str("status", "triaged")}},
			`Set status "triaged" on "cat_sort_uniq reorders the blob"`},

		{"label", Action{Title: title, Events: []entity.Event{str("label.add", "bug")}},
			`Add label "bug" to "cat_sort_uniq reorders the blob"`},

		{"two labels", Action{Title: title, Events: []entity.Event{
			str("label.add", "bug"), str("label.add", "design"),
		}}, `Add labels "bug", "design" to "cat_sort_uniq reorders the blob"`},

		{"label removed", Action{Title: title, Removed: map[string]string{"abc": "bug"},
			Events: []entity.Event{ev("label.remove", entity.Value{}, "abc")}},
			`Remove label "bug" from "cat_sort_uniq reorders the blob"`},

		// A removal names the add it retracts, not a value. With no record of
		// what that add put there, the phrase counts rather than invents.
		{"label removed, value unknown", Action{Title: title,
			Events: []entity.Event{ev("label.remove", entity.Value{}, "abc")}},
			`Remove a label from "cat_sort_uniq reorders the blob"`},

		{"assign", Action{Title: title, Events: []entity.Event{str("assignee.add", "hdweiss")}},
			`Assign hdweiss to "cat_sort_uniq reorders the blob"`},

		{"unassign", Action{Title: title, Removed: map[string]string{"abc": "hdweiss"},
			Events: []entity.Event{ev("assignee.remove", entity.Value{}, "abc")}},
			`Unassign hdweiss from "cat_sort_uniq reorders the blob"`},

		{"type", Action{Title: title, Events: []entity.Event{str("type", "bug")}},
			`Set type "bug" on "cat_sort_uniq reorders the blob"`},

		{"type cleared", Action{Title: title, Events: []entity.Event{null("type")}},
			`Clear the type of "cat_sort_uniq reorders the blob"`},

		{"milestone", Action{Title: title, Events: []entity.Event{str("milestone", "v1")}},
			`Set milestone "v1" on "cat_sort_uniq reorders the blob"`},

		{"lock with reason", Action{Title: title, Events: []entity.Event{
			boolean("locked", true), str("lock.reason", "resolved"),
		}}, `Lock "cat_sort_uniq reorders the blob" as resolved`},

		{"unlock", Action{Title: title, Events: []entity.Event{boolean("locked", false)}},
			`Unlock "cat_sort_uniq reorders the blob"`},

		{"pin", Action{Title: title, Events: []entity.Event{boolean("pinned", true)}},
			`Pin "cat_sort_uniq reorders the blob"`},

		// Two clauses join into one sentence when they fit. When they do not,
		// the subject collapses instead — see TestBusyActionCollapses.
		{"two changes", Action{Title: "Short title", Events: []entity.Event{
			str("type", "bug"), str("label.add", "design"),
		}}, `Set type "bug" and add label "design" on "Short title"`},

		// Where an issue is filed reads as filing rather than as linking, and a
		// re-file is a remove and an add of which only the add is news.
		{"file under", Action{Title: title, Events: []entity.Event{
			ev(RelAdd, entity.Str(KindParent), "4b0755a3e7697bfdf17e42e9f4b307c161ea2a40"),
		}}, `File "cat_sort_uniq reorders the blob" under 4b0755a3e769`},

		{"re-file", Action{Title: title, Removed: map[string]string{"abc": "parent 885797fb2d9bc0c3da54cfec3815e66b4018a9ed"},
			Events: []entity.Event{
				ev(RelRemove, entity.Value{}, "abc"),
				ev(RelAdd, entity.Str(KindParent), "4b0755a3e7697bfdf17e42e9f4b307c161ea2a40"),
			}}, `File "cat_sort_uniq reorders the blob" under 4b0755a3e769`},

		{"detach", Action{Title: title, Removed: map[string]string{"abc": "parent 4b0755a3e7697bfdf17e42e9f4b307c161ea2a40"},
			Events: []entity.Event{ev(RelRemove, entity.Value{}, "abc")}},
			`Detach "cat_sort_uniq reorders the blob"`},

		// Every other kind reads in its own spelling, including one this client
		// has no opinion about.
		{"link", Action{Title: title, Events: []entity.Event{
			ev(RelAdd, entity.Str(KindBlockedBy), "4b0755a3e7697bfdf17e42e9f4b307c161ea2a40"),
		}}, `Link blocked-by 4b0755a3e769 to "cat_sort_uniq reorders the blob"`},

		{"unlink", Action{Title: title, Removed: map[string]string{"abc": "related 4b0755a3e7697bfdf17e42e9f4b307c161ea2a40"},
			Events: []entity.Event{ev(RelRemove, entity.Value{}, "abc")}},
			`Unlink related 4b0755a3e769 from "cat_sort_uniq reorders the blob"`},

		{"two links", Action{Title: "Short title", Events: []entity.Event{
			ev(RelAdd, entity.Str(KindBlockedBy), "4b0755a3e7697bfdf17e42e9f4b307c161ea2a40"),
			ev(RelAdd, entity.Str(KindRelated), "885797fb2d9bc0c3da54cfec3815e66b4018a9ed"),
		}}, `Link 2 relations to "Short title"`},

		// A removal names the add it retracts, so with no record of what that
		// add put there the phrase counts rather than invents.
		{"unlink, pair unknown", Action{Title: title,
			Events: []entity.Event{ev(RelRemove, entity.Value{}, "abc")}},
			`Unlink a relation from "cat_sort_uniq reorders the blob"`},

		// An op this client has never heard of still gets a commit. Refusing to
		// name it would be worse: the event would be written with no record of
		// who wrote it or when.
		{"unknown op", Action{Title: title, Events: []entity.Event{str("triage.priority", "high")}},
			`Write triage.priority on "cat_sort_uniq reorders the blob"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := subjectOf(t, tc.action); got != tc.want {
				t.Errorf("subject =\n %s\nwant\n %s", got, tc.want)
			}
		})
	}
}

// A busy edit has more to say than a subject can carry. Past the budget the
// subject names the issue and the clauses move into the body as a list.
func TestBusyActionCollapses(t *testing.T) {
	a := Action{
		ID:      "cf21c65dbeed8a27cbed92ce1e14bd6c735faf7f",
		Title:   "cat_sort_uniq reorders the blob",
		Removed: map[string]string{"abc": "merge"},
		Events: []entity.Event{
			str("title", "cat_sort_uniq permanently reorders every blob"),
			str("type", "bug"),
			str("label.add", "storage"),
			ev("label.remove", entity.Value{}, "abc"),
		},
	}
	msg := a.Message()
	subject, body, _ := strings.Cut(msg, "\n")

	if subject != `Update "cat_sort_uniq reorders the blob"` {
		t.Errorf("subject = %q", subject)
	}
	if n := len([]rune(subject)); n > subjectBudget {
		t.Errorf("subject is %d columns, over the %d budget", n, subjectBudget)
	}
	for _, want := range []string{
		`- rename to "cat_sort_uniq permanently reorders every blob"`,
		`- set type "bug"`,
		`- add label "storage"`,
		`- remove label "merge"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	// The subject already carries the old title, so repeating it would be
	// noise.
	if strings.Contains(body, "Previously:") {
		t.Errorf("collapsed message repeats the old title:\n%s", body)
	}
}

// The body is what makes plain `git log` readable: a comment's text, an
// issue's description, the name a rename replaced.
func TestBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action Action
		want   string
	}{
		{"create carries the description", Action{Events: []entity.Event{
			str("create", "issue"), str("title", "T"), str("description", "why it matters"),
		}}, "why it matters"},

		{"comment carries its text", Action{Title: "T",
			Events: []entity.Event{str("comment", "Reproduced on a fresh clone.")}},
			"Reproduced on a fresh clone."},

		// An edit's new text is prose the action introduced, and the only
		// place a plain `git log` can show it.
		{"comment edit carries its new text", Action{Title: "T",
			Events: []entity.Event{ev("comment.edit", entity.Str("Reproduced on 2.53 as well."), "be9e96b43964")}},
			"Reproduced on 2.53 as well."},

		{"rename carries the old name", Action{Title: "Old name",
			Events: []entity.Event{str("title", "New name")}},
			`Previously: "Old name"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.action.Message(), tc.want) {
				t.Errorf("message missing %q:\n%s", tc.want, tc.action.Message())
			}
		})
	}
}

// Every commit names the entity it wrote, and a bridge's trailers follow it.
// This is the only join from a commit back to the issue.
func TestTrailers(t *testing.T) {
	a := Action{
		ID:       "123616ff3a21be9597d958f67f1740b124f34451",
		Title:    "T",
		Events:   []entity.Event{str("comment", "hi")},
		Trailers: []Trailer{{Key: "Origin", Value: "https://example.com/1"}},
	}
	msg := a.Message()
	if !strings.HasSuffix(msg, "Issue: 123616ff3a21be9597d958f67f1740b124f34451\nOrigin: https://example.com/1\n") {
		t.Errorf("trailers not at the foot:\n%s", msg)
	}
	// A blank line before them, or git does not read them as trailers at all.
	if !strings.Contains(msg, "\n\nIssue: ") {
		t.Errorf("no blank line before the trailers:\n%s", msg)
	}
}

// A title carrying a newline would turn the rest of the subject into the body,
// and one carrying 300 characters would turn --oneline into a wall.
func TestTitlesAreTamed(t *testing.T) {
	a := Action{Title: "x", Events: []entity.Event{str("title", "one\ntwo\nthree")}}
	subject := subjectOf(t, a)
	if subject != `Rename to "one two three"` {
		t.Errorf("subject = %q", subject)
	}

	long := strings.Repeat("verylongword ", 40)
	a = Action{Title: long, Events: []entity.Event{str("comment", "hi")}}
	if n := len([]rune(subjectOf(t, a))); n > titleBudget+20 {
		t.Errorf("subject is %d columns for a %d-column title", n, len(long))
	}
}

package giteaissue

import (
	"testing"

	giteaapi "github.com/hdweiss/git-issue/internal/bridge/gitea/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

// seedRepo builds a small repository's worth of history through the fake's own
// mutations, so the import is tested against state a real Gitea would have
// produced rather than against hand-written JSON.
func seedRepo(t *testing.T) (*fakeGitea, []giteaapi.Issue) {
	t.Helper()
	f := newFakeGitea(t)
	c := f.client()
	tg := fakeTarget()

	// Issue 1: filed, then renamed, labelled, milestoned, commented, closed.
	one := f.createIssue(map[string]any{"title": "Original title", "body": "First body."})
	if err := c.EditIssue(tg, one.Number, edit(str2("Renamed once"), nil, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.AddLabels(tg, one.Number, []int64{f.labels["bug"], f.labels["storage"]}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveLabel(tg, one.Number, f.labels["storage"]); err != nil {
		t.Fatal(err)
	}
	mid := f.milestone["v1"]
	if err := c.EditIssue(tg, one.Number, giteaapi.EditIssueOption{Milestone: &mid}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddComment(tg, one.Number, "A comment."); err != nil {
		t.Fatal(err)
	}
	closed := "closed"
	if err := c.EditIssue(tg, one.Number, giteaapi.EditIssueOption{State: &closed}); err != nil {
		t.Fatal(err)
	}

	// Issue 2: current state carries a label and an assignee, but no timeline
	// entry names them — reconcile has to reconstruct them. updated_at is past
	// created_at, as it would be on a real server once anything was set.
	two := f.createIssue(map[string]any{"title": "Reconciled", "body": ""})
	two.Labels = []string{"confirmed"}
	two.Assignees = []string{"alice"}
	two.UpdatedAt = f.tick()

	issues, err := c.Fetch(tg, giteaapi.Filter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 {
		t.Fatalf("fetched %d issues, want 2", len(issues))
	}
	return f, issues
}

func edit(title *string, body *string, state *string, milestone *int64) giteaapi.EditIssueOption {
	return giteaapi.EditIssueOption{Title: title, Body: body, State: state, Milestone: milestone}
}

func str2(s string) *string { return &s }

func TestImportFoldsToExpectedState(t *testing.T) {
	_, issues := seedRepo(t)
	tg := fakeTarget()

	states := map[int64]entity.State{}
	for _, iss := range issues {
		e, err := Import(entity.SHA1, tg, iss, nil)
		if err != nil {
			t.Fatalf("import %d: %v", iss.Number, err)
		}
		states[iss.Number] = entity.Fold(vocab, e.Events())
	}

	one := states[1]
	if got := one.Scalar("title").Display(); got != "Renamed once" {
		t.Errorf("issue 1 title = %q", got)
	}
	if got := issue.Status(one); got != issue.StatusClosed {
		t.Errorf("issue 1 status = %q", got)
	}
	if got := one.Scalar("milestone").Display(); got != "v1" {
		t.Errorf("issue 1 milestone = %q", got)
	}
	assertSet(t, "issue 1 labels", one.List("label"), "bug")
	if len(one.Thread) != 1 {
		t.Errorf("issue 1 has %d comments, want 1", len(one.Thread))
	}

	two := states[2]
	if two.Scalar("status").Present {
		t.Error("an open issue should carry no status event")
	}
	if two.Scalar("description").Present {
		t.Error("issue 2 has an empty body and should carry no description event")
	}
	assertSet(t, "issue 2 labels", two.List("label"), "confirmed")
	assertSet(t, "issue 2 assignees", two.List("assignee"), "alice")
}

func TestImportIsSelfVerifying(t *testing.T) {
	_, issues := seedRepo(t)
	tg := fakeTarget()
	for _, iss := range issues {
		e, err := Import(entity.SHA1, tg, iss, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range e.Events() {
			id, err := entity.HashLine(entity.SHA1, ev.Raw)
			if err != nil {
				t.Fatal(err)
			}
			if id != ev.ID {
				t.Errorf("issue %d: event id %s is not the hash of its line %s", iss.Number, ev.ID, id)
			}
		}
		if e.Events()[0].Op != "create" || e.Events()[0].ID != e.ID {
			t.Errorf("issue %d: entity id is not the create event's id", iss.Number)
		}
	}
}

func TestImportIsIdempotent(t *testing.T) {
	_, issues := seedRepo(t)
	tg := fakeTarget()
	for _, iss := range issues {
		a, err := Import(entity.SHA1, tg, iss, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Import(entity.SHA1, tg, iss, nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(a.Blob()) != string(b.Blob()) {
			t.Errorf("issue %d differs on re-import\n--- first ---\n%s\n--- second ---\n%s", iss.Number, a.Blob(), b.Blob())
		}
		if a.ID != b.ID {
			t.Errorf("issue %d id changed between imports: %s then %s", iss.Number, a.ID, b.ID)
		}
	}
}

func assertSet(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", label, got, want)
		return
	}
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("%s = %v, missing %q", label, got, w)
		}
	}
}

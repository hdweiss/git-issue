package entity

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hdweiss/git-issue/internal/gitx"
)

func applyRepo(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "a@example.com"},
		{"config", "user.name", "A"},
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
	return NewStore(repo, "issues/open", Vocabulary{Scalars: []string{"title"}})
}

// act wraps events as one action, the unit Apply commits. The message and the
// author are what a commit says; neither reaches the blob.
func act(events ...Event) Action {
	return Action{
		Author:  gitx.Identity{Name: "A", Email: "a@example.com", When: time.Unix(events[0].TS, 0)},
		Message: "test action",
		Events:  events,
	}
}

// event builds a canonical event with a fixed nonce, so a test can rebuild the
// identical line and exercise the union's dedup.
func event(t *testing.T, c int64, op, val, nonce string) Event {
	t.Helper()
	e, err := NewEventWithNonce(SHA1, Event{
		V: FormatVersion, C: c, TS: c, A: "github:someone", Op: op, N: nonce, Val: Str(val),
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// Apply writes new entities, adds only what is missing on a second run, and
// reports each case the way a pull does.
func TestApplyUnionsAndConverges(t *testing.T) {
	s := applyRepo(t)

	create := event(t, 100, "create", "issue", "1111111111111111")
	title := event(t, 100, "title", "Imported", "2222222222222222")
	first := []Incoming{{ID: create.ID, Actions: []Action{act(create, title)}}}

	pull, err := s.Apply("test", first)
	if err != nil {
		t.Fatal(err)
	}
	if len(pull.Changes) != 1 || pull.Changes[0].Kind != Added {
		t.Fatalf("first apply reported %+v, want one Added", pull.Changes)
	}
	// One commit per action, and the count is reported — a caller decides
	// whether an import was big enough to be worth advising a repack after.
	if pull.Commits != 1 {
		t.Errorf("first apply wrote %d commits, want 1", pull.Commits)
	}
	if pull.Changes[0].ID != create.ID {
		t.Errorf("reported id %s, want %s", pull.Changes[0].ID, create.ID)
	}

	// The same events again must add nothing. This is the property the whole
	// nonce derivation exists for: without it a re-import would double every
	// entity instead of converging on it.
	pull, err = s.Apply("test", first)
	if err != nil {
		t.Fatal(err)
	}
	if !pull.UpToDate() || len(pull.Changes) != 0 {
		t.Errorf("re-apply moved the ref: %+v", pull.Changes)
	}
	// And wrote no commit at all. An action whose events are all present
	// already must not leave an empty one behind, or every re-import would
	// grow the ref by the size of the tracker.
	if pull.Commits != 0 {
		t.Errorf("re-apply wrote %d commits, want none", pull.Commits)
	}

	// One new event on a known entity is an update, and the existing lines
	// must survive it byte for byte.
	comment := event(t, 200, "comment", "a new comment", "3333333333333333")
	pull, err = s.Apply("test", []Incoming{{ID: create.ID, Actions: []Action{act(create, title), act(comment)}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pull.Changes) != 1 || pull.Changes[0].Kind != Updated {
		t.Fatalf("second apply reported %+v, want one Updated", pull.Changes)
	}
	// Two actions were offered and only the second carried anything new, so
	// only it becomes a commit.
	if pull.Commits != 1 {
		t.Errorf("second apply wrote %d commits, want 1", pull.Commits)
	}

	body := notesShow(t, s, create.ID)
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("blob has %d lines, want 3:\n%s", len(lines), body)
	}
	for i, want := range []Event{create, title, comment} {
		if lines[i] != string(want.Raw) {
			t.Errorf("line %d = %s, want %s", i, lines[i], want.Raw)
		}
	}

	// And it folds, through the same path a listing uses.
	st, err := s.Load(gitx.Note{Blob: s.Repo.RefSHA(s.FullRef()) + ":" + create.ID, Entity: create.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Scalar("title").Display(); got != "Imported" {
		t.Errorf("folded title = %q", got)
	}
	if len(st.Thread) != 1 {
		t.Errorf("folded %d comments, want 1", len(st.Thread))
	}
}

// Applying nothing is not an error and does not create a commit: a filtered
// import that matched no issues is an ordinary outcome.
func TestApplyNothing(t *testing.T) {
	s := applyRepo(t)
	pull, err := s.Apply("test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !pull.UpToDate() {
		t.Error("applying nothing moved the ref")
	}
	if sha := s.Repo.RefSHA(s.FullRef()); sha != "" {
		t.Errorf("applying nothing created the ref at %s", sha)
	}
}

func notesShow(t *testing.T, s *Store, id string) string {
	t.Helper()
	cmd := exec.Command("git", "notes", "--ref=issues/open", "show", id)
	cmd.Dir = s.Repo.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("notes show: %v\n%s", err, out)
	}
	return string(out)
}

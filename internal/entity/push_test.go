package entity

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/hdweiss/git-issue/internal/gitx"
)

// clonePair builds two repositories that share one remote, the way two people
// working from the same origin do. Testing a push against a real remote rather
// than a mock is deliberate: everything interesting here — what fast-forwards,
// what the tracking ref does, what git rejects — is git's behaviour, not ours.
func clonePair(t *testing.T) (a, b *Store) {
	t.Helper()

	origin := t.TempDir()
	run(t, origin, "init", "-q", "--bare")

	open := func(dir string) *Store {
		run(t, dir, "init", "-q")
		run(t, dir, "config", "user.email", "a@example.com")
		run(t, dir, "config", "user.name", "A")
		run(t, dir, "remote", "add", "origin", origin)
		repo, err := gitx.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		return NewStore(repo, "issues/open", Vocabulary{Scalars: []string{"title"}})
	}
	return open(t.TempDir()), open(t.TempDir())
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// file writes one entity and returns its id.
func file(t *testing.T, s *Store, c int64, nonce string) string {
	t.Helper()
	create := event(t, c, "create", "issue", nonce)
	title := event(t, c, "title", "Filed "+nonce, nonce+"t")
	if _, err := s.Apply("", []Incoming{{ID: create.ID, Actions: []Action{act(create, title)}}}); err != nil {
		t.Fatal(err)
	}
	return create.ID
}

// A push sends the ref, reports what the remote gained, and leaves the tracking
// ref where the remote now is.
func TestPushGit(t *testing.T) {
	a, b := clonePair(t)
	id := file(t, a, 100, "1111111111111111")

	push, err := a.PushGit("origin")
	if err != nil {
		t.Fatal(err)
	}
	if push.UpToDate() {
		t.Fatal("pushing a new entity reported up to date")
	}
	if len(push.Changes) != 1 || push.Changes[0].Kind != Added || push.Changes[0].ID != id {
		t.Fatalf("push reported %+v, want one Added for %s", push.Changes, id)
	}
	if got := a.Repo.RefSHA(a.TrackingRef("origin")); got != push.New {
		t.Errorf("tracking ref at %s, want %s", got, push.New)
	}

	// A second push has nothing to say, which is what the tracking-ref update
	// above buys: without it every push would re-report the whole tracker.
	again, err := a.PushGit("origin")
	if err != nil {
		t.Fatal(err)
	}
	if !again.UpToDate() || len(again.Changes) != 0 {
		t.Errorf("re-push reported %+v", again.Changes)
	}

	// And the other clone can read what was pushed.
	pull, err := b.PullGit("origin")
	if err != nil {
		t.Fatal(err)
	}
	if len(pull.Changes) != 1 || pull.Changes[0].ID != id {
		t.Errorf("the other clone pulled %+v", pull.Changes)
	}
}

// Pushing with nothing on the ref is an ordinary no-op, not an error: a
// repository with no issues has nothing to send.
func TestPushGitEmpty(t *testing.T) {
	a, _ := clonePair(t)
	push, err := a.PushGit("origin")
	if err != nil {
		t.Fatal(err)
	}
	if !push.UpToDate() {
		t.Errorf("pushing an empty tracker reported %+v", push.Changes)
	}
}

// A remote holding entities this clone has not seen is refused rather than
// overwritten. The blob is a grow-only set, so a force would silently drop
// whatever the other side had written.
func TestPushGitRefusesNonFastForward(t *testing.T) {
	a, b := clonePair(t)

	file(t, a, 100, "1111111111111111")
	if _, err := a.PushGit("origin"); err != nil {
		t.Fatal(err)
	}

	// b starts from what a pushed, then both sides write independently.
	if _, err := b.PullGit("origin"); err != nil {
		t.Fatal(err)
	}
	file(t, b, 200, "2222222222222222")
	if _, err := b.PushGit("origin"); err != nil {
		t.Fatal(err)
	}
	file(t, a, 300, "3333333333333333")

	_, err := a.PushGit("origin")
	if !errors.Is(err, ErrNonFastForward) {
		t.Fatalf("diverged push returned %v, want ErrNonFastForward", err)
	}

	// Pull, then push: the documented remedy, and it terminates.
	if _, err := a.PullGit("origin"); err != nil {
		t.Fatal(err)
	}
	push, err := a.PushGit("origin")
	if err != nil {
		t.Fatalf("push after pull still failed: %v", err)
	}
	if push.UpToDate() {
		t.Error("push after pull sent nothing")
	}

	// Both entities survive on the remote. This is the property the refusal
	// protects: a force would have taken one of them out.
	if _, err := b.PullGit("origin"); err != nil {
		t.Fatal(err)
	}
	notes, err := b.Notes()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 3 {
		t.Errorf("remote holds %d entities after the merge, want 3", len(notes))
	}
}

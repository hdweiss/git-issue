package origins

import (
	"os"
	"testing"
)

// The journal survives the process that wrote it, which is the whole point: a
// mapping earned and then lost costs a duplicate issue upstream that nothing
// undoes.
func TestJournalSurvivesAndCommits(t *testing.T) {
	s := testStore(t)

	j, err := s.OpenJournal(tracker)
	if err != nil {
		t.Fatal(err)
	}
	if !j.Empty() {
		t.Fatal("a fresh journal is not empty")
	}
	if err := j.Append(KindIssue, "4f2a1c9", "I_kwDOAbCdEf"); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(KindComment, "4f2a1c9", "8b31e07", "IC_kwDOxYzAbC"); err != nil {
		t.Fatal(err)
	}

	// A second open, as the next run after a crash would do, finds them.
	recovered, err := s.OpenJournal(tracker)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Lines()) != 2 {
		t.Fatalf("recovered %d lines, want 2", len(recovered.Lines()))
	}

	l := load(t, s, tracker)
	recovered.Into(l)
	save(t, s, l)
	recovered.Done()

	got := load(t, s, tracker)
	if up, ok := got.Upstream("4f2a1c9"); !ok || up != "I_kwDOAbCdEf" {
		t.Errorf("journal did not reach the ledger: %q %v", up, ok)
	}
	if c, ok := got.Comment("4f2a1c9", "8b31e07"); !ok || c != "IC_kwDOxYzAbC" {
		t.Errorf("comment mapping did not reach the ledger: %q %v", c, ok)
	}

	// Done removes the file, so the run after this one has nothing to replay.
	after, err := s.OpenJournal(tracker)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Empty() {
		t.Errorf("journal survived Done: %v", after.Lines())
	}
}

// Replaying a journal whose lines are already on the ref changes nothing. The
// recovery path has to be safe to run when the crash happened after the commit
// but before the file was removed.
func TestJournalReplayIsIdempotent(t *testing.T) {
	s := testStore(t)

	j, err := s.OpenJournal(tracker)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(KindIssue, "4f2a1c9", "I_kwDOAbCdEf"); err != nil {
		t.Fatal(err)
	}

	l := load(t, s, tracker)
	j.Into(l)
	save(t, s, l)
	before := s.Repo.RefSHA(s.Ref)

	replay := load(t, s, tracker)
	j.Into(replay)
	save(t, s, replay)
	if now := s.Repo.RefSHA(s.Ref); now != before {
		t.Errorf("replaying a committed journal moved the ref %s -> %s", before, now)
	}
	if replay.Len() != 1 {
		t.Errorf("replay duplicated a line: %d", replay.Len())
	}
}

// A corrupt journal degrades to an empty one. It is a crash-recovery aid, and
// refusing to push because it is malformed would be strictly worse than
// ignoring it — the same call the sync watermark makes.
func TestUnreadableJournalIsEmpty(t *testing.T) {
	s := testStore(t)
	j, err := s.OpenJournal(tracker)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(KindIssue, "aaa", "I_aaa"); err != nil {
		t.Fatal(err)
	}

	// A directory where the file should be is unreadable in the way that
	// matters: os.ReadFile fails and the journal must not.
	os.Remove(j.path)
	if err := os.MkdirAll(j.path, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := s.OpenJournal(tracker)
	if err != nil {
		t.Fatalf("an unreadable journal errored: %v", err)
	}
	if !got.Empty() {
		t.Errorf("an unreadable journal was not empty: %v", got.Lines())
	}
}

package origins

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/hdweiss/git-issue/internal/gitx"
)

const (
	tracker = "github.com/acme/git-issue"
	fork    = "github.com/hdweiss/git-issue"
)

func testStore(t *testing.T) *Store {
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
	return NewStore(repo)
}

func load(t *testing.T, s *Store, name string) *Ledger {
	t.Helper()
	l, err := s.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func save(t *testing.T, s *Store, l *Ledger) {
	t.Helper()
	if err := s.Save(l, "test\n"); err != nil {
		t.Fatal(err)
	}
}

// A ledger round-trips through the ref, and answers all three of the questions
// push and pull ask of it.
func TestRoundTrip(t *testing.T) {
	s := testStore(t)

	l := load(t, s, tracker)
	if l.Len() != 0 {
		t.Fatalf("a tracker never recorded loaded %d lines", l.Len())
	}
	l.Add(KindIssue, "4f2a1c9", "I_kwDOAbCdEf")
	l.Add(KindURL, "4f2a1c9", "https://github.com/acme/git-issue/issues/42")
	l.Add(KindComment, "4f2a1c9", "8b31e07", "IC_kwDOxYzAbC")
	save(t, s, l)

	got := load(t, s, tracker)
	if up, ok := got.Upstream("4f2a1c9"); !ok || up != "I_kwDOAbCdEf" {
		t.Errorf("Upstream = %q, %v", up, ok)
	}
	if id, ok := got.Entity("I_kwDOAbCdEf"); !ok || id != "4f2a1c9" {
		t.Errorf("Entity = %q, %v", id, ok)
	}
	if u, ok := got.URL("4f2a1c9"); !ok || u != "https://github.com/acme/git-issue/issues/42" {
		t.Errorf("URL = %q, %v", u, ok)
	}
	if c, ok := got.Comment("4f2a1c9", "8b31e07"); !ok || c != "IC_kwDOxYzAbC" {
		t.Errorf("Comment = %q, %v", c, ok)
	}
	if !got.ClaimedComment("IC_kwDOxYzAbC") {
		t.Error("ClaimedComment did not recognise a comment it holds")
	}
	if got.ClaimedComment("IC_somethingElse") {
		t.Error("ClaimedComment claimed a comment it does not hold")
	}
}

// Saving the same content twice must not move the ref. A push that earned no
// new mapping has nothing to say, and saying it anyway would be a commit per
// run for nothing.
func TestSaveIsIdempotent(t *testing.T) {
	s := testStore(t)
	l := load(t, s, tracker)
	l.Add(KindIssue, "4f2a1c9", "I_kwDOAbCdEf")
	save(t, s, l)

	first := s.Repo.RefSHA(s.Ref)
	if first == "" {
		t.Fatal("save did not create the ref")
	}
	save(t, s, load(t, s, tracker))
	if now := s.Repo.RefSHA(s.Ref); now != first {
		t.Errorf("re-saving identical content moved the ref %s -> %s", first, now)
	}
}

// A kind this client does not know is carried through untouched. That is what
// lets a later kind — an identity map — be added without a format change, and
// without an older client eating it.
func TestUnknownKindsSurvive(t *testing.T) {
	s := testStore(t)
	l := load(t, s, tracker)
	l.Add(KindIssue, "4f2a1c9", "I_kwDOAbCdEf")
	l.Add("user", "a@example.com", "alice")
	l.Add("something-else", "with", "four", "fields")
	save(t, s, l)

	body := string(load(t, s, tracker).Bytes())
	for _, want := range []string{
		"user a@example.com alice",
		"something-else with four fields",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("unknown line %q did not survive:\n%s", want, body)
		}
	}
}

// Destroying one entity type must take that type's mappings and no others. An
// issue mapping deleted here would be re-created upstream by the next issue
// push, as a second copy of an issue that is already there.
func TestForgetTakesOneTypesMappings(t *testing.T) {
	s := testStore(t)
	l := load(t, s, tracker)

	l.Add(KindIssue, "4f2a1c9", "I_kwDOAbCdEf")
	l.Add(KindURL, "4f2a1c9", "https://github.com/acme/git-issue/issues/7")
	l.Add(KindComment, "4f2a1c9", "e1", "IC_issue")

	l.Add(KindReview, "9b3d5e2", "PR_kwDOAbCdEf")
	l.Add(KindURL, "9b3d5e2", "https://github.com/acme/git-issue/pull/41")
	l.Add(KindComment, "9b3d5e2", "e2", "IC_review")
	l.Add(KindThread, "9b3d5e2", "e3", "PRRT_review")
	l.Add(KindVerdict, "9b3d5e2", "e4", "PRR_review")
	// A kind this build does not know, about the review being forgotten. Its
	// second field may not be an entity id at all, so it stays.
	l.Add("something-else", "9b3d5e2", "and", "more")

	if got := l.Forget(KindReview); got != 5 {
		t.Errorf("Forget removed %d lines, want 5", got)
	}

	body := string(l.Bytes())
	for _, want := range []string{
		"issue 4f2a1c9 I_kwDOAbCdEf",
		"url 4f2a1c9 https://github.com/acme/git-issue/issues/7",
		"comment 4f2a1c9 e1 IC_issue",
		"something-else 9b3d5e2 and more",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%q should have survived:\n%s", want, body)
		}
	}
	for _, gone := range []string{"PR_kwDOAbCdEf", "pull/41", "IC_review", "PRRT_review", "PRR_review"} {
		if strings.Contains(body, gone) {
			t.Errorf("%q should be gone:\n%s", gone, body)
		}
	}

	// The indexes go with the lines: a claim left in one of them would make the
	// next import skip a comment this clone no longer holds.
	if l.ClaimedComment("IC_review") {
		t.Error("a forgotten comment is still claimed")
	}
	if _, ok := l.ClaimedVerdict("PRR_review"); ok {
		t.Error("a forgotten verdict is still claimed")
	}
	if _, ok := l.Entity("PR_kwDOAbCdEf"); ok {
		t.Error("a forgotten review still resolves")
	}
	if !l.ClaimedComment("IC_issue") {
		t.Error("the issue's own claim went too")
	}

	// Forgetting what is not there changes nothing.
	if got := l.Forget(KindReview); got != 0 {
		t.Errorf("a second Forget removed %d lines, want 0", got)
	}
}

// One entity, two trackers: the fork case. Each tracker is a separate blob, so
// the mappings do not see each other.
func TestTrackersAreIndependent(t *testing.T) {
	s := testStore(t)

	up := load(t, s, fork)
	up.Add(KindIssue, "4f2a1c9", "I_upstream")
	save(t, s, up)

	down := load(t, s, tracker)
	down.Add(KindIssue, "4f2a1c9", "I_fork")
	save(t, s, down)

	if got, _ := load(t, s, fork).Upstream("4f2a1c9"); got != "I_upstream" {
		t.Errorf("upstream ledger says %q", got)
	}
	if got, _ := load(t, s, tracker).Upstream("4f2a1c9"); got != "I_fork" {
		t.Errorf("fork ledger says %q", got)
	}

	names, err := s.Trackers()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("Trackers() = %v, want both", names)
	}
}

// Dropping a tracker removes its mappings and leaves every other tracker
// alone — one commit, and no entity rewritten.
func TestDrop(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{fork, tracker} {
		l := load(t, s, name)
		l.Add(KindIssue, "4f2a1c9", "I_"+name)
		save(t, s, l)
	}

	if err := s.Drop(tracker, "unlink\n"); err != nil {
		t.Fatal(err)
	}
	if l := load(t, s, tracker); l.Len() != 0 {
		t.Errorf("dropped tracker still holds %d lines", l.Len())
	}
	if l := load(t, s, fork); l.Len() != 1 {
		t.Errorf("dropping one tracker disturbed another: %d lines", l.Len())
	}

	// Dropping a whole host takes every tracker under it.
	if err := s.Drop("github.com", "unlink host\n"); err != nil {
		t.Fatal(err)
	}
	names, err := s.Trackers()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("dropping the host left %v", names)
	}

	// And dropping what is not there says so rather than writing an empty
	// commit.
	if err := s.Drop("nowhere.example", "unlink\n"); err == nil {
		t.Error("dropping an unrecorded tracker succeeded")
	}
}

// Merge is a union of lines and is idempotent — the same properties the entity
// blobs' merge has, and for the same reason: every line is a standalone fact.
func TestMergeUnionsAndIsIdempotent(t *testing.T) {
	s := testStore(t)

	mine := load(t, s, tracker)
	mine.Add(KindIssue, "aaa", "I_aaa")
	save(t, s, mine)

	// A second ledger written on a side ref stands in for another clone's copy.
	other := "refs/git-issue/origins-other"
	side := &Store{Repo: s.Repo, Ref: other}
	theirs := load(t, side, tracker)
	theirs.Add(KindIssue, "bbb", "I_bbb")
	if err := side.Save(theirs, "theirs\n"); err != nil {
		t.Fatal(err)
	}

	if err := s.Merge(other); err != nil {
		t.Fatal(err)
	}
	merged := load(t, s, tracker)
	if merged.Len() != 2 {
		t.Fatalf("merge produced %d lines, want 2:\n%s", merged.Len(), merged.Bytes())
	}
	if _, ok := merged.Upstream("bbb"); !ok {
		t.Error("merge did not bring in the other side's mapping")
	}

	// The merge has to record the other side as a parent, or the result does
	// not contain their history and the next push is rejected for a conflict
	// that does not exist.
	if !s.Repo.IsAncestor(side.Repo.RefSHA(other), s.Repo.RefSHA(s.Ref)) {
		t.Error("merged ledger does not contain the other side's history; a push would be refused")
	}

	before := s.Repo.RefSHA(s.Ref)
	if err := s.Merge(other); err != nil {
		t.Fatal(err)
	}
	if now := s.Repo.RefSHA(s.Ref); now != before {
		t.Errorf("merging twice moved the ref %s -> %s", before, now)
	}
}

// Merging a side that already contains ours is a fast-forward: take their
// commit rather than build an identical merge on top of it.
func TestMergeFastForwards(t *testing.T) {
	s := testStore(t)
	other := "refs/git-issue/origins-other"

	l := load(t, s, tracker)
	l.Add(KindIssue, "aaa", "I_aaa")
	save(t, s, l)
	if err := s.Repo.UpdateRef(other, s.Repo.RefSHA(s.Ref)); err != nil {
		t.Fatal(err)
	}

	// Advance the side ref alone, so ours is strictly behind it.
	side := &Store{Repo: s.Repo, Ref: other}
	ahead := load(t, side, tracker)
	ahead.Add(KindIssue, "bbb", "I_bbb")
	if err := side.Save(ahead, "theirs\n"); err != nil {
		t.Fatal(err)
	}

	if err := s.Merge(other); err != nil {
		t.Fatal(err)
	}
	if got, want := s.Repo.RefSHA(s.Ref), s.Repo.RefSHA(other); got != want {
		t.Errorf("fast-forward left the ref at %s, want %s", got, want)
	}
	if merged := load(t, s, tracker); merged.Len() != 2 {
		t.Errorf("fast-forward produced %d lines, want 2", merged.Len())
	}
}

// Duplicate mappings are reported, not fatal. Both directions mean a duplicate
// was created somewhere, and both need a person rather than a resolution rule.
func TestDuplicateMappingsWarn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines [][]string
		want  string
	}{
		{
			name:  "one entity, two upstreams",
			lines: [][]string{{"aaa", "I_one"}, {"aaa", "I_two"}},
			want:  "mapped to both",
		},
		{
			name:  "one upstream, two entities",
			lines: [][]string{{"aaa", "I_one"}, {"bbb", "I_one"}},
			want:  "mapped from both",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLedger(tracker)
			for _, f := range tc.lines {
				l.Add(KindIssue, f...)
			}
			warnings := l.Warnings()
			if len(warnings) == 0 {
				t.Fatal("no warning for a duplicate mapping")
			}
			if !strings.Contains(warnings[0], tc.want) {
				t.Errorf("warning %q does not mention %q", warnings[0], tc.want)
			}
		})
	}
}

// The rendering is stable across runs. The lines are held in a map, and Go
// randomises map order, so an unsorted render would produce a different blob
// every time and a commit on every save.
func TestBytesAreStable(t *testing.T) {
	build := func() string {
		l := newLedger(tracker)
		for _, id := range []string{"ccc", "aaa", "bbb", "ddd", "eee"} {
			l.Add(KindIssue, id, "I_"+id)
			l.Add(KindURL, id, "https://example.test/"+id)
		}
		return string(l.Bytes())
	}
	first := build()
	for i := 0; i < 5; i++ {
		if got := build(); got != first {
			t.Fatalf("render %d differs:\n%s\n---\n%s", i, first, got)
		}
	}
}

// An empty ledger renders to nothing rather than to a blank line, so a tracker
// whose mappings were all removed does not leave a one-byte blob behind.
func TestEmptyRendersEmpty(t *testing.T) {
	if got := newLedger(tracker).Bytes(); len(got) != 0 {
		t.Errorf("empty ledger rendered %q", got)
	}
}

package cli

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hdweiss/git-issue/internal/gitx"
)

// gitIn runs git in a directory and returns its output, failing the test on
// error. A copy of cmd/git-issue's, because a test helper crossing a package
// boundary is not worth a package of its own.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// frames splits what progress wrote into the successive in-place redraws, so a
// test can assert on the last one the reader would have seen.
func frames(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\r")
}

func TestProgressPercentageWhenTotalHolds(t *testing.T) {
	var b strings.Builder
	p := Progress{W: &b, Noun: "issues"}
	p.Update(2, 5)
	p.Update(5, 5)
	p.Done()

	last := frames(b.String())
	if got := last[len(last)-1]; got != "Reading issues: 100% (5/5), done." {
		t.Errorf("final frame = %q", got)
	}
	if !strings.Contains(b.String(), "Reading issues: 40% (2/5)") {
		t.Errorf("missing the 40%% frame:\n%q", b.String())
	}
}

// A push sets Verb, so the bar reads "Pushing issues: …" while it writes rather
// than "Reading" — same shape, the word the rest of the command uses.
func TestProgressVerbLeadsTheLine(t *testing.T) {
	var b strings.Builder
	p := Progress{W: &b, Verb: "Pushing", Noun: "issues"}
	p.Update(0, 3)
	p.Update(3, 3)
	p.Done()

	last := frames(b.String())
	if got := last[len(last)-1]; got != "Pushing issues: 100% (3/3), done." {
		t.Errorf("final frame = %q", got)
	}
}

// An aborted run keeps its last real frame: the percentage and the count stay,
// with no ", done." and no collapse to a bare running total.
func TestProgressStopKeepsLastFrame(t *testing.T) {
	var b strings.Builder
	p := Progress{W: &b, Verb: "Pushing", Noun: "issues"}
	p.Update(213, 1480)
	p.Stop()

	last := frames(b.String())
	if got := strings.TrimRight(last[len(last)-1], " "); got != "Pushing issues: 14% (213/1480)" {
		t.Errorf("final frame = %q", got)
	}
	if strings.Contains(b.String(), "done.") {
		t.Errorf("Stop() wrote a done marker:\n%q", b.String())
	}
}

func TestProgressPlainCountWithoutTotal(t *testing.T) {
	var b strings.Builder
	p := Progress{W: &b, Noun: "issues"}
	p.Update(25, 0)
	p.Update(50, 0)
	p.Done()

	last := frames(b.String())
	if got := last[len(last)-1]; got != "Reading issues: 50, done." {
		t.Errorf("final frame = %q", got)
	}
}

// A totalCount the run blows past — a filter the server did not apply to it —
// drops back to the plain running count rather than showing over 100%.
func TestProgressAbandonsTotalOncePassed(t *testing.T) {
	var b strings.Builder
	p := Progress{W: &b, Noun: "issues"}
	p.Update(2, 3)
	p.Update(6, 3)
	p.Done()

	last := frames(b.String())
	if got := strings.TrimRight(last[len(last)-1], " "); got != "Reading issues: 6, done." {
		t.Errorf("final frame = %q", got)
	}
}

// A totalCount the run never reaches — again a filter applied to the count but
// not the connection — is not what the closing line reports.
func TestProgressPlainWhenTotalNotReached(t *testing.T) {
	var b strings.Builder
	p := Progress{W: &b, Noun: "issues"}
	p.Update(3, 100)
	p.Done()

	last := frames(b.String())
	if got := strings.TrimRight(last[len(last)-1], " "); got != "Reading issues: 3, done." {
		t.Errorf("final frame = %q", got)
	}
}

// A shorter redraw clears the tail of the longer line it replaces, so no
// fragment of "(2/3)" survives when the line drops to a plain count.
func TestProgressRedrawClearsLongerLine(t *testing.T) {
	var b strings.Builder
	p := Progress{W: &b, Noun: "issues"}
	p.Update(2, 3)
	p.Update(400, 3)

	fr := frames(b.String())
	plain := strings.TrimRight(fr[len(fr)-1], " ")
	if plain != "Reading issues: 400" {
		t.Errorf("redraw left a fragment: %q", fr[len(fr)-1])
	}
	if len(fr[len(fr)-1]) < len("Reading issues: 40% (2/3)") {
		t.Errorf("redraw did not pad over the previous line: %q", fr[len(fr)-1])
	}
}

// Nothing drawn, nothing to close: a fetch that never called back leaves no
// stray newline.
func TestProgressSilentWhenNeverUpdated(t *testing.T) {
	var b strings.Builder
	p := Progress{W: &b, Noun: "issues"}
	p.Done()
	if b.String() != "" {
		t.Errorf("done() wrote %q with no updates", b.String())
	}
}

// The automatic repack runs only after a bulk import. An ordinary pull writes a
// handful of commits, and repacking a repository on every one of them would
// cost far more than the reading speed it buys back.
func TestRepackThreshold(t *testing.T) {
	repo := repackRepo(t)
	for _, tc := range []struct {
		commits int
		gcAuto  string
		want    bool
	}{
		{0, "", false},
		{1, "", false},
		{AutoRepackAt - 1, "", false},
		{AutoRepackAt, "", true},
		{20000, "", true},
		// Someone who has told git never to maintain this repository on its own
		// has already said the thing that matters.
		{20000, "0", false},
		{20000, "6700", true},
	} {
		gitIn(t, repo.Dir, "config", "gc.auto", orDefault(tc.gcAuto, "6700"))
		var b strings.Builder
		Repack(repo, tc.commits, &b)
		if got := b.Len() > 0; got != tc.want {
			t.Errorf("%d commits, gc.auto=%q: repacked=%v, want %v\n%s",
				tc.commits, tc.gcAuto, got, tc.want, b.String())
		}
		if tc.want && !strings.Contains(b.String(), strconv.Itoa(tc.commits)) {
			t.Errorf("%d commits: the notice does not say how many:\n%s", tc.commits, b.String())
		}
	}
}

// And when it runs it really repacks: one pack, every object still readable.
// A typo in the flags would otherwise only show up on somebody's real import.
func TestRepackRuns(t *testing.T) {
	repo := repackRepo(t)
	before := packCount(t, repo.Dir)

	var b strings.Builder
	Repack(repo, AutoRepackAt, &b)
	if !strings.Contains(b.String(), "Repacking") {
		t.Fatalf("no repack notice:\n%s", b.String())
	}

	if after := packCount(t, repo.Dir); after != 1 {
		t.Errorf("%d packs after the repack, want 1 (was %d)", after, before)
	}
	// The note survived it, which is the only thing a repack must never break.
	if got := gitIn(t, repo.Dir, "notes", "--ref=issues/open", "show", repackNote); !strings.Contains(got, "an event line") {
		t.Errorf("note unreadable after the repack: %q", got)
	}
}

const repackNote = "b8c19884bc845e84f22ae2bd298417e1399a937b"

// repackRepo is a repository with a note in it and loose objects to pack.
func repackRepo(t *testing.T) *gitx.Repo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	gitIn(t, t.TempDir(), "init", "-q", dir)
	gitIn(t, dir, "config", "user.email", "a@example.com")
	gitIn(t, dir, "config", "user.name", "A")
	gitIn(t, dir, "notes", "--ref=issues/open", "add", "-m", "an event line", repackNote)

	repo, err := gitx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func packCount(t *testing.T, dir string) int {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	return len(packs)
}

// orDefault keeps the table readable: a blank case means "leave gc.auto at
// git's own default" rather than "unset it".
func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

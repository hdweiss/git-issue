package main

import (
	"os"
	"strings"
	"testing"
)

// commentOn posts a comment and returns the entry id the command printed,
// which is the id everything else addresses it by.
func commentOn(t *testing.T, bin, dir, id string, args ...string) string {
	t.Helper()
	got := gitIssue(t, bin, dir, append([]string{"comment", id}, args...)...)
	if got.code != 0 {
		t.Fatalf("comment: exit %d: %s", got.code, got.stderr)
	}
	return strings.TrimSpace(got.stdout)
}

// The whole life of a comment: posted with -m, addressed afterwards by an
// abbreviated prefix of its own id, rewritten, and retracted.
func TestCommentLifecycle(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Anchor blobs are pruned by gc").stdout)

	entry := commentOn(t, bin, dir, id, "-m", "Confirmed on 2.42 and on 2.53.")
	other := commentOn(t, bin, dir, id, "-m", "A second entry.")
	if entry == "" || entry == other {
		t.Fatalf("comment ids are %q and %q", entry, other)
	}

	// The id the command printed is an abbreviation of the id show prints, so
	// what was reported can be pasted straight back.
	shown := gitIssue(t, bin, dir, id).stdout
	if !strings.Contains(shown, "comment "+entry) {
		t.Errorf("show does not carry the reported id %s\n%s", entry, shown)
	}
	if !strings.Contains(shown, "Confirmed on 2.42 and on 2.53.") {
		t.Errorf("show does not carry the comment\n%s", shown)
	}

	// A four-character prefix is enough, because the prefix is resolved
	// inside the issue and competes only with that issue's own comments.
	edited := gitIssue(t, bin, dir, "edit", id[:4], entry[:4], "-m", "Confirmed on 2.42, 2.53 and on next.")
	if edited.code != 0 {
		t.Fatalf("edit: exit %d: %s", edited.code, edited.stderr)
	}
	if !strings.Contains(edited.stdout, "edited "+entry) {
		t.Errorf("edit did not report the entry it rewrote\n%s", edited.stdout)
	}

	// An edit addresses the entry by id, so the id does not move when the
	// body does — which is what makes it safe to have written down.
	shown = gitIssue(t, bin, dir, id).stdout
	if !strings.Contains(shown, "comment "+entry) {
		t.Errorf("the entry's id changed under an edit\n%s", shown)
	}
	if !strings.Contains(shown, "Confirmed on 2.42, 2.53 and on next.") || strings.Contains(shown, "and on 2.53.\n") {
		t.Errorf("the edit did not take\n%s", shown)
	}

	// An edit that says what the entry already says writes nothing, the way
	// an issue edit that changes nothing does.
	again := gitIssue(t, bin, dir, "edit", id, entry, "-m", "Confirmed on 2.42, 2.53 and on next.")
	if again.code != 0 || !strings.Contains(again.stdout, "nothing changed") {
		t.Errorf("a no-op edit reported %q (exit %d)", again.stdout, again.code)
	}

	// A retraction hides the body and keeps the entry, so it converges where
	// a missing tree entry would not.
	removed := gitIssue(t, bin, dir, "remove", id, other[:4])
	if removed.code != 0 {
		t.Fatalf("remove: exit %d: %s", removed.code, removed.stderr)
	}
	if !strings.Contains(removed.stdout, "retracted "+other) {
		t.Errorf("remove did not report the entry it retracted\n%s", removed.stdout)
	}

	shown = gitIssue(t, bin, dir, id).stdout
	if !strings.Contains(shown, "comment "+other) || !strings.Contains(shown, "(comment retracted)") {
		t.Errorf("the retracted entry is not shown as retracted\n%s", shown)
	}
	if strings.Contains(shown, "A second entry.") {
		t.Errorf("the retracted body is still displayed\n%s", shown)
	}
	if second := gitIssue(t, bin, dir, "remove", id, other); second.code != 0 ||
		!strings.Contains(second.stdout, "already retracted") {
		t.Errorf("retracting twice reported %q (exit %d)", second.stdout, second.code)
	}

	// The issue itself is untouched by any of it: remove with two arguments
	// is a different operation from remove with one.
	if listed := gitIssue(t, bin, dir, "list").stdout; !strings.Contains(listed, "Anchor blobs are pruned by gc") {
		t.Errorf("retracting a comment unlisted the issue\n%s", listed)
	}
}

// -i nests a reply under the entry it names, the way show already renders a
// forest for whatever arrives with a parent.
func TestCommentReply(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Nest replies under their parent").stdout)

	root := commentOn(t, bin, dir, id, "-m", "A root entry.")
	reply := commentOn(t, bin, dir, id, "-m", "A reply.", "-i", root[:4])
	if reply == "" || reply == root {
		t.Fatalf("comment ids are %q and %q", root, reply)
	}

	// The reply is indented one level under the root it names, the same
	// indent WriteComment gives any other reply.
	shown := gitIssue(t, bin, dir, id).stdout
	if !strings.Contains(shown, "\n    comment "+reply) {
		t.Errorf("the reply is not nested under its parent\n%s", shown)
	}

	// An unrelated entry stays at the root, unaffected by the reply beside it.
	other := commentOn(t, bin, dir, id, "-m", "An unrelated entry.")
	shown = gitIssue(t, bin, dir, id).stdout
	if !strings.Contains(shown, "\ncomment "+other) {
		t.Errorf("an unrelated comment was nested\n%s", shown)
	}

	// An unknown reply target is refused the same way an unknown comment is
	// anywhere else.
	if got := gitIssue(t, bin, dir, "comment", id, "-m", "text", "-i", "ffffffff"); got.code == 0 ||
		!strings.Contains(got.stderr, "unknown comment") {
		t.Errorf("replying to an unknown comment reported %q (exit %d)", got.stderr, got.code)
	}
}

// Every write lands on the notes ref as its own commit, saying what somebody
// did in the words the vocabulary already uses.
func TestCommentLog(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Reject short ids").stdout)

	entry := commentOn(t, bin, dir, id, "-m", "git notes refuses them.")
	gitIssue(t, bin, dir, "edit", id, entry, "-m", "git notes refuses them, and so should we.")
	gitIssue(t, bin, dir, "remove", id, entry)

	log := gitIssue(t, bin, dir, "log", "--oneline").stdout
	for _, want := range []string{
		`Comment on "Reject short ids"`,
		`Edit a comment on "Reject short ids"`,
		`Retract a comment on "Reject short ids"`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log is missing %q\n%s", want, log)
		}
	}

	// A comment's text is the body of its own commit, which is what makes a
	// plain `git log` readable as the tracker.
	if full := gitIssue(t, bin, dir, "log").stdout; !strings.Contains(full, "git notes refuses them, and so should we.") {
		t.Errorf("the edit's new text is not in the log body\n%s", full)
	}
}

// Without -m an editor opens, the way git commit does without one. Editing an
// existing comment opens on what it currently says.
func TestCommentEditor(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Colour the status").stdout)

	env, opened := fakeEditor(t, "Written in the editor.")
	posted := gitIssueEnv(t, bin, dir, []string{env}, "comment", id)
	if posted.code != 0 {
		t.Fatalf("comment: exit %d: %s", posted.code, posted.stderr)
	}
	entry := strings.TrimSpace(posted.stdout)

	// A new comment opens on an empty buffer: everything in it is the ignored
	// block, so nothing is left once that is stripped.
	buffer, err := os.ReadFile(opened)
	if err != nil {
		t.Fatal(err)
	}
	if left := stripComments(string(buffer)); left != "" {
		t.Errorf("a new comment's buffer is not empty: %q", left)
	}
	if !strings.Contains(string(buffer), "Colour the status") {
		t.Errorf("the buffer does not name the issue being commented on\n%s", buffer)
	}
	if shown := gitIssue(t, bin, dir, id).stdout; !strings.Contains(shown, "Written in the editor.") {
		t.Errorf("the edited body did not reach the issue\n%s", shown)
	}

	// An edit opens on the entry as it stands, so the editor amends rather
	// than starting from nothing.
	env, opened = fakeEditor(t, "Rewritten in the editor.")
	if got := gitIssueEnv(t, bin, dir, []string{env}, "edit", id, entry); got.code != 0 {
		t.Fatalf("edit: exit %d: %s", got.code, got.stderr)
	}
	if buffer, err = os.ReadFile(opened); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buffer), "Written in the editor.") {
		t.Errorf("the editor did not open on the current body\n%s", buffer)
	}
	if shown := gitIssue(t, bin, dir, id).stdout; !strings.Contains(shown, "Rewritten in the editor.") {
		t.Errorf("the rewritten body did not reach the issue\n%s", shown)
	}

	// An empty buffer aborts and writes nothing, the way an empty title
	// aborts an issue.
	env, _ = fakeEditor(t, "")
	aborted := gitIssueEnv(t, bin, dir, []string{env}, "comment", id)
	if aborted.code == 0 || !strings.Contains(aborted.stderr, "the comment is empty") {
		t.Errorf("an empty buffer reported %q (exit %d)", aborted.stderr, aborted.code)
	}
}

// The ways of getting it wrong, each refused with what to do instead.
func TestCommentRefusals(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	id := strings.TrimSpace(gitIssue(t, bin, dir, "add", "-t", "Reject short ids").stdout)
	entry := commentOn(t, bin, dir, id, "-m", "A comment.")

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		// -m with nothing in it is somebody saying the comment is empty, not
		// somebody asking for an editor.
		{"empty message", []string{"comment", id, "-m", ""}, "a comment needs a body"},
		{"empty edit", []string{"edit", id, entry, "-m", ""}, "a comment needs a body"},

		// -m has no meaning for an issue: it would have to say which of six
		// fields it was setting.
		{"message without a comment", []string{"edit", id, "-m", "text"}, "name the comment to rewrite"},

		{"unknown comment", []string{"edit", id, "ffffffff", "-m", "text"}, "unknown comment"},
		{"no comment", []string{"edit", id, "", "-m", "text"}, "no comment given"},
		{"third argument", []string{"remove", id, entry, "extra"}, "at most one comment"},
		{"comment on nothing", []string{"comment", "ffffffff", "-m", "text"}, "unknown issue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := gitIssue(t, bin, dir, tc.args...)
			if got.code == 0 {
				t.Fatalf("succeeded: %s", got.stdout)
			}
			if !strings.Contains(got.stderr, tc.want) {
				t.Errorf("stderr is %q, want %q", got.stderr, tc.want)
			}
		})
	}

	// A retracted entry has no text to rewrite, and saying so beats writing an
	// edit that nothing will ever display.
	gitIssue(t, bin, dir, "remove", id, entry)
	if got := gitIssue(t, bin, dir, "edit", id, entry, "-m", "text"); got.code == 0 ||
		!strings.Contains(got.stderr, "is retracted") {
		t.Errorf("editing a retracted comment reported %q (exit %d)", got.stderr, got.code)
	}

	// With no -m and no editor to open there is nothing to do but say so.
	noEd := gitIssueEnv(t, bin, dir, noEditor, "comment", id)
	if noEd.code == 0 || !strings.Contains(noEd.stderr, "no editor to open") {
		t.Errorf("commenting with no editor reported %q (exit %d)", noEd.stderr, noEd.code)
	}
}

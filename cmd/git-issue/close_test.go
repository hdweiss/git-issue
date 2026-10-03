package main

import (
	"regexp"
	"strings"
	"testing"
)

// squeeze collapses runs of spaces so a header assertion does not depend on the
// column width, which is set by the widest key on the issue.
func squeeze(s string) string { return regexp.MustCompile(` +`).ReplaceAllString(s, " ") }

// add files an issue in dir and returns its abbreviated id.
func add(t *testing.T, bin, dir, title string) string {
	t.Helper()
	r := gitIssue(t, bin, dir, "add", "-t", title)
	if r.code != 0 {
		t.Fatalf("add %q: %s", title, r.stderr)
	}
	return strings.TrimSpace(r.stdout)
}

// close and reopen are one status event each, resolved last-write-wins, and
// idempotent — a close of a closed issue writes nothing.
func TestCloseReopen(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := add(t, bin, dir, "Anchor blobs are pruned by gc")

	got := gitIssue(t, bin, dir, "close", id)
	if got.code != 0 {
		t.Fatalf("close: %s", got.stderr)
	}
	if !strings.Contains(got.stdout, "closed "+id) || !strings.Contains(got.stdout, "  closed  ") {
		t.Errorf("close row:\n%s", got.stdout)
	}
	if show := squeeze(gitIssue(t, bin, dir, "show", id).stdout); !strings.Contains(show, "Status: closed") {
		t.Errorf("not closed after close:\n%s", show)
	}

	// A bare close writes no reason.
	if show := squeeze(gitIssue(t, bin, dir, "show", id).stdout); strings.Contains(show, "Reason:") {
		t.Errorf("bare close wrote a reason:\n%s", show)
	}

	// Idempotent.
	if again := gitIssue(t, bin, dir, "close", id); again.code != 0 || strings.TrimSpace(again.stdout) != "nothing changed" {
		t.Errorf("second close: exit %d, stdout %q", again.code, again.stdout)
	}

	reopened := gitIssue(t, bin, dir, "reopen", id)
	if reopened.code != 0 || !strings.Contains(reopened.stdout, "reopened "+id) {
		t.Errorf("reopen: exit %d\n%s", reopened.code, reopened.stdout)
	}
	if show := squeeze(gitIssue(t, bin, dir, "show", id).stdout); !strings.Contains(show, "Status: open") {
		t.Errorf("not open after reopen:\n%s", show)
	}
	if again := gitIssue(t, bin, dir, "reopen", id); strings.TrimSpace(again.stdout) != "nothing changed" {
		t.Errorf("second reopen: %q", again.stdout)
	}
}

// --as writes status.reason as a separate scalar, the hyphen normalises to the
// blob's spelling, and reopen leaves the reason where it is — the stale pairing
// docs/issues.md accepts. show hides it while the status is non-terminal.
func TestCloseReason(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := add(t, bin, dir, "Out of scope for now")

	if r := gitIssue(t, bin, dir, "close", id, "--as", "not-planned"); r.code != 0 {
		t.Fatalf("close --as: %s", r.stderr)
	}
	show := squeeze(gitIssue(t, bin, dir, "show", id).stdout)
	if !strings.Contains(show, "Status: closed") || !strings.Contains(show, "Reason: not_planned") {
		t.Errorf("close --as not-planned:\n%s", show)
	}

	// Reopen keeps the reason in the blob but hides it from a reader.
	gitIssue(t, bin, dir, "reopen", id)
	show = squeeze(gitIssue(t, bin, dir, "show", id).stdout)
	if !strings.Contains(show, "Status: open") {
		t.Fatalf("not reopened:\n%s", show)
	}
	if strings.Contains(show, "Reason:") {
		t.Errorf("reason shown while open:\n%s", show)
	}
	if blob := gitIn(t, dir, "log", "-p", "refs/notes/issues/open"); !strings.Contains(blob, "not_planned") {
		t.Errorf("reopen erased the reason from the blob")
	}

	// An unknown reason is refused, not written.
	if r := gitIssue(t, bin, dir, "close", id, "--as", "bogus"); r.code == 0 {
		t.Errorf("close --as bogus was accepted")
	}
}

// close --as duplicate <of> writes the status, the reason and a duplicate-of
// link in one action; naming the id without --as duplicate is refused.
func TestCloseAsDuplicate(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	dup := add(t, bin, dir, "Same as the fanout question")
	canon := add(t, bin, dir, "The fanout question")

	if r := gitIssue(t, bin, dir, "close", dup, "--as", "duplicate", canon); r.code != 0 {
		t.Fatalf("close --as duplicate: %s", r.stderr)
	}
	show := squeeze(gitIssue(t, bin, dir, "show", dup).stdout)
	for _, want := range []string{"Status: closed", "Reason: duplicate", "Duplicate of: " + canon} {
		if !strings.Contains(show, want) {
			t.Errorf("missing %q:\n%s", want, show)
		}
	}

	// The far end derives the inverse.
	if other := squeeze(gitIssue(t, bin, dir, "show", canon).stdout); !strings.Contains(other, "Duplicated by: "+dup) {
		t.Errorf("no derived inverse:\n%s", other)
	}

	// A second id without --as duplicate is refused rather than taken to mean
	// something else.
	if r := gitIssue(t, bin, dir, "close", dup, canon); r.code == 0 {
		t.Errorf("close <id> <id> without --as duplicate was accepted")
	}
	// An issue cannot duplicate itself.
	if r := gitIssue(t, bin, dir, "close", dup, "--as", "duplicate", dup); r.code == 0 {
		t.Errorf("close --as duplicate self was accepted")
	}
}

// edit --status writes a raw value a bridge uses; a client that knows the Azure
// DevOps states dots Done closed and finds it with --state closed.
func TestEditStatusRaw(t *testing.T) {
	bin, dir := build(t), emptyRepo(t)
	id := add(t, bin, dir, "Fixed, awaiting verification")

	if r := gitIssue(t, bin, dir, "edit", id, "--status", "Resolved"); r.code != 0 {
		t.Fatalf("edit --status: %s", r.stderr)
	}
	if show := squeeze(gitIssue(t, bin, dir, "show", id).stdout); !strings.Contains(show, "Status: Resolved") {
		t.Errorf("status not set:\n%s", show)
	}
	// Resolved is one of the non-terminal ADO states.
	if l := gitIssue(t, bin, dir, "list", "--state", "open").stdout; !strings.Contains(l, id) {
		t.Errorf("Resolved did not count as open:\n%s", l)
	}

	gitIssue(t, bin, dir, "edit", id, "--status", "Done")
	if l := gitIssue(t, bin, dir, "list", "--state", "closed").stdout; !strings.Contains(l, id) {
		t.Errorf("Done did not count as closed:\n%s", l)
	}

	// --status has no cleared state.
	if r := gitIssue(t, bin, dir, "edit", id, "--status", "none"); r.code == 0 {
		t.Errorf("--status none was accepted")
	}
}

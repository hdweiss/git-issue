package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hdweiss/git-issue/internal/issue"
)

// fakeEditor writes a script that replaces the buffer with body, and returns
// the GIT_EDITOR setting that runs it. It also keeps a copy of what it was
// handed, so a test can assert on what the editor was asked to open.
func fakeEditor(t *testing.T, body string) (env string, opened string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "editor.sh")
	opened = filepath.Join(dir, "opened")

	content := "#!/bin/sh\ncat \"$1\" > " + opened + "\n"
	if body != "" {
		content += "cat > \"$1\" <<'END_OF_BUFFER'\n" + body + "\nEND_OF_BUFFER\n"
	} else {
		content += ": > \"$1\"\n"
	}
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return "GIT_EDITOR=" + script, opened
}

// Every field an issue can be born with, given on the command line, reaches
// the rendered issue.
func TestAddFields(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	added := gitIssue(t, bin, dir, "add",
		"-t", "Reject short ids", "-d", "line one\nline two",
		"--type", "bug", "-l", "design,storage", "-l", "ui",
		"-a", "alice@example.com", "-m", "v1")
	if added.code != 0 {
		t.Fatalf("add failed: %s", added.stderr)
	}

	shown := gitIssue(t, bin, dir, strings.TrimSpace(added.stdout)).stdout
	for _, want := range []string{
		"Reject short ids", "line one", "line two",
		"Type:      bug", "Labels:    design, storage, ui",
		"Assignees: alice@example.com", "Milestone: v1",
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("show is missing %q\n%s", want, shown)
		}
	}
}

// The long and short spellings are the same flag, and flags may follow as
// readily as precede — nobody types arguments in a fixed order.
func TestAddFlagSpellings(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	for _, args := range [][]string{
		{"add", "--title", "Spellings", "--description", "body"},
		{"add", "-t", "Spellings", "-d", "body"},
		{"add", "-t=Spellings", "-d=body"},
	} {
		got := gitIssue(t, bin, dir, args...)
		if got.code != 0 {
			t.Fatalf("%v: exit %d: %s", args, got.code, got.stderr)
		}
	}
}

// A bare word used to be the title. It is refused rather than quietly taken as
// something else, and the message says what to type instead.
func TestAddRefusesPositional(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	got := gitIssueEnv(t, bin, dir, noEditor, "add", "a bare title")
	if got.code != 1 || !strings.Contains(got.stderr, "--title 'a bare title'") {
		t.Errorf("exit %d, stderr %q", got.code, got.stderr)
	}
}

// No title is not enough to write an issue, so the editor opens. The title and
// description come from the buffer; every other field is carried in from the
// flags and echoed, read-only, in the ignored block.
func TestAddOpensEditorWithoutTitle(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	env, opened := fakeEditor(t, `Written in the editor

The body, which contains a colon: right here, and a # heading too.`)

	added := gitIssueEnv(t, bin, dir, []string{env}, "add",
		"-l", "ui,design", "-a", "bob@example.com", "--type", "feature", "-m", "v2", "-d", "prefilled")
	if added.code != 0 {
		t.Fatalf("add failed: %s", added.stderr)
	}

	// What the editor was handed: the given description prefilled, and the
	// flags' fields echoed inside the ignored block.
	buffer, err := os.ReadFile(opened)
	if err != nil {
		t.Fatal(err)
	}
	editable, block, found := strings.Cut(string(buffer), "<!---")
	if !found || !strings.Contains(block, "-->") {
		t.Fatalf("no ignored block in the buffer\n%s", buffer)
	}
	if !strings.Contains(editable, "prefilled") {
		t.Errorf("the given description was not prefilled\n%s", buffer)
	}
	for _, want := range []string{"feature", "ui, design", "bob@example.com", "v2"} {
		if !strings.Contains(block, want) {
			t.Errorf("ignored block is missing the echo of %q\n%s", want, block)
		}
	}

	shown := gitIssue(t, bin, dir, strings.TrimSpace(added.stdout)).stdout
	for _, want := range []string{
		"Written in the editor", "Type:      feature", "Labels:    design, ui",
		"Assignees: bob@example.com", "Milestone: v2", "colon: right here", "# heading too",
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("show is missing %q\n%s", want, shown)
		}
	}
	// The description given on the command line was replaced by the edit, not
	// appended to it.
	if strings.Contains(shown, "prefilled") {
		t.Errorf("the edited description did not replace the given one\n%s", shown)
	}
}

// A title is enough, so no editor opens — unless --edit asks for one.
func TestAddEditFlag(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	env, opened := fakeEditor(t, "Edited after all\n\nbody")
	if got := gitIssueEnv(t, bin, dir, []string{env}, "add", "-t", "No editor wanted"); got.code != 0 {
		t.Fatalf("add failed: %s", got.stderr)
	}
	if _, err := os.Stat(opened); err == nil {
		t.Error("an editor opened for an add that had a title")
	}

	added := gitIssueEnv(t, bin, dir, []string{env}, "add", "-t", "Editor wanted", "--edit")
	if added.code != 0 {
		t.Fatalf("add --edit failed: %s", added.stderr)
	}
	if shown := gitIssue(t, bin, dir, strings.TrimSpace(added.stdout)).stdout; !strings.Contains(shown, "Edited after all") {
		t.Errorf("--edit did not take what the editor saved\n%s", shown)
	}
}

// An empty title aborts, as an empty message aborts a commit: nothing is
// written, and what was typed is kept where it can be recovered.
func TestAddEmptyTitleAborts(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	before := gitIssue(t, bin, dir).stdout

	env, _ := fakeEditor(t, "")
	got := gitIssueEnv(t, bin, dir, []string{env}, "add")
	if got.code != 1 || !strings.Contains(got.stderr, "aborting") {
		t.Errorf("exit %d, stderr %q", got.code, got.stderr)
	}
	if after := gitIssue(t, bin, dir).stdout; after != before {
		t.Errorf("an aborted add changed the listing\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", editFile)); err != nil {
		t.Errorf("the buffer was not kept: %v", err)
	}
}

// The description is Markdown: a line that opens with '#' is a heading, kept
// verbatim, not read as a comment and dropped.
func TestAddKeepsMarkdownHeadings(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	env, _ := fakeEditor(t, "A title\n\n# Overview\n\nSome prose.\n")

	added := gitIssueEnv(t, bin, dir, []string{env}, "add")
	if added.code != 0 {
		t.Fatalf("add failed: %s", added.stderr)
	}
	if shown := gitIssue(t, bin, dir, strings.TrimSpace(added.stdout)).stdout; !strings.Contains(shown, "# Overview") {
		t.Errorf("the '# Overview' heading was dropped\n%s", shown)
	}
}

// Deleting the closing --> along with the block does not spill the ignored
// text into the description: an unterminated <!--- runs to the end of buffer.
func TestAddUnterminatedIgnoredBlock(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	env, _ := fakeEditor(t, "A title\n\nreal body\n\n<!---\ntype  bug\nleftover guidance\n")

	added := gitIssueEnv(t, bin, dir, []string{env}, "add")
	if added.code != 0 {
		t.Fatalf("add failed: %s", added.stderr)
	}
	shown := gitIssue(t, bin, dir, strings.TrimSpace(added.stdout)).stdout
	if !strings.Contains(shown, "real body") || strings.Contains(shown, "leftover guidance") {
		t.Errorf("the dangling block leaked into the description\n%s", shown)
	}
}

// The template round trips: the title and description editTemplate writes,
// parseTemplate reads back unchanged, whatever the ignored block holds.
func TestTemplateRoundTrip(t *testing.T) {
	want := issue.Fields{
		Title:       "Anchor blobs are pruned by gc",
		Description: "First paragraph.\n\n# A heading, with a colon: here.",
	}
	note := editNote(nil, "Editing issue abcd.", issue.Fields{Type: "bug", Labels: []string{"design"}}, nil)

	got := parseTemplate(editTemplate(want, note))
	if got.Title != want.Title || got.Description != want.Description {
		t.Errorf("round trip differs\nwant %+v\ngot  %+v", want, got)
	}
}

func TestParseTemplate(t *testing.T) {
	cases := []struct {
		name, buffer string
		want         issue.Fields
	}{{
		name:   "title and description",
		buffer: "A title\n\nthe body\n",
		want:   issue.Fields{Title: "A title", Description: "the body"},
	}, {
		name:   "the ignored block is dropped wherever it sits",
		buffer: "A title\n\n<!---\nguidance\n-->\n\nthe body\n",
		want:   issue.Fields{Title: "A title", Description: "the body"},
	}, {
		name:   "a description needs no blank line after the title",
		buffer: "A title\nthe body\n",
		want:   issue.Fields{Title: "A title", Description: "the body"},
	}, {
		name:   "trailing blank lines are trimmed, a blank first line is no title",
		buffer: "A title\n\n\nthe body\n\n\n",
		want:   issue.Fields{Title: "A title", Description: "the body"},
	}, {
		name:   "a blank first line means an empty title, which aborts",
		buffer: "\n\nthe body\n",
		want:   issue.Fields{Description: "the body"},
	}, {
		name:   "a colon or a # in the description is just text",
		buffer: "A title\n\nnote: this stays\n# and so does this\n",
		want:   issue.Fields{Title: "A title", Description: "note: this stays\n# and so does this"},
	}, {
		name:   "a leading # on the title line is stripped",
		buffer: "#  Reject short ids\n\nthe body\n",
		want:   issue.Fields{Title: "Reject short ids", Description: "the body"},
	}, {
		name:   "an unterminated block runs to the end of the buffer",
		buffer: "A title\n\nthe body\n<!---\nleftover\n",
		want:   issue.Fields{Title: "A title", Description: "the body"},
	}, {
		name:   "a buffer with only a block is an empty title, which aborts",
		buffer: "<!---\nonly guidance\n-->\n",
		want:   issue.Fields{},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTemplate(tc.buffer)
			if got.Title != tc.want.Title || got.Description != tc.want.Description {
				t.Errorf("want %+v\ngot  %+v", tc.want, got)
			}
		})
	}
}

// A link given before the editor opened survives it.
//
// The buffer has no line for links, so a round trip through it cannot say
// anything about them — and a field an editor cannot show is a field it must
// not be able to drop. Filing an issue under another one and then being sent to
// the editor for the title is the ordinary way to write a sub-issue, and it
// used to lose the parent.
func TestAddKeepsLinksAcrossTheEditor(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	const parent = "885797fb2d9b"

	for _, tc := range []struct {
		name   string
		args   []string
		header string
	}{
		{"the positional", []string{"add", parent}, "Parent"},
		{"--rel", []string{"add", "--rel", "blocked-by:" + parent}, "Blocked by"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := fakeEditor(t, "A subtask\n\nbody\n")
			added := gitIssueEnv(t, bin, dir, []string{env}, tc.args...)
			if added.code != 0 {
				t.Fatalf("add failed: %s", added.stderr)
			}
			id := strings.TrimSpace(added.stdout)
			shown := gitIssue(t, bin, dir, "show", id).stdout
			got, ok := headerLine(shown, tc.header)
			if !ok {
				t.Fatalf("the editor dropped the link: no %s: line\n%s", tc.header, shown)
			}
			// The title the id resolves to is the fixture's business; that it
			// resolves to the issue named is this test's.
			if !strings.HasPrefix(got, parent+"  ") {
				t.Errorf("%s = %q, want the link to %s", tc.header, got, parent)
			}
		})
	}

	// And the listing nests what the editor did not touch.
	tree := gitIssue(t, bin, dir, "list", "--tree").stdout
	if !strings.Contains(tree, "─ A subtask") {
		t.Errorf("list --tree did not nest the sub-issue\n%s", tree)
	}
}

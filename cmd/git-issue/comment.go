package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
	"github.com/hdweiss/git-issue/internal/render"
)

// commentUsage explains what addresses a comment afterwards, which is the part
// of commenting that is not obvious: the id show prints, never a position in
// the thread.
const commentUsage = `usage: git issue comment <id> [-m <message>] [-F <path>] [-i <comment>]

       -m, --message <text>          the comment's text
       -F, --file <path>             read the text from a file, or '-' for stdin
       -i, --in-reply-to <comment>   reply to that entry instead of starting a
                                     new thread

Without -m an editor opens, the way git commit does without one. -F reads the
text from a file instead, '-' reads it from stdin, and piped stdin is picked
up without -F too, so 'echo done | git issue comment <id>' posts a comment. An
empty comment aborts and writes nothing.

A comment is addressed by its own id, which 'git issue show' prints above each
entry and this command prints once it has written one. Ids abbreviate like
object names, and the prefix only has to be unique within the issue:

       git issue comment <id> -i <comment>   reply to it
       git issue edit    <id> <comment>      rewrite it
       git issue remove  <id> <comment>      retract it

Commenting is its own verb rather than 'git issue add <id>', which already
means something else: a positional id says what to file the new issue under.`

// commentFile is the buffer a comment is written in, alongside git's own
// COMMIT_EDITMSG and add's ISSUE_EDITMSG.md, and kept afterwards for the same
// reason: an aborted comment is still something somebody typed. The .md suffix
// is what puts the editor in Markdown mode.
const commentFile = "ISSUE_COMMENT_EDITMSG.md"

// commentGuide is the guidance placed inside the buffer's ignored block, after
// a line naming the issue. Like editGuide it must not contain "<!---" or "-->".
const commentGuide = `Write the comment above, in Markdown. Anything in this block will be ignored
on save, and an empty comment aborts without writing anything.`

func cmdComment(args []string) error {
	message, given, file, replyTo, targets, err := parseMessage("comment", args, commentUsage, 1)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(commentUsage)
		return nil
	}
	if err != nil {
		return err
	}
	if given && file != "" {
		return fmt.Errorf("-m and -F both give the text; drop one\n%s", commentUsage)
	}

	s, err := open()
	if err != nil {
		return err
	}
	who, err := author(s)
	if err != nil {
		return err
	}
	id, st, err := s.Find(targets[0], issue.Type)
	if err != nil {
		return err
	}

	var parent string
	if replyTo != "" {
		entry, err := st.FindComment(replyTo)
		if err != nil {
			return err
		}
		parent = entry.ID()
	}

	// No -m: the text comes from -F, from piped stdin, or from an editor — the
	// same call `git commit` makes without one.
	if !given {
		buf, fromInput, err := readBuffer(file, file != "", true)
		if err != nil {
			return err
		}
		switch {
		case fromInput:
			if message = stripComments(buf); strings.TrimSpace(message) == "" {
				return fmt.Errorf("aborting: the comment is empty")
			}
		default:
			if !s.Repo.CanEdit() {
				return fmt.Errorf("no message given, and no editor to open: use -m, -F, or set core.editor")
			}
			if message, err = editBody(s, "", commentNote(id, st, parent)); err != nil {
				return err
			}
		}
	}

	entry, err := issue.Comment(s, who, time.Now().Unix(), id, st, message, parent)
	if err != nil {
		return err
	}
	fmt.Println(render.Abbrev(entry))
	return nil
}

// editComment is `git issue edit <id> <comment>`: one entry of the thread,
// rewritten from -m, from -F / piped stdin, or opened in the editor filled in
// with what it currently says.
func editComment(s *entity.Store, id string, st entity.State, prefix, message string, given bool, file string, fileGiven bool) error {
	who, err := author(s)
	if err != nil {
		return err
	}
	entry, err := st.FindComment(prefix)
	if err != nil {
		return err
	}
	if entry.Retracted {
		return fmt.Errorf("comment %s is retracted; its text is gone from what the issue says", render.Abbrev(entry.ID()))
	}

	if !given {
		buf, fromInput, err := readBuffer(file, fileGiven, true)
		if err != nil {
			return err
		}
		switch {
		case fromInput:
			if message = stripComments(buf); strings.TrimSpace(message) == "" {
				return fmt.Errorf("aborting: the comment is empty")
			}
		default:
			if !s.Repo.CanEdit() {
				return fmt.Errorf("no message given, and no editor to open: use -m, -F, or set core.editor")
			}
			if message, err = editBody(s, entry.Body.Display(), commentNote(id, st, "")); err != nil {
				return err
			}
		}
	}

	changed, err := issue.EditComment(s, who, time.Now().Unix(), id, st, entry, message)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Println("nothing changed")
		return nil
	}
	fmt.Printf("edited %s\n", render.Abbrev(entry.ID()))
	return nil
}

// removeComment is `git issue remove <id> <comment>`: a tombstone that hides
// one entry's body.
//
// It does not erase the text — the comment event stays in the blob, which is
// what lets the retraction converge — and it does not cascade to replies.
func removeComment(s *entity.Store, id string, st entity.State, prefix string) error {
	who, err := author(s)
	if err != nil {
		return err
	}
	entry, err := st.FindComment(prefix)
	if err != nil {
		return err
	}

	removed, err := issue.RemoveComment(s, who, time.Now().Unix(), id, st, entry)
	if err != nil {
		return err
	}
	if !removed {
		fmt.Printf("%s is already retracted\n", render.Abbrev(entry.ID()))
		return nil
	}
	fmt.Printf("retracted %s\n", render.Abbrev(entry.ID()))
	return nil
}

// parseMessage parses a command that takes -m, -F, -i and up to max
// positional ids.
//
// It reports whether -m was given at all, which is not the same question as
// whether it carries anything: no -m opens the editor, while an empty -m is
// someone saying the comment is empty. That is refused rather than answered
// with an editor they did not ask for. -F reads the text from a file, or from
// stdin for "-"; file is "" when it was not given. -i names the entry the new
// comment replies to, as a prefix resolved against the issue's thread by the
// caller, once it holds one.
func parseMessage(name string, args []string, usage string, max int) (message string, given bool, file string, replyTo string, targets []string, err error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, flagName := range []string{"m", "message"} {
		fs.StringVar(&message, flagName, "", "the comment's text")
	}
	for _, flagName := range []string{"F", "file"} {
		fs.StringVar(&file, flagName, "", "read the comment's text from a file, or '-' for stdin")
	}
	for _, flagName := range []string{"i", "in-reply-to"} {
		fs.StringVar(&replyTo, flagName, "", "reply to that entry instead of starting a new thread")
	}

	targets, err = parseTargets(fs, name, args, usage, max)
	if err != nil {
		return "", false, "", "", nil, err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "m" || f.Name == "message" {
			given = true
		}
	})
	return message, given, file, replyTo, targets, nil
}

// commentNote is the text for the ignored block at the foot of a comment
// buffer: a line naming the issue, then the shared guidance. parent, where
// given, is the entry the new comment replies to.
func commentNote(id string, st entity.State, parent string) string {
	line := "Comment on issue " + render.Abbrev(id)
	if title := issue.FieldsOf(st).Title; title != "" {
		line += " — " + title
	}
	if parent != "" {
		line += ", in reply to " + render.Abbrev(parent)
	}
	return line + "\n\n" + commentGuide
}

// editBody opens the editor on a comment and returns what came back. note is
// the text placed inside the buffer's ignored block. The buffer is written into
// the git directory rather than a temp file so that an aborted comment is
// somewhere findable afterwards.
func editBody(s *entity.Store, body, note string) (string, error) {
	dir, err := s.Repo.CommonDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, commentFile)

	var b strings.Builder
	b.WriteString(body)
	if body != "" {
		b.WriteString("\n")
	}
	b.WriteString("\n<!---\n" + strings.TrimRight(note, "\n") + "\n-->\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	if err := s.Repo.EditFile(path); err != nil {
		return "", err
	}

	edited, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	got := stripComments(string(edited))
	if strings.TrimSpace(got) == "" {
		return "", fmt.Errorf("aborting: the comment is empty (what you wrote is kept in %s)", path)
	}
	return got, nil
}

// stripComments drops the `<!--- ... -->` spans the editor's user was not
// writing. A comment is prose all the way down, so what is left is the body.
func stripComments(buffer string) string {
	return strings.Trim(ignoredBlock.ReplaceAllString(buffer, ""), "\n")
}

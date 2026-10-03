package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/render"
	"github.com/hdweiss/git-issue/internal/review"
)

const addUsage = `usage: git review add [<id>] [-t <title>] [-d <description>] [-F <path>]
                      [--base <branch>] [--head <branch>] [--revision <sha>]
                      [--closes <id>] [-l <label>] [-a <who>] [-m <milestone>]
                      [--rel <kind>:<id>] [--draft] [-e]

       <id>                        file the new review under this one
       -t, --title <title>         the one-line summary
       -d, --description <text>    the body
       -F, --file <path>           read title + description from a file ('-' = stdin)
           --base <branch>         the branch this merges into
           --head <branch>         the branch this merges from, remote-qualified
           --revision <sha>        the commit it proposes; defaults to the head's
           --closes <id>           an issue this closes; repeatable
       -l, --label <label>         repeatable, or one comma-separated list
       -a, --reviewer <who>        ask someone to read it; repeatable
       -m, --milestone <name>      the milestone's name
           --rel <kind>:<id>       any other link; repeatable
           --draft                 open it as a draft
       -e, --edit                  open the editor even when a title was given

With no --base and --head, both are taken from where you are: the head is the
branch you are on, qualified with its remote when it has one, and the base is
that remote's default branch. So 'git review add -t "…"' on a feature branch
opens a review of it against main and needs nothing else.

--head is remote-qualified — 'origin/feature-x', 'fork/feature-x' — because a
fork's head has no meaning as a bare name, and outside contributions are the
common case rather than the edge. A branch with no remote is written bare, which
is what a purely local review of your own work is.

--revision pins the commit the review describes. It defaults to whatever the
head resolves to now, and it is what anchors, verdicts and checks address —
which is why it is stored rather than resolved on every read: a branch deleted
after a merge is the normal end of a review's life, and the sha still means
something afterwards.

--closes records that this review is meant to close an issue. It records intent
and nothing more: nothing here closes that issue, now or on merge. An issue's
status changes when an event on the issue's own blob says so, by a person, as
its own action — a cascade could not converge, and a review merged in a fork
should not close an issue upstream.

Without a title an editor opens, as git commit does without -m: the first line
is the title, as a '# ' heading, the rest is the description, in Markdown. An
empty first line aborts and writes no review.`

// editGuide is the guidance placed inside the buffer's ignored block. It must
// not contain the "<!---" or "-->" markers itself — the first "-->" closes the
// block.
const editGuide = `The first line is the title, as a '# ' heading; everything below it is the
description, in Markdown. Anything in this block will be ignored on save.

Fields other than the title and description are set with flags, not here —
pass them to 'git review add', or run 'git review edit <id>' afterwards.`

// The name of the buffer add hands to the editor, alongside git's own
// COMMIT_EDITMSG, and kept afterwards for the same reason: an aborted review is
// still something somebody typed.
const editFile = "REVIEW_EDITMSG.md"

// commentFile is the buffer a comment is written in. Separate from editFile so
// that an aborted comment and an aborted review do not overwrite each other.
const commentFile = "REVIEW_COMMENTMSG.md"

// ignoredBlock matches an HTML comment the buffer's user is not writing: from
// `<!---` to the next `-->`, or to the end of the buffer when the closing
// marker was deleted along the way.
var ignoredBlock = regexp.MustCompile(`(?s)<!---.*?(?:-->|\z)`)

func cmdAdd(args []string) error {
	opts, err := parseAddFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(addUsage)
		return nil
	}
	if err != nil {
		return err
	}

	s, err := open()
	if err != nil {
		return err
	}
	who, err := author(s)
	if err != nil {
		return err
	}

	fields := opts.fields
	if opts.parent != "" {
		under, _, err := s.Find(opts.parent, review.Type)
		if err != nil {
			if !looksLikeID(opts.parent) {
				return fmt.Errorf("add files the new review under the review named; give the title as --title '%s'\n%s", opts.parent, addUsage)
			}
			return err
		}
		fields.Relations = append(fields.Relations, review.Relation{Kind: review.KindParent, Target: under})
	}

	for _, target := range opts.closes.values {
		fields.Relations = append(fields.Relations, review.Relation{Kind: review.KindCloses, Target: target})
	}
	for _, arg := range opts.rels.values {
		kind, target, err := review.ParseRelation(arg)
		if err != nil {
			return err
		}
		fields.Relations = append(fields.Relations, review.Relation{Kind: kind, Target: target})
	}
	// Every target is expanded before it is written: a `ref` names an entity,
	// and an abbreviation only means something relative to the ref it was
	// measured against.
	if fields.Relations, err = resolveRelations(s, fields.Relations); err != nil {
		return err
	}

	fillBranches(s.Repo, &fields, opts.baseGiven, opts.headGiven, opts.revGiven)

	// Implicit stdin only while the title is still missing — see readBuffer.
	buf, fromInput, err := readBuffer(opts.file, opts.file != "", fields.Title == "")
	if err != nil {
		return err
	}
	switch {
	case fromInput:
		if fields.Description != "" {
			return fmt.Errorf("--description and the piped/-F input both give the body; drop one\n%s", addUsage)
		}
		fields = applyBuffer(fields, buf, fields.Title != "")
		if fields.Title == "" {
			return fmt.Errorf("no title: the first line of the input is empty\n%s", addUsage)
		}
	case opts.edit || fields.Title == "":
		if fields.Title == "" && !s.Repo.CanEdit() {
			return fmt.Errorf("no title given, and no editor to open: use --title, -F, or set core.editor")
		}
		fields, err = editFields(s, fields, editNote(s, "Opening a new review.", fields))
		if err != nil {
			return err
		}
	}

	id, err := review.Create(s, who, time.Now().Unix(), fields)
	if err != nil {
		return err
	}
	fmt.Println(render.Abbrev(id))
	return nil
}

// fillBranches supplies the base, head and revision nobody named.
//
// The defaults come from where the caller is standing, which is the whole point
// of the command being usable with a title alone: on a feature branch, the head
// is that branch and the base is what the remote says it merges into. A
// repository with no remote gets a bare head and whichever default branch it
// holds, which is the local-review case.
//
// A flag that was given is never second-guessed, including an empty one: a
// caller that says --base "" means to leave it unset.
func fillBranches(repo *gitx.Repo, f *review.Fields, baseGiven, headGiven, revGiven bool) {
	branch := repo.CurrentBranch()
	remote := repo.BranchRemote()

	if !headGiven && branch != "" {
		if remote != "" {
			f.Head = remote + "/" + branch
		} else {
			f.Head = branch
		}
	}
	if !baseGiven {
		if base := repo.DefaultBranch(remote); base != "" && base != branch {
			f.Base = base
		}
	}
	if !revGiven {
		// HEAD, not the head branch's own ref: they are the same thing on a
		// checked-out branch, and on a detached HEAD the commit is the only
		// honest answer.
		f.HeadSHA = repo.RefSHA("HEAD")
	}
}

type addOptions struct {
	fields review.Fields
	rels   listFlag
	closes listFlag
	parent string
	edit   bool
	file   string

	// Whether a branch flag was given at all, as opposed to given empty. The
	// difference decides whether fillBranches supplies a default.
	baseGiven, headGiven, revGiven bool
}

func parseAddFlags(args []string) (addOptions, error) {
	var (
		opts      addOptions
		labels    listFlag
		reviewers listFlag
		draft     bool
	)
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, name := range []string{"t", "title"} {
		fs.StringVar(&opts.fields.Title, name, "", "the review's one-line summary")
	}
	for _, name := range []string{"d", "description"} {
		fs.StringVar(&opts.fields.Description, name, "", "the review's body")
	}
	for _, name := range []string{"m", "milestone"} {
		fs.StringVar(&opts.fields.Milestone, name, "", "the milestone's name")
	}
	for _, name := range []string{"l", "label"} {
		fs.Var(&labels, name, "a label; repeatable, or comma-separated")
	}
	for _, name := range []string{"a", "reviewer"} {
		fs.Var(&reviewers, name, "someone to read it; repeatable, or comma-separated")
	}
	for _, name := range []string{"e", "edit"} {
		fs.BoolVar(&opts.edit, name, false, "open the editor even when a title was given")
	}
	for _, name := range []string{"F", "file"} {
		fs.StringVar(&opts.file, name, "", "read the title and description from a file, or '-' for stdin")
	}
	fs.StringVar(&opts.fields.Base, "base", "", "the branch this merges into")
	fs.StringVar(&opts.fields.Head, "head", "", "the branch this merges from")
	fs.StringVar(&opts.fields.HeadSHA, "revision", "", "the commit this review proposes")
	fs.BoolVar(&draft, "draft", false, "open it as a draft")
	fs.Var(&opts.closes, "closes", "an issue this review closes; repeatable")
	fs.Var(&opts.rels, "rel", "link this review to another entity: <kind>:<id>; repeatable")

	rest, err := parsePermuted(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, fmt.Errorf("%s\n%s", err, addUsage)
	}
	if len(rest) > 1 {
		return opts, fmt.Errorf("add takes one review to file this one under, got '%s' as well\n%s", rest[1], addUsage)
	}
	if len(rest) == 1 {
		opts.parent = rest[0]
	}

	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "base":
			opts.baseGiven = true
		case "head":
			opts.headGiven = true
		case "revision":
			opts.revGiven = true
		}
	})
	if draft {
		opts.fields.Draft = &draft
	}
	opts.fields.Labels, opts.fields.Assignees = labels.values, reviewers.values
	return opts, nil
}

// looksLikeID reports whether a word could be an abbreviated object name at
// all, which is what tells a prefix that resolved to nothing from a title typed
// where an id goes.
func looksLikeID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// listFlag collects a repeatable flag. A comma-separated value is split too, so
// `-l bug,design` and `-l bug -l design` mean the same thing.
type listFlag struct{ values []string }

func (l *listFlag) String() string { return strings.Join(l.values, ",") }

func (l *listFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			l.values = append(l.values, part)
		}
	}
	return nil
}

// readBuffer resolves the text a command should use in place of an editor
// session.
//
// allowImplicit must be false for any request the flags already complete.
// Non-terminal stdin is not the same thing as a pipe somebody is feeding: a
// script, a git hook or a CI runner hands down a pipe that carries no data and
// is never closed, and io.ReadAll on it blocks until the process is killed. A
// command that could have finished without reading must therefore not read at
// all, and a pipe alongside a complete request is spelled `-F -`.
func readBuffer(file string, fileGiven, allowImplicit bool) (text string, ok bool, err error) {
	switch {
	case fileGiven && file != "" && file != "-":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", false, err
		}
		return string(b), true, nil
	case fileGiven:
		b, err := io.ReadAll(os.Stdin)
		return string(b), true, err
	case allowImplicit && !gitx.IsTerminal(os.Stdin):
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", false, err
		}
		if strings.TrimSpace(string(b)) == "" {
			return "", false, nil
		}
		return string(b), true, nil
	default:
		return "", false, nil
	}
}

// applyBuffer folds buffer text into fields the flags already set.
func applyBuffer(fields review.Fields, text string, titleGiven bool) review.Fields {
	if titleGiven {
		fields.Description = strings.TrimSpace(ignoredBlock.ReplaceAllString(text, ""))
		return fields
	}
	title, desc := parseTemplate(text)
	fields.Title, fields.Description = title, desc
	return fields
}

// editFields opens the editor on the review's title and description and returns
// fields with those two brought to what came back — every other field crosses
// untouched, since the buffer has no way to show or change it.
func editFields(s *entity.Store, fields review.Fields, note string) (review.Fields, error) {
	dir, err := s.Repo.CommonDir()
	if err != nil {
		return fields, err
	}
	path := filepath.Join(dir, editFile)

	if err := os.WriteFile(path, []byte(editTemplate(fields.Title, fields.Description, note)), 0o644); err != nil {
		return fields, err
	}
	if err := s.Repo.EditFile(path); err != nil {
		return fields, err
	}

	edited, err := os.ReadFile(path)
	if err != nil {
		return fields, err
	}
	title, desc := parseTemplate(string(edited))
	if title == "" {
		return fields, fmt.Errorf("aborting: the title is empty (what you wrote is kept in %s)", path)
	}
	fields.Title, fields.Description = title, desc
	return fields, nil
}

// editTemplate is the buffer the editor opens on: the title on the first line,
// the description below it, and an ignored `<!--- ... -->` block last.
func editTemplate(title, description, note string) string {
	var b strings.Builder
	b.WriteString("# " + title + "\n")
	if description != "" {
		b.WriteString("\n" + description + "\n")
	}
	b.WriteString("\n<!---\n" + strings.TrimRight(note, "\n") + "\n-->\n")
	return b.String()
}

// parseTemplate reads back what the editor saved: every `<!--- ... -->` span is
// dropped, the first remaining line is the title, and the rest is the
// description.
//
// Leading blank lines are kept, not trimmed: a first line left blank is how the
// buffer says "no title", which aborts the way an empty `git commit` message
// does.
func parseTemplate(buffer string) (title, description string) {
	body := strings.TrimRight(ignoredBlock.ReplaceAllString(buffer, ""), " \t\n")
	title, desc, _ := strings.Cut(body, "\n")
	title = strings.TrimSpace(title)
	if h := strings.TrimLeft(title, "#"); h != title {
		title = strings.TrimSpace(h)
	}
	return title, strings.TrimSpace(desc)
}

// editNote builds the text for the buffer's ignored block: an intro, the shared
// guidance, then a read-only echo of the fields that are set with flags.
func editNote(s *entity.Store, intro string, fields review.Fields) string {
	var b strings.Builder
	b.WriteString(intro + "\n\n" + editGuide + "\n")

	var rows [][2]string
	add := func(k, v string) {
		if v != "" {
			rows = append(rows, [2]string{k, v})
		}
	}
	add("base", fields.Base)
	add("head", fields.Head)
	if fields.HeadSHA != "" {
		add("revision", render.Abbrev(fields.HeadSHA))
	}
	add("labels", strings.Join(fields.Labels, ", "))
	add("reviewers", strings.Join(fields.Assignees, ", "))
	add("milestone", fields.Milestone)
	if fields.Draft != nil && *fields.Draft {
		add("draft", "yes")
	}
	if fields.Status != "" && fields.Status != review.StatusOpen {
		add("status", fields.Status)
	}
	add("status-reason", fields.StatusReason)
	for _, r := range fields.Relations {
		add(r.Kind, relationLabel(s, r.Target))
	}
	if len(rows) > 0 {
		width := 0
		for _, r := range rows {
			width = max(width, len(r[0]))
		}
		b.WriteString("\nSet with flags, shown here only for reference:\n\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "  %-*s  %s\n", width, r[0], r[1])
		}
	}
	return b.String()
}

// relationLabel names a link's far end for the echo.
//
// A target on the issue ref reads as a bare id here, which is honest: this
// command holds one ref, and a `closes` target routinely lives in another
// repository entirely where no lookup could succeed.
func relationLabel(s *entity.Store, target string) string {
	id, st, err := s.Find(target, review.Type)
	if err != nil {
		return render.Abbrev(target)
	}
	if title := review.FieldsOf(st).Title; title != "" {
		return render.Abbrev(id) + "  " + title
	}
	return render.Abbrev(id)
}

// author is who the events will say wrote them. An unset user.email is refused
// rather than guessed at: the `a` field is the authoritative record of who wrote
// an event.
func author(s *entity.Store) (string, error) {
	who, err := s.Repo.Config("user.email")
	if err != nil || who == "" {
		return "", fmt.Errorf("set user.email first")
	}
	return who, nil
}

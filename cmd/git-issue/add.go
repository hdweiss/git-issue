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
	"github.com/hdweiss/git-issue/internal/issue"
	"github.com/hdweiss/git-issue/internal/render"
)

// addUsage spells out the fields, since which ones an issue can be born with
// is the part of add that is not obvious.
const addUsage = `usage: git issue add [<id>] [-t <title>] [-d <description>] [-F <path>]
                     [--type <type>] [-l <label>] [-a <assignee>] [-m <milestone>]
                     [--rel <kind>:<id>] [-e]

       <id>                        file the new issue under this one
       -t, --title <title>         the one-line summary
       -d, --description <text>    the body
       -F, --file <path>           read title + description from a file ('-' = stdin)
           --type <type>           bug, feature, task — the vocabulary is open
       -l, --label <label>         repeatable, or one comma-separated list
       -a, --assignee <who>        repeatable, or one comma-separated list
       -m, --milestone <name>      the milestone's name
           --rel <kind>:<id>       link to another issue; repeatable
       -e, --edit                  open the editor even when a title was given

Without a title an editor opens, as git commit does without -m: the first line
is the title, as a '# ' heading, the rest is the description, in Markdown. Any
-d text already given is filled in; the other fields are echoed, read-only, in
an ignored block. An empty first line aborts and writes no issue.

-F reads that buffer from a file instead, and '-' reads it from stdin, so
'cat notes.md | git issue add' files an issue from the file — a leading '#' on
its first line is dropped. Piped stdin is picked up without -F too when a title
or description is still missing; with -t given, the piped text is the
description. -d and piped/-F input may not both set the body.

An id before the flags files the new issue under that one, the way
'git checkout -b <branch> <start-point>' says where to start from. Omit it for
a top-level issue — that is what omitting it means, so 'none' is not accepted
here. It is the same field --rel writes, said the way this command's one
positional says it: 'add <id>' and '--rel parent:<id>' write the same link.

--rel writes any other kind of link the issue is born with: blocked-by,
duplicate-of and related are the ones with names here, and a kind this build
has never heard of is written as given rather than refused.

The editor shows links read-only and cannot change them; to re-file an issue
that already exists, or detach one, use 'git issue edit <id> --parent'.`

// editGuide is the guidance placed inside the buffer's ignored block. The
// per-command intro ("Filing a new issue.", "Editing issue abcd.") is prepended
// by editNote. It must not contain the "<!---" or "-->" markers itself — the
// first "-->" closes the block.
const editGuide = `The first line is the title, as a '# ' heading; everything below it is the
description, in Markdown. Anything in this block will be ignored on save.

Fields other than the title and description are set with flags, not here —
pass them to 'git issue add', or run 'git issue edit <id>' afterwards.`

// The name of the buffer add hands to the editor, alongside git's own
// COMMIT_EDITMSG, and kept afterwards for the same reason: an aborted issue is
// still something somebody typed. The .md suffix is what puts the editor in
// Markdown mode.
const editFile = "ISSUE_EDITMSG.md"

// ignoredBlock matches an HTML comment the buffer's user is not writing: from
// `<!---` to the next `-->`, or to the end of the buffer when the closing
// marker was deleted along the way, so a mangled block never lands in a field.
var ignoredBlock = regexp.MustCompile(`(?s)<!---.*?(?:-->|\z)`)

// readBuffer resolves the text add/edit/comment should use in place of an
// editor session. A -F path is read as given, with "-" or "" meaning stdin.
// Without -F, and only when allowImplicit says the command has nothing else to
// go on, non-terminal stdin is read too — so `cat notes.md | git issue add`
// works without spelling out `-F -`. ok is false when there is nothing to use
// and the caller should fall back to an editor or its own "nothing to write"
// error.
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

// applyBuffer folds buffer text into fields the flags already set. When the
// title came from the command line the whole text is the description; otherwise
// it is parsed as a buffer — the first line the title, the rest the description.
func applyBuffer(fields issue.Fields, text string, titleGiven bool) issue.Fields {
	if titleGiven {
		fields.Description = strings.TrimSpace(ignoredBlock.ReplaceAllString(text, ""))
		return fields
	}
	parsed := parseTemplate(text)
	fields.Title, fields.Description = parsed.Title, parsed.Description
	return fields
}

func cmdAdd(args []string) error {
	fields, rels, parent, edit, file, err := parseAddFlags(args)
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
	if parent != "" {
		under, _, err := s.Find(parent, issue.Type)
		if err != nil {
			// A bare word used to be a mistyped title, and for anything that is
			// not a possible object name it still is — that message is far more
			// use than reporting the title as an unknown issue.
			if !looksLikeID(parent) {
				return fmt.Errorf("add files the new issue under the issue named; give the title as --title '%s'\n%s", parent, addUsage)
			}
			return err
		}
		fields.Relations = append(fields.Relations, issue.Relation{Kind: issue.KindParent, Target: under})
	}
	// --rel says the same thing for every other kind of link. The positional is
	// kept as its own spelling because filing under an issue is what `add`
	// takes an argument for, not because a parent is a different sort of link.
	for _, arg := range rels.values {
		r, err := resolveRelation(s, "", entity.State{}, arg)
		if err != nil {
			return err
		}
		fields.Relations = append(fields.Relations, r)
	}

	// The title and description can come from -F, from piped stdin, or from an
	// editor — an issue needs a title, so one of the three has to supply it
	// when --title did not. -F and a pipe skip the editor entirely.
	//
	// Implicit stdin is offered only while the title is still missing, which is
	// the case that has nothing else to go on. With -t the request is already
	// complete, and a complete request must never block on input nobody is
	// going to send: `git issue add -t x --type bug` from a script, a hook or
	// CI inherits a pipe that carries no data and never closes, and reading it
	// hangs forever. Piping a body alongside -t is still possible, spelled
	// `-F -`.
	buf, fromInput, err := readBuffer(file, file != "", fields.Title == "")
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
	case edit || fields.Title == "":
		if fields.Title == "" && !s.Repo.CanEdit() {
			return fmt.Errorf("no title given, and no editor to open: use --title, -F, or set core.editor")
		}
		fields, err = editFields(s, fields, editNote(s, "Filing a new issue.", fields, fields.Relations))
		if err != nil {
			return err
		}
	}

	id, err := issue.Create(s, who, time.Now().Unix(), fields)
	if err != nil {
		return err
	}
	fmt.Println(render.Abbrev(id))
	return nil
}

// parseAddFlags reads add's flags and the one positional it takes: the issue
// to file the new one under.
//
// A positional id means the same thing here as on list — the issue this
// command hangs off — which is also git's own reading of a trailing id in
// `git checkout -b <branch> <start-point>`.
func parseAddFlags(args []string) (issue.Fields, listFlag, string, bool, string, error) {
	var (
		fields    issue.Fields
		edit      bool
		file      string
		labels    listFlag
		assignees listFlag
		rels      listFlag
	)
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	// Long and short spellings are separate flags to Go's flag package, which
	// makes no distinction between one dash and two: -t, --t, -title and
	// --title all reach the same variable.
	for _, name := range []string{"t", "title"} {
		fs.StringVar(&fields.Title, name, "", "the issue's one-line summary")
	}
	for _, name := range []string{"d", "description"} {
		fs.StringVar(&fields.Description, name, "", "the issue's body")
	}
	for _, name := range []string{"m", "milestone"} {
		fs.StringVar(&fields.Milestone, name, "", "the milestone's name")
	}
	for _, name := range []string{"l", "label"} {
		fs.Var(&labels, name, "a label; repeatable, or comma-separated")
	}
	for _, name := range []string{"a", "assignee"} {
		fs.Var(&assignees, name, "an assignee; repeatable, or comma-separated")
	}
	for _, name := range []string{"e", "edit"} {
		fs.BoolVar(&edit, name, false, "open the editor even when a title was given")
	}
	for _, name := range []string{"F", "file"} {
		fs.StringVar(&file, name, "", "read the title and description from a file, or '-' for stdin")
	}
	fs.StringVar(&fields.Type, "type", "", "the kind of work: bug, feature, task")
	fs.Var(&rels, "rel", "link this issue to another: <kind>:<id>; repeatable")

	rest, err := parsePermuted(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return fields, rels, "", false, "", err
		}
		return fields, rels, "", false, "", fmt.Errorf("%s\n%s", err, addUsage)
	}
	if len(rest) > 1 {
		return fields, rels, "", false, "", fmt.Errorf("add takes one issue to file this one under, got '%s' as well\n%s", rest[1], addUsage)
	}
	parent := ""
	if len(rest) == 1 {
		parent = rest[0]
	}

	fields.Labels, fields.Assignees = labels.values, assignees.values
	return fields, rels, parent, edit, file, nil
}

// looksLikeID reports whether a word could be an abbreviated object name at
// all. It is what tells a prefix that resolved to nothing from a title typed
// where an id goes, so each gets the message that helps.
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

// listFlag collects a repeatable flag. A comma-separated value is split too,
// so `-l bug,design` and `-l bug -l design` mean the same thing.
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

// editFields opens the editor on the issue's title and description and returns
// fields with those two brought to what came back — every other field crosses
// untouched, since the buffer has no way to show or change it. note is the text
// placed inside the buffer's ignored block. The buffer is written into the git
// directory rather than a temp file so that an aborted issue is somewhere
// findable afterwards.
func editFields(s *entity.Store, fields issue.Fields, note string) (issue.Fields, error) {
	dir, err := s.Repo.CommonDir()
	if err != nil {
		return fields, err
	}
	path := filepath.Join(dir, editFile)

	if err := os.WriteFile(path, []byte(editTemplate(fields, note)), 0o644); err != nil {
		return fields, err
	}
	if err := s.Repo.EditFile(path); err != nil {
		return fields, err
	}

	edited, err := os.ReadFile(path)
	if err != nil {
		return fields, err
	}
	got := parseTemplate(string(edited))
	if got.Title == "" {
		return fields, fmt.Errorf("aborting: the title is empty (what you wrote is kept in %s)", path)
	}
	fields.Title, fields.Description = got.Title, got.Description
	return fields, nil
}

// editTemplate is the buffer the editor opens on: the title on the first line,
// the description below it, and an ignored `<!--- ... -->` block last, holding
// the guidance and a read-only echo of everything set with flags.
func editTemplate(fields issue.Fields, note string) string {
	var b strings.Builder
	b.WriteString("# " + fields.Title + "\n")
	if fields.Description != "" {
		b.WriteString("\n" + fields.Description + "\n")
	}
	b.WriteString("\n<!---\n" + strings.TrimRight(note, "\n") + "\n-->\n")
	return b.String()
}

// parseTemplate reads back what the editor saved: every `<!--- ... -->` span is
// dropped (an unterminated one runs to the end of the buffer), the first
// remaining line is the title, and the rest is the description. A leading `#`
// on the title line is stripped — the template writes the title as a Markdown
// heading, and a file handed to `-F` usually does too — but `#` in the
// description is left alone, since that is a real heading there.
//
// Leading blank lines are kept, not trimmed: the template puts the title on the
// first line, so a first line left blank is how the buffer says "no title",
// which aborts the same way an empty `git commit` message does.
func parseTemplate(buffer string) issue.Fields {
	body := strings.TrimRight(ignoredBlock.ReplaceAllString(buffer, ""), " \t\n")
	title, desc, _ := strings.Cut(body, "\n")
	title = strings.TrimSpace(title)
	if h := strings.TrimLeft(title, "#"); h != title {
		title = strings.TrimSpace(h)
	}
	return issue.Fields{
		Title:       title,
		Description: strings.TrimSpace(desc),
	}
}

// editNote builds the text for the buffer's ignored block: a per-command intro,
// the shared guidance, then an aligned echo of the fields that are set with
// flags. The echo is read-only — nothing here is parsed back — so it can show
// links and status without the round trip being able to clear them.
func editNote(s *entity.Store, intro string, fields issue.Fields, rels []issue.Relation) string {
	var b strings.Builder
	b.WriteString(intro + "\n\n" + editGuide + "\n")

	var rows [][2]string
	add := func(k, v string) {
		if v != "" {
			rows = append(rows, [2]string{k, v})
		}
	}
	add("type", fields.Type)
	add("labels", strings.Join(fields.Labels, ", "))
	add("assignees", strings.Join(fields.Assignees, ", "))
	add("milestone", fields.Milestone)
	if fields.Status != "" && fields.Status != issue.StatusOpen {
		add("status", fields.Status)
	}
	add("status-reason", fields.StatusReason)
	for _, r := range rels {
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

// relationLabel names a link's far end for the echo: its abbreviated id and,
// when this repository holds it, its title.
func relationLabel(s *entity.Store, target string) string {
	id, st, err := s.Find(target, issue.Type)
	if err != nil {
		return render.Abbrev(target)
	}
	if title := issue.FieldsOf(st).Title; title != "" {
		return render.Abbrev(id) + "  " + title
	}
	return render.Abbrev(id)
}

// author is who the events will say wrote them. An unset user.email is refused
// rather than guessed at: the `a` field is the authoritative record of who
// wrote an event, and git blame on a note cannot correct it.
func author(s *entity.Store) (string, error) {
	who, err := s.Repo.Config("user.email")
	if err != nil || who == "" {
		return "", fmt.Errorf("set user.email first")
	}
	return who, nil
}

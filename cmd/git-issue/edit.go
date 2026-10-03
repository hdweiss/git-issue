package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
	"github.com/hdweiss/git-issue/internal/render"
)

// `git issue edit` writes a field two ways: through the editor, which is what
// opens when nothing else was said, and straight from a flag.
//
// Both ends produce an issue.Fields and go through issue.Update, so the events
// a flag writes are the events the editor writes. What a flag must not do is
// reach a field the editor does not show — see issue.Fields.Parent.

// editFlags is what `edit` was told to change. Go's flag package cannot tell
// an unset string from one set to "", and the two mean opposite things here —
// "leave it alone" against "empty it" — so which flags were actually given is
// recorded separately.
type editFlags struct {
	title        string
	description  string
	typ          string
	milestone    string
	status       string
	statusReason string
	parent       string
	labels       listFlag
	assignees    listFlag
	rels         listFlag
	dropLabels   listFlag
	dropPeople   listFlag
	dropRels     listFlag

	message string
	file    string
	edit    bool

	given map[string]bool
}

// was reports whether a flag was given at all, whatever it carries.
func (e editFlags) was(name string) bool { return e.given[name] }

// fields reports whether anything about the issue itself was named. No is what
// opens the editor, the way `git commit` opens one without -m.
func (e editFlags) fields() bool {
	for _, name := range []string{"title", "description", "type", "milestone", "status", "status-reason", "parent"} {
		if e.was(name) {
			return true
		}
	}
	return len(e.labels.values)+len(e.assignees.values)+len(e.rels.values)+
		len(e.dropLabels.values)+len(e.dropPeople.values)+len(e.dropRels.values) > 0
}

// relations reports whether any flag named a link, which is what decides
// between reconciling the relation set and leaving it alone entirely.
func (e editFlags) relations() bool {
	return e.was("parent") || len(e.rels.values) > 0 || len(e.dropRels.values) > 0
}

func cmdEdit(args []string) error {
	flags, targets, err := parseEditFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(editUsage)
		return nil
	}
	if err != nil {
		return err
	}

	// -m has no meaning for an issue: it would have to say which of six fields
	// it was setting, and it does not. On this command it is a comment's text,
	// which is why the milestone is spelled in full here and -m is not it.
	if len(targets) == 1 && flags.was("message") {
		return fmt.Errorf("-m gives a comment its new text; name the comment to rewrite, or use --milestone to file the issue\n%s", editUsage)
	}
	if len(targets) == 2 && flags.fields() {
		return fmt.Errorf("a comment has only its text to change; drop the field flags, or name the issue alone to change its fields\n%s", editUsage)
	}
	if flags.was("message") && flags.was("file") {
		return fmt.Errorf("-m and -F both give the text; drop one\n%s", editUsage)
	}

	s, err := open()
	if err != nil {
		return err
	}
	entID, st, err := s.Find(targets[0], issue.Type)
	if err != nil {
		return err
	}
	if len(targets) == 2 {
		return editComment(s, entID, st, targets[1], flags.message, flags.was("message"), flags.file, flags.was("file"))
	}
	return editIssue(s, entID, st, flags)
}

func parseEditFlags(args []string) (editFlags, []string, error) {
	var flags editFlags
	fs := flag.NewFlagSet("edit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	// Long and short spellings are separate flags to Go's flag package. The
	// names are add's, so the word that filed an issue is the word that
	// changes it — with one deliberate exception: -m is the comment message
	// here, not the milestone, and a short flag may not mean two things.
	for _, name := range []string{"t", "title"} {
		fs.StringVar(&flags.title, name, "", "the issue's one-line summary")
	}
	for _, name := range []string{"d", "description"} {
		fs.StringVar(&flags.description, name, "", "the issue's body")
	}
	for _, name := range []string{"m", "message"} {
		fs.StringVar(&flags.message, name, "", "a comment's new text")
	}
	for _, name := range []string{"l", "label"} {
		fs.Var(&flags.labels, name, "add a label; repeatable, or comma-separated")
	}
	for _, name := range []string{"a", "assignee"} {
		fs.Var(&flags.assignees, name, "add an assignee; repeatable, or comma-separated")
	}
	for _, name := range []string{"e", "edit"} {
		fs.BoolVar(&flags.edit, name, false, "open the editor even when a field was given")
	}
	for _, name := range []string{"F", "file"} {
		fs.StringVar(&flags.file, name, "", "read the title and description (or a comment's text) from a file, or '-' for stdin")
	}
	fs.StringVar(&flags.typ, "type", "", "the kind of work: bug, feature, task")
	fs.StringVar(&flags.milestone, "milestone", "", "the milestone's name")
	fs.StringVar(&flags.status, "status", "", "the raw status value; 'close'/'reopen' are the two common ones")
	fs.StringVar(&flags.statusReason, "status-reason", "", "the raw close reason, or 'none' to clear it")
	fs.StringVar(&flags.parent, "parent", "", "the issue to file this one under")
	fs.Var(&flags.rels, "rel", "link this issue to another: <kind>:<id>; repeatable")
	fs.Var(&flags.dropRels, "no-rel", "drop a link: <kind>:<id>, or <kind> for every one of them")
	fs.Var(&flags.dropLabels, "remove-label", "drop a label; repeatable, or comma-separated")
	fs.Var(&flags.dropPeople, "remove-assignee", "drop an assignee; repeatable, or comma-separated")

	targets, err := parseTargets(fs, "edit", args, editUsage, 2)
	if err != nil {
		return flags, nil, err
	}

	// Which flags were given, under the long name, so that a value of "" can
	// be told from no value at all.
	flags.given = map[string]bool{}
	long := map[string]string{"t": "title", "d": "description", "m": "message", "l": "label", "a": "assignee", "e": "edit", "F": "file"}
	fs.Visit(func(f *flag.Flag) {
		name := f.Name
		if full, ok := long[name]; ok {
			name = full
		}
		flags.given[name] = true
	})
	return flags, targets, nil
}

// editIssue is `git issue edit <id>`: the issue as it stands, changed by
// whatever flags said so, or opened in the editor when none did.
func editIssue(s *entity.Store, id string, st entity.State, flags editFlags) error {
	who, err := author(s)
	if err != nil {
		return err
	}

	fields, err := editedFields(s, id, st, flags)
	if err != nil {
		return err
	}

	// The title and description can come from -F, from piped stdin, or from an
	// editor. -F and a pipe replace the buffer wholesale — first line included,
	// so piping a body can rename the issue the same way retyping the editor's
	// first line does.
	//
	// Implicit stdin is offered only when no field was named at all, which is
	// the one case that has nothing else to go on. Naming a field makes the
	// request complete, and a complete request must never block on input
	// nobody is going to send: `git issue edit <id> -l bug` from a script, a
	// hook or CI inherits a pipe that carries no data and never closes, and
	// reading it hangs forever. Piping a body alongside a field flag is still
	// possible, spelled `-F -`.
	buf, fromInput, err := readBuffer(flags.file, flags.was("file"), !flags.fields())
	if err != nil {
		return err
	}
	switch {
	case fromInput:
		if flags.was("description") {
			return fmt.Errorf("--description and the piped/-F input both give the body; drop one\n%s", editUsage)
		}
		fields = applyBuffer(fields, buf, flags.was("title"))
		if fields.Title == "" {
			return fmt.Errorf("aborting: the title is empty\n%s", editUsage)
		}
	case !flags.fields() || flags.edit:
		// No field named: the editor is the whole request. With one, it opens
		// only when asked for, prefilled with what the flags already say — the
		// same call add makes for --edit.
		if !s.Repo.CanEdit() {
			return fmt.Errorf("no field given, and no editor to open: name a field, use -F, or set core.editor")
		}
		// editFields carries every field but the title and description across
		// untouched: the buffer has no line for them, so an editor session can
		// neither change a link nor clear a status. rels is what the echo in
		// the ignored block shows — the flags' set when one named a link, the
		// issue's current links otherwise.
		rels := fields.Relations
		if rels == nil {
			rels = issue.Relations(st)
		}
		note := editNote(s, fmt.Sprintf("Editing issue %s.", render.Abbrev(id)), fields, rels)
		if fields, err = editFields(s, fields, note); err != nil {
			return err
		}
	}

	changed, detached, err := issue.Update(s, who, time.Now().Unix(), id, st, fields)
	if err != nil {
		return err
	}
	if len(changed) == 0 && len(detached) == 0 {
		fmt.Println("nothing changed")
		return nil
	}
	// A symmetric link is stored at whichever end wrote it, so dropping one
	// writes to an issue nobody named. Say which, rather than leaving it to be
	// discovered in the log.
	for _, far := range detached {
		fmt.Printf("also detached from %s\n", short(far))
	}

	// Re-read rather than render what was just typed: what the listing shows
	// is what the blob folds to, and after a concurrent write those are not
	// always the same thing.
	_, updated, err := s.Find(id, issue.Type)
	if err != nil {
		return err
	}
	render.WriteRow(os.Stdout, "updated ", issue.Row(id, updated), uniqueIDs(s))
	fmt.Printf("%s changed\n", strings.Join(changed, ", "))
	return nil
}

// editedFields applies the flags to what the issue currently says.
//
// It starts from the issue's own state rather than from nothing, so a flag
// changes one field and leaves every other alone — which is what makes the
// flag path and the editor path write the same events.
func editedFields(s *entity.Store, id string, st entity.State, flags editFlags) (issue.Fields, error) {
	fields := issue.FieldsOf(st)

	// A title has no cleared state, since an issue needs one, so 'none' there
	// is simply a title.
	if flags.was("title") {
		fields.Title = flags.title
	}
	for _, f := range []struct {
		name  string
		value string
		into  *string
	}{
		{"description", flags.description, &fields.Description},
		{"type", flags.typ, &fields.Type},
		{"milestone", flags.milestone, &fields.Milestone},
		{"status-reason", flags.statusReason, &fields.StatusReason},
	} {
		if flags.was(f.name) {
			*f.into = clearable(f.value)
		}
	}

	// status is the one scalar with no cleared state: an issue is open when it
	// has no status event, so `git issue reopen` is how you get back there, not
	// `--status none`.
	if flags.was("status") {
		v := strings.TrimSpace(flags.status)
		if v == "" || strings.EqualFold(v, filterNone) {
			return fields, fmt.Errorf("--status needs a value; use 'git issue reopen %s' to set it back to open", render.Abbrev(id))
		}
		fields.Status = v
	}

	var err error
	if fields.Labels, err = editList("label", fields.Labels, flags.labels, flags.dropLabels); err != nil {
		return fields, err
	}
	if fields.Assignees, err = editList("assignee", fields.Assignees, flags.assignees, flags.dropPeople); err != nil {
		return fields, err
	}

	if fields.Relations, err = editRelations(s, id, st, flags); err != nil {
		return fields, err
	}
	return fields, nil
}

// editRelations brings the issue's links to what the flags asked for: --rel
// adds one, --no-rel drops one, and --parent re-files or detaches.
//
// The result is the whole set the issue should end with, because that is what
// issue.Update reconciles against; it works out the adds and the removes that
// name the event ids they undo. Nil — no flag mentioned a link — is what leaves
// the set alone, which is not the same as an empty set asking for all of them
// to go.
func editRelations(s *entity.Store, id string, st entity.State, flags editFlags) ([]issue.Relation, error) {
	if !flags.relations() {
		return nil, nil
	}

	// `--rel none` empties the set, the way `-l none` empties the labels.
	added, none, err := splitNone("relation", flags.rels.values)
	if err != nil {
		return nil, err
	}
	want := []issue.Relation{}
	if !none {
		want = append(want, issue.Relations(st)...)
	}

	for _, arg := range added {
		r, err := resolveRelation(s, id, st, arg)
		if err != nil {
			return nil, err
		}
		want = append(want, r)
	}

	for _, arg := range flags.dropRels.values {
		kind, target, err := issue.ParseRelation(arg)
		if err != nil {
			// A kind on its own drops every link of that kind, which is how a
			// single-valued one is detached without naming what it points at.
			kind, target = strings.ToLower(strings.TrimSpace(arg)), ""
		}
		want = slices.DeleteFunc(want, func(r issue.Relation) bool {
			return r.Kind == kind && (target == "" || strings.HasPrefix(r.Target, target))
		})
	}

	// --parent is the same field said another way, and single-valued: whatever
	// it names replaces every parent the issue had.
	if flags.was("parent") {
		parent, err := resolveParent(s, id, st, flags.parent)
		if err != nil {
			return nil, err
		}
		want = slices.DeleteFunc(want, func(r issue.Relation) bool { return r.Kind == issue.KindParent })
		if parent != "" {
			want = append(want, issue.Relation{Kind: issue.KindParent, Target: parent})
		}
	}
	return want, nil
}

// resolveRelation reads one `<kind>:<id>` and resolves the id against the ref.
//
// A target is required to name an issue this repository holds. A relation to an
// id nothing here knows is far more often a typo than a cross-repo link, and
// docs/issues.md has no representation for the second one yet.
func resolveRelation(s *entity.Store, id string, st entity.State, arg string) (issue.Relation, error) {
	kind, want, err := issue.ParseRelation(arg)
	if err != nil {
		return issue.Relation{}, err
	}
	if kind == issue.KindParent {
		// --rel writes a link; it has no way to say "no link", and a word that
		// resolved to nothing would silently write nothing at all.
		if strings.EqualFold(want, filterNone) {
			return issue.Relation{}, fmt.Errorf("--rel adds a link; to detach, use --parent none or --no-rel parent")
		}
		parent, err := resolveParent(s, id, st, want)
		return issue.Relation{Kind: kind, Target: parent}, err
	}

	target, _, err := s.Find(want, issue.Type)
	if err != nil {
		return issue.Relation{}, err
	}
	if target == id {
		return issue.Relation{}, fmt.Errorf("an issue cannot be linked to itself")
	}
	return issue.Relation{Kind: kind, Target: target}, nil
}

// clearable maps the word that empties a field onto the empty value. 'none' is
// what the filters already call an empty field, so setting one uses the same
// word rather than a --no-<field> flag per field.
func clearable(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), filterNone) {
		return ""
	}
	return value
}

// editList brings one list field to what the flags asked for: -l adds, and
// --remove-label takes away. `-l none` empties it, and cannot be combined with
// a name — the same refusal the filters make, for the same reason.
//
// The result is the whole membership the issue should end with, because that
// is what issue.Update reconciles against; it works out the individual adds
// and the removes that name the event ids they undo.
func editList(field string, have []string, add, drop listFlag) ([]string, error) {
	want, none, err := splitNone(field, add.values)
	if err != nil {
		return nil, err
	}
	if none {
		have = nil
	}
	have = append(slices.Clone(have), want...)

	for _, gone := range drop.values {
		have = slices.DeleteFunc(have, func(v string) bool { return strings.EqualFold(v, gone) })
	}
	return have, nil
}

// resolveParent works out what --parent was asking for: an id to file this
// issue under, or "" to detach it.
//
// A cycle is refused where it can be seen. That is a courtesy rather than a
// guarantee — `parent` is last-write-wins, so two clones can still produce one
// between them offline, which is why a reader breaks cycles too.
func resolveParent(s *entity.Store, id string, st entity.State, want string) (string, error) {
	if want == "" || strings.EqualFold(strings.TrimSpace(want), filterNone) {
		return "", nil
	}

	parent, parentState, err := s.Find(want, issue.Type)
	if err != nil {
		return "", err
	}
	if parent == id {
		return "", fmt.Errorf("an issue cannot be filed under itself")
	}

	// Walk up from the proposed parent. Reaching this issue means the link
	// would close a loop; seen guards against one that is already there.
	seen := map[string]bool{parent: true}
	for at := issue.Parent(parentState); at != ""; {
		if at == id {
			return "", fmt.Errorf("%s is already filed under %s; that would make a loop", render.Abbrev(parent), render.Abbrev(id))
		}
		if seen[at] {
			break
		}
		seen[at] = true
		_, above, err := s.Find(at, issue.Type)
		if err != nil {
			// The chain leaves this repository, so there is no loop to see.
			break
		}
		at = issue.Parent(above)
	}
	return parent, nil
}

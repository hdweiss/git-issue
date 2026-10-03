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
	"github.com/hdweiss/git-issue/internal/render"
	"github.com/hdweiss/git-issue/internal/review"
)

const editUsage = `usage: git review edit <id> [<comment>] [-t <title>] [-d <description>]
                       [-F <path>] [--base <branch>] [--head <branch>]
                       [--revision <sha>] [-l <label>] [-a <who>]
                       [--milestone <name>] [--status <s>] [--status-reason <r>]
                       [--draft] [--no-draft] [--closes <id>] [--parent <id>]
                       [--rel <kind>:<id>] [--no-rel <kind>[:<id>]]
                       [--dismiss <verdict>] [-e] [-m <message>]

       -t, --title <title>          the one-line summary
       -d, --description <text>     the body
       -F, --file <path>            read title + description, or a comment's text,
                                     from a file ('-' = stdin)
           --base <branch>          the branch this merges into
           --head <branch>          the branch this merges from
           --revision <sha>         the commit this review proposes
           --sync                   set --revision from the head branch as it
                                     stands now
       -l, --label <label>          add a label; repeatable, or comma-separated
       -a, --reviewer <who>         ask someone to read it; repeatable
           --remove-label <label>   drop a label
           --remove-reviewer <who>  drop a reviewer
           --milestone <name>       the milestone's name
           --status <state>         the raw status; 'close' / 'reopen' are the
                                     two common ones
           --status-reason <r>      the raw close reason, or 'none' to clear it
           --draft / --no-draft     whether it is soliciting verdicts
           --closes <id>            an issue this closes; repeatable
           --parent <id>            file this review under another one
           --rel <kind>:<id>        any other link; repeatable
           --no-rel <kind>[:<id>]   drop one link, or every link of a kind
           --dismiss <verdict>      retract somebody's verdict by its id
       -e, --edit                   open the editor even when a field was given
       -m, --message <text>         a comment's new text, without opening an editor

With no field flag the review opens in a Markdown editor on its title and
description; the other fields are echoed, read-only, in an ignored block. With
one, the change is written straight away. Either way it is written back as one
event per field that actually changed, so two people editing two different
fields offline both survive the merge.

--revision is what a review's history of revisions is made of: each one is an
event, and the resolving one is the current head. Pushing new commits and then
running --sync is how a review follows its branch. Doing so makes every verdict
cast against the old revision stale, which is reported and never repaired by
moving anything.

--dismiss is the only write here aimed at somebody else's event. Retracting your
own verdict needs no flag — casting another supersedes it — so this exists for
taking an approval off a review, and it names the verdict by the id 'git review
show' prints beside it.

--status writes the raw value a bridge round-trips. 'merged' is refused here: it
is a claim about the code rather than about the review, so it may only be
written by a client that observed the merge. A review you are finished with but
nobody merged is 'git review close'.

Pass 'none' to empty a field: --parent none detaches the review, --milestone
none unfiles it, -l none drops every label, --rel none drops every link.

A second id names a comment inside that review, and the editor opens on its text
alone.`

func cmdEdit(args []string) error {
	opts, targets, err := parseEditFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(editUsage)
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
	id, st, err := s.Find(targets[0], review.Type)
	if err != nil {
		return err
	}

	if len(targets) == 2 {
		return editComment(s, who, id, st, targets[1], opts)
	}
	return editReview(s, who, id, st, opts)
}

func editReview(s *entity.Store, who, id string, st entity.State, opts editOptions) error {
	if opts.dismiss != "" {
		return dismissVerdict(s, who, id, st, opts)
	}

	want := review.FieldsOf(st)
	fields := &want

	if opts.titleSet {
		want.Title = opts.title
	}
	if opts.descSet {
		want.Description = clearable(opts.desc)
	}
	if opts.baseSet {
		want.Base = clearable(opts.base)
	}
	if opts.headSet {
		want.Head = clearable(opts.head)
	}
	if opts.revSet {
		want.HeadSHA = clearable(opts.revision)
	}
	if opts.sync {
		head := headRevision(s, st)
		if head == "" {
			return fmt.Errorf("--sync needs the head branch in this repository; %q does not resolve here", review.HeadRef(st))
		}
		want.HeadSHA = head
	}
	if opts.milestoneSet {
		want.Milestone = clearable(opts.milestone)
	}
	if opts.statusSet {
		if strings.EqualFold(opts.status, review.StatusMerged) {
			return fmt.Errorf("'merged' says the head reached the base, which only a client that saw it happen may write; use 'git review close' for a review nobody merged")
		}
		want.Status = opts.status
	}
	if opts.reasonSet {
		want.StatusReason = clearable(opts.reason)
	}
	if opts.draftSet {
		want.Draft = &opts.draft
	}

	if err := applyLists(fields, st, opts); err != nil {
		return err
	}
	if err := applyRelations(s, fields, st, opts); err != nil {
		return err
	}

	// With no flag at all the editor opens, the way `git commit` does with no
	// -m. -F or a pipe takes its place.
	if !opts.anyField() {
		buf, fromInput, err := readBuffer(opts.file, opts.file != "", true)
		if err != nil {
			return err
		}
		switch {
		case fromInput:
			want = applyBuffer(want, buf, false)
			if want.Title == "" {
				return fmt.Errorf("no title: the first line of the input is empty\n%s", editUsage)
			}
		default:
			if !s.Repo.CanEdit() {
				return fmt.Errorf("no field given, and no editor to open: pass a flag, -F, or set core.editor")
			}
			want, err = editFields(s, want, editNote(s, "Editing review "+render.Abbrev(id)+".", want))
			if err != nil {
				return err
			}
		}
	} else if opts.edit {
		var err error
		want, err = editFields(s, want, editNote(s, "Editing review "+render.Abbrev(id)+".", want))
		if err != nil {
			return err
		}
	}

	changed, detached, err := review.Update(s, who, time.Now().Unix(), id, st, want)
	if err != nil {
		return err
	}
	if len(changed) == 0 && len(detached) == 0 {
		fmt.Println("nothing changed")
		return nil
	}
	if len(changed) > 0 {
		fmt.Printf("%s  %s\n", render.Abbrev(id), strings.Join(changed, ", "))
	}
	// A symmetric link is stored at whichever end wrote it, so letting go of it
	// reaches a review nobody named. Saying so is the price of that.
	for _, other := range detached {
		fmt.Printf("%s  relations (the other end of a symmetric link)\n", render.Abbrev(other))
	}
	return nil
}

// headRevision resolves the review's head branch to a commit in this
// repository, or "".
func headRevision(s *entity.Store, st entity.State) string {
	head := review.HeadRef(st)
	if head == "" {
		return s.Repo.RefSHA("HEAD")
	}
	for _, candidate := range []string{
		"refs/remotes/" + head,
		"refs/heads/" + head,
		head,
	} {
		if sha := s.Repo.RefSHA(candidate); sha != "" {
			return sha
		}
	}
	return ""
}

// applyLists brings the label and reviewer lists to what the flags asked for.
// -l and -a add to what is there; --remove-* takes one away; 'none' clears.
func applyLists(want *review.Fields, st entity.State, opts editOptions) error {
	want.Labels = mergeList(st.List("label"), opts.labels.values, opts.removeLabels.values)
	want.Assignees = mergeList(st.List("assignee"), opts.reviewers.values, opts.removeReviewers.values)
	return nil
}

// mergeList applies additions and removals to a list field's current members.
// The word 'none' among the additions clears the field, which is the same word
// the filters use for "has none".
func mergeList(have, add, remove []string) []string {
	for _, v := range add {
		if strings.EqualFold(v, filterNone) {
			have = nil
			add = nil
			break
		}
	}
	out := append([]string(nil), have...)
	for _, v := range add {
		if !containsFold(out, v) {
			out = append(out, v)
		}
	}
	for _, v := range remove {
		for i := 0; i < len(out); i++ {
			if strings.EqualFold(out[i], v) {
				out = append(out[:i], out[i+1:]...)
				i--
			}
		}
	}
	return out
}

// applyRelations works out the whole set of links the review should end with.
//
// Nil means no flag mentioned them, which is not the same as an empty set
// asking for all of them to go — so the field is only populated when a relation
// flag was actually given.
func applyRelations(s *entity.Store, want *review.Fields, st entity.State, opts editOptions) error {
	if !opts.anyRelation() {
		return nil
	}

	rels := review.Relations(st)
	keep := make([]review.Relation, 0, len(rels))
	drop := map[string]bool{}
	dropKind := map[string]bool{}

	for _, arg := range opts.noRels.values {
		if strings.EqualFold(arg, filterNone) {
			// Every link of every kind.
			want.Relations = []review.Relation{}
			return nil
		}
		if kind, target, err := review.ParseRelation(arg); err == nil {
			// Expanded the same way an add's target is, so --no-rel takes the
			// link --rel wrote rather than missing it by twenty-eight
			// characters.
			full, err := resolveTarget(s, target)
			if err != nil {
				return err
			}
			drop[(review.Relation{Kind: kind, Target: full}).Key()] = true
			continue
		}
		// A bare kind drops every link of that kind.
		dropKind[strings.ToLower(strings.TrimSpace(arg))] = true
	}

	for _, r := range rels {
		if drop[r.Key()] || dropKind[r.Kind] {
			continue
		}
		// --parent replaces the one it keeps at most one of, so the surviving
		// parents go rather than accumulating.
		if opts.parentSet && r.Kind == review.KindParent {
			continue
		}
		keep = append(keep, r)
	}

	// New links only: what survived from the blob already carries a full id,
	// while a flag carries whatever was typed.
	var fresh []review.Relation
	if opts.parentSet && opts.parent != "" && !strings.EqualFold(opts.parent, filterNone) {
		fresh = append(fresh, review.Relation{Kind: review.KindParent, Target: opts.parent})
	}
	for _, target := range opts.closes.values {
		fresh = append(fresh, review.Relation{Kind: review.KindCloses, Target: target})
	}
	for _, arg := range opts.rels.values {
		kind, target, err := review.ParseRelation(arg)
		if err != nil {
			return err
		}
		fresh = append(fresh, review.Relation{Kind: kind, Target: target})
	}
	fresh, err := resolveRelations(s, fresh)
	if err != nil {
		return err
	}

	want.Relations = append(keep, fresh...)
	return nil
}

// dismissVerdict retracts one verdict by the id `show` prints beside it.
func dismissVerdict(s *entity.Store, who, id string, st entity.State, opts editOptions) error {
	var match []review.Verdict
	for _, v := range st.Members(review.VerdictField) {
		if strings.HasPrefix(v.ID, opts.dismiss) {
			for _, folded := range review.VerdictsOf(st) {
				if folded.ID == v.ID {
					match = append(match, folded)
				}
			}
		}
	}
	switch len(match) {
	case 0:
		return fmt.Errorf("unknown verdict '%s'", opts.dismiss)
	case 1:
	default:
		return fmt.Errorf("ambiguous verdict '%s': matches %d verdicts", opts.dismiss, len(match))
	}

	wrote, err := review.Dismiss(s, who, time.Now().Unix(), id, st, match[0], opts.message)
	if err != nil {
		return err
	}
	if !wrote {
		fmt.Println("nothing changed")
		return nil
	}
	fmt.Printf("%s  dismissed %s by %s\n", render.Abbrev(id), match[0].Value, match[0].Author)
	return nil
}

// editComment rewrites one thread entry's text.
func editComment(s *entity.Store, who, id string, st entity.State, prefix string, opts editOptions) error {
	c, err := st.FindComment(prefix)
	if err != nil {
		return err
	}

	body := opts.message
	if body == "" {
		buf, fromInput, err := readBuffer(opts.file, opts.file != "", true)
		if err != nil {
			return err
		}
		if fromInput {
			body = strings.TrimSpace(ignoredBlock.ReplaceAllString(buf, ""))
		} else {
			body, err = editText(s, c.Body.Display(), "Editing a comment on review "+render.Abbrev(id)+".")
			if err != nil {
				return err
			}
		}
	}

	wrote, err := review.EditComment(s, who, time.Now().Unix(), id, st, c, body)
	if err != nil {
		return err
	}
	if !wrote {
		fmt.Println("nothing changed")
		return nil
	}
	fmt.Println(render.Abbrev(c.ID()))
	return nil
}

// editText opens the editor on a comment's own text, with no title line.
func editText(s *entity.Store, body, intro string) (string, error) {
	dir, err := s.Repo.CommonDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, commentFile)

	note := intro + "\n\nEverything outside this block is the comment, in Markdown.\nAn empty comment aborts."
	buf := body + "\n\n<!---\n" + note + "\n-->\n"
	if err := os.WriteFile(path, []byte(buf), 0o644); err != nil {
		return "", err
	}
	if err := s.Repo.EditFile(path); err != nil {
		return "", err
	}
	edited, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(ignoredBlock.ReplaceAllString(string(edited), ""))
	if out == "" {
		return "", fmt.Errorf("aborting: the comment is empty (what you wrote is kept in %s)", path)
	}
	return out, nil
}

// clearable maps the word 'none' onto the empty string a cleared scalar is
// written from, the same word the filters use for "has none".
func clearable(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), filterNone) {
		return ""
	}
	return v
}

type editOptions struct {
	title, desc, base, head, revision string
	milestone, status, reason         string
	parent, dismiss, message, file    string
	draft, sync, edit                 bool

	titleSet, descSet, baseSet, headSet, revSet bool
	milestoneSet, statusSet, reasonSet          bool
	draftSet, parentSet                         bool

	labels, reviewers             listFlag
	removeLabels, removeReviewers listFlag
	rels, noRels, closes          listFlag
}

// anyField reports whether a flag set a field, which is what decides between
// writing straight away and opening the editor.
func (o editOptions) anyField() bool {
	return o.titleSet || o.descSet || o.baseSet || o.headSet || o.revSet || o.sync ||
		o.milestoneSet || o.statusSet || o.reasonSet || o.draftSet ||
		len(o.labels.values) > 0 || len(o.reviewers.values) > 0 ||
		len(o.removeLabels.values) > 0 || len(o.removeReviewers.values) > 0 ||
		o.anyRelation()
}

func (o editOptions) anyRelation() bool {
	return o.parentSet || len(o.rels.values) > 0 || len(o.noRels.values) > 0 || len(o.closes.values) > 0
}

func parseEditFlags(args []string) (editOptions, []string, error) {
	var (
		opts    editOptions
		noDraft bool
	)
	fs := flag.NewFlagSet("edit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, name := range []string{"t", "title"} {
		fs.StringVar(&opts.title, name, "", "the one-line summary")
	}
	for _, name := range []string{"d", "description"} {
		fs.StringVar(&opts.desc, name, "", "the body")
	}
	for _, name := range []string{"F", "file"} {
		fs.StringVar(&opts.file, name, "", "read from a file, or '-' for stdin")
	}
	for _, name := range []string{"m", "message"} {
		fs.StringVar(&opts.message, name, "", "a comment's new text")
	}
	for _, name := range []string{"e", "edit"} {
		fs.BoolVar(&opts.edit, name, false, "open the editor even when a field was given")
	}
	for _, name := range []string{"l", "label"} {
		fs.Var(&opts.labels, name, "add a label; repeatable")
	}
	for _, name := range []string{"a", "reviewer"} {
		fs.Var(&opts.reviewers, name, "add a reviewer; repeatable")
	}
	fs.Var(&opts.removeLabels, "remove-label", "drop a label")
	fs.Var(&opts.removeReviewers, "remove-reviewer", "drop a reviewer")
	fs.StringVar(&opts.base, "base", "", "the branch this merges into")
	fs.StringVar(&opts.head, "head", "", "the branch this merges from")
	fs.StringVar(&opts.revision, "revision", "", "the commit this review proposes")
	fs.BoolVar(&opts.sync, "sync", false, "set the revision from the head branch as it stands now")
	fs.StringVar(&opts.milestone, "milestone", "", "the milestone's name")
	fs.StringVar(&opts.status, "status", "", "the raw status")
	fs.StringVar(&opts.reason, "status-reason", "", "the raw close reason")
	fs.BoolVar(&opts.draft, "draft", false, "mark it a draft")
	fs.BoolVar(&noDraft, "no-draft", false, "mark it ready")
	fs.StringVar(&opts.parent, "parent", "", "file this review under another one")
	fs.StringVar(&opts.dismiss, "dismiss", "", "retract a verdict by its id")
	fs.Var(&opts.closes, "closes", "an issue this review closes; repeatable")
	fs.Var(&opts.rels, "rel", "link to another entity: <kind>:<id>; repeatable")
	fs.Var(&opts.noRels, "no-rel", "drop one link, or every link of a kind")

	targets, err := parseTargets(fs, "edit", args, editUsage, 2)
	if err != nil {
		return opts, nil, err
	}

	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "t", "title":
			opts.titleSet = true
		case "d", "description":
			opts.descSet = true
		case "base":
			opts.baseSet = true
		case "head":
			opts.headSet = true
		case "revision":
			opts.revSet = true
		case "milestone":
			opts.milestoneSet = true
		case "status":
			opts.statusSet = true
		case "status-reason":
			opts.reasonSet = true
		case "draft", "no-draft":
			opts.draftSet = true
		case "parent":
			opts.parentSet = true
		}
	})
	if noDraft {
		if opts.draft {
			return opts, nil, fmt.Errorf("--draft and --no-draft both say what the review is; use one")
		}
		opts.draft = false
	}
	return opts, targets, nil
}

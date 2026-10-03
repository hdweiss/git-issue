// Command git-review is an offline-first review tracker whose entire state
// lives in a git repository's object store.
//
// A review is a change proposal — a base, a head, commentary anchored to the
// code, verdicts, and a terminal state of merged or abandoned. A pull request
// is a review with an entry in the origin ledger; a review of a local branch is
// one that has none, and every command works the same on both.
//
// Installed on PATH it works as a git subcommand. `git review` and
// `git review <id>` are shortcuts for the two commands used most, list and
// show.
//
// The specification is docs/ — storage-model.md for where bytes live,
// blob-format.md for what is inside one entity, reviews.md for the review
// vocabulary.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	// Embedded so timezone lookup works on a host with no tzdata, which a
	// static binary otherwise cannot count on.
	_ "time/tzdata"

	"github.com/hdweiss/git-issue/internal/cli"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/render"
	"github.com/hdweiss/git-issue/internal/review"
)

var version = "dev"

const helpText = `usage: git review <command> [<args>]

examine what is proposed
   list      list reviews, newest first
   show      show one review, or one thread on it
   status    what is blocking a review from merging
   checks    the check runs against a review's head commit
   diff      the change a review proposes, computed from base…head
   log       the tracker's own history, one commit per change

propose and change reviews
   add       open a new review
   edit      change a review's fields, or rewrite a comment
   comment   comment on a review, optionally anchored to a line
   close     close a review, or resolve one thread on it
   reopen    reopen a review, or unresolve one thread
   remove    unlist a review, or retract one comment

read a review
   approve            record that this should merge
   request-changes    record that it should not merge as it stands

work with the code
   checkout  check out a review's head; with a comment, open it where it sits

sync with a git repo or a bridge
   pull      retrieve reviews from a git repo, GitHub, or Azure DevOps
   push      send reviews to a git repo, GitHub, or Azure DevOps

git review with no id is git review list; git review <id> is git review show <id>.

See 'git review <command> --help' for what a command's flags do.`

const reviewUsage = `usage: git review [<id> [<comment>]] [--oneline] [--format <oneline|medium>]
                  [--tree] [--no-tree] [--state <s>] [-l <label>] [-a <who>] [--author <who>]
                  [--base <branch>] [--draft] [--checks <state>]

Shorthand for ` + "`git review list`" + ` and ` + "`git review show <id> [<comment>]`" + ` — the
filters are list's; see 'git review list --help' for what they and the format
flags do. To edit a review, use 'git review edit <id>'.

An id here shows that review, and never lists. To list what is filed under one,
name it after list: 'git review list <id>'.`

const listUsage = `usage: git review list [<id>] [--oneline] [--format <oneline|medium>]
                       [--tree] [--no-tree] [--state <state>] [-l <label>]
                       [-a <who>] [--author <who>] [--base <branch>] [--draft]
                       [--checks <state>]

       <id>                 the whole subtree filed under this review; 'none'
                             for the reviews filed under nothing at all
       --oneline            one review per line: id, status, lifecycle, title,
                            checks, verdict counts and comment count (the default)
       --format medium      a block per review, like git log

       --tree               nest each review under its parent, even down a pipe
       --no-tree            list flat, even on a terminal

       --state <state>      open, merged, closed, all (the default), or a
                             bridged forge's own state
       --base <branch>      reviews targeting this branch
       --draft              only drafts; --no-draft excludes them
       --checks <state>     passing, failing, pending, or none
   -l, --label <label>      reviews carrying this label; repeatable, AND-ed
   -a, --reviewer <who>     reviews this person was asked to read; repeatable
       --author <who>       reviews opened by an identity containing this text

Filters combine: a review is listed only when it passes all of them. Matching
ignores case throughout; --author is a substring, the rest match a whole value.

--checks reads the checks ref, which is not core state: a clone that has never
fetched it holds no check runs, so every review answers '--checks none' there
rather than the filter failing.

On a terminal the lifecycle column is a glyph — 🔍 under review, 📝 draft,
🔀 merged, 🚫 closed — and the status word becomes a coloured dot. Piped, or
with NO_COLOR set, the words come back and nothing is truncated.`

const showUsage = `usage: git review show <id> [<comment>]

Prints the review in full: its branches and revision, each person's current
verdict, the head commit's check runs, and every thread — where in the code it
sits, whether it is resolved, and whether its anchor still describes the head.

An anchored thread carries its file and line range on the entry itself, with the
commit those line numbers count against: 'git show <revision>:<path>' is the
code the comment was written about. The index at the end lists the same threads
together with their reply counts.

A second id names a comment inside that review, and prints that entry and the
replies under it. Comment ids abbreviate, and the prefix only has to be unique
within the review.

A thread is marked 'outdated' when the file it is anchored to changed between
the commit it was written against and the current head, and 'detached' when
that commit is not reachable from the head at all — a rebase or an amend. Note
that these are different: an anchor's commit stays an ancestor across every
ordinary push, so reachability alone would call a thread current long after the
lines under it were replaced. Neither is ever repaired by moving the anchor.

--web opens the review's page on the forge it came from, using 'git web--browse'.

Output longer than a screen opens in a pager, the way 'git show' does.`

type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit status %d", int(c)) }

func main() {
	err := run(os.Args[1:])
	if err == nil {
		return
	}
	var code exitCode
	if errors.As(err, &code) {
		os.Exit(int(code))
	}
	fmt.Fprintf(os.Stderr, "git-review: %s\n", err)
	os.Exit(1)
}

// run dispatches on the first argument only when it is one of a fixed set of
// reserved words. Anything else, including nothing at all, is the shortcut
// form: `git review` for `git review list`, `git review <id>` for
// `git review show <id>`.
func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "add":
			return cmdAdd(args[1:])
		case "list":
			return cmdList(args[1:])
		case "show":
			return cmdShow(args[1:])
		case "status":
			return cmdStatus(args[1:])
		case "checks":
			return cmdChecks(args[1:])
		case "diff":
			return cmdDiff(args[1:])
		case "checkout":
			return cmdCheckout(args[1:])
		case "comment":
			return cmdComment(args[1:])
		case "edit":
			return cmdEdit(args[1:])
		case "close":
			return cmdClose(args[1:])
		case "reopen":
			return cmdReopen(args[1:])
		case "approve":
			return cmdVerdict(review.VerdictApprove, args[1:])
		case "request-changes":
			return cmdVerdict(review.VerdictRequestChanges, args[1:])
		case "remove":
			return cmdRemove(args[1:])
		case "log":
			return cmdLog(args[1:])
		case "pull":
			return cmdPull(args[1:])
		case "push":
			return cmdPush(args[1:])
		// destroy has no line in helpText: it deletes every review in the
		// repository and is not part of the day-to-day vocabulary.
		case "destroy":
			return cmdDestroy(args[1:])
		case "--version":
			fmt.Println("git-review", version)
			return nil
		case "help", "--help", "-h":
			fmt.Println(helpText)
			return nil
		}
	}
	return cmdReview(args)
}

// open is the review store.
//
// No bridge vocabulary is composed in yet, unlike git-issue's: neither bridge
// contributes a namespaced review scalar. The seam is Vocabulary.With, and this
// is where it would be used.
func open() (*entity.Store, error) {
	return entity.OpenStore("", review.RefOpen, review.Vocabulary)
}

// openChecks is the checks ref, which is not an entity namespace: its keys are
// commit shas, its blobs have no create event, and nothing on it has an entity
// id. See docs/reviews.md.
//
// A repository that has never fetched it opens fine and reads empty, which is
// the behaviour every caller here is built for — core state may never depend on
// this ref.
func openChecks(repo *gitx.Repo) *entity.Store {
	return entity.NewStore(repo, review.RefChecks, review.ChecksVocabulary)
}

// checksFor is the head commit's check runs, or nothing.
//
// Never an error: a missing ref, an unfetched commit and a commit nobody has
// run anything against are all "no checks", and none of them should stop a
// review from being shown.
func checksFor(repo *gitx.Repo, st entity.State) []review.Check {
	head := review.Head(st)
	if head == "" {
		return nil
	}
	checks, err := review.LoadChecks(openChecks(repo), head)
	if err != nil {
		return nil
	}
	return checks
}

// uniqueIDs measures how much of each entity id a reader has to type for it to
// name one review and no other.
func uniqueIDs(s *entity.Store) render.Unique {
	notes, err := s.Notes()
	if err != nil {
		return nil
	}
	return render.Uniquify(entity.IDs(notes))
}

// The listing formats, named after git log's own.
const (
	formatOneline = "oneline"
	formatMedium  = "medium"
)

type listOptions struct {
	format string
	tree   bool
	noTree bool
	under  string
}

// cmdReview is the default command: `git review` on its own, `git review <id>`,
// and every flagged variant of either.
func cmdReview(args []string) error {
	opts, filter, web, rest, err := parseReviewFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(reviewUsage)
		return nil
	}
	if err != nil {
		return err
	}

	if web {
		if len(rest) == 0 {
			return fmt.Errorf("--web opens one review's page; name the review\n%s", reviewUsage)
		}
		if opts.format != "" || opts.tree || opts.noTree || filter.active() {
			return fmt.Errorf("--web opens one review; it takes no listing flags\n%s", reviewUsage)
		}
		return openReviewWeb(rest)
	}

	if len(rest) > 0 {
		if opts.format != "" {
			return fmt.Errorf("--oneline and --format list reviews; git review <id> shows one, not both\n%s", reviewUsage)
		}
		if opts.tree || opts.noTree {
			return fmt.Errorf("--tree and --no-tree shape a listing; to list what is filed under one review, use 'git review list %s'\n%s", rest[0], reviewUsage)
		}
		if filter.active() {
			return fmt.Errorf("filters narrow a listing; git review <id> shows one review, unfiltered\n%s", reviewUsage)
		}
		return showReview(rest)
	}
	return listReviews(opts, filter)
}

func parseReviewFlags(args []string) (opts listOptions, filter listFilter, web bool, rest []string, err error) {
	var (
		oneline bool
		labels  listFlag
		who     listFlag
	)
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bindListFlags(fs, &opts, &oneline)
	bindFilterFlags(fs, &filter, &labels, &who)
	fs.BoolVar(&web, "web", false, "open the review's upstream page in a browser")

	rest, err = parsePermuted(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, filter, web, nil, err
		}
		return opts, filter, web, nil, fmt.Errorf("%s\n%s", err, reviewUsage)
	}

	if ferr := resolveFilter(&filter, labels, who); ferr != nil {
		return opts, filter, web, nil, fmt.Errorf("%s\n%s", ferr, reviewUsage)
	}
	if ferr := resolveFormat(&opts, oneline, reviewUsage); ferr != nil {
		return opts, filter, web, nil, ferr
	}
	return opts, filter, web, rest, nil
}

func bindListFlags(fs *flag.FlagSet, opts *listOptions, oneline *bool) {
	fs.StringVar(&opts.format, "format", "", "oneline or medium")
	fs.BoolVar(oneline, "oneline", false, "one review per line; the default")
	fs.BoolVar(&opts.tree, "tree", false, "nest each review under its parent, even down a pipe")
	fs.BoolVar(&opts.noTree, "no-tree", false, "list flat, even on a terminal")
}

func resolveFormat(opts *listOptions, oneline bool, usage string) error {
	if opts.tree && opts.noTree {
		return fmt.Errorf("--tree and --no-tree both say how to draw a listing; use one")
	}
	if oneline {
		if opts.format != "" && opts.format != formatOneline {
			return fmt.Errorf("--oneline and --format %s both name a format; use one", opts.format)
		}
		opts.format = formatOneline
	}
	switch opts.format {
	case "", formatOneline:
	case formatMedium:
		if opts.tree {
			return fmt.Errorf("--tree nests a listing and --format medium prints a block per review; use one")
		}
	default:
		return fmt.Errorf("unknown format '%s'; try oneline or medium\n%s", opts.format, usage)
	}
	return nil
}

// resolveTree settles whether a oneline listing nests: --tree and --no-tree say
// so outright, and with neither it nests on a terminal and stays flat down a
// pipe.
func resolveTree(opts listOptions) bool {
	switch {
	case opts.noTree:
		return false
	case opts.tree:
		return true
	default:
		return gitx.IsTerminal(os.Stdout)
	}
}

func cmdList(args []string) error {
	opts, filter, err := parseListFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(listUsage)
		return nil
	}
	if err != nil {
		return err
	}
	return listReviews(opts, filter)
}

func parseListFlags(args []string) (listOptions, listFilter, error) {
	var (
		opts    listOptions
		oneline bool
		filter  listFilter
		labels  listFlag
		who     listFlag
	)
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bindListFlags(fs, &opts, &oneline)
	bindFilterFlags(fs, &filter, &labels, &who)

	rest, err := parsePermuted(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, filter, err
		}
		return opts, filter, fmt.Errorf("%s\n%s", err, listUsage)
	}
	if len(rest) > 1 {
		return opts, filter, fmt.Errorf("list takes one review to list under, got '%s' as well\n%s", rest[1], listUsage)
	}
	if len(rest) == 1 {
		opts.under = rest[0]
	}

	if ferr := resolveFilter(&filter, labels, who); ferr != nil {
		return opts, filter, fmt.Errorf("%s\n%s", ferr, listUsage)
	}
	if ferr := resolveFormat(&opts, oneline, listUsage); ferr != nil {
		return opts, filter, ferr
	}
	if opts.format == "" {
		opts.format = formatOneline
	}
	return opts, filter, nil
}

func listReviews(opts listOptions, filter listFilter) error {
	format := opts.format
	if format == "" {
		format = formatOneline
	}

	s, err := open()
	if err != nil {
		return err
	}
	notes, err := s.Notes()
	if err != nil {
		return err
	}

	uniq := render.Uniquify(entity.IDs(notes))

	root, err := listRoot(s, opts.under)
	if err != nil {
		return err
	}

	// The checks ref is read once for the whole listing rather than per review:
	// it is one batch over one ref, and asking per row would be a fold of it per
	// review. A ref this clone does not hold reads empty and every row simply
	// carries no check column.
	runs := loadAllChecks(s)

	var (
		matched    []string
		states     = map[string]entity.State{}
		rootPassed bool
	)
	h, err := survey(s, notes, runs, func(id string, st entity.State) {
		if !filter.matches(st, runs[review.Head(st)]) {
			return
		}
		if id == root {
			rootPassed = true
		}
		matched = append(matched, id)
		if format == formatMedium {
			states[id] = st
		}
	})
	if err != nil {
		return err
	}
	if opts.under != "" {
		under := filedUnder(h.parents, root)
		matched = slices.DeleteFunc(matched, func(id string) bool { return !under[id] })
	}

	p := cli.StartPager(s)
	defer p.Finish()

	if format == formatMedium {
		links := indexLinks(h)
		entries := make([]render.LogEntry, 0, len(matched))
		for _, id := range matched {
			st := states[id]
			entries = append(entries, review.Log(id, st, time.Local, links.of(id), runs[review.Head(st)]))
		}
		render.WriteLog(p, entries, uniq)
		return nil
	}

	rows := make([]render.Row, 0, len(matched))
	for _, id := range matched {
		rows = append(rows, h.rows[id])
	}

	if !resolveTree(opts) {
		render.WriteList(p, rows, uniq)
		return nil
	}

	// A tree drawn under a named root shows that root as the spine's top even
	// when a filter would have dropped it, so the branches have something to
	// hang from.
	if root != "" && !rootPassed {
		if row, ok := h.rows[root]; ok {
			rows = append(rows, row)
		}
	}
	render.WriteTree(p, nest(render.SortRows(rows), h.parents), uniq)
	return nil
}

// listRoot resolves the positional argument a listing is scoped to. The word
// `none` is the reviews filed under nothing, spelled the way the filters spell
// "has none".
func listRoot(s *entity.Store, under string) (string, error) {
	switch under {
	case "":
		return "", nil
	case filterNone:
		return "", nil
	}
	id, _, err := s.Find(under, review.Type)
	return id, err
}

// loadAllChecks folds the whole checks ref into a map from commit sha to that
// commit's current check results.
//
// Read once per listing. A missing ref, or one that cannot be read, is an empty
// map and never an error: core state may not depend on it.
func loadAllChecks(s *entity.Store) map[string][]review.Check {
	store := openChecks(s.Repo)
	notes, err := store.Notes()
	if err != nil || len(notes) == 0 {
		return map[string][]review.Check{}
	}
	out := make(map[string][]review.Check, len(notes))
	_ = store.Each(notes, func(commit string, st entity.State) error {
		if checks := review.ChecksOf(st); len(checks) > 0 {
			out[commit] = checks
		}
		return nil
	})
	return out
}

// cmdShow prints a review, or — given a second id — one comment inside it.
func cmdShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	web := fs.Bool("web", false, "open the review's upstream page in a browser")
	targets, err := parseTargets(fs, "show", args, showUsage, 2)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(showUsage)
		return nil
	}
	if err != nil {
		return err
	}
	if *web {
		return openReviewWeb(targets)
	}
	return showReview(targets)
}

// parseTargets parses what a command addresses: a review id, and for the
// commands that reach inside one, the id of a comment in it.
func parseTargets(fs *flag.FlagSet, name string, args []string, usage string, max int) ([]string, error) {
	rest, err := parsePermuted(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, fmt.Errorf("%s\n%s", err, usage)
	}
	if len(rest) == 0 || rest[0] == "" {
		return nil, fmt.Errorf("%s", usage)
	}
	if len(rest) > max {
		if max == 1 {
			return nil, fmt.Errorf("%s takes one review, got '%s' as well\n%s", name, rest[max], usage)
		}
		return nil, fmt.Errorf("%s takes a review and at most one comment, got '%s' as well\n%s", name, rest[max], usage)
	}
	return rest, nil
}

// parsePermuted parses flags that appear before, after, or around the
// positional arguments, and returns the positionals.
//
// Go's flag package stops at the first non-flag argument, so plain Parse reads
// `close abc -m x` as one positional plus two ignored words — the flag is
// silently dropped rather than refused, which is the worst of the options.
func parsePermuted(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

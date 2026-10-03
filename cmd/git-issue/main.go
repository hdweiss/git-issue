// Command git-issue is an offline-first issue tracker whose entire state lives
// in a git repository's object store.
//
// Installed on PATH it works as a git subcommand: add, list, show, comment,
// edit, remove and pull. `git issue` and `git issue <id>` are shortcuts for the
// two commands used most, list and show.
//
// The specification is docs/ — storage-model.md for where bytes live,
// blob-format.md for what is inside one entity, issues.md for the issue
// vocabulary.
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

	// Embedded so timezone lookup works on a host with no tzdata, which a
	// static binary otherwise cannot count on.
	_ "time/tzdata"

	giteaapi "github.com/hdweiss/git-issue/internal/bridge/gitea/api"
	giteaissue "github.com/hdweiss/git-issue/internal/bridge/gitea/issue"
	ghissue "github.com/hdweiss/git-issue/internal/bridge/github/issue"
	"github.com/hdweiss/git-issue/internal/cli"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/issue"
	"github.com/hdweiss/git-issue/internal/origins"
	"github.com/hdweiss/git-issue/internal/remote"
	"github.com/hdweiss/git-issue/internal/render"
)

var version = "dev"

// helpText is what `help`, `--help` and `-h` print: the command list and the
// two shortcuts, with no flag detail — that lives under each command's own
// --help, which this points to.
const helpText = `usage: git issue <command> [<args>]

examine what is filed
   list      list issues, newest first
   show      show one issue, or one comment on it
   log       the tracker's own history, one commit per change

file and change issues
   add       create a new issue
   edit      change an issue's fields, or rewrite a comment
   close     close an issue; --as gives a reason
   reopen    reopen a closed issue
   comment   post a comment on an issue
   remove    unlist an issue, or retract one comment

sync with a git repo or a bridge
   pull      retrieve issues from a git repo or a bridge
   push      send issues to a git repo or a bridge

git issue with no id is git issue list; git issue <id> is git issue show <id>.

See 'git issue <command> --help' for what a command's flags do.`

// issueUsage is what the shortcuts' own errors are appended with — a bad flag
// combination on a bare `git issue` or `git issue <id>`. It stays minimal and
// points at list/show for the flags, rather than repeating them.
const issueUsage = `usage: git issue [<id> [<comment>]] [--oneline] [--format <oneline|medium>]
                 [--tree] [--no-tree] [--state <s>] [--type <t>] [-l <label>] [-a <who>] [--author <who>]

Shorthand for ` + "`git issue list`" + ` and ` + "`git issue show <id> [<comment>]`" + ` — the
filters are list's; see 'git issue list --help' for what they and the format
flags do. To edit an issue, use 'git issue edit <id>'.

An id here shows that issue, and never lists. To list what is filed under one,
name it after list: 'git issue list <id>'.`

// listUsage spells out the formats and the filters, which are the parts of list
// that are not obvious. The format names are git log's own.
const listUsage = `usage: git issue list [<id>] [--oneline] [--format <oneline|medium>]
                      [--tree] [--no-tree] [--state <state>] [--type <type>]
                      [-l <label>] [-a <assignee>] [--author <who>]

       <id>                 the whole subtree filed under this issue; 'none'
                             for the issues filed under nothing at all
       --oneline            one issue per line: id, status, type, title, labels
                            and comment count (the default)
       --format medium      a block per issue, like git log: the header lines
                            show prints, then the title alone
       --tree               nest each issue under its parent, even down a pipe
       --no-tree            list flat, even on a terminal

       --state <state>      open, closed, all (the default), or a bridged
                             tracker's own state, e.g. Resolved
       --type <type>        issues whose type matches
   -l, --label <label>      issues carrying this label; repeatable, AND-ed
   -a, --assignee <who>     issues with this assignee; repeatable, AND-ed
       --author <who>       issues opened by an identity containing this text

Filters mirror the add flags and combine: an issue is listed only when it
passes all of them. Matching ignores case throughout; --author is a substring,
the rest match a whole value. Pass 'none' to -l or -a for the issues that have
no label, or no assignee, at all.

An id scopes the listing the way it does on add: there it says what to file a
new issue under, here what to list under. It selects the whole subtree beneath
that issue — the same issues however the listing is drawn. To see that issue
itself rather than what is under it, use 'git issue show <id>' — or
'git issue <id>', which is the same thing, and is why an id alone never lists.

A oneline listing nests under each issue's parent on a terminal and prints flat
down a pipe or into a file, so what a person reads is a tree and what a program
parses keeps its columns. --tree forces the tree anywhere; --no-tree forces it
off. The tree keeps the id, status and type columns as a grid and puts the
hierarchy in the title column, so what you scan and copy stays where it is. An
issue whose parent is not in the listing is a root, marked with an arrow when
it has one: the parent may be filtered out, in another repository, or not
imported, and the tree says so rather than implying there is nothing above it.
Siblings keep a listing's own order, newest first. --tree and --format medium
are refused together — a header block has nowhere to put a spine.

On a terminal the oneline format replaces the status word with a coloured dot —
green for open, purple for closed — and shortens a title that would otherwise
wrap onto a second line. An id is bright only as far as it has to be to name
one issue and no other, and faded after that, so it shows how much of itself
you need to type. Piped, or with NO_COLOR set, the word is printed instead and
nothing is truncated, so the output stays greppable.`

// showUsage is deliberately thin: show only reads. Editing is a separate
// command, so that a read command can never also be the one that writes.
const showUsage = `usage: git issue show <id> [<comment>]

Prints the issue in full. Ids abbreviate like object names, and an ambiguous
prefix is refused rather than resolved arbitrarily. On a terminal every id it
prints — the issue's own and each comment's — is bright as far as it has to be
to name that one thing, and faded after that. To edit it instead, use
'git issue edit <id>'.

A second id names a comment inside that issue, and prints that entry and the
replies under it rather than the whole thread. Comment ids abbreviate too, and
the prefix only has to be unique within the issue.

--web opens the issue's page on the tracker it was pulled from or pushed to,
using 'git web--browse' so the browser is git's web.browser config. It prints
nothing and takes no comment; an issue with no upstream has no page to open.

Output longer than a screen opens in a pager, the way 'git show' does. The
pager is $GIT_PAGER, then core.pager, then $PAGER, then less; set any of them
to 'cat' to page nothing.`

// editUsage explains what "one event per field" buys, since that is why an
// edit is safe to run concurrently with someone else's, where a snapshot
// write would not be.
const editUsage = `usage: git issue edit <id> [<comment>] [-t <title>] [-d <description>]
                      [-F <path>] [--type <type>] [-l <label>] [-a <assignee>]
                      [--milestone <name>] [--status <s>] [--status-reason <r>]
                      [--parent <id>] [--rel <kind>:<id>] [-e] [-m <message>]

       -t, --title <title>          the one-line summary
       -d, --description <text>     the body
       -F, --file <path>            read title + description, or a comment's text,
                                     from a file ('-' = stdin)
           --type <type>            bug, feature, task — the vocabulary is open
       -l, --label <label>          add a label; repeatable, or comma-separated
       -a, --assignee <who>         add an assignee; repeatable, or comma-separated
           --remove-label <label>   drop a label the issue carries
           --remove-assignee <who>  drop an assignee
           --milestone <name>       the milestone's name
           --status <state>         the raw status; 'git issue close' / 'reopen'
                                     are the two common ones
           --status-reason <r>      the raw close reason, or 'none' to clear it
           --parent <id>            file this issue under another one
           --rel <kind>:<id>        link to another issue; repeatable
           --no-rel <kind>[:<id>]   drop one link, or every link of a kind
       -e, --edit                   open the editor even when a field was given
       -m, --message <text>         a comment's new text, without opening an editor

With no field flag the issue opens in a Markdown editor on its title and
description; the other fields are echoed, read-only, in an ignored block. With
one, the change is written straight away and nothing opens. -F, or piped stdin,
takes the place of that editor — first line the title, the rest the description,
so 'cat notes.md | git issue edit <id>' rewrites both; pass -t or -d to pin one.
Either way it is written back as one event per field that actually changed — a
field nobody touched is left alone — so two people can edit two different fields
of the same issue offline and have both survive the merge. A blank first line
aborts, and an edit that changes nothing writes nothing.

Pass 'none' to empty a field: --parent none detaches the issue, --milestone
none unfiles it, -l none drops every label, --rel none drops every link. An empty string does the same. The
word means what it means in a listing, where 'git issue list -l none' selects
the issues carrying no label. A title is the exception — an issue needs one, so
'none' there is simply a title. So is --status: an issue with no status event
is open, so 'git issue reopen' is how it goes back, not '--status none'. The
close reason clears with 'none' like any other scalar.

--status and --status-reason write the raw values a bridge round-trips. For the
day-to-day open/closed switch, 'git issue close' and 'git issue reopen' are the
verbs — close takes '--as completed|not-planned|duplicate' for the reason.

-l and -a add to what is already there rather than replacing it; --remove-label
and --remove-assignee take one member away, and -l none / -a none clear it. --rel
does the same for links, in kinds: parent, blocked-by,
duplicate-of and related are the ones with names here, and a kind this build
has never heard of is written as given rather than refused. --parent is the
same field said another way, and keeps at most one.

-m is a comment's new text here, not a milestone as it is on add. It applies
only when a comment is named, and the milestone is spelled --milestone in full:
one short flag may not mean two things.

A second id names a comment inside that issue instead, and the editor opens on
its text alone. Comment ids abbreviate like object names and are what 'git
issue show' prints above each entry; the prefix only has to be unique within
the issue, and on a terminal show prints exactly that much of it bright. The rewrite is an event of its own addressing the entry by id, so
the original text stays in the blob and the thread keeps its history.`

// removeUsage explains what "unlist" means, since remove looking recoverable
// — and then not always being so under a concurrent edit — is not obvious
// from the name alone.
const removeUsage = `usage: git issue remove <id> [<comment>]

Unlists the issue; it does not erase it. The blob stays in the notes ref's
own history, so this is recoverable locally, and a clone that has not
fetched the removal still has the issue in full. It does not converge: a
concurrent edit elsewhere resurrects the whole issue on the next merge.

A second id retracts one comment inside that issue instead. That one does
converge — it is a tombstone event, not a missing tree entry — and it hides
the entry's body without erasing the text or touching any reply to it.`

// pullUsage spells out the target syntax, which is the part of pull that is
// not obvious: the scheme picks where issues are read from.
const pullUsage = `usage: git issue pull [--all] [--full] [--since <date>] [--limit <n>] [--type <type>] [--token <token>] [[git:|github:|ado:|gitea:]<remote>]

       (no remote)          the current branch's own remote: always its notes
                             ref, and its issues too if it is GitHub, Azure DevOps or Gitea
       origin, git:origin   another copy of the notes refs
       github:origin        GitHub's API, via the bridge
       github:owner/name    a repository this clone has no remote for
       ado:origin           Azure DevOps, via the bridge
       ado:origin#Web/Auth  only that area and everything under it
       ado:origin#          forget the saved area and choose again
       gitea:origin         Gitea or Forgejo, via the bridge

       --all                import closed issues too; the default is open only
       --full               re-read everything, ignoring where the last import got to
       --since <date>       import issues updated since a date, e.g. 2026-01-01
       --limit <n>          stop after this many issues
       --type <type>        import only these types; repeatable, or comma-separated
       --token <token>      use this token instead of the ones git and gh hold

A bridge import resumes from where the last one finished, so a repeat pull asks
only for what changed. A git pull is incremental already: git sends the objects
this clone is missing and nothing else.

An import that writes more than a thousand commits repacks the object store on
its way out, and says so. Replaying whole issue histories leaves the notes ref
several times slower to read until its deltas are recomputed, which is what
'git repack -adf' does and what 'git gc' does not. Set gc.auto to 0 to skip it.`

// exitCode is a command's request to exit with a specific status and say
// nothing further. It exists for the commands that hand a terminal straight to
// git: git has already reported whatever went wrong on its own stderr, and
// wrapping that in a second message of ours would only say it twice.
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
	fmt.Fprintf(os.Stderr, "git-issue: %s\n", err)
	os.Exit(1)
}

// run dispatches on the first argument only when it is one of a fixed set of
// reserved words — add, list, show, comment, edit, remove, pull and help. Anything
// else, including nothing at all, is the shortcut form: `git issue` for
// `git issue list`, `git issue <id>` for `git issue show <id>`.
//
// The shortcut is not itself a word, so an id can never collide with it. It
// can collide with a reserved word, though: "add" is coincidentally a valid
// hex prefix. That is the same tradeoff git itself makes between commands and
// refs, and it is accepted here for the same reason — reserved words are few,
// short, and not the shape a hash prefix is usually typed as.
func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "add":
			return cmdAdd(args[1:])
		case "list":
			return cmdList(args[1:])
		case "show":
			return cmdShow(args[1:])
		case "comment":
			return cmdComment(args[1:])
		case "edit":
			return cmdEdit(args[1:])
		case "close":
			return cmdClose(args[1:])
		case "reopen":
			return cmdReopen(args[1:])
		case "remove":
			return cmdRemove(args[1:])
		case "log":
			return cmdLog(args[1:])
		case "pull":
			return cmdPull(args[1:])
		case "push":
			return cmdPush(args[1:])
		// destroy has no line in helpText: it deletes every issue in the
		// repository and is not part of the day-to-day vocabulary.
		case "destroy":
			return cmdDestroy(args[1:])
		// version has no word of its own — only the flag, to keep the command
		// list short for the thing almost nobody types.
		case "--version":
			fmt.Println("git-issue", version)
			return nil
		// Asking for help is not an error: it goes to stdout and exits zero, so
		// it can be piped and so a script does not fail on it.
		case "help", "--help", "-h":
			fmt.Println(helpText)
			return nil
		}
	}
	return cmdIssue(args)
}

// open is the issue store, with every bridge's vocabulary composed in.
//
// The issue type stays platform-neutral and each bridge contributes its own
// namespaced scalars, so this line is the only place that knows which bridges
// are compiled in.
func open() (*entity.Store, error) {
	vocab := issue.Vocabulary.With(ghissue.Vocabulary).With(giteaissue.Vocabulary)
	return entity.OpenStore("", issue.RefOpen, vocab)
}

// uniqueIDs measures how much of each entity id a reader has to type for it to
// name one issue and no other. A rendering shows that much in full colour and
// fades the rest, so an id says where it stops mattering.
//
// Measured over the whole ref rather than over the rows about to be printed:
// what the id has to survive is Store.Resolve, which sees every issue however
// the listing was filtered.
//
// A ref that cannot be read yields nil, which paints ids whole. How an id is
// coloured is not worth failing a command over.
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

// listOptions is how a listing is shaped rather than which issues it holds —
// the filters are listFilter's job.
type listOptions struct {
	format string
	// tree and noTree are the --tree / --no-tree flags. With neither set a
	// listing nests on a terminal and prints flat down a pipe; see resolveTree.
	tree   bool
	noTree bool
	// under is the positional argument: the issue whose subtree to list, or
	// the word `none` for the issues filed under nothing. Empty is every
	// issue.
	under string
}

// cmdIssue is the default command: `git issue` on its own, `git issue <id>`,
// and every flagged variant of either. No id lists; one id shows it. Neither
// form edits — that is `git issue edit <id>`, its own command.
func cmdIssue(args []string) error {
	opts, filter, web, rest, err := parseIssueFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(issueUsage)
		return nil
	}
	if err != nil {
		return err
	}
	if len(rest) > 2 {
		return fmt.Errorf("git issue takes an issue and at most one comment, got '%s' as well\n%s", rest[2], issueUsage)
	}
	if web {
		if len(rest) == 0 {
			return fmt.Errorf("--web opens one issue; name it\n%s", issueUsage)
		}
		if opts.format != "" || opts.tree || opts.noTree || filter.active() {
			return fmt.Errorf("--web opens one issue; it takes no listing flags\n%s", issueUsage)
		}
		return openIssueWeb(rest)
	}

	if len(rest) > 0 {
		if opts.format != "" {
			return fmt.Errorf("--oneline and --format list issues; git issue <id> shows one, not both\n%s", issueUsage)
		}
		// --tree and --no-tree are refused here rather than taken to mean the
		// subtree. An id alone shows an issue; letting a flag turn that into a
		// listing is exactly the ambiguity `list <id>` exists to keep out of
		// the shortcut.
		if opts.tree || opts.noTree {
			return fmt.Errorf("--tree and --no-tree shape a listing; to list what is filed under one issue, use 'git issue list %s'\n%s", rest[0], issueUsage)
		}
		if filter.active() {
			return fmt.Errorf("filters narrow a listing; git issue <id> shows one issue, unfiltered\n%s", issueUsage)
		}
		return showIssue(rest)
	}
	return listIssues(opts, filter)
}

// parseIssueFlags resolves the default command's flags without yet knowing
// whether an id follows: the format and filter flags only make sense on the
// no-id form, and cmdIssue is what tells them apart once rest is known.
func parseIssueFlags(args []string) (opts listOptions, filter listFilter, web bool, rest []string, err error) {
	var (
		oneline   bool
		labels    listFlag
		assignees listFlag
	)
	fs := flag.NewFlagSet("issue", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bindListFlags(fs, &opts, &oneline)
	bindFilterFlags(fs, &filter, &labels, &assignees)
	fs.BoolVar(&web, "web", false, "open the issue's upstream page in a browser")

	rest, err = parsePermuted(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, filter, web, nil, err
		}
		return opts, filter, web, nil, fmt.Errorf("%s\n%s", err, issueUsage)
	}

	if ferr := resolveFilter(&filter, labels, assignees); ferr != nil {
		return opts, filter, web, nil, fmt.Errorf("%s\n%s", ferr, issueUsage)
	}
	if ferr := resolveFormat(&opts, oneline, issueUsage); ferr != nil {
		return opts, filter, web, nil, ferr
	}
	return opts, filter, web, rest, nil
}

// bindListFlags registers the flags that shape a listing rather than narrow it.
func bindListFlags(fs *flag.FlagSet, opts *listOptions, oneline *bool) {
	fs.StringVar(&opts.format, "format", "", "oneline or medium")
	fs.BoolVar(oneline, "oneline", false, "one issue per line; the default")
	fs.BoolVar(&opts.tree, "tree", false, "nest each issue under its parent, even down a pipe")
	fs.BoolVar(&opts.noTree, "no-tree", false, "list flat, even on a terminal")
}

// resolveFormat settles which format was asked for, and refuses two answers.
//
// --oneline is the same request as --format oneline, spelled the short way git
// log spells it. Both naming a format and disagreeing is refused rather than
// resolved by precedence, which is the call pull makes for --full with --since:
// a winner picked here is one the caller has to guess at.
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
		// A header block has nowhere to put a spine, so there is no honest way
		// to draw a forest in it.
		if opts.tree {
			return fmt.Errorf("--tree nests a listing and --format medium prints a block per issue; use one")
		}
	default:
		return fmt.Errorf("unknown format '%s'; try oneline or medium\n%s", opts.format, usage)
	}
	return nil
}

// resolveTree settles whether a oneline listing nests. --tree and --no-tree say
// so outright; with neither, it nests on a terminal and stays flat when stdout
// is a pipe or a file — so what a person reads is a tree and what a program
// parses keeps its columns. --format medium never nests and does not call this.
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
	return listIssues(opts, filter)
}

// parseListFlags resolves how to print, which issues to keep, and which issue
// to list under. --oneline is the same request as --format oneline, spelled
// the short way git log spells it; the filter flags are add's, so a word
// selects the same issues it would have created.
//
// The positional is an issue id, and it means the same thing it means on add:
// the issue this command hangs off. There it says what to file a new issue
// under, here what to list under.
func parseListFlags(args []string) (listOptions, listFilter, error) {
	var (
		opts      listOptions
		oneline   bool
		filter    listFilter
		labels    listFlag
		assignees listFlag
	)
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bindListFlags(fs, &opts, &oneline)
	bindFilterFlags(fs, &filter, &labels, &assignees)

	rest, err := parsePermuted(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, filter, err
		}
		return opts, filter, fmt.Errorf("%s\n%s", err, listUsage)
	}
	if len(rest) > 1 {
		return opts, filter, fmt.Errorf("list takes one issue to list under, got '%s' as well\n%s", rest[1], listUsage)
	}
	if len(rest) == 1 {
		opts.under = rest[0]
	}

	if ferr := resolveFilter(&filter, labels, assignees); ferr != nil {
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

// cmdShow prints an issue, or — given a second id — one comment inside it.
//
// Two ids mean a comment here for the same reason they do on edit and remove:
// an issue is what a command reaches for, and a comment is always somewhere
// inside one.
func cmdShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	web := fs.Bool("web", false, "open the issue's upstream page in a browser")
	targets, err := parseTargets(fs, "show", args, showUsage, 2)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(showUsage)
		return nil
	}
	if err != nil {
		return err
	}
	if *web {
		return openIssueWeb(targets)
	}
	return showIssue(targets)
}

// openIssueWeb sends the reader to the issue's page on the tracker it came from.
// The address is on the origin ledger — where a bridge records it on pull and
// push — so an issue that has never touched a bridge has none, and there is no
// per-comment page to open either.
func openIssueWeb(targets []string) error {
	if len(targets) == 2 {
		return fmt.Errorf("show --web opens an issue; a comment has no page of its own\n%s", showUsage)
	}
	s, err := open()
	if err != nil {
		return err
	}
	id, _, err := s.Find(targets[0], issue.Type)
	if err != nil {
		return err
	}
	url, err := issueWebURL(s, id)
	if err != nil {
		return err
	}
	if url == "" {
		return fmt.Errorf("no upstream page for this issue: it has not been pulled from or pushed to a bridge")
	}
	fmt.Fprintf(os.Stderr, "Opening %s\n", url)
	return s.Repo.WebBrowse(url)
}

// issueWebURL is the issue's web address as some bridge recorded it, taking the
// first across trackers if more than one has pushed it. An entity with no
// recorded address, or no origin ledger at all, is "" and not an error.
func issueWebURL(s *entity.Store, id string) (string, error) {
	led := origins.NewStore(s.Repo)
	trackers, err := led.Trackers()
	if err != nil {
		return "", err
	}
	for _, tracker := range trackers {
		l, err := led.Load(tracker)
		if err != nil {
			return "", err
		}
		if url, ok := l.URL(id); ok {
			return url, nil
		}
	}
	return "", nil
}

// parseTargets parses what a command addresses: an issue id, and for the
// commands that reach inside one, the id of a comment in it. Both abbreviate,
// and both are resolved by their own command — this only counts them.
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
			return nil, fmt.Errorf("%s takes one issue, got '%s' as well\n%s", name, rest[max], usage)
		}
		return nil, fmt.Errorf("%s takes an issue and at most one comment, got '%s' as well\n%s", name, rest[max], usage)
	}
	return rest, nil
}

func listIssues(opts listOptions, filter listFilter) error {
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

	// Measured over every issue on the ref, not over the rows a filter left,
	// so a prefix this listing shows in full is one `git issue show` accepts.
	uniq := render.Uniquify(entity.IDs(notes))

	// The positional names an issue, so it is resolved against the whole ref
	// before anything is folded: an unknown or ambiguous prefix is an error on
	// the terminal rather than an empty listing that looks like an answer.
	root, err := listRoot(s, opts.under)
	if err != nil {
		return err
	}

	var (
		matched    []string
		states     = map[string]entity.State{}
		rootPassed bool
	)
	h, err := survey(s, notes, func(id string, st entity.State) {
		if !filter.matches(st) {
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

	// A long listing runs past a screen; page it like `git log`, newest issue
	// at the top. Short ones fall straight through — see cli.StartPager.
	p := cli.StartPager(s)
	defer p.Finish()

	if format == formatMedium {
		links := indexLinks(h)
		entries := make([]render.LogEntry, 0, len(matched))
		for _, id := range matched {
			entries = append(entries, issue.Log(id, states[id], time.Local, links.of(id)))
		}
		render.WriteLog(p, entries, uniq)
		return nil
	}

	listed := make([]render.Row, 0, len(matched)+1)
	for _, id := range matched {
		listed = append(listed, h.rows[id])
	}
	if resolveTree(opts) {
		if root != "" {
			// filedUnder selects the subtree without the issue that was named —
			// it is not filed under itself — but a tree still draws that issue
			// as the root its branches hang from. Without it every top branch
			// would be marked as having a parent the reader just named. It
			// appears only if it passed the filter, the same as any other row.
			if rootPassed {
				listed = append(listed, h.rows[root])
			}
			h.parents[root] = ""
		}
		render.WriteTree(p, nest(listed, h.parents), uniq)
		return nil
	}
	render.WriteList(p, listed, uniq)
	return nil
}

// listRoot resolves what `list <id>` names. The word `none` is not an id and
// never can be — an id is hex — so it needs no escaping: it resolves to the
// empty parent, which is exactly what the blob of an unfiled issue holds.
func listRoot(s *entity.Store, under string) (string, error) {
	if under == "" || strings.EqualFold(under, filterNone) {
		return "", nil
	}
	id, _, err := s.Find(under, issue.Type)
	return id, err
}

// showIssue prints the issue named by targets[0], or the comment targets[1]
// names inside it.
func showIssue(targets []string) error {
	s, err := open()
	if err != nil {
		return err
	}
	id, st, err := s.Find(targets[0], issue.Type)
	if err != nil {
		return err
	}

	// Resolved before the pager opens, so an unknown or ambiguous prefix is an
	// error on the terminal rather than one screen of a pager.
	var entry entity.Comment
	if len(targets) == 2 {
		if entry, err = st.FindComment(targets[1]); err != nil {
			return err
		}
	}

	// What this issue points at, and what points back. Neither is in this
	// issue's own blob — a relation is a kind and an id there and nothing more
	// — so answering it means folding the ref, which a comment rendering does
	// not need and does not pay for.
	links := issue.Links{}
	uniq := render.Unique(nil)
	if len(targets) < 2 {
		notes, err := s.Notes()
		if err != nil {
			return err
		}
		h, err := survey(s, notes, nil)
		if err != nil {
			return err
		}
		links = indexLinks(h).of(id)
		uniq = render.Uniquify(entity.IDs(notes))
	}

	// A full issue with a long thread runs past a screen, and so can a comment
	// with a long one under it; page both like `git show`. Short ones fall
	// straight through — see cli.StartPager.
	p := cli.StartPager(s)
	defer p.Finish()

	detail := issue.Detail(id, st, time.Local, links)
	if len(targets) == 2 {
		render.WriteComment(p, detail, entry)
		return nil
	}
	render.WriteDetail(p, detail, uniq)
	return nil
}

// cmdRemove removes the tree entry. That unlists the issue; it does not erase
// it. The blob stays in the notes ref's own history, so this is recoverable
// locally, and a clone that has not fetched the removal still has the issue in
// full.
//
// It also does not converge. Confirmed empirically: the removal propagates to
// other clones when nobody else touched that note, but a concurrent edit
// anywhere in the issue resurrects the whole thing on the next merge. A
// removal that survives concurrent edits would need an entity-level tombstone
// event, which docs/blob-format.md does not currently define.
// A second id retracts one comment instead, which is a different operation
// entirely: a tombstone event that converges, rather than a tree entry that
// goes away.
func cmdRemove(args []string) error {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	targets, err := parseTargets(fs, "remove", args, removeUsage, 2)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(removeUsage)
		return nil
	}
	if err != nil {
		return err
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
		return removeComment(s, entID, st, targets[1])
	}

	// Rendered from state already folded in memory, so it survives the note
	// going away — and written to stdout rather than a buffer so a terminal
	// gets the same coloured row a listing would. The id is measured before
	// the removal, so the row shows the prefix that named this issue while it
	// was still there rather than falling back to the whole id.
	row := issue.Row(entID, st)
	uniq := uniqueIDs(s)
	if err := s.Remove(entID); err != nil {
		return fmt.Errorf("could not remove note for %s", entID)
	}
	render.WriteRow(os.Stdout, "removed ", row, uniq)
	return nil
}

// cmdPull reads issues from somewhere else into this repository.
//
// The target is `[scheme:]remote` — `origin` and `git:origin` fetch another
// copy of the notes ref, `github:origin` imports through the bridge. Naming a
// target, with or without a scheme, always means exactly the scheme given —
// git unless the bridge was asked for by name.
//
// With no target at all there is no scheme to have named, so pullDefault
// decides instead: it resolves the current branch's own remote and always
// reads its notes ref, and reads the bridge too if that remote turns out to
// be GitHub's.
func cmdPull(args []string) error {
	opts, rest, err := parsePullFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(pullUsage)
		return nil
	}
	if err != nil {
		return err
	}
	target := ""
	if len(rest) > 0 {
		target = rest[0]
	}

	s, err := open()
	if err != nil {
		return err
	}
	spec := remote.Parse(s.Repo, target)
	if target == "" {
		return pullDefault(s, spec, opts)
	}
	switch spec.Scheme {
	case remote.SchemeGit:
		return pullGit(s, spec, opts)
	case remote.SchemeGitHub:
		return pullGitHub(s, spec, opts)
	case remote.SchemeADO:
		return pullADO(s, spec, opts)
	case remote.SchemeGitea:
		return pullGitea(s, spec, opts)
	default:
		return fmt.Errorf("unknown source '%s'", spec.Scheme)
	}
}

// pullDefault is what a fully bare `git issue pull` does. The git leg always
// runs: it is the native path, needs no credentials, and unlike the bridge it
// can actually report "up to date" against a ref rather than an API's idea of
// what changed. A bridge leg runs only when the remote it resolved to is that
// bridge's — a plain git remote has no API behind it to ask, and anything else
// would be exactly the URL-guessing an explicit scheme exists to avoid (see
// remote.LooksLikeGitHub and remote.LooksLikeADO).
//
// --all, --full, --since and --limit only mean anything to the bridge leg;
// pullGit's own check for --all is skipped here on purpose, via pullGitRef
// directly, since rejecting it would make --all unusable on the one target
// that can actually use it.
func pullDefault(s *entity.Store, spec remote.Spec, opts pullOptions) error {
	// At most one bridge can be the remote's, since no URL is both.
	scheme := ""
	switch {
	case spec.LooksLikeGitHub():
		scheme = remote.SchemeGitHub
	case spec.LooksLikeADO():
		scheme = remote.SchemeADO
	case spec.LooksLikeGitea():
		scheme = remote.SchemeGitea
	case looksProbedGitea(spec):
		// Gitea is self-hosted and has no definitive URL marker, so a remote
		// that is not GitHub or ADO and does not name itself is probed once:
		// a Gitea host answers GET /api/v1/version, anything else does not.
		scheme = remote.SchemeGitea
	}

	err := pullGitRef(s, spec)
	// A forge remote is rarely pushed a notes ref at all — that is what the
	// bridge exists for — so finding none there is not a failure on its own.
	// It only stops being swallowed once there is no bridge leg left to
	// redeem the pull, or the git leg failed on something else entirely: no
	// such remote, a network problem, an auth problem.
	empty := err != nil && errors.Is(err, entity.ErrNoRemoteRef)
	if err != nil && (scheme == "" || !empty) {
		return err
	}
	if scheme == "" {
		return nil
	}
	if !empty {
		// Something about the git leg was worth printing; separate it from
		// the bridge leg's own report the way two people's output never runs
		// together on one line.
		fmt.Println()
	}

	bridged := spec
	bridged.Scheme = scheme
	switch scheme {
	case remote.SchemeADO:
		return pullADO(s, bridged, opts)
	case remote.SchemeGitea:
		return pullGitea(s, bridged, opts)
	default:
		return pullGitHub(s, bridged, opts)
	}
}

// looksProbedGitea asks a not-yet-identified http(s) remote whether it is a
// Gitea host. It runs only for a fully bare `git issue pull` whose remote is
// neither GitHub nor ADO nor self-identifying, and makes exactly one request.
func looksProbedGitea(spec remote.Spec) bool {
	url := strings.ToLower(spec.URL)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return false
	}
	target, err := giteaapi.ParseTarget(spec.URL)
	if err != nil {
		return false
	}
	_, err = giteaapi.New(target.API(), "").Version()
	return err == nil
}

func parsePullFlags(args []string) (pullOptions, []string, error) {
	var (
		opts  pullOptions
		since string
		types listFlag
	)
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.all, "all", false, "import closed issues too, not just open ones")
	fs.BoolVar(&opts.full, "full", false, "re-read everything, ignoring where the last import got to")
	fs.StringVar(&opts.token, "token", "", "token to use instead of the ones git and the platform CLIs hold")
	fs.StringVar(&since, "since", "", "only import issues updated since this date")
	fs.IntVar(&opts.limit, "limit", 0, "stop after this many issues")
	fs.Var(&types, "type", "import only issues of this type; repeatable, or comma-separated")

	rest, err := parsePermuted(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, nil, err
		}
		return opts, nil, fmt.Errorf("%s\n%s", err, pullUsage)
	}

	if since != "" {
		at, err := parseDate(since)
		if err != nil {
			return opts, nil, err
		}
		opts.since = at
		opts.sinceGiven = true
	}
	// Both name a starting point, and they would disagree. Refusing beats
	// picking a winner the caller would have to guess at.
	if opts.full && opts.sinceGiven {
		return opts, nil, fmt.Errorf("--full and --since both say where to start; use one")
	}
	opts.types = types.values
	return opts, rest, nil
}

// parsePermuted parses flags that appear before, after, or around the
// positional arguments, and returns the positionals.
//
// Go's flag package stops at the first non-flag argument, so plain Parse reads
// `pull origin --all` as the target `origin` plus two ignored words — the flag
// is silently dropped rather than refused, which is the worst of the options.
// Nobody types arguments in a fixed order, and git itself does not require it.
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

// parseDate accepts a plain date as readily as a full timestamp, since a plain
// date is what anyone types.
func parseDate(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if at, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return at, nil
		}
	}
	return time.Time{}, fmt.Errorf("could not read '%s' as a date; try 2006-01-02", s)
}

// pullGit fetches the remote's copy of the notes ref and unions it into the
// local one, then reports what moved in the same columns `list` uses.
func pullGit(s *entity.Store, spec remote.Spec, opts pullOptions) error {
	// --all selects which issues a bridge asks for. A git pull does not
	// choose: it fetches a ref, and takes whatever the remote filed on it.
	// Accepting the flag silently would imply it had narrowed something.
	if opts.all {
		return fmt.Errorf("--all applies to a bridge import; a git pull fetches whatever %s holds", s.FullRef())
	}
	return pullGitRef(s, spec)
}

// pullGitRef is the git half of a pull, without the --all check pullGit does
// on top: a bare `git issue pull` runs this unconditionally, since --all
// there belongs to its own bridge leg rather than this one.
func pullGitRef(s *entity.Store, spec remote.Spec) error {
	// A URL can be fetched from, but there is nowhere sensible to keep a
	// remote-tracking ref for one, and without that there is no before/after
	// to report. Requiring a configured remote keeps the two consistent.
	if !spec.IsGitRemote() {
		return fmt.Errorf("no remote named '%s'; git issue pull needs a configured remote", spec.Target)
	}

	// The origin ledger travels with the notes ref, and is fetched first for the
	// reason it is pushed first: issues that arrive without their mappings will
	// be re-created upstream by this clone's next bridge push, while mappings
	// for issues not yet here are simply looked up and missed.
	//
	// Its failure is held rather than returned. Whatever stopped it — no
	// credential, no network, no such remote — is about to stop the notes fetch
	// too, and that leg's error is the one worth reporting: it knows to point at
	// the bridge when the remote is a forge. Only a ledger that failed on its own
	// is worth a word of its own.
	ledgerErr := origins.NewStore(s.Repo).Pull(spec.Name)

	pull, err := s.PullGit(spec.Name)
	if err != nil {
		return pullGitError(spec, pull.Ref, err)
	}
	if ledgerErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not fetch the origin ledger: %s\n", ledgerErr)
	}
	return report(pull, "From "+spec.URL, uniqueIDs(s))
}

// pullGitError explains why a git-mode pull did not happen, and points at the
// bridge when the remote is a forge.
//
// The hint hangs off any failure, not just a missing ref: a private GitHub
// remote fails while asking for a password, long before git gets far enough to
// notice the ref is absent, and that is precisely the moment someone needs to
// be told the issues are not in git refs at all.
func pullGitError(spec remote.Spec, ref string, err error) error {
	hint := ""
	if spec.LooksLikeGitHub() {
		hint = fmt.Sprintf("\n       issues hosted on GitHub are read through the bridge: git issue pull github:%s", spec.Target)
	}
	if errors.Is(err, entity.ErrNoRemoteRef) {
		// noRemoteRefError, not a plain fmt.Errorf: pullDefault tells this
		// case apart from a real failure with errors.Is, which a %s-built
		// message — there is nothing here worth formatting %w into — would
		// not answer to once this string is all that is left of err.
		return &noRemoteRefError{msg: fmt.Sprintf("%s has no %s; nothing has been pushed there yet%s", spec.Target, ref, hint)}
	}
	return fmt.Errorf("%s%s", err, hint)
}

// noRemoteRefError is ErrNoRemoteRef dressed for display. Its Is method is
// what lets pullDefault recognise the case after the message has already
// been built, rather than duplicating pullGitError's own check.
type noRemoteRefError struct{ msg string }

func (e *noRemoteRefError) Error() string        { return e.msg }
func (e *noRemoteRefError) Is(target error) bool { return target == entity.ErrNoRemoteRef }

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// pullOptions are the flags shared by both sources, plus the ones only the
// bridge uses.
type pullOptions struct {
	all   bool
	full  bool
	token string
	limit int
	// types narrows a bridge import to these entity types. Azure DevOps is
	// where it earns its place — a project holds Epics, Bugs and Test Cases
	// alike — but the flag is spelled as `add` and `list` spell it and names
	// no platform.
	types []string

	// since narrows the import; sinceGiven distinguishes a date the caller
	// asked for from one carried over from the last sync.
	since      time.Time
	sinceGiven bool
}

// sync is how this command reports a pull or a push. The machinery is
// internal/cli's, shared with git-review so the two cannot drift into two
// different-looking syncs of the same object store; what this supplies is the
// issue vocabulary.
var sync = cli.Sync{Noun: "issue", Row: issue.Row}

func report(pull entity.Pull, header string, uniq render.Unique) error {
	return sync.Report(pull, header, uniq)
}

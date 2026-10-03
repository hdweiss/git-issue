# Using git-issue

The full walkthrough. [README.md](../README.md) has the one-line-per-command
summary; this covers every flag, what the output means, and the reasoning behind
the choices. Where something is a storage rule rather than a UI decision, the
spec doc that fixes it is cited — `docs/issues.md` for the vocabulary,
`docs/blob-format.md` for the event format, `docs/storage-model.md` for the
refs.

`git issue` on its own is `git issue list`; `git issue <id>` is `git issue show
<id>`. Either spelling works everywhere. Ids abbreviate like git object names,
and an ambiguous prefix is refused rather than resolved arbitrarily.

## Listing

`git issue` and `git issue list` print one issue per line — id, status, type,
title, then whatever labels and comments the issue carries — newest issue first,
the way `git log` opens on the newest commit:

```
e9037839d7f8  open         Anchor blobs are pruned by gc
885797fb2d9b  open    bug  Reject "short" ids  [bug, design]
45cf71414905  open         cat_sort_uniq reorders the blob  💬 5
0bcd4859d4fe  closed       Duplicate of the fanout question
```

On a terminal a listing longer than a screen opens in the pager, the same
`$GIT_PAGER` / `core.pager` / `$PAGER` / `less` chain `show` uses; piped or
redirected, it prints straight through.

A terminal also nests the listing under each issue's parent — the tree is the
default there, since the spine is what a person reads. Piped or redirected the
listing goes flat, so it stays greppable and diffable. [`--tree`](#--tree) and
`--no-tree` force it either way; `--format medium` never nests.

### On a terminal

The type column disappears in a tracker that never sets one. Where it is shown
it becomes an emoji — 🐞 for a bug, 🏆 for a feature, 👑 for an epic, 📖 for a
user story, ✅ for a task, following Azure DevOps' work-item colours where they
have an obvious glyph. The match is loose, so `Bug` and `backend-bug` land in
the same place; a type nothing matches keeps its word, and a blank one shows ✅,
the same as a task.

The id turns yellow like a commit line in `git log` — but only as far as it has
to: the leading characters that already name one issue and no other are the
bright ones, and the rest of the column fades, so the id shows how much of
itself you need to type.

The status word gives way to a coloured dot — green for open, purple for closed.
The tail is laid out from the right edge: the comment count sits at the far
right, its number right-aligned so the 💬 lines up whether the count is 1 or
100, and the labels are bold and right-justified just left of it, so their
closing bracket lines up down the listing however long the titles run.

A title that would push its line past the edge of the terminal is shortened to
fit, marked with an ellipsis; the labels and the comment count are never
dropped, since they are what the columns promised.

### Piped

`NO_COLOR` drops the emoji and the colour but keeps the width-aware layout.
Piped or redirected output drops all of it — the status word and the type word
come back, titles print in full, and the listing goes flat, so it stays
greppable and diffable however narrow the window that produced it.

### `--format medium`

Prints the same issues as `git log` prints commits: the same header block a
single issue uses, then the title, and no body. Refused together with `--tree` —
a header block has nowhere to put a spine.

### Filters

`--state open|closed|all`, `--type`, `-l/--label`, `-a/--assignee` and
`--author` narrow the listing. The names and the repeatable, comma-separated
form of `-l` and `-a` are `add`'s, so the word that filed an issue is the word
that finds it. Filters combine with AND, match case-insensitively, and work on
the `git issue` shortcut as well as `git issue list`. `--author` is a substring
match; the rest match a whole value.

Pass `none` to `-l` or `-a` — `git issue list --label none` — for the issues
with no label, or no assignee, at all. See [Emptying a field](#emptying-a-field).

## Creating an issue

Every field is a flag, and a title is the only one an issue cannot be written
without:

```sh
git issue add -t "Reject short ids" -d "git notes refuses them; we should too."
git issue add -t "Colour the status" --type feature -l ui,design -a alice -m v2
git issue add 4b0755a3e769 -t "Refuse a short id in notes show"   # under an epic
```

| Flag | Field |
| --- | --- |
| `-t`, `--title` | the one-line summary (required) |
| `-d`, `--description` | the body |
| `-F`, `--file <path>` | take the title + description from a file (`-` for stdin) |
| `--type` | `bug`, `feature`, `task` — the vocabulary is open |
| `-l`, `--label` | repeatable, or one comma-separated list |
| `-a`, `--assignee` | repeatable, or one comma-separated list |
| `-m`, `--milestone` | the milestone's name |
| `--rel <kind>:<id>` | a link the issue is born with; repeatable |
| `-e`, `--edit` | open the editor even when a title was given |

An id before the flags files the new issue under that one, the way `git checkout
-b <branch> <start-point>` says where to start from. Omit it for a top-level
issue. It is the same field `--rel` writes: `add <id>` and `--rel parent:<id>`
write the same link. See [Relations](#relations).

### The editor

**Without a title, an editor opens** — the same call `git commit` makes when no
`-m` is given. `-e` / `--edit` opens one even when a title was given. The buffer
is Markdown and holds two things: the title on its first line, as a `#` heading,
the description below.

```markdown
# Reject short ids

`git notes` refuses a name this short, and so should we. Four hex characters
stop being unambiguous once the ref has a few hundred entries.

<!---
Filing a new issue. The first line is the title, as a '# ' heading; everything
below it is the description, in Markdown. This block is ignored on save.

Fields are set with flags, not here — pass them to `git issue add`, or run
`git issue edit <id>` afterwards:

  type        bug
  labels      design, ui
-->
```

The first line is the title, the way the first line of a commit message is its
subject; the blank line under it is conventional, not required. The title is
written as a `#` heading and read back with the `#` stripped, so a file handed
to `-F` (below) can be an ordinary Markdown document. Everything after the first
line is the description, Markdown throughout — a `#` there is a real heading,
left alone.

**Anything between `<!---` and `-->` is ignored.** That block is where the editor
writes its instructions and a read-only echo of the fields the issue already
carries, so the type and the labels are in front of you while you write without
being something a saved buffer could change. Edit it, delete it, leave it —
none of it is read back. An unterminated `<!---` is ignored through to the end
of the buffer, so removing the closing `-->` by mistake cannot leak the echo
into the description.

Only the title and the description are editable here, by design. The buffer
makes a round trip through an editor, and a field you can empty by deleting its
line is a field the editor can clear without meaning to — worse for a link,
which `docs/issues.md` stores at one end only, so blanking it here would detach
the far issue too. Every other field is a flag — `--type`, `-l`, `--milestone`,
`--parent` and the rest — on `add` or on a later `edit`. What those flags
carried is echoed in the ignored block, never dropped.

An empty first line aborts and writes nothing, leaving the buffer in
`.git/ISSUE_EDITMSG.md` the way an aborted commit leaves `COMMIT_EDITMSG`. The
`.md` extension is what puts the editor in Markdown mode.

### From a file or a pipe

`-F <path>` takes that same buffer from a file instead of an editor, and `-F -`
takes it from standard input:

```sh
git issue add -F bug-report.md
cat bug-report.md | git issue add -F -
git issue add -t "Known title" -F body.md      # -t wins; the file is the body
```

Piped input is picked up **without** `-F` too, but only while the command still
needs content it has no other way to get — so `cat notes.md | git issue add`
just works. The rules:

- no `-t`: the input is the whole buffer — first line the title (a leading `#`
  dropped), the rest the description.
- `-t` given: the request is already complete, so implicit stdin is **not**
  read. Spell the pipe `-F -` to make it the description, first line and all.
- `-d` and piped/`-F` input may not both set the body — that is refused, not
  silently resolved.

No editor opens when input is waiting; it replaces the editor. An empty first
line still aborts.

### A complete request never reads stdin

Non-terminal stdin is not the same thing as a pipe somebody is feeding. A
script, a git hook and a CI runner all hand a command a pipe that carries no
data and is never closed, and reading one blocks until the process is killed —
so a command that could have finished without reading must not read at all.
`git issue add -t "Title" --type bug` and `git issue edit <id> -l bug` are
complete as typed, and both return rather than waiting on input nobody is going
to send.

The cost is that a pipe alongside a named field has to be asked for, `-F -`,
instead of being inferred. That is the same trade [`none`](#emptying-a-field)
makes: one explicit word, in exchange for a rule with no case that silently does
the wrong thing.

## Editing one

`git issue edit` writes a field two ways. Name one on the command line and the
change is written straight away; name none and the same buffer `add` uses opens
on the issue's title and description, with the rest of its fields echoed in the
ignored block for reference. `show` stays read-only.

```sh
git issue edit 4b0755a3e769 -t "A new title" --type bug
git issue edit 4b0755a3e769 -l perf --remove-label ui
git issue edit 4b0755a3e769 --milestone none      # empty the field
git issue edit 4b0755a3e769                        # no flags: the editor opens
```

The flags are `add`'s, plus `--remove-label` / `--remove-assignee` for taking
one member off a list, `--parent` / `--rel` / `--no-rel` for links (see
[Relations](#relations)), and `-e` to open the editor anyway, prefilled.

`-F <path>` (and `-F -`, and a bare pipe) work here as they do on `add`: the
input replaces the editor buffer, first line and all — so `cat notes.md |
git issue edit 4b07` rewrites both the title and the description, the same as
retyping the editor's first line would. To change only the body, pass `-t` with
the title to keep and `-F -` for the input, which is then the body alone. It is
still written back as one event per field that actually changed.

A bare pipe is read only when **no field was named at all**, for the reason
under [A complete request never reads stdin](#a-complete-request-never-reads-stdin):
`git issue edit <id> -l bug` is a finished request, and a finished request must
not block on a pipe nobody is feeding. Naming a field and piping a buffer is
still possible, spelled `-F -`.

Pass `none` to empty anything that has an empty state — `--milestone none`,
`--parent none`, `-l none` for every label, `--rel none` for every link — the
same word a listing already uses for "has none", rather than a `--no-<field>`
flag for each. See [Emptying a field](#emptying-a-field). A title is the
exception, since an issue needs one, so `-t none` is simply a title.

`-l` and `-a` add to the list rather than replacing it; `--remove-label` and
`--remove-assignee` take one member off, and `-l none` / `-a none` clear it
outright. On this command `-m` is a comment's new text, not the milestone as it
is on `add`, so the milestone is spelled `--milestone` in full — one short flag
may not mean two things.

```sh
git issue edit 4b0755a3e769
```

```
updated 4b0755a3e769  open  feature  Anchor blobs are pruned by gc  [design, ui]
title, type, labels changed
```

### One event per changed field

**Saving writes one event per field that actually changed**, and nothing else —
a field nobody touched is not rewritten, and the events already in the blob are
appended to rather than replaced. That is what lets two people edit two
different fields of the same issue offline and have both survive the merge; a
client that wrote the whole record back would silently revert whichever field it
had not seen. A cleared field is set to `null`, which is how `docs/issues.md`
spells "no milestone".

Labels and assignees are lists, so they diff into `label.add` and `label.remove`
pairs — and a remove names the id of the add it undoes, never the label's text.
Two clones that added `design` independently hold two adds, and removing the
label retracts both.

An edit that changes nothing writes nothing at all, and a blank first line
aborts the same way it does in `add`.

## Closing an issue

```sh
git issue close 4b0755a3e769                       # status -> closed
git issue close 4b0755a3e769 --as not-planned      # …and why
git issue close 4b0755a3e769 --as duplicate 60c4    # …and the issue it defers to
git issue reopen 4b0755a3e769                       # status -> open
```

```
closed 4b0755a3e769  closed  Anchor blobs are pruned by gc
```

`close` and `reopen` write **one `status` event and nothing else** — the same
scalar `edit` writes as `--status`, resolved last-write-wins on `(c, id)` like
any other. They are verbs rather than flags because closing an issue is the
change made most, and `git issue edit <id> --status closed` is a clumsy way to
say it. A `close` of an already-closed issue, or a `reopen` of an open one,
writes nothing and prints `nothing changed`, exactly as `edit` does.

### `--as`, the reason

`--as` records **why** the issue is closing, as a second, independent scalar
(`status.reason` in [docs/issues.md](issues.md)):

| `--as` | Meaning |
| --- | --- |
| `completed` | Resolved as intended |
| `not-planned` | Closed without being done: won't fix, out of scope, stale |
| `duplicate` | Deferred to another issue |

The hyphen is sugar — the blob spells it `not_planned` — and these three are
all `--as` takes; an unrecognised reason is refused rather than written, since
the reason is a small closed vocabulary on every tracker this syncs with. The
raw field is still reachable as `git issue edit <id> --status-reason <value>`
for a value a newer client or a bridge introduced.

A bare `close` writes **no reason at all** — one event, not two. The reason is
opt-in because it is genuinely separate information, and because the two axes
resolve independently: an issue closed `--as not-planned` and later reopened
has `status = open` with `status.reason = not_planned` still resolving, since
nothing superseded it. `reopen` deliberately does **not** clear it — that would
lose the record of why it was closed the first time, and it would still race a
concurrent re-close. A reader ignores the reason whenever the status is
non-terminal, so `show` simply does not print a `Reason:` line for an open
issue. This is the stale pairing [docs/issues.md](issues.md) describes, surfaced
rather than papered over.

### `--as duplicate`

Marking a duplicate is two facts — this issue is closed, and it defers to
another — so `close` writes both when the canonical issue is named as a second
argument:

```sh
git issue close 4b0755a3e769 --as duplicate 60c4130770cd
```

That is `status = closed`, `status.reason = duplicate`, and a `duplicate-of`
link to `60c4130770cd`, in one action. The link is the same one `git issue edit
<id> --rel duplicate-of:<id>` writes; naming the id without `--as duplicate` is
refused, so the second argument never silently means something else. `reopen`
does not remove the link — a reopened duplicate still points where it pointed.

### A status that is neither open nor closed

An issue pulled from Azure DevOps carries that platform's own state verbatim —
`New`, `Active`, `Resolved`, `Done` — and `git issue edit <id> --status <value>`
is how you set one locally. `close` and `reopen` are the two common points on
that axis given their own verbs; `--status` reaches the rest. It has no `none`:
an issue with no status event is open, so `reopen` is how it goes back, not a
cleared field. git-issue knows which of the Azure DevOps states end a work
item's life (`Done`, `Removed`, `Closed`) and which do not, so a listing dots
them the right colour and `--state closed` finds them; a state from a
customised process it has not seen is treated as open until told otherwise. See
`status` in [docs/issues.md](issues.md).

## Commenting

```sh
git issue comment 4b0755a3e769 -m "Confirmed on 2.42 and on 2.53."
git issue comment 4b0755a3e769                  # no -m: an editor opens
git issue comment 4b0755a3e769 -F notes.md      # …or take the text from a file
cat notes.md | git issue comment 4b0755a3e769   # …or a pipe
```

```
7ccaf98a7073
```

What it prints is the comment's own id, and that is what addresses it from then
on. Commenting is its own verb rather than `git issue add <id>`, which already
means something else: a positional id there says what to file the new issue
under.

### The editor

**Without `-m`, an editor opens** on the comment — `git commit` without a
message, again. The buffer is Markdown; an `<!--- … -->` block at its foot names
the issue being commented on, and is ignored on save the same way the issue
editor's block is. `git issue edit <id> <comment>` opens the same buffer with
the comment's current text filled in. An empty comment aborts and writes
nothing, keeping what was typed in `.git/ISSUE_COMMENT_EDITMSG.md`.

`-F <path>` reads the comment text from a file rather than an editor, `-F -`
reads it from stdin, and — as with `add` — a bare pipe is picked up without
`-F`, so `echo "Confirmed." | git issue comment <id>` posts one. `-m` and `-F`
may not both be given. The same holds for rewriting one with
`git issue edit <id> <comment> -F -`.

### Addressing a comment

**One argument names an issue; two name a comment inside it.** The second
argument is an abbreviated event id, resolved exactly as the first one is — like
an object name, with an ambiguous prefix refused rather than picked between.

```sh
git issue show   4b07 a3f1      # that entry and the replies under it
git issue edit   4b07 a3f1      # rewrite it — -m, or an editor on its text
git issue remove 4b07 a3f1      # retract it
```

The prefix is resolved **within the issue already named**, so it competes only
with that issue's own comments and four hex digits is normally enough — no worse
to type than a number. On a terminal `git issue show` says exactly how many: the
id above each entry is bright as far as it has to be to name that comment and no
other, and faded after that, usually a character or two.

It is deliberately **not an ordinal** — not "the third comment" — however much
shorter that would be to type. A thread is ordered by `(c, id)`, so a comment
merged in from another clone lands wherever its clock puts it, routinely in the
middle. An ordinal would silently renumber every entry after it on a pull, and
two clones would disagree about which comment is number 3; an edit typed from a
listing read a moment earlier would then rewrite somebody else's text. Event ids
do not move — a comment's id is `git hash-object` of its own event line, and a
bridged entry derives its nonce from the upstream id, so the same id survives a
merge and a re-import alike. This is the rule `docs/blob-format.md` already
imposes on the events themselves: address by event id, never by value or
position.

An ordinal is fine as *output* — `show` may print one beside an entry; it may
never be accepted as an argument.

### Reading one branch

That second id reads as well as it writes: `git issue show <id> <comment>`
prints that entry and the replies under it instead of the whole thread, which is
how you read one branch of a long discussion without the rest of it.

```sh
git issue show 4b0755a3e769 a3f1
```

```
comment a3f1c0de5b7a94e2f0d1c6b8a4e73f25d9081bcd
Author: rev@example.com
Date:   Mon Aug 17 22:58:20 2026 +0200

    Confirmed: absent in a fresh clone — and after gc.

    comment 8c40b21e7fa6d3958e0b4c17da29f65038e1b7c4
    Author: hdweiss@gmail.com
    Date:   Mon Aug 17 22:59:20 2026 +0200

        Only after gc, though.
```

The entry is rendered exactly as the full issue renders it, only starting at
column zero — same headers, same nesting, same amount of each id lit up — so
what you read zoomed in is what you read in place.

### Edits and retractions

**An edit does not overwrite anything.** It is an event of its own that
addresses the entry by id and carries the new body, so the original text stays
in the blob and the thread keeps its history; a reader folds them and the last
edit wins. An edit that changes nothing writes nothing.

**A retraction is a tombstone, not a deletion.** `git issue remove <id>
<comment>` hides the entry's body — `show` prints `(comment retracted)` in its
place, and a listing stops counting it — but the text stays in the blob, which
is what lets the retraction converge where an erasure could not. It does not
cascade to replies: a cascade would let one person destroy other people's
content as a side effect of withdrawing their own.

## Emptying a field

The word is **`none`**, in every command that takes a value:

```sh
git issue edit 7d20 --parent none        # detach it
git issue edit 7d20 --milestone none     # unfile it
git issue edit 7d20 -l none              # drop every label
git issue list -l none                   # the issues carrying no label
git issue list none                      # the issues with no parent
```

One word for an empty field, whether a command is selecting on one or setting
one. `-l none` and `-a none` already meant this in the filters; `edit` adopts
the same word rather than growing a `--no-parent`, `--unset-milestone` family,
where every clearable field costs a second flag and the two spellings drift.

An empty string clears too — `--milestone ""` — because `issue.Update` already
writes a cleared scalar as `null` (`docs/issues.md`). `null` itself is not
accepted on the command line: that is the word the blob uses, and keeping the
CLI's word and the storage's word distinct is the same separation `status`
keeps.

Two consequences, both accepted:

- **A milestone or a type literally called "none" cannot be set.** This cost is
  already paid — a label called `none` cannot be filtered for today — and the
  alternative is a quoting rule nobody would remember.
- **A title has no cleared state**, since an issue needs one, so `-t none` is
  simply a title. Stated in `edit --help` rather than left as a trap.

`--parent none` is unambiguous whatever happens to the rest: an id is hex, and
`none` is not.

## Relations

An issue can point at another one: an epic and its sub-issues, a bug and what
blocks it, a duplicate and the issue it defers to. All of them are one field — a
set of `(kind, target)` links — rather than a field per kind, which is also how
the trackers this syncs with model it.

```sh
git issue add 60c4130770cd -t "Raw-state filters" --type task   # born under it
git issue edit 8d2129a3cf07 --parent 60c4130770cd               # re-file it
git issue edit 8d2129a3cf07 --parent none                       # detach it
git issue edit 8d2129a3cf07 --rel blocked-by:529296cfc8a8       # any other kind
git issue edit 8d2129a3cf07 --no-rel blocked-by:529296cfc8a8    # take one back
git issue edit 8d2129a3cf07 --no-rel blocked-by                 # or all of a kind
git issue add -t "Repack after import" --rel related:8d2129a3cf07
```

| Kind | Written on | The far end reads |
| --- | --- | --- |
| `parent` | the child | Children |
| `blocked-by` | the blocked issue | Blocks |
| `duplicate-of` | the duplicate | Duplicated by |
| `related` | either end | Related |

The kinds above are the ones this build has words for, not the ones it accepts.
An unknown kind — one a newer client wrote, or one a bridge carried in from a
tracker with its own vocabulary — is kept and shown as itself rather than
coerced into something it is not.

### One end writes

**A link is stored on one end only** — the end whose own state it constrains —
so it has exactly one writer and no second copy to keep in step. The other end
is worked out by inverting the field across the issues on the ref, never stored.
Filing five issues under an epic writes five blobs and leaves the epic's own
untouched.

`related` is the exception, because it constrains neither end: either may write
it, so a repository that has seen it from both sides holds two records of one
link. A reader shows it once, and `--no-rel` lets go of both — the one write
that reaches an issue you did not name, and it says which on stderr.

### The positional slot

An id in a command's positional slot is **the issue this command hangs off**.
That is one rule across two commands:

```sh
git issue add 9c1e -t "Raw-state filters"    # file a new issue under 9c1e
git issue list 9c1e                           # the whole subtree filed under 9c1e
git issue list none                           # the issues filed under nothing
```

`git issue list 9c1e` selects the same issues however it is drawn — every
descendant of `9c1e`, nested on a terminal and flat down a pipe. The named
issue itself is not among them: it is not filed under itself. A tree still
draws it as the root its branches hang from, so a piped listing has one row a
terminal one heads with; `show`, below, is the way to see just that issue.
`none` is the exception — "filed under nothing" is read one level deep, the
unparented roots, not every issue.

`git issue <id>` on its own stays `show` — the same token means "show this"
without the word `list` and "list under this" with it. No flag changes meaning
depending on a positional, and `git issue <id> --tree` (or `--no-tree`) is
refused rather than taken to mean the subtree: that is a flag turning `show`
into `list`. `show` answers the question one level deep anyway, a header per
kind of link with each id resolved to a title:

```
issue 8d2129a3cf07926fb73d8d22fddb8dbbd05a0743
Author:     hdweiss@gmail.com
Date:       Sat Aug 29 11:29:42 2026 +0200
Status:     open
Type:       bug
Parent:     60c4130770cd  Azure DevOps bridge
Children:   529296cfc8a8  Raw-state filters
Blocked by: 468b743a1262  Anchor blobs are pruned by gc

    Area picker drops the subtree
```

`add` spells the hierarchy as a positional and `edit` as a flag because their
argument slots differ, not by taste: `edit`'s second positional already names a
comment. `--parent` survives as sugar rather than as a second field — it is the
one kind with a positional spelling on `add`, a nesting in `--tree` and a `none`
of its own, so `--rel parent:<id>` being the same write is worth the alias,
while every other kind reaches the same field through `--rel`.

### `--tree` / `--no-tree`

A oneline listing nests under each issue's parent **on a terminal** and prints
**flat down a pipe**. The tree is what a person reads, the flat form is what a
script parses, and neither has to be asked for. `--tree` forces the nesting on
anywhere — into a file, through `less`, in a pipeline — and `--no-tree` forces
it off on a terminal; the two are refused together.

The spine sits in the **title column**, not at the left edge, so the id, status
and type columns stay a grid to scan down and copy out of — and the title,
already the part a narrow terminal trims, is the part that gives.

```
9c1e40b7a2d5  ●  👑  Azure DevOps bridge                          💬 2
1c4f88b0e2da  ●  ✅  ├─ Push
7d20b5ff1c38  ●  🐞  ├─ Area picker drops the subtree    [bug]     💬 5
a0417ec6b93f  ●  ✅  │  └─ Raw-state filters
3f8a10c94e77  ●  ✅  └─ The import
e9037839d7f8  ●  ✅  Anchor blobs are pruned by gc
885797fb2d9b  ●  🐞  ↑ Reject "short" ids                [bug, design]
```

- **A root is an issue whose parent is not in the listing.** That is the rule
  `entity.Forest` already applies to a comment thread — an entry whose parent is
  unknown belongs at root and is never hidden pending its parent's arrival — and
  it collapses four cases into one: a parent filtered out, a parent on the
  archived ref, a parent in another repository, a parent never imported.
- **`↑` marks a root that has a parent it cannot show.** One column, no colour,
  so it survives being piped. Without it a filtered tree is indistinguishable
  from a flat one and quietly claims there is nothing above.
- **A filter selects the rows; the tree only nests what was selected.** So a
  tree holds the issues a flat listing holds, in a different order and indent —
  give or take the root row a named subtree is drawn under. Pulling unmatched
  ancestors in as context rows would break that, and a context row could only be
  told apart by colour, which a pipe drops.
- **Siblings keep the listing's own order**, newest first, and a root sorts on
  its own creation date — so the top level reads the way `list` already reads.
- **Cycles are broken where they are found.** `parent` is last-write-wins on
  `(c, id)`, so two clones can produce A→B→A offline however carefully each one
  checked before writing. A write refuses a cycle it can see; the renderer drops
  the edge that revisits an id, re-roots there, and says so on stderr.
- `--tree` with `--format medium` is refused: a header block has nowhere to put
  a spine. `--format medium` on its own is always flat, terminal or not.

### Two clones, one field

Two things follow from a set of links rather than a single value. Two clones can
each file the other's issue under the other while offline, both having checked
first, and the merge is a faithful record of what happened — so a write refuses
a loop it can see, and a tree breaks any it finds. The same goes for two clones
filing one issue under two different epics: both links survive the merge, `show`
lists them, and `--tree` nests under the later of the two rather than pretending
the other was never written.

### Bridges

Links are **imported and pushed** by both bridges. Azure DevOps takes all four
kinds, as patch operations on a work item's relations; GitHub takes `parent` and
`blocked-by`, as sub-issue and dependency mutations. Either way a whole tree
filed locally goes up in one push, each issue created before whatever links to
it, and each is created *with* its links rather than linked afterwards — a link
an issue was born with is not history.

What is not sent is said out loud rather than dropped. GitHub has no mutation for
marking a duplicate and no `related` link at all; and a link whose target is not
on the tracker — an issue in another repository, or one never pushed — is
reported per link, naming what to push first.

An entity id names an object store, so a parent outside this repository is the
unsolved case throughout — see `parent` in `docs/issues.md` and `TODO.md`. The
rendering degrades to `↑` for it, which is the whole of what a tree can honestly
say about a link it cannot follow.

## What happened when

Every write is a commit on the notes ref, one per action — an issue being filed,
a comment being posted, a label being added — authored by whoever did it and
dated when they did it. `git issue log` is `git log` over that:

```sh
git issue log                   # everything, oldest action last
git issue log 4b0755a3e769      # one issue's history
git issue log --oneline -p      # every git log flag passes straight through
```

```
1aaaa83 Assign jdoe to "Bump golang.org/x/crypto from 0.31.0 to 0.36.0"
53a4842 Create issue "Bump golang.org/x/crypto from 0.31.0 to 0.36.0"
2a5aaeb Update "cat_sort_uniq reorders the blob"
234c61e Create issue "cat_sort_uniq reorders the blob"
ada5192 Close "Anchor blobs do not survive gc or a fresh clone" as completed
4917599 Rename to "Anchor blobs do not survive gc or a fresh clone"
e99f0d6 Add label "bug" to "Anchor blobs are pruned by gc"
1a46167 Comment on "Anchor blobs are pruned by gc"
2c9b148 Create issue "Anchor blobs are pruned by gc"
```

In full, a created issue's title and description are the commit's subject and
body, a comment's text is the body of its own commit, and an `Issue:` trailer
names the entity:

```
commit 1a46167f...
Author: jdoe <jdoe@users.noreply.github.com>
Date:   Tue Jun 2 15:52:10 2026 +0000

    Comment on "Anchor blobs are pruned by gc"

    Confirmed on 2.42 and on 2.53. It reproduces with a single issue in the
    tracker, so it isn't a fanout problem.

    Issue: b8c19884bc845e84f22ae2bd298417e1399a937b
    Origin: https://github.com/example/tracker/issues/142
```

Because the authors are real, git's own tools work on the tracker unchanged —
`git shortlog -sn refs/notes/issues/open` counts contributors, `--author` and
`.mailmap` behave as they do anywhere else, and `-p` shows the exact event lines
each action wrote. An import maps a GitHub login to its noreply address for
this; that is a byline, not proof, and the authoritative record of who wrote an
event is the `a` field inside it.

Two things the log is not. It is **not a source of truth** — commit messages are
prose about events whose own bytes are authoritative, and `git issue show` is
what says what an issue currently is. And it **does not converge the way the
blob does**: two clones that import the same issues independently end up with
identical blobs and two separate sets of commits, so a merged log lists each
action twice.

### Repacking after a large import

A commit per action means several versions of every issue's blob, and `git
fast-import` chains those versions oldest-first — so the version a listing reads,
the newest, sits at the deep end and replays the whole history of that issue. It
costs nothing day to day and a lot after a bulk import.

**A pull that writes more than a thousand commits repacks the object store on
its way out**, and says so:

```
Repacking after 17773 commits; this is a one-off and may take a while.
```

Measured on 2,000 issues imported with their full history:

```
git issue list       1.74s  ->  0.21s
notes history       149 MiB ->  19 MiB
```

It runs `git repack -adf`, which is not what `git gc` does — repack without `-f`
reuses the deltas it already has, so a gc moves that 1.74s to 1.58s and the
chains survive it. `--aggressive` is not used either: it widens the delta window
for a great deal more time and gains nothing here, because the problem is which
end of the chain the current version sits at, not how well it compresses.

The repack itself takes about 12 seconds on a repository with 240,000 objects of
code history in it, and longer on a bigger one. `git config gc.auto 0` turns it
off, along with git's own automatic maintenance. A resuming pull writes a
handful of commits and never triggers it.

## Syncing

Issues live on `refs/notes/issues/open`, so they sync like anything else in git.
`pull` does the fetch and the merge, and reports what moved the way `git fetch`
does:

```sh
git issue pull origin           # fetch another clone's notes refs and merge
git issue pull                  # origin is the default, as in git
git issue push origin           # send yours back
```

```
From /srv/git/project.git
   3f9a1c2..8b4e0d1  refs/notes/issues/open -> origin/notes/issues/open
Updating 3f9a1c2..8b4e0d1
Fast-forward
 A 4b0755a3e769  open    Anchor blobs are pruned by gc
 M e9037839d7f8  closed  Notes merge reorders lines permanently  [storage]
2 issues changed, 1 added(+), 1 updated
```

The shape is `git pull`'s: the fetch ref-update line, whether the merge was a
fast-forward, the changed issues where git lists changed files, and a tally in
git's own words. A pull that moved nothing prints just `Already up to date.`

### Targets

The target is `[scheme:]remote`. A bare remote is a git fetch; `github:` and
`ado:` read a platform's API instead:

```sh
git issue pull github:origin              # open issues from the repo origin points at
git issue pull --all github:origin        # closed ones too
git issue pull github:owner/name          # a repo you have no remote for
git issue pull --since 2026-01-01 --limit 200 --all github:origin
```

The precise rules for what a scheme is and where it reads from are under
[Pull and push targets](#pull-and-push-targets) below.

### Azure DevOps

`ado:` works against both Azure DevOps Services and an on-prem Azure DevOps
Server — the organization and project come out of the clone URL either way, so
there is nothing to configure:

```sh
git issue pull ado:origin                 # open work items, in the area you picked
git issue pull ado:origin#Web/Auth        # that area and everything under it
git issue pull --type Bug ado:origin      # only bugs
git issue pull ado:origin#                # forget the saved area and choose again
```

A real Azure DevOps project is organization-wide, so the first pull draws the
area tree and asks which part of it you work in:

```
Pick an area to track in MyProj:

   0  MyProj   everything
   1    Web
   2      Auth
   3      Api
   4    Mobile

Area [0]:
```

The answer is saved per clone, and every pull after that needs no flags. It is
never asked when stdin is not a terminal, so a pipeline reads the whole project
rather than blocking on a prompt nobody can see.

**The area is scope, not state.** It is the query's filter and where a push
files a *new* work item — nothing else. It reaches no event, no ledger line and
no output, because where a work item is filed is a fact about it in Azure DevOps
rather than about the issue. So `git issue list` cannot tell a `Web` issue from
a `Mobile` one, and a push never moves a work item that already exists; both
follow from the same decision. See [docs/bridge-ado.md](bridge-ado.md).

States come across as Azure DevOps spells them — `New`, `Active`, `Resolved`,
`Done` — rather than being flattened onto open and closed, so `--state Resolved`
works and a mapping across bridges stays a question for later.

**A repeat pull asks only for what changed.** Each import records how far it
read, and the next one resumes from there — 153 issues in 11.7s the first time,
0.79s the second. A git pull is incremental already, because git's transport
sends the objects this clone is missing and nothing else.

## Pushing

`push` is `pull` turned around, and takes the same targets:

```sh
git issue push origin           # the notes ref and the origin ledger
git issue push github:origin    # write local changes back through the bridge
git issue push github:acme/git-issue    # seed a fork with your issues
git issue push --dry-run github:origin  # the plan, and nothing written
git issue push ado:origin       # write local changes back to Azure DevOps
git issue push ado:origin#Web/Api 4f2a1c9   # file this one under that area
```

A bridge push shows what it intends to do and asks first:

```
To github.com/acme/git-issue
 A 8b31e07f2a4c  open  Crash on empty input
 M 4f2a1c9b0d51  open  Fix the parser
 ! 1d0e5b2a7c93  open  Off-by-one in seek
   4f2a1c9b0d51  title, labels
   1d0e5b2a7c93  title changed on both sides
2 issues to push, 1 conflicted

Push 2 issues to github.com/acme/git-issue? [y/N]
```

A create carries no note — the `A` and the title already say it is new, and a
fork seed of hundreds would otherwise be a page of identical "new issue" lines,
which is exactly what `git push` does not print. Once confirmed, the write draws
the same `pushed/total` bar a pull draws while fetching:

```
Pushing issues: 100% (312/312), done.
312 issues pushed to github.com/acme/git-issue
```

**Nothing is forced.** A remote holding issues you have not seen is refused, and
the fix is `git issue pull` then push again — both refs here are grow-only sets
of lines, so merging never discards anyone's write. A field both sides changed
conflicts that one issue, and every other issue still goes.

**A push works out what to send by asking the tracker.** There is no record of
what was pushed last time; instead the importer is replayed against upstream's
current state, and what it produces is by definition what upstream already
knows. Comparing folded values rather than events is what makes a mirror settle:
a change you push comes back as the tracker's own event saying the same thing,
and the next push has nothing to do.

**One issue can live in several trackers at once.** Where each one lives is kept
on `refs/git-issue/origins`, one file per tracker, not inside the issues — so
pulling from an upstream and pushing to a fork links the same issue to both, and
dropping a tracker you no longer care about deletes one file instead of
rewriting every issue. That ledger travels with the notes ref, and a bridge push
writes nothing else: `git issue log` stays a history of the tracker rather than
of its own bookkeeping.

### Resuming

Seeding a fork is **resumable**. An issue is created upstream only when the
ledger has no entry for it there, so an interrupted run continues where it
stopped instead of filing everything twice.

```sh
git issue pull github:origin              # resumes from the last import
git issue pull --full github:origin       # re-read everything, ignore the watermark
```

The watermark lives in `.git/git-issue/sync.json` and is never pushed: it says
what *this* clone has read, which is true of nowhere else. Delete it and the
next pull is a full one — the issues themselves are in the object store, and a
re-import converges on what is already there rather than duplicating it.

**An import reads open issues by default.** Most repositories carry far more
resolved tickets than live ones, and pulling a decade of them by default would
make the first sync of a large project the slowest thing you ever did with this.
`--all` lifts the filter.

Each scope keeps its own watermark, so `--all` starts from where the last
`--all` finished rather than from where the last open-only pull did. An `--all`
run advances both, since it has necessarily seen every open issue too.

The filter has one consequence worth knowing: an issue closed upstream since
your last import is not in the result set, so its local copy goes on saying
`open` until an `--all` run — a filter cannot report the absence of what it
filtered out.

An import is **idempotent**: event ids are derived from GitHub's own identifiers
and timestamps rather than generated, so running it twice adds nothing and the
ref does not move. Issues arrive with their real history where GitHub keeps any
— every rename, every label added and removed, with the actor and time of each.

### Credentials

Credentials are never stored by this program. It uses, in order, `--token`,
`GITHUB_TOKEN` / `GH_TOKEN`, `gh auth token`, then `git credential fill` (so
whatever helper you already have — keychain, libsecret, or the GitHub CLI's).
`gh` comes before `git credential fill` because fill drops to an interactive
terminal prompt when no helper answers, which would otherwise mask a working
`gh auth login`.

Deleting a bridge from your life is `git issue pull` against a plain git remote:
the issues are already yours, in your object store, with no GitHub in the loop.

## Pull and push targets

The target of a sync is `[scheme:]remote`. The scheme names *where* the entities
are read from; the remote names *which* repository.

| Written | Reads from |
| --- | --- |
| `origin` | the notes refs on the remote `origin` |
| `git:origin` | the same, said explicitly |
| `github:origin` | GitHub's API, for the repository `origin`'s URL points at |
| `github:owner/name` | GitHub's API, for a repository this clone has no remote for |
| `ado:origin` | Azure DevOps, for the repository `origin`'s URL points at |
| `ado:origin#Web/Auth` | that project, scoped to the area `Web\Auth` and everything under it |

Two rules make this unambiguous and unsurprising:

- **A bare target means `git`.** The git path needs no credentials and no API,
  so an unqualified command can never be the one that prompts for a token and
  imports ten thousand entities. Reading from a forge is always spelled out.
- **Only a known scheme is a scheme.** The `https:` in an https URL and the
  `git@host:` of an scp-style one are part of the target, so no amount of URL
  punctuation can be mistaken for a bridge name.

A missing target is `origin`, exactly as in git, whether or not a scheme was
given: `github:` on its own is the bridge against `origin`.

A bridge import **resumes**: it records the newest upstream `updatedAt` it read,
per repository and per scope, and the next pull asks only for what changed.
`--full` ignores that and re-reads everything. A git-mode pull needs no such
machinery, because git's transport already sends only the objects this clone is
missing.

A git-mode pull fetches into a **remote-tracking notes ref** —
`refs/remotes/<remote>/notes/issues/open` — and then unions that into the local
ref. Under `refs/remotes/` rather than inside `refs/notes/`, so that a wildcard
`git push origin refs/notes/*:refs/notes/*` cannot publish one clone's idea of
what another remote holds. The tracking ref is also what makes a before/after
diff possible, and that diff is the whole fetch summary.

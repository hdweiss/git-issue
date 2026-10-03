# git-issue

A fast, distributed issue tracker for humans that feels like git and lives in the
repository it tracks.

Issues, comments, pull requests and review threads are git objects on their own
refs, next to the code they are about, and they sync the way code does
— fetch and push. There is no server, no database and nothing to run: a static
8 MB binary, and the `git` you already have.

It also integrates with the tracker your project already uses. `git issue pull
github:origin` brings a repository's issues into the object store; `gitea:` and
`ado:` do the same for Gitea/Forgejo and Azure DevOps.
Each pushes local changes back the same way, resuming from where the
last sync finished, and prints its plan before it writes anything. So the
terminal is where you work and the tracker is where the rest of the world looks,
and neither has to know how the other is spelled.

What makes that more than a storage trick is that it **converges**. Two people
can work on the same issue with no connection between them and both keep their
work, because merging is a set union and not a decision anybody has to make.

## What it looks like

Three commands, and most of the idea.

**`git issue`** lists what is open — one issue per line, newest first, id then
status, type, title, labels and comment count, nested under each issue's parent:

```
e9c1e40b7a2d  ●  👑  Azure DevOps bridge                          💬 2
1c4f88b0e2da  ●  ✅  ├─ Push
7d20b5ff1c38  ●  🐞  ├─ Area picker drops the subtree    [bug]     💬 5
a0417ec6b93f  ●  ✅  │  └─ Raw-state filters
3f8a10c94e77  ●  ✅  └─ The import
e9037839d7f8  ●  ✅  Anchor blobs are pruned by gc
885797fb2d9b  ●  🐞  ↑ Reject "short" ids                [bug, design]
```

On a terminal the status is a coloured dot, the type an emoji, and a title that
would wrap is trimmed to fit. Piped or with `NO_COLOR` set, the words come back,
titles print in full and the listing goes flat, so it stays greppable. `--tree` /
`--no-tree` force the nesting either way; [docs/ui.md](docs/ui.md#listing) covers
the rendering.

**`git issue log`** is `git log` over the tracker itself. Every write is a commit
by whoever made it, so a project's issue history reads like its code history:

```
40b6d91 Close "Area picker drops everything below the chosen node" as completed
8c0627e Create issue "Raw-state filters"
1c3263a Assign alice to "Area picker drops everything below the chosen node"
de2b8f8 Rename to "Area picker drops everything below the chosen node"
547476a Comment on "Area picker drops the subtree"
1b678b7 Add label "bug" to "Area picker drops the subtree"
4a19b60 Create issue "Area picker drops the subtree"
```

Every `git log` flag passes straight through — `git issue log <id>` narrows to
one issue, `-p` shows the exact events an action wrote. In full, a rename carries
the title it replaced, a comment carries its text, and an `Issue:` trailer names
the entity:

```
commit de2b8f87670aee4e4f4d711e1b07dd0d064ec7db
Author: Alice <alice@example.com>
Date:   Fri Sep 4 22:38:14 2026 +0200

    Rename to "Area picker drops everything below the chosen node"

    Previously: "Area picker drops the subtree"

    Issue: fe44d6edd0d4d635f6e431323aa7cecee15cf18f
```

**`git issue pull`** reports the way `git pull` does — the ref update, whether it
fast-forwarded, then the issues that moved, where git would list changed files:

```
From /srv/git/project.git
   3f9a1c2..8b4e0d1  refs/notes/issues/open -> origin/notes/issues/open
Updating 3f9a1c2..8b4e0d1
Fast-forward
 A 4b0755a3e769  open    Anchor blobs are pruned by gc
 M e9037839d7f8  closed  Notes merge reorders lines permanently  [storage]
2 issues changed, 1 added(+), 1 updated
```

That is the "what changed since I last looked" view a web tracker makes you
reconstruct from a notification list. The full set of commands — filing, editing,
commenting, closing, relations, and the same verbs again for pull requests — is
under [Commands](#commands).

## Why

### It converges

An issue is an append-only set of events — one per field change, comment, label
or link — and each event's id is `git hash-object` of its own bytes. Merging two
clones is the union of their lines, and folding that set to current state is
order-independent, so:

- Two people editing **different fields** of one issue offline both survive the
  merge. There is no conflict, because nothing overwrites anything.
- Removing a label names the id of the `label.add` it undoes, never the label's
  text — so two clones that added `design` independently both get retracted.
- Two identical comments posted in the same second stay two comments; every
  event carries a nonce.
- **Re-importing what you just pushed is a no-op.** A bridged event derives its
  nonce from upstream identity, so the ref does not even move.

This is the part that is hard to retrofit, and it is what "distributed" has to
mean for a tracker rather than for a database underneath one. A store that
replicates but resolves conflicts by newest-timestamp-wins, or by asking you to
pick a side, will silently throw away somebody's comment — and "resolve this
conflict" is not an answer you can give a person about their own writing. What is
merged here is the issue, not the file it happens to live in.

### The history is the real one

An import does not just fetch current state — it brings the *upstream* history
across, where the platform keeps any. Every rename, every label added and
removed, with the actor and the time of each, reconstructed as events. So
`git issue log <id>` is the whole story of an issue and not just the story of
your copy of it, and `git issue pull` can tell you what changed since you last
looked instead of leaving you to reconstruct it from a notification list.

### The issues live with the code

A clone carries the tracker with it, in the same object store as the commits that
fix it. Nothing has to be exported, backed up or kept in step: `git clone` is the
backup, and an issue cannot go missing while the repository it belongs to
survives.

Because every write is a real commit by a real author, git's own tools work on
the tracker unchanged — `git shortlog -sn refs/notes/issues/open` counts
contributors, `--author` and `.mailmap` behave as they do anywhere else.

### No web UI in the loop

Filing, reading, commenting and closing happen where you are already working.
There is no tab to switch to, no page to load, and no network round trip per
read — a listing is a local fold over the object store.

### Fast enough to stay in the flow

Filing an issue takes about **15 ms**; a listing takes 7, and showing one takes
13. Twenty issues filed back to back take a third of a second in total. That is
below the threshold where you notice a tool is running, which is the whole point:
a thought worth writing down gets written down *now*, in the middle of whatever
you were doing, instead of being deferred until you next have a browser open —
which usually means never.

It stays that way with a real backlog. A couple of thousand issues list in about
a fifth of a second, because a listing is one batched `cat-file` and one fold,
not a process per issue. And it is one static binary of about 8 MB with no cgo
and no runtime dependencies, so there is no interpreter to start and no
connection to open before the first byte of output.

### It feels like git — the output too

Not just the flags. `git issue log` *is* `git log`, and every one of its flags
passes straight through. Ids abbreviate like object names, and an ambiguous
prefix is refused rather than resolved arbitrarily. `--format medium` is shaped
like `git log`'s medium format. Output pages through `$GIT_PAGER`, colours on a
terminal and goes plain, flat and greppable down a pipe. `git issue add` with no
title opens an editor, the way `git commit` does, and an empty buffer aborts.

The point is that almost nothing here has to be learned separately. See
[docs/ui.md](docs/ui.md) for the reasoning behind each of those choices.

### Nothing to run

No daemon, no database file, no sidecar process, no schema to migrate. State is
git objects, and `git gc` is the whole maintenance story.

### It works offline

A consequence of the above rather than the pitch, but a real one: on a plane, on
a connection that keeps dropping, or inside a network that cannot reach
github.com at all, nothing is degraded. A pull brings a tracker's state into the
object store, and from then on that tracker is just another remote.

## Prior art

Validated against several tools that solve overlapping problems the same way:
**git-bug** (custom object model, CLI/TUI/web, GitHub/GitLab bridges),
**git-appraise** (Google's code review tool, built directly on `git notes`), and
Gerrit's **NoteDb** (change/account metadata stored as git commits instead of SQL
rows). Their design choices — and a few of their known pitfalls — directly
informed the decisions in [`docs/`](docs/).

**beads** solves an adjacent problem and is worth using for it: memory for a
coding agent, a dependency graph of tasks that keeps an agent oriented across a
long-horizon job. That is a scratchpad; this is a system of record, and the two
compose rather than compete. `git issue add -F -` takes a Markdown buffer on
standard input, which is the seam between them today.

## Install

Prebuilt binaries for Linux, macOS and Windows are on the
[releases page](https://github.com/hdweiss/git-issue/releases). With
[mise](https://mise.jdx.dev):

```sh
mise use -g github:hdweiss/git-issue    # installs git-issue and git-review
```

## Build

Go 1.26 and `git` 2.42 or newer are the only requirements. The build is static —
no cgo, no runtime dependencies.

```sh
make                        # -> bin/git-issue
make test                   # unit tests, doc-hash vectors, golden CLI parity
make install                # PREFIX ?= /usr/local; binary and man page

export PATH="$PWD/bin:$PATH" # git picks it up as a subcommand
```

## Commands

`git issue` on its own lists; `git issue <id>` shows one. Everything else is a
named subcommand. Ids abbreviate like git object names — as few characters as it
takes to be unambiguous — and a positional id is always "the issue this command
hangs off". The full walkthrough, with the reasoning behind each choice, is in
[docs/ui.md](docs/ui.md).

### Read

```sh
git issue                         # list, newest first; shortcut for `git issue list`
git issue --state open --type bug -l urgent   # filters, mirrored from add's flags
git issue --format medium         # a block per issue, in the shape of `git log`
git issue list                    # on a terminal, nested under each issue's parent; flat down a pipe
git issue list --no-tree          # flat, even on a terminal (--tree forces the nesting back on)
git issue list <id>               # the whole subtree filed under that issue
git issue list none               # the issues filed under nothing
git issue <id>                    # show one; shortcut for `git issue show <id>`
git issue show <id> <comment>     # one comment and the replies under it
git issue show <id> --web         # open its page on the tracker it came from
```

### Write

```sh
git issue add -t "Title" -d "Body"            # every field is a flag
git issue add                                  # no title: an editor opens, like git commit
git issue add -t "Title" --type bug -l ui,design -a alice -m v2
git issue add <id> -t "A sub-issue"           # filed under that issue
git issue edit <id> -t "New title" --type bug  # one event per changed field, no editor
git issue edit <id> -l perf --remove-label ui
git issue edit <id> --milestone none           # `none` empties a field
git issue edit <id>                            # no flags: the issue opens in the editor
git issue close <id>                            # set status to closed
git issue close <id> --as not-planned          # …with a reason
git issue close <id> --as duplicate <id>       # …and a duplicate-of link
git issue reopen <id>                          # set it back to open
git issue edit <id> --status Active            # a bridge's own state, verbatim
git issue comment <id> -m "Confirmed."         # post a comment; no -m opens an editor
git issue edit <id> <comment> -m "..."         # rewrite one comment
git issue remove <id>                          # unlist an issue (does not erase)
git issue remove <id> <comment>                # retract one comment (a tombstone)
```

### Relations

```sh
git issue edit <id> --parent <id>              # file under another issue
git issue edit <id> --parent none              # detach
git issue edit <id> --rel blocked-by:<id>      # any other kind of link
git issue edit <id> --no-rel blocked-by        # drop every link of a kind
```

`parent`, `blocked-by`, `duplicate-of` and `related` are the kinds with names;
an unknown kind is kept as written. A link is stored on one end only and the far
end is derived. See [docs/ui.md](docs/ui.md#relations).

### History

```sh
git issue log                     # the ref's own history, one commit per action
git issue log <id>                # limited to one issue
git issue log --oneline -p        # every git log flag passes straight through
```

### Sync

```sh
git issue pull                    # the branch's remote — its notes refs, and its
                                   # GitHub / Azure DevOps / Gitea issues if it is one
git issue pull origin             # another clone's notes refs
git issue pull github:origin      # GitHub's API; open issues, resuming from last time
git issue pull --all github:owner/name     # closed issues too, from a repo you have no remote for
git issue pull ado:origin#Web/Auth         # an Azure DevOps area and everything under it
git issue pull gitea:origin                # a Gitea or Forgejo repository
git issue push origin             # send the notes ref and the origin ledger
git issue push github:origin      # write local changes back through the bridge
git issue push --dry-run github:origin     # the plan, and nothing written
```

A bare target is always plain git — no credentials, no API. `github:`, `ado:` and
`gitea:` read a platform's API instead; each resumes from where the last pull finished and
converge on re-import, and nothing is ever force-pushed. See
[docs/ui.md](docs/ui.md#syncing) for the walkthrough, credentials, and the
resume watermark.

### Help

```sh
git issue -h                      # every command, one line each
git issue list --help             # the filters and formats, in full
git issue edit --help             # what "one event per field" buys
```

Use `-h`, not `--help`: git turns `git issue --help` into a man-page lookup
before the binary ever runs.

## How it works

Each issue is one `git notes` blob: a grow-only set of events — one per field
change, comment, label, or link — that fold to the issue's current state.
Concurrent edits merge with git's own `cat_sort_uniq` notes strategy, so two
people editing different fields of one issue offline both survive the merge with
no conflict. Issues live on `refs/notes/issues/open`, with
`refs/notes/issues/archived` reserved for terminal ones, and the map from an
issue to its upstream counterpart on GitHub, Azure DevOps or Gitea lives on
`refs/git-issue/origins` — never inside the issue, so one issue can be linked to
an upstream and a fork at once.

## Reviews

`git review` is the second entity type, built from the same tree: a change
proposal with a base, a head, commentary anchored to the code, verdicts, and a
terminal state of merged or abandoned. A pull request is a review with an entry
in the origin ledger; a review of a local branch — by a person or by an agent —
is one that has none, and every command works the same on both.

```sh
git review add -t "Fix the area subtree walk"   # base and head from the branch you are on
git review comment <id> --on internal/issue/area.go:42 -m "off by one"
git review approve <id>                          # or request-changes
git review close <id> <comment>                  # resolve that thread
git review status <id>                           # what is blocking the merge
git review checks --commit HEAD --set build=fail # a build script, no review needed
```

Three things are worth knowing before reading further. A comment's anchor
records the *revision* it was written against, so a thread reads as **outdated**
when the file under it changed and **detached** when its commit was rewritten
away — different things, never repaired by moving the anchor. A verdict carries
the revision too, so a push makes it stale rather than silently still counting.
And check runs live on their own ref keyed by **commit**, not by review, because
a build result is a fact about a commit: `git log --notes=checks/runs` renders
them for free, and a rebase needs no logic at all.

`--closes` records intent and nothing more — nothing closes an issue on merge,
because a cascade could not converge and a review merged in a fork should not
close an issue upstream. See [docs/reviews.md](docs/reviews.md).

```sh
git review pull github:origin        # import open pull requests
git review pull --all github:origin  # merged and closed ones too
git review push github:origin        # write local changes back
git review push ado:origin           # Azure DevOps pull requests, same way
git review push origin               # the notes refs and the origin ledger
```

The GitHub import brings across fields, comments, review threads with their
anchors, verdicts with the revision each was cast against, `closes` links, and
the head commit's check runs — the last onto `refs/notes/checks/runs`, keyed by
commit, never into a review's blob. Note what it does *not* import: GitHub's own
`isOutdated`. Whether an anchor still describes the head is derived here, from
the object store, because the comparison GitHub makes is not the one
`docs/reviews.md` defines.

The push goes back the same way: fields, labels, review requests, comments,
threads with their anchors, resolutions, verdicts and dismissals. It prints its
plan and asks before it writes. Four things are reported unsent rather than
guessed at — `closes`, because GitHub derives closing links from keywords in the
description; `status.reason`, because a pull request has no close reason;
`merged`, because a merge is performed upstream; and `head`, because a tracker
learns that from the git remote. A review you opened locally is filed only once
its branch is on the remote, and pushing the branch stays your `git push`.

The Azure DevOps push works the same way — fields, labels, comments, review
threads with their anchors and resolution, and votes. A verdict's message has
nowhere to live on a vote, so it goes up as its own discussion thread. Reviewer
changes and `closes` artifact links are reported unsent for now; a completion is
the server's to perform.

## How it fits together

The specification is in [`docs/`](docs/): `storage-model.md` for where bytes
live, `blob-format.md` for what one entity is, `issues.md` and `reviews.md` for
the two vocabularies, and `bridge-*.md` for the platform mappings.
[`AGENTS.md`](AGENTS.md) describes the implementation. Everything outside
`internal/issue` and `internal/review` is entity-agnostic, so a third type is a
package and a line in the Makefile.

## License

MIT — see [`LICENSE`](LICENSE).

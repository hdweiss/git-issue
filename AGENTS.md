# AGENTS.md

Orientation for anyone (human or agent) picking this up cold.

## What this is

An offline-first issue tracker whose entire state lives in a git repository's
object store. **`docs/` is the specification and the primary work**; `cmd/` and
`internal/` are a Go implementation of it, and the spec leads.

Design is validated against git-bug, git-appraise, and Gerrit's NoteDb. When a
claim in `docs/` says "confirmed empirically", it was measured in a throwaway
repo, not reasoned about. Keep that standard.

## Read in this order

| File | Scope |
| --- | --- |
| `docs/storage-model.md` | Where bytes live: refs, keying, sync, provenance, indexing. Entity-agnostic. |
| `docs/blob-format.md` | What is inside one entity's blob: events, ids, ordering, the three field kinds. Entity- and platform-agnostic. |
| `docs/issues.md` | The issue entity type: its ops and vocabularies. Platform-neutral. |
| `docs/bridge-github.md` | GitHub mapping, both directions. All GitHub-specific detail belongs here and nowhere else. `docs/bridge-ado.md` and `docs/bridge-gitea.md` are its Azure DevOps and Gitea counterparts. |
| `docs/ui.md`, `docs/existing-tools.md`, `TODO.md` | The CLI walkthrough and the reasoning behind each choice, prior art, unscoped ideas and the coverage matrix. |

The layering is deliberate and load-bearing: **PR support is planned**, so
nothing type-specific may leak into the first two documents, and nothing
platform-specific into the first three.

## Invariants

Violating any of these is a correctness bug, not a style preference.

1. **Order by `(c, id)`. Never by `ts`.** `ts` is display metadata written by
   someone else's clock.
2. **An event's id is `git hash-object` of its canonical line, newline
   included.** No `id` field is ever stored. An entity's id is its `create`
   event's id.
3. **Canonical JSON (RFC 8785) is a correctness requirement**, because ids hash
   these bytes. Two implementations that serialize differently fork identity.
4. **Every event gets a fresh random nonce** — except bridged events, which
   derive it from upstream identity so re-import converges.
5. **Never `git cat-file -e` an entity id.** Ids name objects that generally do
   not exist.
6. **Preserve unrecognized events and fields byte for byte.** This is what makes
   new ops, new entity types, and new bridges additive.
7. **Every field is a scalar, a list (OR-Set), or a thread.** There is no fourth
   kind. Adding one is a spec change.
8. **Address removals, edits, annotations and reactions by event id, never by
   value.** A list member is the pair (`val`, `ref`), not `val` alone.
9. **In-blob state is truth; ref placement is an index.** Same for any query
   index: local, derived, disposable, never synced. The **origin ledger**
   (`refs/git-issue/origins`) is the one exception and a third category: it is
   neither entity state nor an index, and it *is* synced, because a clone
   without it re-creates every issue upstream on its first push.
10. **Platform-specific data goes in a namespaced op** (`github.origin`), and
    core folding must produce complete state while ignoring every one of them.
11. **Read object data in batch.** `git cat-file --batch`, never per entity.

## Measured facts — do not re-derive

At 20,000 entities unless noted.

| | |
| --- | --- |
| `list`, one `notes show` + one `jq` per entity | 58.6s |
| `list`, one batched `cat-file` + one fold | 0.755s |
| Floor: enumerate the notes tree | 0.15s |
| Floor: read every note blob, no parsing | 0.54s |
| Cache hit keyed on notes ref SHA | 3ms |
| `git diff --name-only <old-ref> <new-ref>` | 3ms, returns exactly what changed |
| Generate 1000 test issues | 2.0s |
| GitHub import, 100 issues with comments and timelines (4 GraphQL pages) | 6.6s |
| Write 50,000 commits (one per action, 5,000 entities, sharded tree) | 5.3s, 27.5 MB packed |
| The same against a **flat** notes tree | 47.5s |
| The same, entities interleaved chronologically rather than one at a time | 5.8s, same packed size |
| One commit per entity instead (5,000 commits) | 1.21s, 4.7 MB packed |
| One commit for the whole run | 0.41s, 2.4 MB packed |
| `list` over 2,008 issues, straight after a 17,773-commit import | 1.74s |
| The same after `git gc` | 1.58s — reuses the deltas, so it fixes nothing |
| The same after `git repack -adf` | 0.21s, notes history 149 MiB → 19 MiB |
| `git repack -adf` itself, 240,000-object repo with code history | 11.7s on 6 cores |
| `list` over the same issues imported in one commit (the old behaviour) | 0.24s, notes history 4.6 MiB |
| The same 100 issues re-imported | no-op, ref does not move |
| Full import of git-bug/git-bug, 153 open issues | 11.7s |
| The same, resuming from the recorded watermark | 0.79s, 1 issue re-read |
| Gitea push plan, 1002 mapped issues, one `GET /issues/{index}` each | 12.6s |
| The same through the paged issue list (21 requests) | 0.9s |
| The same one at a time, pool raised from 12 workers to 32 | 21s — **slower** |
| One authenticated Gitea request against one anonymous, same host | ~3.7× |
| Gitea push plan over a 1196-issue mirror with long comment threads | peak RSS 108 MB |
| The same before the page walk learned to stop | climbed past 1.9 GB, then SIGKILL |
| 200-comment thread | 5.1 MB written for a 50 KB blob; 2.4 MB loose → **112 KB after gc** |

The bulk generator these came from is gone with the rest of the prototype. To
re-measure, write a throwaway script that emits four canonical events per issue
and one `git notes add` per entity — `internal/issue.Create` is the shape.

Confirmed against the live GitHub API, importing `git-bug/git-bug`:

- `timelineItems(itemTypes:)` takes **enum names** (`LABELED_EVENT`) while the
  response's `__typename` is the **type name** (`LabeledEvent`). Sending type
  names to `itemTypes` fails the whole query.
- A real re-import is a genuine no-op: same events, same ids, union adds
  nothing, ref unmoved. This is the nonce and clock derivation working.
- A `label.remove`'s `ref` reproduces under stock `git hash-object` of the
  `label.add` line it names — the OR-Set is real, not approximated.
- `filterBy: {since:}` compares against `updatedAt` and is **inclusive**, so
  resuming from a recorded watermark re-reads exactly the boundary issue. That
  is the desired direction to err in; the union discards it.
- A `states: null` argument means "every state". An empty list is not the same
  thing and matches nothing at all.

Behaviour confirmed by experiment:

- git auto-shards the notes tree (250 `00`–`ff` subtrees at 20k). No bucketing
  code needed. The switch from flat happens **between 64 and 96 notes**, and
  any `git notes` command re-shards the whole tree to git's own preference —
  which renames every note, so a path-limited `git log` must name every fanout
  depth or it silently loses the history under the others.
- git reads a notes tree of **mixed** fanout correctly, so a writer placing new
  entities at a depth git would not have chosen is a settling difference, not a
  breakage.
- The commit log **does not converge** the way the blob does: two clones that
  import the same issue independently write byte-identical blobs and two
  disjoint sets of commits, so a merged `git log` lists every action twice.
- **`fast-import` chains a note's blob versions oldest-first**, so the version
  a listing actually reads — the newest — sits at the deep end and replays the
  note's whole history. Measured on a bulk import: tip blobs at delta depth 23
  mean, 50 max, and `list` 7x slower. `git repack -adf` re-picks the bases so
  tips land at depth 0–1; **plain `git gc` does not**, because repack without
  `-f` reuses the deltas it already has, and `--aggressive` costs much more for
  nothing. `cmd/git-issue.repack` runs `git repack -adf` automatically after an
  import of more than `autoRepackAt` commits, unless `gc.auto` is 0.
- `git notes` accepts **any full-length hex name**, existing object or not. It
  rejects a short id: `fatal: failed to resolve 'deadbeefdeadbeef'`.
- A blob written only to mint an id is unreachable, pruned by `gc --prune=now`,
  and **absent from a fresh clone** — while list/show/append keep working.
- `cat_sort_uniq` merges concurrent edits to different fields with no conflict,
  and **permanently reorders lines alphabetically**.
- `git blame` on a note **reattributes pre-existing lines to the merge commit**
  after a merge. Attribution is a property of how bytes were written; the `a`
  field inside the event is authoritative.
- Without a nonce, two people posting identical comment text in the same second
  produce byte-identical blobs and one comment is silently lost.
- A client whose clock was 3 years fast won every title merge until 2033.
- `git issue remove` propagates when nobody else touched the note, but **any
  concurrent edit resurrects the whole entity** on the next merge.

GitHub specifics, verified live:

- The timeline API gives `labeled`/`unlabeled`/`assigned`/`unassigned` with
  actor, timestamp and a stable event id — so OR-Set history reconstructs
  faithfully.
- `RenamedTitleEvent` gives **complete title history**; bodies have none.
- PR review comments thread via `in_reply_to_id`, normalized to **one level**
  (48 of 100 sampled were replies; none pointed at another reply).
- `subscribed`/`unsubscribed` is the highest-volume event type. Drop it.

## The implementation

Go, one static binary per command. The layering is load-bearing: **only
`internal/issue` knows what an issue is, and only `internal/review` knows what a
review is.** Neither imports the other, and nothing below them imports either.

| Package | Scope |
| --- | --- |
| `internal/gitx` | Every call this tracker makes to `git`, the editor it launches included. A leaf: imports nothing of ours. |
| `internal/bridge/github/api` | Every call this tracker makes to GitHub, reads and mutations. A second leaf: knows GitHub, not us. Imported as `ghapi`. |
| `internal/bridge/ado/api` | The same for Azure DevOps, Services and Server alike. A third leaf. REST, because there is no GraphQL API. Imported as `adoapi`. |
| `internal/bridge/gitea/api` | The same for Gitea, Forgejo and Codeberg. A fourth leaf. REST; there is no canonical host, so a target must name one. Imported as `giteaapi`. |
| `internal/entity` | `docs/blob-format.md` in code — canonical JSON, events, the fold, reading a ref, appending to one, sync, the sync watermark. |
| `internal/origins` | The origin ledger: which upstream object each entity is, per tracker. A fourth leaf — knows `gitx` and opaque strings, no events and no platform. |
| `internal/render` | List and detail renderers. Callers supply their own headers. |
| `internal/issue` | The issue vocabulary and refs, the commit-message grammar, and the three-way push diff. |
| `internal/review` | The review vocabulary and refs: verdicts, comment anchors, thread resolution, the checks ref, and the "what is blocking a merge" report. The second type-specific package; it imports nothing of `internal/issue`. |
| `internal/cli` | Command plumbing both binaries need and neither owns. The pager, and nothing else yet. |
| `internal/remote` | The `[scheme:]target` syntax of a pull or push. |
| `internal/bridge` | The `Bridge` contract a writable tracker satisfies, and the parent of every platform's code. Names no platform itself. |
| `internal/bridge/github/issue` | GitHub issues → issue events, and deltas → mutations. One of the two packages that know both. Imported as `ghissue`. |
| `internal/bridge/ado/issue` | Azure DevOps work items → issue events, and deltas → JSON-Patch. The other. Imported as `adoissue`. |
| `internal/bridge/gitea/issue` | Gitea issues → issue events, and deltas → REST mutations. Closest in shape to `bridge/github/issue`. Imported as `giteaissue`. |
| `internal/bridge/github/review` | GitHub pull requests ↔ review events, both directions. A sibling of `bridge/github/issue` rather than part of it, which is what keeps `cmd/git-review` clear of the issue vocabulary: the two types are siblings, and so are their bridges. Imported as `ghreview`. |
| `internal/bridge/ado/review` | Azure DevOps pull requests ↔ review events, both directions. A sibling of `bridge/ado/issue` for the same reason `github/review` is a sibling of `github/issue`. Imported as `adoreview`. |
| `cmd/git-issue` | Argument dispatch. |
| `cmd/git-review` | The same, for reviews. |

`internal/issue/message.go` is the commit-message grammar: it turns one action
onto the subject, body and trailers a commit carries. It sits in the type
package because naming what changed needs the type's vocabulary — which is also
why `internal/entity` takes a rendered message rather than rendering one.

`internal/entity` is four files in dependency order — `canonical.go`,
`event.go`, `fold.go`, `store.go` — with the package doc in `doc.go`. The
import graph is a DAG with one branch, at the type layer, and must stay one:

```
                                    ,-  issue  <-  bridge  <-  bridge/{github,ado,gitea}/issue  <-  cmd/git-issue
gitx  <-  entity  <-  render  <----+                                ^                              |
  ^                                 `-  review  <-  bridge/{github,ado}/review  <-  cmd/git-review
  |                                                                 |                              |
  '--- origins, cli ---------------------------------------------)  |  (--------------------------'
                              bridge/{github,ado,gitea}/api ------- '   remote  <-  both commands
```

Everything platform-specific lives under `internal/bridge/<platform>/`: the raw
API leaf in `api/` (imported as `ghapi` / `adoapi` / `giteaapi`), the issue
mapping in `issue/` (`ghissue` / `adoissue` / `giteaissue`), the review mapping
in `review/` (`ghreview` / `adoreview`). Directory nesting is the grouping;
package names stay platform-prefixed and unique so a command importing all three
issue bridges needs no aliases. `bridge/gitea` has no `review/` because there is
no Gitea PR-review support. The `api/` leaves still import nothing of ours — the
nesting is cosmetic.

`issue` and `review` are siblings: neither imports the other, and a target that
crosses between them is an id resolved by prefix over the other ref's note keys
(`cmd/git-review/relations.go`), which needs a ref name and no vocabulary at
all. That is what lets `git review add --closes <issue-id>` work without
`git-review` learning what an issue is.

`bridge/<platform>/issue` sits after `issue` because mapping a work item or a
GitHub issue onto events requires knowing what an issue is. Those are the *only*
packages allowed to know both; the `api/` leaves beside them have never heard of
an event, and `issue` below that has never heard of any platform.

The recipe held when it was used. A bridge is an `api/` leaf plus
`internal/bridge/<platform>/issue` satisfying `bridge.Bridge`, one case in
`cmd`'s dispatch, and one entry in `remote.Schemes`. Adding Azure DevOps moved two
other things, and both were a guess being replaced by a fact rather than the
layering giving way:

- `push_bridge.go` inferred "is this GitHub?" from substrings of a tracker
  name, to know how a bridge spells an author. `resolveBridge` knows the
  scheme, so it says so instead.
- `bridge.Lookup` joined `bridge.Claimed`, because a bridge that resolves links
  needs to ask what an upstream object is filed as here. A relation holds an
  entity id, and no upstream id yields one without that question.

`internal/issue.Diff` is the whole push comparison and names no platform, so a
second bridge reuses it rather than reimplementing it. What a bridge supplies is
only the two ends: reading the tracker's current state, and turning an
`issue.Delta` into that platform's mutations.

`internal/review.Diff` is the same thing for the other type, and it is a second
function rather than a generalization of the first. The three kinds it compares
that an issue has none of — anchored comments grouped into threads, a resolved
bit per thread, and verdicts — are review vocabulary, and pushing them up into
`internal/issue` would put reviews into the issue type to avoid duplicating a
loop.

`internal/bridge` was deliberately **not** made generic to hold both. It is
typed on `issue.Delta`, and a type parameter would put `internal/issue` on
`cmd/git-review`'s import graph — the one thing the sibling arrangement exists to
prevent — in exchange for an interface with one implementation.

The review push pipeline's types — `Candidate`, `Plan`, `Skip`, `Result`,
`CommentOrigin`, `Lookup`, `Ledger` — live in `internal/review/push.go`, hoisted
there when `adoreview` gained a push half. They name a review and an upstream
tracker and nothing platform-specific, so `ghreview` and `adoreview` both speak
them and `cmd/git-review` drives both through one `reviewPusher` interface
(`runBridgePush`). `ghreview` keeps its old names as type aliases.

`origins` hangs off `gitx` alone and is reached from `cmd`, never from a bridge:
a bridge is handed the lookups it needs as small interfaces (`issue.Mapped`,
`review.Mapped`, `bridge.Claimed`, `bridge.Lookup`, `review.Ledger`), so it
cannot grow a dependency on where the ledger is stored.

The ledger gained two line kinds for reviews, `thread` and `verdict`, and both
exist because a review's parts are *several* upstream objects where an issue's
are one. A thread and its root entry are different objects — the thread is what
gets resolved, the entry is what gets edited — and a verdict is a third, because
the upstream review carrying it is what a dismissal has to name. `verdict`
indexes separately from `comment` rather than into it: an upstream review with a
body is also a comment, and one index would let a verdict's mapping answer
"which comment is this" and post the body twice.

A bridge's namespaced scalars would reach the fold through `Vocabulary.With`,
composed in `cmd/git-issue`'s `open()`. Both `ghissue.Vocabulary` and
`adoissue.Vocabulary` are empty, and the second one is worth reading as evidence:
Azure DevOps' area path looked like the obvious first namespaced scalar and
turned out not to be state at all. An area is a fact about the work item over
there, so it is scope — a query predicate and a default for where a create
files — and it reaches no event, no ledger line and no output. The seam is kept
because it is where a bridge with genuine namespaced state would plug in.

`gitx` therefore reports the repository's hash algorithm as a plain string
rather than an `entity.ObjectFormat`; typing it the other way makes `gitx`
import `entity` and closes a cycle against `entity/store.go`.

Adding a third type is `internal/<type>` + `cmd/git-<type>` + one entry in the
Makefile's `BINS`, and a man page, since `MANPAGES` derives from `BINS`.
`internal/entity` and `internal/render` must never import a type package; that
rule is what keeps that true, and `internal/review` was built against it without
moving anything below `render`.

Two things did move, and both were the seam working rather than giving way:

- **`entity.Fold` grew thread annotations.** A review comment's anchor and its
  thread's resolution are events addressing a comment by id, which is the
  annotation rule a list member already lived under — but the fold only knew
  `comment.edit` and `comment.remove`, so anything else fell through and was
  ignored. Any other `comment.<name>` with a `ref` now lands in
  `State.Annotations`, keyed by entry and then by name, without `entity`
  learning what an anchor is.
- **`render.Row` grew `Notes` / `NoteIcons`.** Exactly the `Type` / `TypeIcon`
  pair, for the same reason: a review's check tally and verdict counts are
  glyphs on a terminal and words down a pipe, and a listing that gets grepped
  must not have ✓ in it. `render` still has no vocabulary of its own — the
  caller writes both forms.

Neither needed a change to `docs/blob-format.md`. The two field kinds a review
looked like it needed — a verdict per author, a check per name — are ordinary
OR-Sets read through a **latest-per-key reader rule**, which is the shape
`docs/issues.md` already prescribes for parent cardinality. A fourth field kind
was drafted and then not needed, and the reader rule is the better semantics
anyway: a reviewer who approves and then reconsiders keeps both events, so the
record that they once approved survives.

```sh
make            # -> bin/git-issue
make test       # unit tests, doc-hash vectors, golden CLI parity
make install    # PREFIX ?= /usr/local

export PATH="$PWD/bin:$PATH"    # makes `git issue` work as a subcommand
git issue add -t "title" -d "body"   # every field is a flag
git issue add                        # no title: an editor opens, like git commit
git issue add -t "title" --edit      # a Markdown editor anyway: title + description
git issue add -F notes.md            # title + description from a file ('-' = stdin)
cat notes.md | git issue add         # a bare pipe works too; `git issue comment` as well
git issue add <id-prefix> -t "title" # filed under that issue
git issue                        # lists; shortcut for `git issue list`
git issue --format medium        # `git log`-shaped: header block, then the title
git issue list                   # nested under each issue's parent on a terminal; flat down a pipe
git issue list --no-tree         # flat anywhere (--tree forces the nesting on)
git issue list <id-prefix>       # the whole subtree filed under that issue
git issue list none              # the issues filed under nothing
git issue <id-prefix>       # shortcut for `git issue show <id-prefix>`
git issue show <id-prefix> <comment-prefix>   # one entry and the replies under it
git issue edit <id-prefix>       # the issue in the editor; one event per change
git issue edit <id-prefix> -t "title" --parent <id>   # a field, no editor
git issue edit <id-prefix> --parent none              # `none` empties a field
git issue edit <id-prefix> --rel blocked-by:<id>      # any other kind of link
git issue edit <id-prefix> --no-rel blocked-by        # every link of one kind
git issue remove <id-prefix>

git issue log                   # the ref's own history, one commit per action
git issue log <id-prefix>       # limited to one issue
git issue log --oneline -p      # every git log flag passes through

git issue -h                     # every command, one line each (git eats `git issue --help`)
git issue list --help            # --oneline / --format, in full
git issue edit --help            # what "one event per field" buys, in full

git issue pull                  # the current branch's own remote; its GitHub
                                 # issues too, if that remote is GitHub
git issue pull origin           # another clone's notes refs
git issue pull github:origin    # GitHub's API; open issues, resuming from last time
git issue pull --all github:origin       # closed ones too, on their own watermark
git issue pull --full github:origin      # ignore the watermark, re-read everything
git issue pull --limit 25 --since 2026-01-01 --all github:owner/name

git issue push                  # the branch's remote: its refs, and its
                                 # GitHub issues too if that remote is GitHub
git issue push origin           # the notes ref and the origin ledger
git issue push github:origin    # write local changes back through the bridge
git issue push github:owner/name        # seed a fork; resumable and idempotent
git issue push --dry-run github:origin  # the plan, and nothing written
git issue push -y github:origin <id>    # one issue, no prompt

git update-ref -d refs/notes/issues/open   # wipe
git update-ref -d refs/git-issue/origins   # forget every upstream link
```

**A bridge push never writes to the notes ref.** Everything it produces goes to
`refs/git-issue/origins`, in one commit per run, so `git issue log` stays a
history of the tracker rather than of its own bookkeeping. Mappings are
journalled to `.git/git-issue/origins-journal/<tracker>` as they are earned and
committed once at the end; a run that dies leaves the journal, and the next one
commits it first. Losing a mapping means a duplicate issue upstream that nothing
undoes, which is why that path exists at all.

`testdata/fixture` holds hand-written note blobs with fixed nonces and
timestamps, one per branch of the fold; `testdata/golden` holds the output the
retired prototype produced for them, under `TZ=Europe/Berlin`. The Go
implementation reproduces all of it byte for byte, which is what let the
prototype be deleted. Regenerate a golden only when a rendering change is
intended, and say so in the commit.

**Known deviations from spec:**

- The GitHub import does not write `react`, `comment.edit`, `comment.remove`,
  `draft` or `pinned`-from-current-state. Reactions and comment edits are
  fetchable and simply not done yet. It does write relations: `parent` and
  `blocked-by` from the timeline, `duplicate-of` from current state, and never
  `related`, which GitHub has no link type for (`docs/bridge-github.md`). The
  ADO import writes the `parent` and `related` kinds; the link families whose
  direction is unconfirmed are left alone (`docs/bridge-ado.md`). The Gitea
  bridge writes only `blocked-by` — the sole cross-issue relationship Gitea has
  an API for — and reports `parent`, `duplicate-of` and `related` as unsent
  (`docs/bridge-gitea.md`). On all three, a link whose target this clone does
  not hold — a cross-repository one above all — is left unwritten rather than
  guessed at.
- Push does not write `type` upstream. GitHub's issue types need a repository
  type id, and the read path already degrades around servers that reject the
  field; a type change is reported as not pushed rather than half-attempted.
  `react`, `locked`, `pinned` and `draft` are unpushed because nothing writes
  them locally either; `status` and `status.reason` now do write locally and
  both bridges push them.
- A push resolves label names, milestone titles and assignee logins against the
  target repository. GitHub and ADO fail the push on an unresolved one; the
  Gitea bridge reports it as a skipped field and pushes the rest, because a
  mirror seeded from another tracker routinely names labels and people the
  target has never heard of (`docs/bridge-gitea.md`).
- No write commands for `locked`, `pinned`, `draft`, or reactions. `add` writes
  `title`, `description`, `status`, `type`, `milestone`, `label.add`,
  `assignee.add` and — from its positional and `--rel` — `rel.add`; `edit`
  writes the same vocabulary again, plus `--status` / `--status-reason`,
  `label.remove`, `assignee.remove` and `rel.remove`, as a diff against folded
  state. `close` / `reopen` are sugar over the one `status` event `edit
  --status` writes, and `close --as duplicate <id>` folds in the `duplicate-of`
  link. Nothing writes `rel.note`: it is folded and rendered, and no command
  produces one. Comment edits and reactions are read and rendered but exercised
  only by hand-written JSON. `add`, `list`, `show`, `edit`, `close`, `reopen`,
  `comment`, `remove`, `log`, `pull` and `push` are the whole CLI.
- **`edit` is the one command that writes to an issue nobody named**, and only
  when retracting a *symmetric* link. `related` may be written by either end, so
  a repository that has seen both — two clones, or an Azure DevOps pull, since
  ADO stores `Related` on both work items — holds two members for one link, and
  retracting the near one alone leaves it standing. `issue.Update` therefore
  returns the far entities it wrote to and the command prints them. Adding a link
  is untouched by this: it is still one event at one end.
- **Relations push to both bridges, and Azure DevOps takes more of them.**
  `issue.Diff` compares them as the OR-Set of `(kind, target)` pairs they are,
  and each pusher resolves a target through `Lookup.Upstream`. Azure DevOps
  writes all four kinds as JSON Patch operations on `/relations`; GitHub writes
  `parent` and `blocked-by` through `addSubIssue` / `addBlockedBy` and cannot
  write `duplicate-of` at all, because it has no `markIssueAsDuplicate`
  mutation, only the unmark — and `related` is not a GitHub link. A patch
  removal addresses a relation by its *index* in the array and by nothing else,
  which is why `adoapi.UpdateWorkItem` takes the whole work item and why its
  removals run descending. Everything declined is reported as a `bridge.Skip`,
  never dropped.
- **`show` folds the whole ref** to resolve a relation's target to a title and
  to derive the inverse ends (`Children:`, `Blocks:`), where it used to read one
  note. That is the first command whose
  cost is the unbuilt index rather than the issue in front of it.
- `Terminal()` is a client-side classification over `issue.Statuses`, not a
  field: `open`/`closed` by definition, plus the Azure DevOps process states
  split on that platform's own state categories, and everything else
  non-terminal. A cross-bridge open/closed *mapping* — where it lives — is still
  deferred (`TODO.md`). `show` hides the `Reason:` line while the status is
  non-terminal, as `docs/issues.md` requires.
- `entity/canonical.go` **rejects non-integer JSON numbers** instead of implementing
  ECMAScript's `Number::toString`. The event schema has only integers, and an
  unverified float formatter would be a silent id fork waiting to happen.
- Note merging is still `git notes merge -s cat_sort_uniq`; the direct set-union
  merge in "Decisions already made" is not written yet.
- No index. A listing reads every note and folds it — see "Measured facts" for
  what that costs. The SQLite index keyed on the notes ref SHA is unbuilt.

## Traps

- **Go's `encoding/json` HTML-escapes `<`, `>`, `&` by default** and marshals
  struct fields in declaration order. Both break id stability. Canonical bytes
  come from `entity/canonical.go` and nowhere else.
- **Go map iteration order is randomized.** Anything reaching output must be
  sorted or carried in an explicit order slice, or the same repo renders
  differently on consecutive runs.
- **`event.Event.Raw` is the preservation invariant.** Events keep the bytes
  they arrived as, and nothing re-serializes a line it did not create. An
  "optimization" that rebuilds a blob from parsed state silently destroys the
  data of every client newer than this one.
- **Old-format notes may be present.** An earlier draft used `v` for the
  *value*; it now means format *version*. The fold skips such events rather than
  crashing, so a repo can silently list far fewer issues than it holds.
- **A `ts`-based test proves nothing if the clocks tie.** Give the "losing"
  event a genuinely lower `c`, or the id tie-break decides and the test is
  vacuous.
- **An update appends, never rewrites.** `entity.Apply` grows the blob by
  concatenating raw event lines onto the bytes already there; nothing
  re-serializes a line it did not create. A writer that rebuilt the blob from
  parsed state would drop every event written by a newer client, because
  `Store.parse` silently skips lines it cannot read. This is also why
  `git notes add`/`append` are gone: their commits carry git's own message and
  the local user as author, which is the record the log exists to keep.
  `git notes remove` is the one still used, and it re-shards the tree.

- **A paged API walk must not trust `page` to be honoured.** Gitea's
  `/issues/{index}/comments` ignores `page` and `limit` and returns the whole
  thread every time, so a walk that stops only on a short page never stops: it
  re-reads and re-appends the same comments until the process is OOM-killed. See
  "Not every endpoint paginates" in `docs/bridge-gitea.md` for the three tests
  that end a walk. Assume the same of any new endpoint until measured.

- **`show` renders in local time.** A golden captured in one zone fails in
  another; the CLI tests pin `TZ` per process, and `time/tzdata` is embedded so
  a static binary does not depend on the host having a zone database.

- **`show` and `list` page to a terminal.** When stdout is a tty the rendering
  goes through `$GIT_PAGER` / `core.pager` / `$PAGER` / `less`
  (`cmd/git-issue/pager.go`), with `LESS=FRX` so short output still prints
  inline. Off a tty — pipes, the golden tests — the pager is skipped and output
  is byte-identical. `show`'s Detail renderer uses no width and colours nothing
  but its ids; a listing colours and measures throughout. Either way `*pager`
  exposes `Terminal() *os.File`, and `render.display` measures colour and width
  against that screen rather than the pager pipe, so paged output keeps its
  colour and column layout. The pty test for `show` compares against the piped
  rendering with the escapes stripped back out.

- **A oneline listing nests on a terminal and prints flat off one.**
  `resolveTree` (`cmd/git-issue/main.go`) reads `gitx.IsTerminal(os.Stdout)`
  when neither `--tree` nor `--no-tree` was given, so a person reads a tree and
  a script parses columns. The positional `<id>` selects the whole subtree
  regardless (`filedUnder`), so piping only reshapes the rows, never changes
  which ones there are; the named issue itself is not in that set, but a tree
  adds it back as the root the branches hang from. `--format medium` never
  nests. Tree behaviour is covered by pty tests in `tree_pty_test.go`;
  `tree_test.go` drives the explicit `--tree` flag off a pipe.

- **An id is painted in two pieces.** `render.Uniquify` measures how many
  leading characters name an id and nothing else; `render.paintID` puts those
  in yellow and greys the rest out, the way jj colours a change id. Grey rather
  than the same yellow dimmed: dim is SGR 2, an attribute rather than a colour,
  and enough terminals drop it that the two halves come out identical. The set an id is measured against is the set it is *resolved*
  against, which is not the set on screen: an entity id competes with every
  issue on the ref however the listing was filtered (`cmd/git-issue`'s
  `uniqueIDs`), and a comment id competes only with its own entity's thread,
  the scope `State.FindComment` uses. A nil `Unique` means "not measured" and
  paints ids whole — which is what every rendering did before this existed, and
  what one falls back to when the ref cannot be read.

- **A listing is newest first.** `render.byCreation` sorts creation date
  descending, id descending on a tie — `list`, `list --format medium` and the
  `pull` change summary all share it, so the summary still lines up with the
  `list` that usually follows it. The two list goldens are in that order.

## Decisions already made

**Language: Go.** Chosen for single static binary distribution and prior art
(git-bug, git-appraise, gh, Gitea are all Go), *not* for speed — the measured
bottleneck is object I/O, not compute. With three specific choices:

1. **Shell out to `git`** via a long-lived `cat-file --batch` co-process rather
   than linking a library. go-git has no notes support ([issue
   #915](https://github.com/go-git/go-git/issues/915), open since 2023);
   gitoxide's landed August 2026 and is very new; libgit2 is mature but cgo
   forfeits the static binary.
2. **Hand-written RFC 8785 serializer with conformance vectors.** Never
   `encoding/json` defaults.
3. **Implement note merging directly** (a set union of lines — the fold is
   order-independent, so no sort or uniq is needed) rather than depending on
   `git notes merge -s cat_sort_uniq`.

The first two are implemented; the third is not yet. Still outstanding: a local
SQLite index keyed on the notes ref SHA, refreshed incrementally.

Keep UIs out of this repo (`docs/ui.md`). Commit to a stable machine-readable
output so any UI in any language can consume it.

## Open questions

- **Entity-level tombstone.** `remove` does not converge; a removal that survives
  concurrent edits needs a tombstone event that `docs/blob-format.md` does not define.
- **Cross-repository references.** An entity id is meaningless in another
  object store, which blocks every relation whose target is elsewhere —
  cross-repo duplicates, blockers, and GitHub sub-issues. One op family means
  one grammar question about `ref`, not one per link field.
- **Label `color`/`description`** — drop them, or add a repo-level label entity?
- **Epic as an entity type or as `type: epic` + a `parent` relation?** The
  second needs no new type, ref namespace, or migration.
- **Commenting on an archived issue** — implicit reopen or in-place edit?
- **A duplicate entity across clones.** A clone that imports from a tracker
  before it ever fetches the notes ref has no ledger entry to honour and mints a
  second entity for an issue that already exists. Detectable afterwards — the
  ledger reports two entities mapped to one upstream object — but not repaired.

## Verifying changes to the docs

Worked examples contain real hashes, and the test suite checks them:

```sh
go test ./internal/entity/ -run 'TestDocumentedIDs|TestDocExamples' -v
```

`TestDocExamplesAreCanonical` walks every fenced event line in `docs/` and
asserts it is already canonical and round-trips through the serializer;
`TestDocumentedIDs` re-derives the ids the documents publish. Editing an
example without updating its hash fails the build. Test merge behaviour with
two real clones and `git notes merge -s cat_sort_uniq`, never by reasoning
about it.

# Storage model

Where tracker state lives in the object store, how it is keyed, and how it
syncs. Applies to every entity type — issues, pull requests, and whatever comes
later.

The contents of an individual entity are specified in
[blob-format.md](blob-format.md); the vocabulary of a specific type in that
type's document, e.g. [issues.md](issues.md).

## Reference

- https://github.com/git-bug/git-bug/blob/trunk/doc/spec/bug.md
- https://github.com/git-bug/git-bug/blob/master/doc/design/data-model.md

## Why git objects instead of a database

- **Offline-first for free.** Every clone is a complete, independently usable
  copy of the tracker. No connectivity required to read, create, or edit.
- **Sync reuses git's existing transport.** Fetch/push already solve distributed
  replication; we don't need to build that.
- **Auditability for free.** Because updates are new git commits rather than
  mutated rows, every change to the tracker has a real author, committer,
  timestamp, and (optionally) a cryptographic signature — without any bespoke
  logging system. (With caveats — see "Provenance" below.)
- **One object store.** Backups, mirrors, and forge migrations are just
  `git clone --mirror`. No separate system to keep in sync with the code.

## What is stored where

Each entity is exactly **one note**: a single blob holding that entity's entire
event set, keyed by the entity id, attached to a notes ref. Every field of that
entity — scalars, lists, and comment threads alike — lives in that one blob
together, not split across multiple notes or a tree of sub-blobs. (A
tree-of-trees split was prototyped and works, but adds a tree level and doesn't
pay for itself at normal entity sizes — see "Rejected alternatives".)

Entities are **keyed** using real `git notes`-style SHA→blob mapping, not a
hand-built tree, so git's automatic fanout/sharding applies without any custom
bucketing code. Confirmed empirically: at 20,000 entities, git had sharded the
notes tree into 250 top-level `00`–`ff` subtrees on its own.

### Entity identity

An entity's id is the **git blob hash of its canonical `create` event line** —
the same content-addressing git-bug uses (`"the hash of the first Operation of
the entity, as serialized on disk"`). Display it truncated; 12 hex characters is
plenty.

This has three properties worth keeping:

- **Self-verifying.** The create event is itself a line inside the note, so any
  reader can re-hash that line and confirm the id it was filed under. Confirmed
  empirically: `git hash-object` of the create line reproduces the entity id
  exactly.
- **Reproducible across bridges — but only if the create event is.** Two
  independent syncs derive the same id exactly when they serialize a
  byte-identical create event. This does **not** happen by default, because `n`
  is random. A bridge must derive `n` instead; see "Bridging an external
  tracker" below. Getting this wrong duplicates every entity on every re-sync.
- **No side object to maintain.** Nothing needs to be written, referenced, or
  garbage-collected to keep an id valid.

Two hard constraints, both confirmed empirically:

- **The id must be full object-name length** (40 hex on sha1 repos, 64 on
  sha256), because `git notes` keys are object names. A shorter id is rejected
  outright: `fatal: failed to resolve 'deadbeefdeadbeef' as a valid ref`. A
  repo's hash algorithm therefore determines its id width, which makes a
  sha1→sha256 migration an id-rewriting migration.
- **The id does not need to name an object that exists.** `git notes add`
  happily annotates a 40-hex name that was never an object at all. Writing the
  create event out as a real blob is a convenience for tooling, never a
  requirement.

That second point matters more than it looks. Any blob written purely to mint an
id is unreachable from every ref, is deleted by `git gc --prune=now`, and is
**absent from a fresh clone** — while `list`, `show`, and `append` all keep
working normally, because git notes treats the key as a tree path string rather
than as a pointer to an object. So:

> Never test an entity id for validity with `git cat-file -e`. It passes in a
> fresh working repo and fails after the first `gc`, for every entity.

This supersedes the earlier design, in which entities were identified by a
*random* synthetic anchor object. That scheme worked but bound identity to
nothing: the id was unverifiable, non-reproducible across bridges, and described
in the docs as an object that — as measured above — does not survive.

### One id space, many types

Ids are content hashes, so they are unique across entity types without any
coordination: an issue id can never collide with a pull request id. Two
consequences:

- A **reference from one entity to another** (a PR closing an issue, a child
  naming its epic) is just an id. It needs no disambiguation against other
  types.
- But **resolving** that id does, because notes refs are partitioned by type
  (below) and a bare id doesn't say which namespace to look in. A reference
  should therefore record the target's type alongside its id, or accept a search
  across namespaces.

A reference that may cross **repository** boundaries needs more than that — an
id has no meaning in another repo's object store. That is unsolved; see
`TODO.md`.

## Refs

Refs are named `refs/notes/<type>/<state>`:

- **`refs/notes/issues/open`**, **`refs/notes/issues/archived`** — see
  [issues.md](issues.md).
- A future pull request type gets `refs/notes/prs/<state>`, with its own states.

Two rules bind every type:

**States must be siblings, never parent/child.** `refs/notes/issues` +
`refs/notes/issues/archived` is invalid — a git ref cannot simultaneously be a
leaf and a path prefix; confirmed empirically, fails with "cannot lock ref...
exists". This is why even a type with a single state should still name that
state explicitly, leaving room to add a second one later without a migration.

**State partitioning is an enumeration optimization, never a source of truth.**
Moving an entity between states is a tree-entry move: point a new entry in the
destination ref's tree at the _same_ content-addressed blob the source ref's
tree referenced, then remove the entry from the source. No content is
duplicated. This is two separate ref updates, not one atomic operation —
correctness does not depend on their ordering completing cleanly, because the
entity's own in-blob state field is authoritative. A transient window where an
entity appears on both refs (or briefly neither) is a harmless discoverability
blip, never a correctness bug.

Practical payoff: day-to-day clients fetch only the active state of each type,
keeping ordinary clone/fetch cost bounded even as historical volume grows
without limit over the tracker's lifetime.

## The origin ledger

An entity that came from — or was pushed to — an external tracker corresponds to
some object over there: a GitHub issue's `node_id`, an ADO work item's number.
That correspondence has to be recorded somewhere, and it is **not a fact about
the entity**.

Apply the namespacing test from [blob-format.md](blob-format.md) — "if the
tracker were migrated off that platform entirely, would the field still mean
anything?" — in its harsher form. Stop caring about a fork, and its mapping is
worthless. You should be able to drop it without touching a single entity.

So it lives in its own ref:

```
refs/git-issue/origins  →  tree
  github.com/
    hdweiss/git-issue        (blob)
    acme/git-issue           (blob)
  dev.azure.com/
    org/proj                 (blob)
```

One blob per tracker, keyed by the tracker's own name. Tracker names already
contain slashes, so they become subdirectories for free: dropping a fork is
deleting one path, and dropping every GitHub link is deleting `github.com/`.

**This is a third category of state, and it is worth naming as one.** Entity
state lives in blobs and is authoritative; an index is local, derived and
disposable. The ledger is neither. It is *relationship* state — true of this
repository's dealings with one tracker, not of any entity and not of any other
clone's reading position — and it is **synced**, because a clone that lacks it
will re-create every entity upstream on its first push, which nothing can undo.

### Why not a notes ref

Notes are keyed by object name, which would key the mapping by entity and put us
straight back to rewriting every entity to drop one tracker.

The namespace boundary is also enforced by git. `git notes --ref` "specifies the
full refname when it begins with `refs/notes/`; when it begins with `notes/`,
`refs/` and otherwise `refs/notes/` is prefixed" — so a notes ref must live under
`refs/notes/`, and anything else placed there is at the mercy of tooling that
assumes object-name keys. Any `git notes` command re-shards the tree it touches
into hex fanout, and `notes.displayRef` accepts globs, so a `refs/notes/*`
setting would have `git log` try to attach the ledger to commits.

Two git mechanisms, two namespaces. Neither has a default refspec, so both are
named explicitly when syncing regardless; sharing a prefix would save nothing.

### Blob format

Line-oriented, because the merge is a union of lines — the same constraint the
entity blobs live under. Order carries no meaning and every line stands alone. A
leading kind keyword keeps it additive:

```
issue    4f2a1c9…  I_kwDOAbCdEf
url      4f2a1c9…  https://github.com/acme/git-issue/issues/42
comment  4f2a1c9…  8b31e07…  IC_kwDOxYzAbC
```

Fields are whitespace-separated and opaque. `comment` carries three ids: the
entity, the *local* thread-entry event, and the upstream comment.

**Unrecognised kinds are preserved byte for byte**, exactly as unknown events
are, which is what lets a new kind be added without a format change — an
identity map (`user <local-email> <upstream-login>`) being the obvious next one.

### Merge

A union of lines per path, deduped. Lines are facts, so there is no value-level
conflict to resolve, and the union is idempotent for the same reason the entity
blobs' is.

Two situations are worth reporting as warnings rather than treating as merge
failures, because both mean a duplicate was created somewhere and both need a
human: two entities mapped to one upstream object, and one entity mapped to two
upstream objects in a single tracker.

**Deletion does not converge.** Dropping a tracker locally is undone by a merge
with a clone that still has it — the same limitation entity removal has, for the
same reason. Here it is arguably the right outcome, since that clone does still
have the relationship, and unlike entity removal it is cheap to repeat.

### Cost

At roughly 80 bytes per entity line and 120 per comment line, 2,000 entities
averaging ten comments is about 2.5 MB for one tracker, rewritten whole on each
write. That is fine as a single delta-compressed object, and it is strictly less
churn than the alternative: putting the mapping in the entity blobs costs a
comparable number of bytes *per entity*, cannot be reclaimed because those blobs
are grow-only, and turns "is this upstream object already ours?" into a fold of
every entity in the repository. If one tracker's blob does outgrow this, shard it
by entity-id prefix inside the tracker's directory — but measure first.

## Sync and merge

Sync is an ordinary fetch/push against the notes refspec. Concurrent edits are
reconciled with git's built-in `cat_sort_uniq` notes merge strategy, so there is
no custom merge driver to write or distribute.

Confirmed empirically: two clones editing **different fields of the same
entity** merge with no conflict and fold to the correct combined state.

The strategy's mechanics dictate the blob format, so they belong here:

- The merge is a **set union of lines**, followed by a sort and a dedup.
- It therefore **destroys physical line order permanently**, replacing it with
  alphabetical order. Order is not merely unreliable after a merge — it is
  actively rewritten.
- It operates strictly line-by-line, so any value spanning multiple physical
  lines will interleave with another writer's lines and silently corrupt.

Everything that follows from this — why the blob is a grow-only set rather than
a log, why every event needs a nonce so `uniq` can't eat a duplicate comment —
is specified in [blob-format.md](blob-format.md).

**Ref-update contention.** Concurrent writers on the same ref race via
compare-and-swap; high write volume needs a retry/merge loop, the same as any
distributed git push race. Note that per-type refs already partition this
contention: issue traffic and PR traffic don't race against each other.

## Provenance

`git blame` and `git log -p`, scoped to an entity's blob path, give an audit
trail at line/event granularity with no extra indexing.

**This audit trail degrades on merge, and cannot be relied on alone.** Confirmed
empirically: after a `cat_sort_uniq` merge of two clones' edits, each side's
*new* lines were still attributed correctly, but **two pre-existing lines were
re-attributed to the merge commit** — because the strategy reorders the file and
blame follows line positions. Attribution is a property of how the bytes
happened to be written, not of the data.

For that reason the authoritative author of an event is the `a` field **inside
the event**, which survives any reordering, repacking, or rewrite. Commit
metadata is a corroborating second record, not the primary one.

For bridged data (e.g. synced from GitHub), git's **author vs. committer** split
is still the mechanism for commit-level identity: `author` = the original
external identity + original timestamp, `committer` = the bridge bot + sync
time. Caveats:

- A bridge-asserted author is an unverified label, not cryptographic proof, at
  both the commit level and the event level. Real accountability needs signing.
- The event-level record is the one that must be right. `a` inside the event
  survives any reordering or rewrite, so a writer that batches its commits loses
  the corroborating git-level record and nothing else.
- Writers must replay **one commit per action**, in order — not batch
  multiple people's changes into a single sync commit — or blame collapses
  everyone's contribution onto the bot's identity.
- Fidelity is capped by what the source platform actually retains, and that
  varies per field rather than per platform. On GitHub, **titles have complete
  edit history** — `RenamedTitleEvent` carries the previous title, the new
  title, an actor and a timestamp (verified against the live API) — while
  **bodies have none**, only current content plus `updated_at`. Expect to
  reconstruct some fields faithfully and to collapse others to a single
  import-time event.

### What a commit says

An action is the unit of a commit: one upstream timeline entry, one comment,
one issue being filed, one local `add` or `edit`. Not one event — an issue
filed with a title, a type and two labels is four events and one action — and
not one sync run, which is what collapses everyone's changes onto whoever ran
it.

Given that, the commit metadata is worth filling in properly, because it turns
the ref's own history into a second reading of the tracker:

- **`author`** is who did the thing, at the moment they did it. **`committer`**
  is whoever wrote it locally, now. `git shortlog`, `git log --author` and
  `.mailmap` then work on the tracker with no second mechanism.
- The **subject** says what happened in the vocabulary the entity type already
  uses — `Create issue "…"`, `Comment on "…"`, `Close "…" as completed`. An
  action with more clauses than a subject can carry collapses to `Update "…"`
  and lists them in the body.
- The **body** carries the prose the action introduced: the description an
  issue was filed with, the text of a comment, the title a rename replaced.
  This duplicates bytes that are already in the blob, deliberately — the blob's
  copy is inside a JSON line that only `git log -p` renders.
- An **`Issue:` trailer** names the entity in full. That is the only join from
  a commit back to what it wrote.

Two limits belong with this, because reading a log without them misleads:

- **The log does not converge; the blob does.** Confirmed empirically: two
  clones that import the same issue independently produce byte-identical blobs
  and two disjoint sets of commits, so after a merge `git log` lists every
  action twice. Commit messages are prose about events whose own bytes are the
  truth, and nothing may parse them back into state.
- **Commit order is write order, not history.** A resumed import writes events
  older than ones already on the ref. Read such a log with
  `--author-date-order`.

The cost is real and is paid on every clone. Measured at 5,000 entities with
ten actions each:

| | commits | import | packed |
| --- | --- | --- | --- |
| One commit per run | 1 | 0.41s | 2.4 MB |
| One commit per entity | 5,000 | 1.21s | 4.7 MB |
| One commit per action | 50,000 | 5.30s | 27.5 MB |

Two findings decide the shape of a writer:

- **A large notes tree must not be written flat.** The same 50,000-commit
  import against a flat tree takes **47.5s** rather than 5.3s, because every
  commit rewrites a root tree holding all 5,000 entries. git shards a notes
  tree on its own heuristic — measured: somewhere between 64 and 96 notes — and
  re-shards the whole tree whenever one of its own commands touches the ref. A
  writer that bypasses `git notes` has to shard large trees itself.
- **Chronological order across entities is free.** Emitting every entity's
  actions interleaved by timestamp rather than entity by entity costs 5.8s
  against 5.2s, with identical packed size, and is the difference between a log
  that reads as a history and one that reads as a list of issues each replayed
  from birth.

And one that bites afterwards rather than during the write:

- **A bulk import leaves the ref slow to read until it is repacked.** Writing
  many versions of a blob is what makes this granularity expensive at all, and
  `git fast-import` chains those versions in write order — the oldest as the
  delta base, the newest at the deep end. Every listing reads exactly the
  newest version of every entity, so it replays each one's whole history.
  Measured on a 2,000-issue import that wrote 17,773 commits: tip blobs at
  delta depth 23 mean and 50 max, and a listing at 1.74s against 0.24s for the
  same issues written in a single commit.

  `git repack -adf` re-picks the bases the other way round, putting the current
  versions at depth 0–1: the listing drops to 0.21s — faster than the
  single-commit repository — and the notes history from 149 MiB to 19 MiB.
  **`git gc` does not fix it** (1.74s → 1.58s): repack without `-f` reuses the
  deltas it already has, so the chains survive every gc indefinitely.
  `--aggressive` is not the answer either; it widens the delta window at
  considerable cost, and the problem is which end of the chain the current
  version sits at rather than how well it compresses.

  A writer that produces this state should repair it rather than report it. The
  damage is invisible in the output and the obvious remedy does not work, so
  leaving it to the reader is leaving a repository that is quietly several times
  slower for good. The repair is bounded and one-off — measured at 11.7s on a
  240,000-object repository with code history — and belongs at the end of the
  import that caused it, announced, and skipped where the user has told git not
  to maintain the repository on its own (`gc.auto = 0`).

### Bridging an external tracker

A bridge imports entities it did not create. Two rules make that idempotent.
Platform-specific detail lives in that platform's own document, e.g.
[bridge-github.md](bridge-github.md); what follows applies to any of them.

**Derive every nonce; never generate one.** `n` exists to keep two distinct
events from colliding into one set member, and a random value serves that fine
for locally authored events. For imported events it is actively wrong: a random
`n` makes the event's bytes — and therefore its id, and therefore the whole
entity's id — different on every sync run, so a re-import duplicates everything
instead of converging.

Derive it from the upstream event's own stable identity instead, e.g. the first
16 hex characters of `SHA-256("<upstream node_id or event id>")`. `v` is fixed,
`ts` is the upstream timestamp and `a` is the mapped author, so with `c` derived
as below, a derived nonce is the last piece needed to make re-import a no-op.

**Derive `c` from the upstream timestamp, never from a position in the feed.**
This one is easy to get backwards, because a counter over the upstream events
looks obviously right and is deterministic within a single run.

It is not deterministic *across* runs. Every field of an event is hashed into
its id, `c` included, so `c` must be a pure function of one upstream event and
must never depend on its neighbours. A position does depend on them: delete one
comment upstream, and every later event's position shifts by one, changing every
later event's id. The blob is a grow-only set, so the old lines do not go away —
the next import simply adds a second copy of every event after the deleted one.

The upstream timestamp is the only value available that is both pure and
monotone with upstream order, so it is what `c` gets. This does not weaken
"order by `(c, id)`, never by `ts`": the ordering rule exists because many
unsynchronised clocks write to one tracker, and an imported entity's `c` values
all come from one platform's own server. Events sharing a second tie, and break
on id like any other tie.

One consequence to expect: a bridged entity's clock space is in the region of
1.7 × 10⁹ while a locally created one starts at 1. That is harmless, because `c`
is per entity — but a writer appending to a bridged entity must take
`max(c) + 1` from the folded state rather than assume a small number.

**Remember how far you read, outside the object store.** A bridge that starts
from scratch every run re-reads the whole upstream tracker to discover that
almost nothing changed. What it needs is a watermark per source, and that
watermark is an index in the sense above — local, derived, disposable, never
synced. It belongs in the git directory, not in a ref: it describes one clone's
reading position, which is false in every other clone, and losing it costs a
full re-import and nothing else. See [bridge-github.md](bridge-github.md) for
the properties a safe watermark needs.

Do not derive it from the notes ref's own last commit. That moves whenever
anyone writes locally, which says nothing about how far upstream was read, and
it carries the local clock rather than the source's.

**Replay one commit per upstream action, in order.** Not one commit per issue,
and not one per sync run. This is what preserves per-action blame attribution,
and it is also what lets Lamport clocks come out in an order that matches the
upstream history. See "What a commit says" above for what such a commit carries
and what it costs.

A bridge also maps upstream identity onto a **commit author**, which is a
different mapping from the one that produces `a`. `a` is hashed into every
event's id, so its spelling is part of the specification and two bridges that
disagree stop converging; a commit byline is hashed into nothing, and two
bridges that disagree about it disagree about a byline. Give it an address that
git's own tooling can work with.

What an upstream platform exposes decides how much can be reconstructed rather
than flattened. Where it offers a per-event feed with actors and timestamps —
GitHub's timeline API gives `labeled`/`unlabeled`/`assigned`/`unassigned` events
this way — list fields can be rebuilt as genuine add/remove pairs, with each
remove referencing the specific add it retracts. Where it offers only current
state, the import collapses to one event at import time, attributed to the
author the platform reports.

Not everything upstream should be imported. Personal, non-collaborative state —
watch/subscribe status above all — belongs client-side, and importing it turns
one person's notification preferences into tracker-wide traffic for everyone.
It is also high-volume: subscription events were the single most common event
type in a sample of GitHub timeline data.

### Pushing to an external tracker

A push has to know which local changes the tracker has not seen. The obvious
mechanism — a "last pushed" watermark per entity per tracker — is the read
watermark's mirror image and is wrong for a stronger version of the same reason:
it is a local record of a fact that has to hold in every clone.

**Reconstruct it instead.** A bridge's import is a pure function of what the
platform returned, so the events an import *would* produce from the tracker's
current state are exactly the events the tracker already knows about. That gives
a three-way comparison out of parts that already exist, with nothing new
persisted:

```
upstream = Fold(Import(fetch(mapping)))   what the tracker says now
local    = Fold(blob)                     what this repository says
base     = Fold(blob ∩ Import(...))       what both last agreed came from the tracker
```

`base` is the intersection of raw event lines: an event held by both the blob and
a fresh import is, by construction, one the tracker authored and this clone
received. Only-local-changed gives `base == upstream != local` → push.
Only-upstream gives `base == local != upstream` → nothing to push. Both changed
gives three different values → a conflict, which is the caller's to resolve.
A field pushed on an earlier run resolves `base == local == upstream`, which is
what stops a repeat push from re-pushing forever.

The three field kinds take three rules:

- **Scalar** — three-way as above. The only kind that can conflict.
- **List** — an OR-Set difference; no base needed. Concurrent add and remove is
  add-wins by definition, so **a list field cannot conflict.**
- **Thread** — by the ledger. An entry is upstream iff the ledger records which
  upstream object it is.

**Comparing values rather than event identity is what makes a mirror
terminate.** An entity linked to two trackers accumulates both trackers' events,
so a pushed change comes back as a second event carrying the same value. Compare
folded state and that is a no-op; compare event sets and every sync round trip
finds work to do, forever.

Two consequences to expect rather than discover:

- **Linking one entity to two trackers merges their states.** Every event lands
  in one blob and folds by `(c, id)` regardless of which tracker wrote it, so a
  rename in a fork can win over an older rename upstream. That is right for a
  mirror and wrong for a fork that has genuinely diverged; unlinking is the
  remedy.
- **Attribution does not survive a push.** The receiving platform attributes the
  write to whoever's credential made it, because no API lets one post as someone
  else. The event's own `a` stays correct locally, and is the record that
  matters.

## Querying and indexing

There is **no native query support**. Any listing or filter means reading every
note of the relevant type and folding it. Same conclusion Gerrit reached, hence
NoteDb plus a Lucene index rather than NoteDb alone.

Measured at 20,000 entities:

| Approach | Time |
| --- | --- |
| One `git notes show` + one `jq` per entity | 58.6s |
| One `git cat-file --batch` + one `jq` for the whole ref | 1.0s |
| Enumerating the notes tree alone (floor) | 0.15s |
| Reading every note blob, no parsing (floor) | 0.54s |

So a full scan bottoms out around 0.7s at that size: batching object reads is
worth ~60x, and anything beyond it must come from **not reading every note**.

Two properties make an incremental index cheap:

- The **notes ref SHA is a perfect cache key**. It changes if and only if some
  entity changed, so a cached listing keyed on it is correct by construction.
- `git diff --name-only <old-ref> <new-ref>` returns exactly the entities that
  changed, in ~3ms at 20k. An index can be refreshed proportionally to what
  changed rather than to total size.

**Any index must be local, derived, and rebuildable — never synced.** A shared
index object would be rewritten by every single write, which maximizes exactly
the ref contention described above. This is the same principle as ref placement:
derived state is an optimization, never a source of truth.

## Rejected / superseded alternatives

- **One git ref per entity.** Rejected: at scale this reproduces a known
  production failure mode (GitLab/Gitaly hit ~115,000 internal refs slowing
  packfile negotiation on every fetch). Refs are advertised on every
  negotiation; trees are not. Note this is a budget shared by all types — it is
  the reason refs partition by *type and state*, not by entity.
- **Flat, unsharded tree of all entities.** Rejected: confirmed empirically that
  a single change to one of 20,000 entities rewrites a ~780KB tree object in
  full, every time, because git objects are immutable.
- **Comments as separate notes, keyed independently, with a back-pointer to the
  parent.** Rejected as the default: gives per-comment isolation, but "list all
  comments for X" then requires either a full scan or a secondary index — the
  index ends up being a reimplementation of the combined-blob approach anyway.
- **Manually bucketed tree-of-trees (`id % 256`).** Superseded by using real
  SHA-keyed `git notes` semantics, which gets equivalent fanout sharding
  automatically, without hand-rolled bucketing logic.
- **Random synthetic anchor object as the entity id.** Superseded by
  content-addressing the create event: same key space, but verifiable,
  reproducible across bridges, and honest about the fact that the object doesn't
  persist.
- **A shared, synced index blob.** Rejected: fastest possible reads (a single
  object), but it turns every write into a rewrite of one hot object shared by
  all writers.

## Known, accepted limitations

- **Write amplification on long threads.** Every append rewrites the whole blob,
  because git objects are immutable. Measured on a 200-comment thread: **5.1 MB
  of blob bytes written for a 50 KB final blob** (~100x), 2.4 MB of loose
  objects on disk — which `git gc` then packs down to **112 KB**. At rest the
  combined blob is fine; delta compression absorbs nearly all of it. The costs
  that remain are loose-object churn between GCs, and **sync**: each new comment
  transfers the entire blob again, not just the new line. This will bite pull
  requests harder than issues, since review traffic per entity is higher.
- **Unbounded history growth.** State partitioning bounds day-to-day fetch cost,
  not total repo size. Eventual history compaction/rewrite is a foreseeable
  future need, intentionally deferred rather than solved here. Note that
  compaction conflicts directly with the preserve-unknown-events rule in
  [blob-format.md](blob-format.md) — any compactor must be able to parse
  everything it rewrites.
- **Cross-repo references** have no defined representation; an id is meaningless
  in another repo's object store. See `TODO.md`.

## Quick reference for implementers (human or agent)

1. Never nest a ref under another ref's exact path — check for parent/child
   collisions before naming new refs, and always name the state component.
2. Never test an entity id with `git cat-file -e`; ids name objects that
   generally do not exist.
3. Treat in-blob fields as authoritative; treat ref placement — both type and
   state — as an index, never a source of truth.
4. Treat any query index the same way: local, derived, disposable. The origin
   ledger is the one thing that is neither entity state nor an index — it is
   synced, and a clone without it re-creates every entity upstream on its first
   push.
5. Never batch multiple external authors' events into a single sync commit if
   you want per-action blame attribution to survive — and don't rely on blame
   alone regardless, since merges reattribute lines. Never read state back out
   of a commit message: the blob converges, the log does not.
6. Read object data in batch (`git cat-file --batch`), not per entity. It is a
   ~60x difference at 20k.

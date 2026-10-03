# Bridge: GitHub

Mapping between GitHub issues and the [issue entity](issues.md). Generic bridge
mechanics — deriving nonces so re-import converges, one commit per upstream
event — are in [storage-model.md](storage-model.md).

This document exists because the tracker's purpose is portability: an issue must
survive the platform it came from. Nothing GitHub-specific belongs in
[issues.md](issues.md), and nothing here may be required to read an issue. A
client that has never heard of GitHub folds a GitHub-bridged issue into complete
state, and simply carries the events below through merges untouched.

## Identity, and where it is recorded

This bridge writes **no namespaced operations at all**. An issue's correspondence
with its GitHub counterpart is not a fact about the issue — see "The origin
ledger" in [storage-model.md](storage-model.md) — so it is recorded there, on
`refs/git-issue/origins`, under the tracker's own name:

```
issue    4f2a1c9…  I_kwDOAbCdEf
url      4f2a1c9…  https://github.com/acme/git-issue/issues/42
comment  4f2a1c9…  8b31e07…  IC_kwDOxYzAbC
```

**The `node_id` is the identity; the URL is only a locator.** They are separate
lines because they have different lifetimes: `node_id` is opaque and stable,
while an issue transferred between repositories keeps its `node_id` and gets a
new URL and a new number. Treating the URL as identity would silently fork an
entity into two on the first transfer. The issue number, when needed for display,
is parsed from the URL rather than stored — it is no more stable than the URL it
comes from.

Keeping this out of the blob is what lets one issue be linked to several GitHub
repositories at once — an upstream and a fork — since the ledger has one file per
tracker and the blob would have had one last-write-wins scalar.

The `node_id` is also what the **nonce derivation** keys on. Per
[storage-model.md](storage-model.md), the create event's `n` is derived from the
upstream identity — here, the first 16 hex characters of
`SHA-256(<node_id>)` — so that a re-import produces byte-identical events and
converges instead of duplicating. Other imported events derive `n` from their
own upstream id: the comment id, or the timeline event id.

Three rules cover every event a bridge can emit, and the exact input string is
part of the specification rather than an implementation detail — two bridges
that hash different strings fork the identity of every issue they both import.

| Event stands for | `n` is the first 16 hex of |
| --- | --- |
| The issue itself (`create`) | `SHA-256(<node_id>)` |
| An upstream object of its own (comment, timeline entry) | `SHA-256(<that object's node id>)` |
| Current state with no upstream event behind it | `SHA-256("<node_id>:<op>")`, or `SHA-256("<node_id>:<op>:<value>")` for a list member |

Node ids are used throughout rather than the numeric REST ids, and that choice
is load-bearing: every GraphQL object carries a globally unique `id`, while a
comment id and a timeline event id are integers drawn from different sequences
and would collide as nonce inputs.

The third rule exists because a bridge cannot always find an upstream event for
something GitHub reports as currently true — see "Reconciling what the timeline
does not cover".

### Clocks

`c` is the upstream event's Unix timestamp, per the rule in
[storage-model.md](storage-model.md). Not its position in the timeline: a
deleted comment shifts every later position and would silently duplicate every
event after it on the next import.

Events synthesised from current state get the issue's creation time, which is
the earliest moment the fact can honestly be attributed to.

## Author identity

GitHub identifies people by `login` plus a numeric id, frequently exposes no
email, and some authors are not people at all — the sampled issue was opened and
closed by `dependabot[bot]`.

Bridged events therefore set `a` to **`github:<login>`**. The scheme prefix
matters for the same reason the nonce rule does: `a` is part of the event's
bytes, so two bridge implementations that spell the same author differently
produce different ids for the same event, and re-import stops converging.

`author_association` (`OWNER`, `MEMBER`, `CONTRIBUTOR`, …) and
`performed_via_github_app` describe the author's *relationship* to the
repository, not their identity. Neither is represented, and neither should be
folded into `a`.

### Commit byline

A commit's author is a second, separate mapping: **`<login> <login@users.noreply.github.com>`**,
dated with the upstream event's own timestamp. The committer is whoever ran the
import, dated now.

The noreply form is GitHub's own, which is what makes `git shortlog`,
`git log --author` and a repository's `.mailmap` work on the tracker's history
with no extra mechanism. It is a bridge-asserted label and proves nothing; the
authoritative record of who wrote an event is `a`, inside the event.

Unlike `a`, nothing hashes this. Two bridges spelling it differently disagree
about a byline and about nothing else — which is why it can be chosen for
readability where `a` cannot.

`ghost` covers a deleted account here too, so a commit from one is authored by
`ghost <ghost@users.noreply.github.com>` rather than by nobody.

## Field mapping

| GitHub | Becomes |
| --- | --- |
| `title` | `title`, plus one per `RenamedTitleEvent` |
| `body` | `description` |
| `state` | `status` — `closed` only; see below |
| `state_reason` | `status.reason` |
| `labels[].name` | `label.add` / `label.remove` |
| `assignees[]` | `assignee.add` / `assignee.remove` |
| `milestone.title` | `milestone` |
| `type.name` | `type` |
| `locked`, `active_lock_reason` | `locked`, `lock.reason` |
| `draft`, `pinned` | `draft`, `pinned` |
| `parent`, `ParentIssueAdded/RemovedEvent` | `rel.add` / `rel.remove`, kind `parent` |
| `BlockedByAdded/RemovedEvent` | `rel.add` / `rel.remove`, kind `blocked-by` |
| `duplicateOf` | `rel.add`, kind `duplicate-of`; see "Relations" |
| comments | `comment`, `comment.edit`, `comment.remove` |
| `in_reply_to_id` | `ref` on the reply's `comment` event |
| reactions | `react` — on the `create` event for issue-body reactions |
| `node_id`, `html_url` | `issue` and `url` lines in the origin ledger |

### Status is never written as `open`

An issue with no `status` event is open ([issues.md](issues.md)), so an import
writes `status` only when the issue reaches a terminal state.

This is not tidiness. A close reconciled from current state carries the issue's
creation time, and so would an explicit `status: open` — equal `c`, leaving the
tie-break on event id to decide whether a closed issue imports as closed. Not
writing the redundant event removes the tie entirely.

### Resuming

`filterBy: {since:}` compares against an issue's `updatedAt`, so a bridge that
records the newest `updatedAt` it imported can hand that back on the next run
and read only what changed.

Three properties make that watermark safe, and all three are worth stating
because each has a plausible-looking alternative that is not safe:

- **Take it from `updatedAt`, not from a local clock.** The watermark is
  compared against GitHub's timestamps, so it has to be one, or the two ends of
  the comparison come from different clocks.
- **Advance it only after the events are on the ref.** Otherwise a failed write
  loses exactly the issues the next run has now been told to skip.
- **Order by `updatedAt` ascending, and a truncated run is still safe.** A run
  cut short by a limit has read a *prefix*: every issue it did not reach has an
  `updatedAt` at or after the watermark, so the next run asks for precisely
  those. `since` is inclusive, so the boundary issue is read twice rather than
  lost, and re-reading an entity adds nothing.

Keep one watermark **per scope**. An open-only run has read every open issue up
to its watermark and knows nothing about closed ones; starting a later
all-states run from that point would silently skip every issue closed before it.
The converse does hold — an all-states run has seen every open issue too, and
may advance both.

The watermark is local, derived and disposable, exactly like any other index
(see [storage-model.md](storage-model.md)). It is not tracker state and must
never be written to a ref: it describes one clone's reading position, which is
false everywhere else. Losing it costs one full re-import and nothing more,
because re-import converges.

### Import scope

A bridge should read **open issues by default** and take an explicit flag to
include terminal ones. The ratio in a long-lived repository is lopsided — most
issues are closed and stay closed — so importing everything by default spends
the whole rate-limit budget on history nobody asked for.

The cost of that default is a real staleness window, and it is worth stating
rather than discovering: an issue that closed upstream since the last import is
not in an open-only result set, so nothing in the response says it changed, and
the local copy keeps resolving to `open`.

The filter cannot fix this on its own. Two things do: a full import, or asking
GitHub for the entities the tracker already holds — the origin ledger records
every one of their node ids precisely so they can be named again in a
`nodes(ids:)` query without a search. Neither is free, so which one runs is the
caller's decision, not the bridge's.

### Identity first, detail afterwards

A pull request import runs in two passes, and the split is what makes it finish
in a reasonable time on a large repository.

A cursor walk is **strictly serial** — the next request's cursor is inside the
previous request's answer — so paging the detail of two hundred pull requests
means two hundred pull requests' worth of comments, reviews and review threads
through one connection, one page at a time. So the serial walk asks for identity
alone: `nodes { id updatedAt }`, a hundred at a time, in a query too light to
time out. Detail is then fetched **by id**, in small batches, several at once —
and batches addressed by id have no cursors between them, which is exactly what
makes them parallel.

Two consequences worth having beyond the speed. A `since` walk now skips
unchanged pull requests before they cost a detail request at all, rather than
after. And a failure is confined to one batch instead of poisoning a cursor
chain.

### A batch GitHub will not finish

GraphQL runs a query inside a server-side time budget, and answers one that
exceeds it with a **502** — sometimes carrying "Something went wrong while
executing your query", sometimes as a bare status. It is not a statement about
the request: the same query asked for again often comes back, so every 5xx is
retried a small number of times on a short backoff.

When it is not weather, it is size. A batch of pull requests from a large, busy
repository can be more than GitHub will execute however many times it is asked
for, because a pull request carries connections nested two deep — review
threads, each with comments — and a batch multiplies them. The remedy is
arithmetic rather than patience: **halve the batch and ask for each half**, down
to one pull request. The same pull requests arrive, in more requests.

This is separate from the node limit, which is a static property of the query
text and is measured by a test. A smaller batch can only declare fewer nodes, so
splitting is always safe; what it buys is execution time, not budget.

### Reconciling what the timeline does not cover

The timeline is authoritative wherever it speaks, and it does not always speak:
an issue transferred between repositories, or one predating a timeline entry
type, can be labelled without ever having been `labeled`.

So after replaying the timeline, a bridge adds an event for anything GitHub
reports as currently true that the replay did not produce — a label with no
surviving `label.add`, an assignee, a milestone, a lock, a closed state, a
parent. These are the events that take their nonce from the third rule above.

Only single-valued current state is worth reconciling this way. `blockedBy` is a
connection, and a connection cannot be reconciled without paging it; reading its
first page alone would drop links silently. Blocking pairs have complete
timeline history, so nothing is lost by reading them from the timeline alone.

Both halves are needed. The timeline alone loses state whose history is gone;
current state alone throws away every attribution and every removal, which is
exactly the fidelity the timeline exists to provide.

### What reconstructs faithfully, and what collapses

The timeline API returns `labeled`, `unlabeled`, `assigned`, `unassigned`,
`milestoned`, `closed` and `renamed` events, each with an actor, a timestamp and
a stable event id. Every one of those becomes a real event with real
attribution, and a `label.remove` can reference the specific `label.add` it
retracts — the OR-Set is populated properly rather than approximated.

**Titles have complete edit history**; `RenamedTitleEvent` carries the previous
title, the new title, an actor and a timestamp. The title written at creation
time is therefore the *earliest rename's* `previousTitle`, not the current
title, with each rename following as its own event.

**Bodies have none** — only current content plus `updated_at` — so a description
imports as a single event at the issue's creation time, and every subsequent
edit upstream is invisible. This asymmetry is a property of GitHub, not of the
format.

`type` (GitHub's issue types) is in the same position as the body: no timeline
history that a bridge can rely on across server versions, so it imports once at
creation time and later changes upstream are invisible. Resist the temptation to
fix that by keying the event on `updated_at` instead — the field would then
produce a fresh event on every import that anything else touched, and the blob
would grow by a line per sync forever.

`lock.reason` passes the upstream string through, lowercased. GraphQL spells the
same values in enum case (`RESOLVED`) that REST spells in lower (`resolved`),
and an issue must not import differently depending on which door the bridge came
in by.

### Threaded comments

Issue comments are flat; only **PR review comments** thread, via
`in_reply_to_id`. That maps straight onto the reply `ref` described in
[blob-format.md](blob-format.md) — same idea, same direction of pointer.

GitHub normalizes depth to one level: in a sample of 100 review comments, 48
were replies and **none pointed at another reply**, because a reply to a reply
is re-parented to the thread root. A bridge should preserve whatever GitHub
reports rather than trying to reconstruct deeper intent, and must not assume the
inverse — locally authored replies can nest arbitrarily, so write-back would
need to flatten.

GitLab models the same feature differently, grouping notes under a discussion id
with an `individual_note` flag rather than pointing each note at a parent. The
reply pointer covers that too: a discussion is the subtree rooted at its first
note, with every other note referencing that note. The pointer form was chosen
partly because it subsumes the container form — the reverse would require
inventing synthetic discussion ids for platforms that have none.

### Fields that need no operation

- **`closed_by`** is the actor of the upstream `closed` event, so it arrives as
  the `a` of the `status` event that closes the issue. A separate op would
  duplicate state the event log already carries, and the two could then
  disagree.
- **`comments` (a count)** and **`sub_issues_summary`** are derived from data
  the blob already holds. Never store a count that can be recomputed: it is one
  more thing to keep consistent under merge, and it cannot be.

### Relations

GitHub's relationships and this vocabulary's are the same shape, so they map
kind for kind ([issues.md](issues.md)):

| GitHub | Kind | Read from |
| --- | --- | --- |
| sub-issues | `parent` | `ParentIssueAdded/RemovedEvent`, and `parent` |
| dependencies | `blocked-by` | `BlockedByAdded/RemovedEvent` |
| duplicates | `duplicate-of` | `duplicateOf` only |
| — | `related` | GitHub has no such link |

Four things decide what is read, and each is a fact about GitHub rather than a
preference:

**Only the dependent end is requested.** A relation is stored on the end whose
state the link constrains, and the inverse is derived by a reader. GitHub raises
an event on both ends — `SubIssueAddedEvent` alongside `ParentIssueAddedEvent`,
`BlockingAddedEvent` alongside `BlockedByAddedEvent` — and the extra one names a
link that would have to be written on the *other* issue's blob, which an import
of this issue cannot do. So it is not requested rather than requested and
dropped.

**A duplicate has no event on the end that stores it.** `MarkedAsDuplicateEvent`
is raised on the **canonical** issue and names the duplicate; the duplicate's own
timeline says nothing at all. Verified against `nodejs/node#56645` (the
canonical, which carries the event and has no `duplicateOf`) and `#58091` (the
duplicate, which has `duplicateOf` and an empty timeline). So `duplicate-of` is
reconciled from current state, with the attribution and removal history that
implies: none. It is in the same position as the body, for the same reason.

**A close reason is not a link.** `stateReason: DUPLICATE` says an issue was
closed as a duplicate, not what it duplicates — `microsoft/vscode#333332` is
closed `DUPLICATE` with `duplicateOf: null`. The two import separately and
neither implies the other.

**`related` has no counterpart.** GitHub's nearest neighbours are
`CrossReferencedEvent`, `ReferencedEvent` and `ConnectedEvent`, which record that
somebody *mentioned* an issue rather than that anybody asserted a relationship.
Importing mentions as `related` would fill a grow-only set with links nobody
wrote and no removal to undo them. Azure DevOps does have the link type, so the
kind stays in the vocabulary and is simply absent here.

#### A link needs the ledger

A relation's target is an **entity id** — the hash of that issue's own create
event — and GitHub reports a node id, so every link is resolved through the
origin ledger. A link whose target this clone does not hold is left unwritten.

That is the cross-repository case too: an issue in another repository is not in
this ledger, so a cross-repo parent silently does not arrive. It is also the
only place where an import is not a pure function of the response — the same
response can yield fewer events on a clone that has not imported the target yet.
It never yields *different* ones, and the blob is a grow-only set, so the link
is simply added when it becomes resolvable. Identity is unaffected: the create
event names nothing outside the issue.

**A batch is filed first and imported second.** The common case for an
unresolvable link is not another repository at all — it is the issue two rows
further down the same page, which the ledger has not reached yet. So an import
reports how many links it could not write, the caller files every issue in the
batch, and the ones that reported any are imported again. The second pass costs
no requests, because the responses are already in hand, and it is what stops a
link between two issues that arrived together from waiting for a run that a
`since` watermark may never make: neither issue has changed upstream, so nothing
would ask for either of them again.

A push's plan resolves links the same way, through `bridge.LinksOnly` — an
import that saw fewer links than the pull did would report a difference that is
not there and push it forever.

### Servers that have none of this

Relationships are as recent as issue types, and GitHub Enterprise Server runs
older schemas: a fragment on `ParentIssueAddedEvent`, the `PARENT_ISSUE_ADDED_EVENT`
value of the `itemTypes` argument, and the `duplicateOf` field are all rejected
outright by a server that has never heard of them. The two features degrade
**independently** and are probed once each, because a server can have one
without the other and giving up a field nobody made us give up loses data for
nothing.

### Events that must be dropped

`subscribed` / `unsubscribed` are personal state (see "Open questions" in
[issues.md](issues.md)). They are also the single highest-volume event type in
sampled GitHub timeline data, so importing them would dominate tracker traffic
while being useful to exactly one person.

`mentioned` and `referenced` are derived from body text and commit messages
rather than being independent facts, and are better recomputed locally than
synced.

## Still unrepresented

- **Label `color` and `description`.** `label.add` carries a name only. Putting
  presentation on every add would duplicate repo-wide data into every issue and
  let two issues disagree about what colour `bug` is. The alternative is a
  repo-level label entity — a new type, which the storage model supports
  cleanly. Deferred rather than decided.
- **Cross-repository references.** A relation's target is an entity id, which is
  meaningless outside its own object store, while GitHub's duplicates and
  sub-issues routinely cross repos. Blocked on the cross-repo question in
  [storage-model.md](storage-model.md) — now one question about one field
  rather than one per link. Until it is answered, such a link is not imported
  at all: the ledger has nothing to resolve it to.
- **Writing relations back.** Every kind imports; none is pushed. `issue.Diff`
  has no relation path, and sending one means translating an entity id back into
  the upstream issue it names — the ledger's job, and the same question the
  import answers in the other direction.
- **`issue_field_values`** (GitHub Projects custom fields) — out of scope per
  `TODO.md`.

## Write-back

The mechanics are platform-neutral and specified in "Pushing to an external
tracker" in [storage-model.md](storage-model.md): reconstruct what the tracker
already knows by replaying `Import` against its current state, compare three
ways, and take the ledger as the authority on which thread entries are already
posted. What belongs here is only the mapping onto GitHub's mutations.

### Mutations

| Local change | Mutation |
| --- | --- |
| `title` | `updateIssue(title:)` |
| `description` | `updateIssue(body:)` |
| `milestone` | `updateIssue(milestoneId:)`, id resolved from the repository |
| `status` → terminal, with `status.reason` | `closeIssue(stateReason:)` |
| `status` → `open` | `reopenIssue` |
| `label.add` / `label.remove` | `addLabelsToLabelable` / `removeLabelsFromLabelable` |
| `assignee.add` / `assignee.remove` | `addAssigneesToAssignable` / `removeAssigneesFromAssignable` |
| `rel.add` / `rel.remove`, kind `parent` | `addSubIssue` / `removeSubIssue`, or `createIssue(parentIssueId:)` |
| `rel.add` / `rel.remove`, kind `blocked-by` | `addBlockedBy` / `removeBlockedBy` |
| a thread entry with no ledger line | `addComment`, then a `comment` line |
| a mapped entry whose body differs | `updateIssueComment` |
| a mapped entry retracted by `comment.remove` | `deleteIssueComment` |
| an entity with no `issue` line for this tracker | `createIssue`, then `issue` and `url` lines |

`status.reason` maps back through the same table [above](#field-mapping) that
reads it, and an unrecognised reason is sent as no reason rather than guessed at.

### Writing a link

A relation names an **entity**, and a mutation names a **node**, so every link
sent goes through the origin ledger in the direction the import does not use:
`Upstream(entity)` rather than `Entity(upstream)`. Three consequences, and none
of them is a detail:

**A parent is written from the other end.** `addSubIssue` and `removeSubIssue`
address the *parent* — `issueId` is the issue that gains or loses a child — while
the link is stored on the child here. Same edge, named from the other side. The
inverse is still never stored; it is only how GitHub spells the mutation.

**A new issue is filed under its parent, not moved there afterwards.**
`createIssue` takes `parentIssueId`, and using it matters for the same reason the
labels go in the create call: `addSubIssue` afterwards raises a
`ParentIssueAddedEvent` for a link the issue was born with, and the next import
reads that back as history that never happened.

**A push applies each delta after the ones it links to.** A link cannot name an
issue that has no node id yet, so the whole tree of a first push would otherwise
need two runs. The command orders the deltas — the epic's `createIssue` earns a
mapping, the mapping is journalled, and the issues filed under it resolve it. A
cycle keeps its original order rather than dropping anything.

`replaceParent` is deliberately never sent. An issue that turns out to have a
parent upstream this clone never saw is a concurrent change, and taking it
silently is what a push must not do — the mutation fails and says so.

### What is not written back

- **`react`** — not imported either, so no local change can exist to push.
- **`type`, `locked`, `lock.reason`, `pinned`, `draft`** — no local write path
  produces them today. Mechanical to add once one does.
- **`duplicate-of`** — GitHub has `unmarkIssueAsDuplicate` and **no**
  `markIssueAsDuplicate` at all, so a duplicate can be undone through the API and
  never made. Sending only the removal would let a mirror drift one way, so
  neither half is sent and both are reported.
- **`related`, and any kind this build does not know** — GitHub has no such link,
  so there is no mutation to guess at.
- **A link whose target is not on this tracker** — including every cross-repo
  one. Reported per link, naming the issue to push first.
- **Entity removal** — unlisting an entity locally is explicitly not a deletion
  ([issues.md](issues.md)), and must never become one upstream.

### Conflicts

A scalar both sides moved since they last agreed is reported against that issue
and the issue is skipped; every other issue in the run still pushes. List fields
cannot conflict, so a contended title never blocks a label change on a different
issue.

Resolution is deliberately manual: pull, look at what the other side did, and
edit. A push that silently picked a winner would be discarding somebody's write
on the strength of a timestamp comparison across two clocks.

### Rate limits and resumability

Creating content is subject to GitHub's secondary limits, which are much
stricter than the primary hourly budget, so seeding a fork is slow. It is also
**resumable and idempotent**: an issue is created only when it has no `issue`
line for that tracker, so a run that is interrupted and repeated creates only
what is missing.

That property depends entirely on the mapping surviving the interruption. A lost
mapping means a duplicate issue upstream and nothing undoes it, so mappings are
journalled locally as they are earned and committed to the ledger ref once the
run finishes — see [storage-model.md](storage-model.md).

### Attribution

Everything a push writes is attributed by GitHub to whoever's token made the
call. Nothing can change that; no API lets one post as someone else. The
authoritative record of who wrote an event remains `a`, inside the event, which
the push does not touch.

# Issues

The issue entity type. Mechanism — serialization, ids, ordering, resolution
rules — is in [blob-format.md](blob-format.md); storage and sync in
[storage-model.md](storage-model.md). This document defines only what is
specific to issues, and stays neutral about where an issue came from — anything
particular to one external platform lives in that platform's bridge document,
e.g. [bridge-github.md](bridge-github.md).

## Identity and refs

- **Entity type name:** `issue`. This is the `val` of the `create` event, and it
  is what makes a blob identifiable independently of where it was filed.
- **`refs/notes/issues/open`** — issues in any non-terminal status.
- **`refs/notes/issues/archived`** — issues that have reached a terminal status.

Ref placement is an enumeration optimization. The `status` field inside the blob
is the single source of truth, and a transient window where an issue appears on
both refs or neither is harmless. Day-to-day clients fetch only
`refs/notes/issues/open`.

## Operations

Structural operations — `create`, `comment`, `comment.edit`, `comment.remove`,
`react`, `react.remove` — are inherited unchanged from
[blob-format.md](blob-format.md) and are not repeated here.

| Op | Kind | `val` | `ref` |
| --- | --- | --- | --- |
| `title` | scalar | The title, one line | — |
| `description` | scalar | The body, newline-escaped | — |
| `status` | scalar | See below | — |
| `label.add` | list | Label name | — |
| `label.remove` | list | — | Id of the `label.add` |
| `assignee.add` | list | Assignee identity | — |
| `assignee.remove` | list | — | Id of the `assignee.add` |
| `rel.add` | list | Relation kind, see below | Id of the related entity |
| `rel.remove` | list | — | Id of the `rel.add` |
| `rel.note` | list | Why the relation is there | Id of the `rel.add` |
| `status.reason` | scalar | See below | — |
| `milestone` | scalar | Milestone name, or `null` to clear | — |
| `type` | scalar | See below | — |
| `locked` | scalar | `true` / `false` | — |
| `lock.reason` | scalar | Free text, see below | — |
| `draft` | scalar | `true` / `false` | — |
| `pinned` | scalar | `true` / `false` | — |

Names are shared vocabulary, not issue-private: a pull request with a title uses
`title` too, with identical scalar semantics, so generic readers and bridges
work across both. See "Operation naming" in [blob-format.md](blob-format.md).

### `status`

| Value | Terminal | Meaning |
| --- | --- | --- |
| `open` | no | Active. The implied status at creation. |
| `closed` | yes | Resolved or abandoned. Eligible for the archived ref. |

An issue with no `status` event is `open`. Statuses are a scalar field, so
concurrent closes and reopens resolve by highest `(c, id)` like any other
scalar — reopening is just a later `status` event, not a special operation.
`git issue close` and `git issue reopen` write `closed` and `open`; every other
value arrives from a bridge or a newer client.

`open` and `closed` are the vocabulary this document defines. The value is an
**open vocabulary** all the same: an external tracker's own spelling is carried
through unchanged rather than flattened, so an issue pulled from Azure DevOps
reads `New`, `Active`, `Resolved` or `Done`, and `--state Resolved` works.
Unknown status values must be preserved and must not be coerced to a known one.

**Terminality is a client-side classification, not a field.** A client decides
which values end an issue's life — making it eligible for the archived ref, and
dotting it closed in a listing. `open` is non-terminal and `closed` terminal by
definition; beyond that a client classifies what it recognises and treats
everything else as **non-terminal**, because wrongly archiving something is
worse than leaving it listed. This implementation also classifies the Azure
DevOps process states, split on that platform's own state categories:

| Category | States | Terminal |
| --- | --- | --- |
| Proposed | `New`, `Proposed`, `Approved`, `To Do` | no |
| InProgress | `Active`, `Committed`, `Doing` | no |
| Resolved | `Resolved` | no |
| Completed | `Done`, `Closed` | yes |
| Removed | `Removed` | yes |

GitHub needs no entry — its `OPEN` / `CLOSED` map onto `open` / `closed` in the
bridge. A **canonical open/closed mapping across bridges** — where the mapping
lives so neither platform's vocabulary leaks into the other's — is still open;
see `TODO.md`. The table above is a client's best-effort classification of
states it happens to know, not that mapping.

### `status.reason`

Why the issue reached its current status, when the status alone doesn't say.
Kept as a **separate scalar rather than a second field on the `status` event**,
because [blob-format.md](blob-format.md) fixes the event schema at eight fields:
entity types add vocabulary, never new top-level fields.

| Value | Meaning |
| --- | --- |
| `completed` | Resolved as intended |
| `not_planned` | Closed without being done: won't fix, out of scope, stale |
| `duplicate` | Closed in favour of another entity — see the `duplicate-of` relation |

Separating the two axes has a consequence worth stating: because each is an
independent scalar, they can resolve to a **stale pairing**. An issue closed as
`not_planned` and later reopened has `status = open` with the earlier
`status.reason = not_planned` still resolving, since nothing superseded it.

> Readers must ignore `status.reason` whenever `status` is non-terminal.

Writers should not attempt to fix this by emitting a clearing event on reopen.
That looks tidier but loses information — the reason the issue was closed the
first time — and it still races, because a concurrent reopen and re-close would
resolve the two scalars independently regardless.

### `milestone`

The milestone's name, or `null` to clear it. Deliberately a **name, not a
reference to a milestone entity**: milestones are repo-level objects with their
own lifecycle on every platform that has them, and modelling that here would
mean a second entity type for something the tracker never resolves. A rename
upstream detaches the association, which is the accepted cost.

### `type`

The kind of work, as a free string. Common values are `bug`, `feature`, and
`task`, but the vocabulary is deliberately open — every platform has a slightly
different set, and coercing them loses information.

This is the field the earlier `type: epic` sketch would have used, and it
overlaps the relations question below: an epic modelled as `type: epic` plus
children pointing at it with a `parent` relation needs no new entity type, no
new ref namespace, and no migration. That is the cheaper path, and it is what
the two fields together already permit.

### `locked` and `lock.reason`

`locked` is moderation state: whether further comments are accepted. It is a
**convention, not an enforcement mechanism** — nothing in the object store can
stop a writer from appending a comment to a locked issue, and a client that
ignores the field is not corrupting anything. Treat it as a display and
policy hint.

`lock.reason` is free text. Platform vocabularies differ (GitHub uses
`off-topic`, `too heated`, `resolved`, `spam`), so bridges pass the upstream
string through rather than mapping it onto a fixed set.

### `draft` and `pinned`

Both booleans, both purely presentational.

`pinned` is worth a caveat: on most platforms pinning is a property of the
*repository's view* — a small ordered set of highlighted issues — rather than of
the issue itself. Storing it per-issue means nothing enforces the platform's
limit and nothing defines the pin order, so a client that pins ten issues gets
ten pinned issues in unspecified order. If ordering ever matters, it needs a
repo-level list, not this flag.

### `assignee`

Assignees are a **list**, not a scalar — multiple assignees are common, and
add/remove semantics match labels exactly. Modelling them as a scalar would make
two people assigning themselves concurrently a silent overwrite instead of two
assignees.

### Relations

Every link from one entity to another is **one op family**. `rel.add` carries
the relation's **kind** in `val` and its **target** in `ref`; `rel.remove`
retracts one by naming the `rel.add` it undoes.

It is an ordinary OR-Set list and adds nothing to the mechanism: a member is the
pair (`val`, `ref`) as [blob-format.md](blob-format.md) defines it, so one kind
pointing at two entities is two relations, two kinds pointing at one entity are
two relations, and the same kind and target added by two clones is one.

| Kind | Written on | Inverse, derived | Expected count |
| --- | --- | --- | --- |
| `parent` | the child | children | one |
| `blocked-by` | the blocked entity | blocks | many |
| `duplicate-of` | the duplicate | duplicated by | one |
| `related` | either end | related | many |

This replaces a scalar per link — `parent` and `duplicate.of`, both now struck
from the vocabulary. That shape cost an op, a resolution rule, a renderer and a
bridge mapping for every kind anyone added, and it could not have reached the
third kind at all: `blocked-by` is many-to-many, and a scalar holds one value.
Trackers that model this generically — Azure DevOps' link types, GitHub's
relationships — map onto one op family here rather than onto a growing list of
private ones. See "Migration" below.

#### The dependent end writes

A relation is stored on exactly one end, and the inverse is derived by inverting
the field across the entities a reader holds. It is never stored.

**Which end: the one whose own state the link constrains.** The child is filed
under the parent, the blocked issue waits on its blocker, the duplicate defers
to the canonical issue. It follows that filing five issues under an epic writes
five blobs and leaves the epic's own untouched.

One direction is not a space optimization. An edge written on both ends is two
members that can disagree, with nothing in the format able to say which end is
right — while an edge written once has one writer and resolves like any other
member.

`related` is the one kind with no dependent end: it is symmetric, so neither end
depends on the other and either may write it. A reader treats the **unordered
pair** as one relation, so the same link written from both ends renders once —
which is a display rule rather than a merge rule, since the two events are
genuinely two members and both are kept.

**A writer retracting a symmetric link retracts it at both ends.** Both members
are the link, so a `rel.remove` naming only the near one leaves it standing: the
far end's member still folds, and a reader still shows the link. This is the one
write that reaches an entity the writer did not name, and it is what a symmetric
kind costs — the price of the display rule above. A command that does it has to
say which other entity it wrote to.

Two members for one link is the ordinary case rather than a corner. Two clones
can each write the link and merge, and a bridge to a tracker that stores such a
link on *both* of its objects — Azure DevOps' `Related` is one — imports a member
with each of them. The far end is retracted only where this repository holds it;
a link naming something not held has no far end to reach.

#### Kinds are open vocabulary

Like `type` and `status`, and for the same reason: every platform has a slightly
different set, and coercing them loses information. An unknown kind must be
preserved and displayed verbatim — never coerced to `related`, which would
quietly assert something nobody wrote.

A kind that means nothing off one platform is namespaced the way an op would be
(`ado.affects`), and the same rule follows it: core state must never depend on
one. A client that ignores every namespaced kind still folds complete state.

#### Cardinality is a writer's policy, not a format rule

Nothing in a grow-only set can enforce "at most one parent". Two replicas can
each file the same issue under a different epic and then merge, and neither
event can be rejected after the fact — the same argument that makes a thread
depth cap unenforceable. So the rule is split:

- **A writer re-filing an issue emits a `rel.remove` for every surviving
  `parent` member** before adding the new one. That is the same reconciliation a
  list field already does.
- **A reader needing *the* parent takes the survivor with the highest
  `(c, id)`** and must show the others rather than hide them. Two parents is a
  fact about the data, and a reader that silently picks one teaches nobody that
  there is something to fix.

**Detaching is a `rel.remove`**, naming the add it retracts. The scalar it
replaces spelled this as an event with an empty `ref`, because last-write-wins
needs something later to win with; an OR-Set does not, and the removal is
addressed rather than positional.

That does change one outcome, deliberately. A detach concurrent with a re-file
used to resolve last-write-wins, which meant one of the two was silently gone;
it now resolves **add-wins**, so the re-file survives and both events stay
visible. Nothing is lost that a reader cannot see.

#### Cycles and absent targets

**Cycles are possible and readers must tolerate them.** Two clones can file A
under B and B under A while offline, each having checked that the other link did
not exist; the fold merges both, and neither is wrong. A writer should refuse a
cycle it can see, and a reader that walks the chain — to render a tree, to
resolve a root — must drop the edge that revisits an entity rather than assume
an acyclicity it cannot enforce.

**A target need not name anything this repository holds**, which is routine
rather than exceptional: a partial fetch, an archived parent, an unimported one.
Render the id; never hide the relation pending its target's arrival.

#### `rel.note`

A relation carries prose the way a thread entry carries an edit: `rel.note` sets
`ref` to the **`rel.add`'s** id and puts the text in `val`, highest `(c, id)`
winning. It is the annotation rule in [blob-format.md](blob-format.md), used
rather than extended.

This is where "why is this link here" goes, and it is why a relation needs no
further fields of its own. Azure DevOps carries exactly this upstream, as a
link's `attributes.comment`.

#### Cross-repo targets

Still unresolved, and the reason to state it here rather than in four places:
an entity id is meaningless outside its own object store, while duplicates,
blockers and — on GitHub — sub-issues routinely cross repository boundaries.

What the single op family buys is that this is now **one grammar question about
`ref` on one op**, rather than the same question repeated for every link field
this document ever grows. The candidate answers are a qualified `ref` naming a
repository alongside the id, or a namespaced companion event annotating the
`rel.add` with an upstream URL. Neither is chosen. See `TODO.md`.

#### Migration

`parent` and `duplicate.of` are removed outright rather than folded as legacy
aliases: nothing writes them and nothing folds them. A blob still carrying one
keeps those bytes — the preservation invariant is not negotiable — but folds
without the link, which is data loss for anyone holding such a blob.

That is acceptable exactly once, and only because no such blob exists outside
this repository's own fixtures, which were rewritten with the change. A tracker
with real history would need the escape hatch [blob-format.md](blob-format.md)
reserves for this: a new ref namespace, migrated by rewrite. `v` is unchanged,
since no surviving field changed meaning.

## Worked example

An issue blob in `cat_sort_uniq` normal form (sorted). Every hash was generated
and verified against the rules in [blob-format.md](blob-format.md).

```
{"a":"hdweiss@gmail.com","c":1,"n":"3f9a1c7e2b","op":"create","ts":1787000000,"v":1,"val":"issue"}
{"a":"hdweiss@gmail.com","c":2,"n":"a1b2c3d4e5","op":"title","ts":1787000000,"v":1,"val":"Anchor blobs are pruned by gc"}
{"a":"hdweiss@gmail.com","c":3,"n":"77de3a9014","op":"status","ts":1787000001,"v":1,"val":"open"}
{"a":"hdweiss@gmail.com","c":4,"n":"0c4411ab8f","op":"label.add","ts":1787000002,"v":1,"val":"design"}
{"a":"hdweiss@gmail.com","c":6,"n":"6d21e0fc73","op":"label.remove","ref":"13687dbd41243bb438112efc972cfb21afaeb63d","ts":1787000400,"v":1}
{"a":"hdweiss@gmail.com","c":7,"n":"4e2b0f1a9c","op":"rel.add","ref":"e878760e137a63e084cddcb8e1ac14b16b3b5313","ts":1787000500,"v":1,"val":"blocked-by"}
{"a":"hdweiss@gmail.com","c":8,"n":"c1d0be7745","op":"rel.note","ref":"240abd6342b5391915861eda15650ee7ea977684","ts":1787000520,"v":1,"val":"Waiting on the anchor-blob fix before this can be reproduced."}
{"a":"rev@example.com","c":5,"n":"b90e12f5aa","op":"comment","ts":1787000300,"v":1,"val":"Confirmed: absent in a fresh clone."}
```

Derived ids:

| Event | Id |
| --- | --- |
| `create` — and therefore the issue id | `4b0755a3e7697bfdf17e42e9f4b307c161ea2a40` |
| `label.add` — the target of the remove | `13687dbd41243bb438112efc972cfb21afaeb63d` |
| `rel.add` — the address the note attaches to | `240abd6342b5391915861eda15650ee7ea977684` |
| `comment` — the address for edits and reactions | `e42ebc4ae3c0aad54ac4a1e7a1f93a195c843db7` |

Three things to read off it:

- The `label.remove` at `c=6` sorts **above** the comment at `c=5`, because
  sorting is alphabetical by serialized bytes and `hdweiss@` precedes `rev@`.
  This is the normal, correct state of a merged blob, and it is why nothing may
  infer order from position.
- The remove names the add's id, not the string `"design"`. A concurrent
  `label.add "design"` from another clone would carry a different id, survive
  this remove, and leave the label present.
- The relation puts its kind in `val` and its target in `ref`, and the note
  addresses the **`rel.add`** rather than the target — so re-filing the same
  link after a detach is a new member with a new id, and it does not inherit the
  old one's note.

Folded state: title `"Anchor blobs are pruned by gc"`, status `open`, no labels,
one comment by `rev@example.com`, and one `blocked-by` relation on the entity
[blob-format.md](blob-format.md) works through, annotated with why.


# Blob format

The contents of a single **entity** — an issue, a pull request, anything else
the tracker learns to store. This document specifies the mechanism only:
serialization, identity, ordering, and the resolution rules available to any
entity type.

The concrete operations and value vocabularies of a given type live in that
type's own document — see [issues.md](issues.md). Where these blobs live and how
they sync is [storage-model.md](storage-model.md).

## It is a set, not a log

One blob holds one entity. Each line is one event: canonical JSON, UTF-8,
newline-terminated.

It is tempting to call this an append-only event log. That is the wrong mental
model and leads to real bugs. Merges use `cat_sort_uniq`, which sorts and dedups
the entire file every time, so the blob is a **grow-only set (G-Set) of events**
whose physical order is rewritten alphabetically behind your back.

Three properties are therefore mandatory for every event type, including ones
added later by a new entity type:

- **Self-describing.** An event must be fully interpretable on its own. Nothing
  may depend on what precedes or follows it.
- **Commutative.** Any order of arrival must fold to the same state.
- **Idempotent.** Seeing the same event twice must equal seeing it once.

Check any proposed event against those three before adding it. Most format
mistakes are caught by that single question.

> Physical line order carries no meaning, ever. Not "usually," not "before the
> first merge." Sort by the fields below.

## Canonical form

Every event is serialized as **canonical JSON per [RFC 8785
(JCS)](https://www.rfc-editor.org/rfc/rfc8785)**: object keys sorted by code
point, no insignificant whitespace, integers written plainly, minimal string
escaping. One event per physical line.

Canonicalization is required because **an event's identity is the hash of its
own bytes** (below). Two implementations that serialize the same logical event
differently would produce two different ids for one event, and `uniq` would keep
both.

Multi-line values (a paragraph description, a comment body) must be escaped onto
a single physical line. `cat_sort_uniq` merges strictly line-by-line, so a value
spanning several physical lines will have its lines interleaved with another
writer's on merge and silently corrupt.

JSON is chosen over protobuf because git already zlib-compresses objects at
rest, which erases most of protobuf's size advantage, and JSON stays legible
under `git show` / `git log -p` without extra tooling. Note the honest limit of
that legibility: an escaped 2 KB body is one enormous physical line, so raw git
is good for **debugging the tracker** and poor for **reading entity content**.
Reading content is the job of a `show` command, not of the format.

Protobuf would only earn its complexity if multiple independent implementations
needed strict cross-version schema evolution guarantees.

## Event fields

| Field | Required | Meaning |
| --- | --- | --- |
| `v` | yes | Format version, integer. See "Versioning". |
| `c` | yes | Lamport clock. The ordering key. |
| `ts` | yes | Unix wall-clock seconds. **Display only.** |
| `a` | yes | Author identity (email or stable identity ref). |
| `op` | yes | Operation name. |
| `n` | yes | Random nonce, ≥64 bits, hex. |
| `val` | per-op | Payload value. |
| `ref` | per-op | Id of another event this one targets. |

These eight fields are the whole schema. An entity type adds vocabulary — new
`op` names and new meanings for `val` — never new top-level fields, so that a
generic reader can parse, order, and re-serialize an entity type it has never
heard of.

`val` and `ref` are the two payload slots, and between them they cover more than
they look like they do. A field that points at another entity is an `op` plus
`ref`; one that must carry both a kind and a target is `val` plus `ref`; and
anything further hangs off the pointing event's own id, since every event has
one and an id is an address. See "List" below.

There is deliberately **no `id` field**. An event's id is derived, never
written — see below.

### Event ids

> An event's id is `git hash-object` of its canonical line, including the
> terminating newline.

That is a plain git blob hash, computable with stock tooling and re-derivable by
any reader straight from the note contents. Consequences:

- **An entity's id is its `create` event's id**, which is what makes entity
  identity self-verifying (see [storage-model.md](storage-model.md)).
- Two events with identical canonical bytes have the same id, which is correct:
  they *are* the same event, and deduping them is exactly right.
- Ids give every comment, list membership, and reaction a stable address — the
  prerequisite for editing a comment, reacting to one, or retracting a specific
  list entry.

### Why the nonce is not optional

`n` exists so that two logically distinct events can never collide into one set
member. Without it, `cat_sort_uniq`'s `uniq` step silently deletes real data.

Confirmed empirically: two different people posting the identical comment text
in the same second produced byte-identical blobs, and the two comments merged
into one. No conflict, no warning, one person's contribution simply gone. A
random nonce makes that impossible by construction.

git-bug carries a nonce on every operation for the same reason.

## Ordering

> Sort by `c`, then by event id. Never sort by `ts`.

`c` is a **Lamport clock**, scoped per entity:

- A new event's `c` is `max(c) + 1` over all events currently known for that
  entity. The writer already reads the blob in order to append, so this is free.
- Merging requires nothing: the union of two sets of events is the set.
- `c` is not unique. Two concurrent events can share a value; that is precisely
  what "concurrent" means here.
- Ties break on **event id**: arbitrary, meaningless, and identical on every
  replica, which is all a tie-break needs to be.

`ts` is recorded for display and **must never influence resolution**. It can
disagree with `c` in both directions, and a reader must not "correct" for that.

### Why not wall-clock time

git-bug: *"you can't rely on the time provided by other people (their clock
might be off) for anything other than just display."*

Confirmed empirically, and it is not a theoretical concern. An edit written by a
client whose clock was three years fast won the merge against a genuinely later,
correct edit — and would keep winning until real time caught up in 2033. One VM
with a broken RTC permanently pins a field, undetectably and unrepairably. No
malice required.

The tie-break rule matters for the same reason. Under a timestamp-ordered
scheme, two titles written at the same second resolved in favour of `"Zebra"`
over `"Apple"` — deterministic, but only because sorted line order happened to
favour the alphabetically later *value*. Tie-breaking on an id keeps the
determinism and drops the accidental dependence on content.

## Field kinds

Every field of every entity type must be one of these three kinds. They are the
complete menu; a type that needs something else needs a change to this document,
not a local invention.

### Scalar — last write wins

One event per assignment, carrying the new value in `val`.

**Resolution: highest `(c, id)` wins.** Each field resolves independently, so a
late writer never clobbers an unrelated concurrent field.

### List — observed-remove set

- An add event carries `val` (the member), and `ref` as well when the member
  *is* a pointer at another entity rather than a bare value.
- A remove event carries `ref`, the **id of the specific add event** it retracts
  — never the member's value.

**A member is the pair (`val`, `ref`)**, with `ref` empty for a value-only list
like `label`. Two adds agreeing on both are one member however many events carry
it — two clones adding the same label, or a push whose own change comes back on
the next import. Two that agree on `val` alone are two members: one relation
kind pointing at two different entities is two links, not one.

This is the same shape a reaction resolves by — the triple (author, `val`,
target) below — and it exists for the same reason. Keying membership on `val`
alone silently collapses distinct members into one, and a collapse in a
grow-only set is unrecoverable: there is no event that says the two were ever
separate.

**Resolution: a member is present iff it has at least one add event whose id is
not named by any remove event.**

This is a standard OR-Set, and it makes concurrent add/remove **add-wins**
deterministically: a remove can only retract adds it has actually seen, so a
concurrent add creates a new id the remove does not cover, and the member stays.

That is a decision, not a detail. Under plain timestamped add/remove lines, a
simultaneous add and remove of the same member had no defined outcome at all: the
resolution fell out of `label.add` sorting before `label.remove` in ASCII, which
is not a semantics anyone chose.

Accumulate-only list fields are forbidden. Without explicit removes keyed to a
specific add, a list can only ever grow.

**A member can be annotated, the same way a thread entry can be edited.** An
annotating event sets `ref` to the *add event's* id and carries `val`; the
highest `(c, id)` among the annotations of one member wins, and an annotation
naming an add this reader does not hold is ignored. It is distinguished from a
remove by its `op` — spelled `<field>.note`, as an add and a remove are spelled
`<field>.add` and `<field>.remove` — exactly as `comment.edit` is distinguished
from `comment.remove`.

That is the general answer to "this member needs to carry a little more than
its own value" — a note on why a link is there, a qualifier a platform
supplies. It costs no schema: an add event already has an id, and an id is an
address. Reach for it before proposing a ninth event field, which would buy the
same thing at the price of a payload slot every op must then account for.

### Thread — addressable, editable entries

An entry event carries `val`; **the event's own id is the entry's address**. An
edit event carries `ref` (the entry id) and `val` (the replacement), and the
latest `(c, id)` among edits of one entry wins. Entries display in `(c, id)`
order.

An entry may also name another entry as its **parent**, by setting `ref` to that
entry's id — the same field an edit uses, distinguished by `op`. A thread is
therefore a forest, not a flat list, which is what platforms that allow replying
to a comment need.

Note that a reply is an ordinary entry event carrying a `ref`, **not a distinct
operation**. This is deliberate: an unknown *op* is preserved but never folded,
so a `comment.reply` op would leave older clients carrying replies they never
display. An unknown *`ref` on a known op* is simply ignored, and the same client
renders the thread flat — every entry present, merely unindented. Degraded but
complete beats silently invisible.

Three rules keep the forest safe:

- **Cycles are impossible and need no check.** To reference an entry you must
  already know its id, and an entry's id is the hash of its own bytes including
  its `ref`. Two entries cannot name each other without a hash preimage attack.
  Content-addressing gives acyclicity here for the same reason it does for
  commits.
- **An entry whose parent is unknown displays at root level.** It must never be
  hidden pending its parent's arrival. A dangling `ref` on an edit or a reaction
  is harmless — there is nothing to attach to and nothing is lost — but a reply
  *is content*, and parents go missing routinely: partial fetch, a parent never
  pushed, a parent in a blob not yet pulled. A fold that waits for the parent
  loses somebody's comment with no error.
- **Depth is a display decision, never a format rule.** A depth cap is
  unenforceable in a grow-only set: events cannot be rejected after the fact,
  and two replicas can each add a reply at the limit and then merge. Clients may
  flatten below some depth for rendering; the format stays silent.

Sibling ordering is unchanged — `(c, id)`, as for a flat thread.

Retraction is a **tombstone, not a deletion**: a remove event names the entry by
`ref`, and readers stop displaying it. The original bytes stay in the blob
forever. A grow-only set has no delete, and pretending otherwise would break
convergence — a replica that had never seen the entry would have nothing to
apply the deletion to, and a replica that later received the entry from a third
clone would resurrect it.

**A tombstoned entry keeps its children.** Retracting a parent hides that
entry's own body and nothing else; replies beneath it stay, rendered under a
placeholder. The alternative — a cascade — would let one person destroy other
people's content as a side effect of withdrawing their own.

This matters for anything that must be *unpublished* rather than merely
retracted. A tombstone hides a comment; it does not remove the text from the
object store, from existing clones, or from `git log -p`. Redaction is a
history-rewriting operation, out of scope here, and no format-level flag will
substitute for it.

## Structural operations

These exist for every entity type and are specified here rather than per type.

| Op | Kind | Meaning |
| --- | --- | --- |
| `create` | — | Establishes the entity. `val` is the **entity type name**. |
| `comment` | thread | `val` is the body. Optional `ref` = parent comment id. |
| `comment.edit` | thread | `ref` = comment id, `val` = new body. |
| `react` | list | `ref` = target event id, `val` = the reaction. |
| `comment.remove` | thread | `ref` = id of the `comment` being retracted. |
| `react.remove` | list | `ref` = id of the `react` event being retracted. |

A reaction is identified by the triple **(author, `val`, target)**, not by its
event id. Two `react` events agreeing on all three are the same reaction however
many times they were emitted — a double click, or the same person reacting from
two offline replicas — and fold to a single reaction by that author. Counting
raw `react` events instead lets one person inflate a tally.

Retraction stays ordinary OR-Set: `react.remove` names a specific `react` event,
and a reaction survives while any un-retracted `react` event for its triple
remains. Concurrent react and un-react therefore resolve add-wins, like any
other list member.

A reaction to the **entity as a whole**, rather than to one comment, sets `ref`
to the `create` event's id. The create event is the one event guaranteed to
exist for every entity, and its id is the entity id. Never point such a reaction
at a scalar event like `description`: a later edit is a new event with a new id,
which would orphan every reaction attached to the old one.

`create` naming its own type is deliberate. Ref placement tells you where an
entity was filed, and — exactly as with lifecycle state — filing is an
enumeration convenience that can be stale, wrong, or lost. The blob itself is
the authority on what it is.

### Operation naming

Entity types are expected to **reuse an op name whenever the semantics match**.
If issues and pull requests both have a title, both use `title`, with the same
scalar semantics. The payoff is that a generic renderer, indexer, or bridge can
display an entity type it does not specifically know about.

Conversely, never reuse a name for different semantics. A type whose "status"
does not behave like a scalar last-write-wins field must call it something else.

### Namespaced operations

An op name containing no dot, or a dot used only to name a sub-operation of a
core concept (`label.add`, `comment.edit`), is **portable vocabulary**: it means
the same thing everywhere and any client may resolve it.

An op prefixed with a platform or bridge name — `github.origin`,
`gitlab.origin`, `jira.key` — is **foreign vocabulary**, carrying data that only
means something to one external system. Three rules apply:

- **Core state must never depend on it.** Folding an entity while ignoring every
  namespaced op must produce complete, correct state. Anything a client needs in
  order to display or resolve an entity belongs in portable vocabulary.
- **One namespace per platform, never a shared field.** Two bridges must be able
  to annotate the same entity simultaneously. A single unnamespaced `origin`
  scalar would make the second bridge's write clobber the first under
  last-write-wins, which is precisely backwards: an entity synced to two
  platforms is *more* portable, not less.
- **Preservation applies as usual.** A client that has never heard of a platform
  carries its events through merges intact, so an entity does not lose its
  GitLab identity by passing through a GitHub-only client.

The test for whether something belongs in a namespace: if the tracker were
migrated off that platform entirely, would the field still mean anything? If
not, namespace it.

## Versioning

`v` is **per event, not per blob.**

A blob-level version header cannot work here. `cat_sort_uniq` would preserve one
header line per version in use, sorted among the events, with no way to say
which one is "the" version of the blob. Per-event placement is the only
merge-safe option, and it costs nothing after compression.

Rules:

- **Additive changes do not bump `v`.** New operations, new entity types, and
  new optional fields are backward compatible by construction, because unknown
  things are preserved (below).
- **Bump `v` only when the meaning of an existing field or resolution rule
  changes.**
- A reader **must ignore events whose `v` exceeds what it understands**, and
  must still fold everything it does understand.
- Changes too incompatible to coexist in one blob get a **new ref namespace**
  (see [storage-model.md](storage-model.md)), migrated by rewrite. This is the
  escape hatch; reaching for it should be rare.

### The preservation invariant

> A client must preserve unrecognized events, and unrecognized fields within
> known events, **byte for byte**. Never rewrite a blob in a way that drops
> them.

Append-only writing gives this for free — which is exactly why it must be
written down rather than assumed. It stops being free the moment anything
rewrites a blob wholesale, which is what history compaction would do. A
compactor that discards what it cannot parse silently destroys the data of every
client newer than itself.

This is also what makes a new entity type safe to introduce: older clients carry
its events intact without understanding them.

## Authorship

`a` is the authoritative author of an event, in preference to git commit
metadata, because it survives the reordering and repacking that degrade `git
blame` (see [storage-model.md](storage-model.md)).

It is still only a **claim**. Nothing in the format prevents a client from
writing someone else's address into `a`. Signed commits are what turn the claim
into evidence; until signing exists, treat `a` as attribution, not proof.

## Worked example

Structural operations only — no type-specific vocabulary. Shown in
`cat_sort_uniq` normal form (sorted). Every hash was generated and verified
against the rules above. The entity type here happens to be `issue`; the
mechanism is identical for any type.

```
{"a":"hdweiss@gmail.com","c":1,"n":"92c4de017f","op":"create","ts":1787000000,"v":1,"val":"issue"}
{"a":"hdweiss@gmail.com","c":4,"n":"e8143aa6b0","op":"react","ref":"be9e96b43964e9f0cad6dac74b9d01cf40a74786","ts":1787000500,"v":1,"val":"+1"}
{"a":"rev@example.com","c":2,"n":"b90e12f5aa","op":"comment","ts":1787000300,"v":1,"val":"Absent in a fresh clone."}
{"a":"rev@example.com","c":3,"n":"5c07be2d31","op":"comment.edit","ref":"be9e96b43964e9f0cad6dac74b9d01cf40a74786","ts":1787000420,"v":1,"val":"Absent in a fresh clone — and after gc."}
```

Derived ids:

| Event | Id |
| --- | --- |
| `create` — and therefore the entity id | `e878760e137a63e084cddcb8e1ac14b16b3b5313` |
| `comment` — the address for both the edit and the reaction | `be9e96b43964e9f0cad6dac74b9d01cf40a74786` |

Three things to read off it:

- The `react` at `c=4` sorts **above** the comment it targets at `c=2`, because
  sorting is alphabetical by serialized bytes and `hdweiss@` precedes `rev@`.
  This is the normal, correct state of a merged blob, and it is why nothing may
  infer order from position.
- Both the edit and the reaction address the comment **by its id**, so neither
  breaks if the body changes or another comment with identical text arrives.
- Folded state: one comment by `rev@example.com`, body `"Absent in a fresh
  clone — and after gc."`, carrying one `+1`.

## Quick reference for implementers (human or agent)

1. Never rely on physical line order to mean anything. Sort by `(c, id)`.
2. Never order by `ts`. It is display metadata written by someone else's clock.
3. Always write a fresh random `n`. It is what stops `uniq` from eating a
   duplicate comment.
4. Serialize canonically (RFC 8785) or ids will not match across
   implementations.
5. Address list removals, edits, annotations and reactions by **event id**,
   never by value. A list member is the pair (`val`, `ref`), not `val` alone.
6. Prefer field-level operations (one scalar set, one comment) over whole-record
   snapshots — snapshots let a late writer silently clobber an unrelated
   concurrent field change.
7. Every field must be a scalar, a list, or a thread. There is no fourth kind.
8. Preserve events and fields you don't understand, byte for byte — including
   entire entity types you have never heard of.

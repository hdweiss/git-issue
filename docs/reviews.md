# Reviews

The review entity type. Mechanism — serialization, ids, ordering, resolution
rules — is in [blob-format.md](blob-format.md); storage and sync in
[storage-model.md](storage-model.md). This document defines only what is
specific to reviews, and stays neutral about where a review came from — anything
particular to one external platform lives in that platform's bridge document.

A review is a **change proposal**: a base, a head, commentary anchored to the
code, verdicts from the people who read it, and a terminal state of merged or
abandoned. A pull request is a review that has an entry in the origin ledger; a
review produced locally — by a person or by an agent reading a branch — is one
that has no entry and never acquires one. There is no second type for that case,
and no flag distinguishing the two: the ledger already answers "does this
correspond to something on a forge", and it answers it per tracker, so a review
opened against a fork and later against the upstream is two ledger lines and one
entity.

Most of the vocabulary is [issues.md](issues.md)'s, reused unchanged. What
follows is the difference.

## Identity and refs

- **Entity type name:** `review`. This is the `val` of the `create` event.
- **`refs/notes/reviews/open`** — reviews in any non-terminal status.
- **`refs/notes/reviews/archived`** — reviews that have reached a terminal one.

Ref placement is an enumeration optimization, exactly as it is for issues, and
the `status` field inside the blob is the single source of truth.

### Why not an issue with a `type`

Because `type` is already spoken for. It is the open bug/feature/task/epic axis
in [issues.md](issues.md), and pull-request-versus-issue is a second axis that
would have to be invented alongside it — at which point the invented axis *is*
the `create` event's type name, and the only thing gained by the detour is a
migration.

Two further reasons, both already stated elsewhere as general rules:

- **Contention.** [storage-model.md](storage-model.md) partitions refs by type
  precisely so that "issue traffic and PR traffic don't race against each
  other". Review traffic is the heavier of the two — a `head.sha` per push, a
  verdict per reviewer per revision, a thread per finding — and putting it on
  `refs/notes/issues/open` makes every issue-only client fetch all of it.
- **Status vocabulary.** A review needs `merged` as a terminal value distinct
  from `closed`. That costs nothing here, because status is open vocabulary; it
  would cost the issue type a value that means nothing to it.

> The review type never writes to `refs/notes/issues/*`.

That is a hard rule, not a convention, and it is what the `closes` relation is
designed around — see below.

## Operations

Structural operations — `create`, `comment`, `comment.edit`, `comment.remove`,
`react`, `react.remove` — are inherited unchanged from
[blob-format.md](blob-format.md).

These are inherited from [issues.md](issues.md) with identical semantics, and
are not re-specified here: `title`, `description`, `status.reason`, `label.add`,
`label.remove`, `assignee.add`, `assignee.remove`, `milestone`, `locked`,
`lock.reason`, `draft`, `rel.add`, `rel.remove`, `rel.note`. Reuse is the point
— a generic renderer or indexer that knows issues displays most of a review
without being taught anything.

What this type adds or redefines:

| Op | Kind | `val` | `ref` |
| --- | --- | --- | --- |
| `status` | scalar | See below — adds `merged` | — |
| `base` | scalar | The branch being merged into | — |
| `head` | scalar | The branch being merged from, remote-qualified | — |
| `head.sha` | scalar | The commit the review currently describes | — |
| `verdict.add` | list | `approve`, `request-changes`, `comment` | The `head.sha` it was cast against |
| `verdict.remove` | list | — | Id of the `verdict.add` |
| `comment.anchor` | thread annotation | Where in the code the comment sits | Id of the `comment` |
| `comment.resolve` | thread annotation | `true` / `false` | Id of the thread's root `comment` |

`type` is **not** part of this vocabulary. A review has no bug/feature/task
axis, and a bridge must not synthesise one.

### `status`

| Value | Terminal | Meaning |
| --- | --- | --- |
| `open` | no | Under review. The implied status at creation. |
| `merged` | yes | The head reached the base. |
| `closed` | yes | Abandoned without merging. |

A review with no `status` event is `open`. As with issues the value is an **open
vocabulary** — Azure DevOps spells these `Active`, `Completed` and `Abandoned`
and those spellings are carried through unchanged — and terminality is a
client-side classification rather than a field. A client classifies what it
recognises and treats everything else as non-terminal.

`merged` carries one rule the other values do not:

> A writer may write `merged` only when it has observed the merge — an import
> from the platform that performed it, or a merge this client performed itself.
> It is never a synonym for "I am finished with this."

The reason is that `merged` is a claim about the code, not about the review.
Every other status says what the people involved decided; this one says the head
is reachable from the base, which a reader can check and which will be wrong if
a client writes it optimistically. A review the author is done with but nobody
merged is `closed`.

### `status.reason`

Inherited, with `superseded` added to the vocabulary and the same rule attached:
readers must ignore `status.reason` whenever `status` is non-terminal, and a
reopened review keeps the reason it was closed with, since nothing supersedes
it.

`completed` is redundant here — `merged` already says it — and should not be
written.

### `base`, `head` and `head.sha`

`base` is a **bare branch name**. The base always lives in the repository the
review targets, so there is nothing for a remote to qualify.

`head` is **remote-qualified** — `origin/fix-area-walk`, `fork/feature-x` — for
reviews that have one, and a bare branch name for a purely local review. The
qualification is not decoration: a fork's head has no meaning as a bare name,
and the fork case is the common one for outside contributions rather than an
edge.

The qualifier is **not a remote name**, though it is one in the clone that wrote
it. A bridge writes the head repository's *owner* — `contributor/fix-area-walk`
— because a remote name is local to one clone and this string is shared, so what
a reader needs is whose fork the branch is in. A client resolving it therefore
has three routes, and needs all three: a remote called that, a remote whose URL
belongs to that owner under any name at all, and the forge's own published head
for the pull request — GitHub's `refs/pull/<n>/head`, which reaches a fork's
branch over the remote and the credential that already work. The last one takes
the pull request's number, which is not in the blob: it is in the URL the origin
ledger recorded, which is one more thing that correspondence buys.

`head.sha` is the commit the review currently describes, rewritten on every
push. It exists rather than being resolved from `head` because `head` is a
moving name whose resolution differs per clone and which routinely stops
resolving at all — a branch deleted after a merge is the normal end of a review's
life, not a failure. Anchors, verdicts and checks all address the sha; nothing
addresses the name.

Three consequences:

- **The sha need not name an object this repository holds.** A fork's commits
  are absent until fetched, and a review's history reaches commits that were
  force-pushed away. This is the same rule relation targets live under in
  [issues.md](issues.md): render it, never hide what depends on it.
- **`head.sha` is the highest-churn field in the vocabulary.** Every append
  rewrites the whole blob ([storage-model.md](storage-model.md) measures the
  amplification), so a long-running review that is pushed to fifty times carries
  fifty of these. That is the accepted cost of making revisions addressable, and
  it is the concrete form of the warning that this type would bite harder than
  issues.
- **The history of `head.sha` events is the revision history.** It is not a
  separate field. Ordering by `(c, id)` gives the revisions in order; the
  resolving one is the current head.

### `draft`

Inherited from [issues.md](issues.md), where it is vocabulary-only. Here it
means something: a draft is a review that is not soliciting verdicts. Like
`locked` it is a **convention, not an enforcement mechanism** — nothing in the
object store stops anyone approving a draft — and clients should treat it as a
display and policy hint.

### Verdicts

`verdict.add` records one person's position on the review. `val` is the position
and `ref` is the `head.sha` it was cast against.

| Value | Meaning |
| --- | --- |
| `approve` | This should merge |
| `request-changes` | This should not merge as it stands |
| `comment` | Read, no position taken |

Open vocabulary, like every other value in this system.

**It is an ordinary OR-Set and needs no new field kind.** A member is the pair
(`val`, `ref`) exactly as [blob-format.md](blob-format.md) defines it, which
means an approval of one revision and a request for changes on the next are two
distinct members rather than a collision, and two clones recording the same
verdict against the same revision are one.

> A reader taking *the* verdict of one author takes the survivor with the
> highest `(c, id)` among that author's members, and must show the others rather
> than hide them.

This is the cardinality rule `issues.md` states for `parent`, applied to a
different field, and it is a **reader rule over ordinary storage** rather than a
new resolution mechanism. The alternative — a scalar per author — would make a
reviewer who approves and then reconsiders lose the record that they ever
approved, which is exactly the thing a review needs to keep.

#### Verdicts go stale, and staleness is derived

A verdict whose `ref` is not the resolving `head.sha` was cast against an older
revision.

> Readers must render such a verdict as stale rather than drop it, and must
> never rewrite its `ref` to the current head.

Whether a stale approval still counts is **policy, not format**. Upstream it is
decided by branch protection; locally a client reports it and stops there. The
reason the sha is stored on the event rather than inferred is the ordering rule
in [blob-format.md](blob-format.md): inferring it would mean comparing the
verdict's `ts` against the `head.sha` events' `ts`, and `ts` must never
influence resolution.

#### Dismissal

`verdict.remove` names the `verdict.add` it retracts. An author's current
position falls back to their next-highest surviving member, or to none.

This is **the only write in the vocabulary aimed at another person's event.**
Retracting your own verdict needs no operation — casting another one supersedes
it under the reader rule above — so `verdict.remove` exists for the case where
somebody else's approval is being taken off the review. A client should say
whose verdict it dismissed.

**Verdicts are advisory.** Nothing in a grow-only set can prevent a merge, for
the same reason nothing can prevent a comment on a locked issue. A
`request-changes` is a statement, and a client that ignores it is not corrupting
anything.

### Review comments

A review comment is an **ordinary thread entry**. Anchoring it to a place in the
code adds an event that points at it; it does not add a field, a kind, or a
second thread mechanism.

#### `comment.anchor`

`ref` is the comment's id and `val` says where the comment sits. The highest
`(c, id)` among the anchors of one comment wins, which is the annotation rule
[blob-format.md](blob-format.md) already defines — used, not extended.

`val` is whitespace-separated and additive, in the spirit of the origin ledger's
lines:

```
<sha> <path> <line>[-<line>] [key=value …]
```

```
9f2c1ab74e0d… internal/issue/area.go 42-44
9f2c1ab74e0d… internal/issue/area.go 42 side=left ctx=8b31e07f
```

The leading three fields are required and positional; anything after them is
`key=value` pairs, and **unrecognised pairs are preserved byte for byte** the
way unknown events and unknown ledger kinds are. `side` distinguishes the two
halves of a split diff; `ctx` is a hash of the anchored lines' content, for the
relocation case below.

Three rules keep this safe, and each is an instance of a rule the format already
has:

- **An unanchored render is complete.** A client that ignores `comment.anchor`
  shows every comment at the top level of the thread — every one present, merely
  not attached to a line. This is the same argument that makes a reply an
  ordinary entry rather than its own op: degraded but complete beats silently
  invisible.
- **Only the root comment's anchor is read.** Replies inherit their thread's
  anchor. A reply that carries one of its own is preserved and ignored; a thread
  is attached to one place in the code, and letting replies wander produces a
  thread whose entries disagree about what they are discussing.
- **Outdated, never migrated.** Show the thread against the commit it was
  written on, whatever has happened to the head since. A client holding `ctx`
  may *offer* to relocate it to where those lines now live, and must not apply
  the relocation silently. An anchor is part of what somebody said; moving it is
  editing their words to point somewhere they never looked.

  This is also why a rendering shows `sha` beside `path` and the line range
  rather than the path alone: the line numbers count against that commit and
  against no other, so the two halves are one fact. Together they are also the
  address of the code itself — `git show <sha>:<path>` — which is what a reader
  who wants to see what was being discussed reaches for next.

Two distinct things make an anchor no longer describe the current head, and a
client that reports them as one will mislabel the common case:

- **Detached** — the anchor's commit is not reachable from the resolving
  `head.sha`. The revision it was written against was rewritten away by a rebase
  or an amend, and the commit may not be in this object store at all.
- **Outdated** — the commit is reachable, but `path` changed between it and the
  head. This is the ordinary case after a plain push, and note that **ancestry is
  not the test**: an anchor's commit stays an ancestor across every non-rewriting
  push, so a client testing reachability alone will call a thread current long
  after the lines under it were replaced.

An anchor that is neither is current, and only then may a client render the
thread inline against the head.

#### `comment.resolve`

`ref` is the id of the thread's **root** comment and `val` is `true` or `false`.
Highest `(c, id)` wins — the annotation rule again.

Addressing the root rather than each entry is what makes resolution a property
of the conversation rather than of the last thing said in it. A `comment.resolve`
naming a reply is applied to the root of that reply's thread; a client should not
write one.

Resolution is **display state, not retraction**. A resolved thread still folds,
still renders, and still carries every word in it — the difference from
`comment.remove` is the difference between *addressed* and *withdrawn*, and
collapsing the two would make "we fixed this" indistinguishable from "I take
that back".

Nothing about resolution is enforced. A thread can be resolved by anyone,
including the person who opened it and including an agent that believes it acted
on the finding.

### Relations

The `rel` family is inherited whole from [issues.md](issues.md) — one op family,
`val` is the kind, `ref` is the target, the dependent end writes, kinds are open
vocabulary. Two kinds this type adds:

| Kind | Written on | Inverse, derived | Expected count |
| --- | --- | --- | --- |
| `closes` | the review | closed by | many |
| `supersedes` | the newer review | superseded by | many |

`parent`, `blocked-by`, `duplicate-of` and `related` mean here what they mean
for issues, and a review filed under an issue with `parent` is what makes the
two types share a tree in a listing.

#### `closes` does not close

> The `closes` relation records intent. Nothing folds it into the target's
> status.

An issue's status changes when an event on the *issue's own blob* says so, and
never as a side effect of a review merging. Two reasons, and the first is
structural:

- **A cascade cannot converge.** Folding one blob would depend on the folded
  state of another, so an entity's state would stop being a function of its own
  events. A clone holding the review but not the issue, or the issue but not the
  review, would fold different answers from the same data — which is precisely
  the property this whole design exists to avoid.
- **The link is not always a mandate.** A review merged in a fork should not
  close an issue upstream, and a review that closes four issues may only really
  finish two of them.

A client may **offer** to close the target, and the close is then an ordinary
write on the issue, by a person, as its own action. That also keeps the hard rule
above intact: the review type never writes to the issue refs, and a bridge
importing "Closes #42" writes one `rel.add` on one blob.

`closes` is written on the review — the end that knows, and the end whose own
completion the link describes. This is the general dependent-end rule reading
slightly against the grain, since the issue is arguably the constrained party,
and it is called out here rather than left for someone to notice.

#### Cross-repository targets

A review routinely closes an issue in another repository, and an entity id is
meaningless outside its own object store. This is the same unresolved question
`issues.md` records, confined to the same single field, and it is not made worse
by this type — but it is made more common, since cross-repo `closes` is ordinary
where cross-repo `blocked-by` is occasional. See `TODO.md`.

## Checks

Build results, quality gates, and anything else a machine asserts about a
commit.

> Checks do not live in the review blob. They live on
> **`refs/notes/checks/runs`**, keyed by commit sha.

This is the one ref in the system whose notes keys name **objects that actually
exist**, and that is the whole argument for it:

- **A check result is a fact about a commit, not about a review.** It is true
  whichever review contains that commit, it is shared by every review and branch
  that contains it, and it outlives the review by exactly as long as the commit
  does. Filing it under the review makes the same result true in one place and
  absent in another.
- **Rebases become correct with no logic.** Old commits keep their old results;
  a rewritten commit is a different key with no results yet. Nothing has to
  detect a force-push, and no anchor has to be migrated.
- **The churn stays off the review ref.** Checks are machine-written, several
  per pipeline per push, and every append rewrites a blob
  ([storage-model.md](storage-model.md)). Keeping them separate applies the same
  contention argument that separates reviews from issues in the first place.
- **`git log --notes=checks/runs` renders build status on the log**, for free,
  with no tooling of ours involved.
- **An agent asking what is failing on `HEAD` needs no review at all.**

### This ref is not an entity namespace

It shares the blob format, the merge strategy and the event schema, and it
shares nothing else:

- **There is no `create` event and no entity id.** The key is a commit sha,
  which is already a coordination-free content-addressed identifier — the exact
  property entity ids are derived to obtain. Deriving a second identity for
  something git has already named would be ceremony.
- **`c` is scoped to the blob**, i.e. per commit, exactly as it is scoped per
  entity elsewhere.
- **It is not fetched by default.** It is useful to share — an agent on another
  machine should not re-poll a forge for what a colleague already fetched — but
  it is re-fetchable from the platform that produced it, so losing it costs a
  refresh. Prune it by age freely.

> Core state must never depend on it. A clone with no checks ref folds complete,
> correct reviews.

### Operations

| Op | Kind | `val` | `ref` |
| --- | --- | --- | --- |
| `check.add` | list | `<name> <conclusion> [key=value …]` | — |
| `check.remove` | list | — | Id of the `check.add` |

```
ci/build pass url=https://… started=1787100100 completed=1787100240
sonarqube/quality-gate fail url=https://… conditions=2
```

`pass`, `fail`, `pending`, `skipped` and `cancelled` are the conclusions this
document defines; the vocabulary is open, and a platform's own spelling is
carried through rather than coerced. Trailing `key=value` pairs are additive and
unrecognised ones are preserved.

An ordinary OR-Set again, with the same reader rule verdicts use:

> The current result of a check is the survivor with the highest `(c, id)` among
> the members sharing its name.

A re-run is a new member. A writer replacing a result **should** emit a
`check.remove` for the members it supersedes, the way a writer re-filing an issue
retracts the parent members it replaces — but a reader must not depend on that
having happened, because a bridge writing from two machines will sometimes not
have seen the member it is superseding.

## Pushing a review

The mechanism is [storage-model.md](storage-model.md)'s: read the tracker's
current state by replaying the importer against it, compare three ways against
the events both sides hold, and send the difference. Everything compared is a
folded **value**, which is what makes a mirror terminate — a change pushed to a
tracker comes back as that tracker's own event carrying the same value.

What is specific to this type is which fields take part, and three comparisons
an issue does not have.

**Never pushed.** `head` and `head.sha` describe a branch and a commit that a
tracker learns from the git remote, not from an API call; a client that wrote
them would be claiming to have moved code it never sent. Pushing the branch is
an ordinary `git push`, and it stays the caller's to run.

**`status: merged` is never pushed either**, for the reason
[§ `status`](#status) gives: it is a claim about the code that only an observer
may make. Asking a platform to merge is a different act with different
consequences, and it belongs to a command that says so.

**Verdicts.** A verdict is sent when two things hold, and they exclude different
things:

- The `verdict.add` event is not in the state the import produced. An event the
  tracker authored is upstream by definition, so this is what stops a client
  re-casting somebody else's position as its own — and it needs no ledger.
- The origin ledger does not already name the upstream object it was submitted
  as. That second test is necessary because the import of a verdict this client
  submitted comes back as a *different* event: a different author spelling, a
  different nonce, and therefore a different id.

The ledger line is `verdict <entity id> <verdict.add event id> <upstream id>`.
It also lets a later import decline to write the verdict a second time, and
recover the local event id — which a dismissal has to name.

**A verdict's message.** Casting a verdict with a message writes two events in
one action: the comment, then the `verdict.add` at the next clock. Upstream
those are one object, so the pair is recognised and sent as one — otherwise the
message would arrive both as the verdict's body and as a comment beside it,
which is exactly what an import of the result would read back as two things.

**Thread resolution** is compared three ways like a scalar, over the thread's
root. A thread created and resolved in the same run is fine: the client learns
the upstream thread's id by creating it.

**Comments have three upstream shapes** — the conversation, an entry of an
anchored thread, and the body of a submitted verdict — and they are not
interchangeable, because the operation that edits one will not address the other
two. A client cannot tell them apart from an upstream id, so it derives the
shape from what the local blob already records: whether the entry is a reply,
whether its root carries an anchor, and whether it is a verdict's message.

## What this type does not model

- **The diff.** Nothing stores it. `base` and `head.sha` name commits that are
  already in the object store, and a diff is computed from them on demand.
  Storing one would be the single fastest way to make a review blob enormous,
  and it would be wrong the moment either end moved.
- **The commits.** Same reason. The revision history is the `head.sha` events;
  the commits themselves are git's.
- **Merge mechanics** — squash versus rebase, merge queues, auto-merge. These
  are instructions to a platform rather than facts about the review, and belong
  to the command that asks the platform to merge.
- **Personal state** — whether you are subscribed, what you have read. Same rule
  as for issues: not collaborative state, and forcing it into the shared notes
  makes one person's preferences into everyone's traffic.

## Worked example

A review blob in `cat_sort_uniq` normal form (sorted). Every hash below was
generated and verified against the rules in [blob-format.md](blob-format.md).

```
{"a":"hdweiss@gmail.com","c":1,"n":"8c31f0a4d2","op":"create","ts":1787100000,"v":1,"val":"review"}
{"a":"hdweiss@gmail.com","c":10,"n":"4d70be1c93","op":"head.sha","ts":1787101200,"v":1,"val":"3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d"}
{"a":"hdweiss@gmail.com","c":11,"n":"0e93da5c28","op":"comment.resolve","ref":"9e282d0023e4d65b5ce26c00d06bb6424a2f42f2","ts":1787101260,"v":1,"val":"true"}
{"a":"hdweiss@gmail.com","c":2,"n":"5b7e19c0af","op":"title","ts":1787100000,"v":1,"val":"Fix the area subtree walk"}
{"a":"hdweiss@gmail.com","c":3,"n":"1d4a08b3e6","op":"base","ts":1787100000,"v":1,"val":"main"}
{"a":"hdweiss@gmail.com","c":4,"n":"e70c2f5a91","op":"head","ts":1787100000,"v":1,"val":"origin/fix-area-walk"}
{"a":"hdweiss@gmail.com","c":5,"n":"a3f5c81b0e","op":"head.sha","ts":1787100000,"v":1,"val":"9f2c1ab74e0d3b5f8c6e2a1d40b7e93c5a8f1d26"}
{"a":"hdweiss@gmail.com","c":6,"n":"60be2d7f14","op":"rel.add","ref":"4b0755a3e7697bfdf17e42e9f4b307c161ea2a40","ts":1787100010,"v":1,"val":"closes"}
{"a":"rev@example.com","c":7,"n":"cf01a29b7d","op":"comment","ts":1787100600,"v":1,"val":"This drops the subtree when the area is a leaf."}
{"a":"rev@example.com","c":8,"n":"72e4b0d95a","op":"comment.anchor","ref":"9e282d0023e4d65b5ce26c00d06bb6424a2f42f2","ts":1787100600,"v":1,"val":"9f2c1ab74e0d3b5f8c6e2a1d40b7e93c5a8f1d26 internal/issue/area.go 42-44"}
{"a":"rev@example.com","c":9,"n":"b81f60ca37","op":"verdict.add","ref":"9f2c1ab74e0d3b5f8c6e2a1d40b7e93c5a8f1d26","ts":1787100601,"v":1,"val":"request-changes"}
```

Derived ids:

| Event | Id |
| --- | --- |
| `create` — and therefore the review id | `102b0ce7406263b5deec152f6557544cc7121075` |
| `comment` — the address the anchor and the resolve attach to | `9e282d0023e4d65b5ce26c00d06bb6424a2f42f2` |
| `rel.add` — the address a `rel.note` would attach to | `73a08eb80dbc7e50f413bcdce6f5b5bb3eb98e1b` |
| `verdict.add` — the address a dismissal would name | `5740e2bf994888abdf4a8a59877b1ab500507a0f` |

The `closes` target is the issue [issues.md](issues.md) works through.

Five things to read off it:

- **`c=10` sorts above `c=2`.** Sorting is alphabetical over serialized bytes,
  so `"c":10` precedes `"c":2` — the clock is not even numerically ordered in
  the file, let alone chronologically. This is the normal state of a merged blob
  and it is why nothing may infer order from position.
- **The head moved after the review was read.** `head.sha` resolves to
  `3ac8e05f…` at `c=10`, and the verdict at `c=9` names `9f2c1ab7…`. The
  `request-changes` is therefore **stale**, and a reader shows it as such rather
  than dropping it or re-pointing it.
- **The thread is detached, and separately resolved.** Take `3ac8e05f…` to be an
  amend of `9f2c1ab7…` — the author rewrote the commit to address the comment —
  so the anchor names a revision no longer reachable from the head, and the
  thread renders against the commit it was written on. The resolve at `c=11` is
  independent of that: one is a fact about the code, the other about the
  conversation, and a plain push that left `9f2c1ab7…` reachable would have made
  the thread outdated rather than detached without changing the resolve at all.
- **The anchor and the resolve both address the comment's id**, not its text and
  not its position in the thread. A second comment with the same body would be a
  different id, carry no anchor, and be unaffected by the resolve.
- **Nothing here reaches the issue.** The `closes` link is one `rel.add` in this
  blob. Issue `4b0755a3e769` is untouched, and stays open until somebody writes
  a `status` event on it.

Folded state: title `"Fix the area subtree walk"`, `main` ← `origin/fix-area-walk`
at `3ac8e05f…`, status `open`, one resolved and outdated thread anchored to
`internal/issue/area.go`, one stale `request-changes` from `rev@example.com`, and
one `closes` link.

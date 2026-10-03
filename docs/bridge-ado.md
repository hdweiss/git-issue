# Bridge: Azure DevOps

Mapping between Azure DevOps work items and the [issue entity](issues.md). Generic bridge
mechanics — deriving nonces so re-import converges, one commit per upstream event — are in
[storage-model.md](storage-model.md).

This document exists because the tracker's purpose is portability: an issue must survive
the platform it came from. Nothing ADO-specific belongs in [issues.md](issues.md), and
nothing here may be required to read an issue. A client that has never heard of Azure
DevOps folds an ADO-bridged issue into complete state.

Both deployments are supported and are the same bridge: **Azure DevOps Services**
(`dev.azure.com`, and the legacy `*.visualstudio.com`) and **Azure DevOps Server / TFS**,
which lives on an arbitrary host and often under a virtual directory.

## The target

```
ado:[<location>][#<area>]
```

`#` separates *where* from *which part of it*. ADO forbids `#` in classification-node
names, and forbids `/` in them too, so neither character needs escaping and neither can be
confused for part of an area path. `#` does not start a comment mid-word in bash or zsh, so
the target never needs quoting.

| Written | Means |
| --- | --- |
| `ado:` | the current branch's remote, else `origin`; the saved area |
| `ado:origin` | that remote, the saved area |
| `ado:origin#Web/Auth` | area `MyProj\Web\Auth` **and everything under it** |
| `ado:origin#` | forget the saved area and ask again |
| `ado:contoso/MyProj#Web` | an explicit collection/project, with no git remote for it |
| `ado:https://corp.local/tfs/DefaultCollection/MyProj/_git/repo` | an explicit on-prem URL |

**An area always means its subtree** — `UNDER`, never `=`. There is no exact-node form.
Subtree is what ADO's own team configuration defaults to and what boards and backlogs scope
by, and work items are usually filed at leaves, so an exact match on a parent node would
return almost nothing while looking like it had worked. It is also the more expressive of
the two: subtree cannot express "this node but not its children", while exact cannot
express "this branch" at all.

An area written on the command line applies to that run only. It does not overwrite the
saved one, for the same reason `--since` does not move the watermark: it is a question, not
a checkpoint.

### Locating the collection and project

ADO clone URLs are `<base>/<collection>/<project>/_git/<repo>`. Split on `/_git/`: the last
two path segments to its left are the collection and the project, and everything before
them is the base URL, virtual directory included. One rule covers every deployment.

| Remote URL | Base | Collection | Project |
| --- | --- | --- | --- |
| `https://dev.azure.com/contoso/MyProj/_git/repo` | `https://dev.azure.com` | `contoso` | `MyProj` |
| `https://corp.local/tfs/DefaultCollection/MyProj/_git/repo` | `https://corp.local/tfs` | `DefaultCollection` | `MyProj` |
| `https://contoso.visualstudio.com/MyProj/_git/repo` | `https://contoso.visualstudio.com` | `contoso` | `MyProj` |
| `git@ssh.dev.azure.com:v3/contoso/MyProj/repo` | `https://dev.azure.com` | `contoso` | `MyProj` |
| `contoso/MyProj` | `https://dev.azure.com` | `contoso` | `MyProj` |

One segment to the left of `_git/` is the `*.visualstudio.com` form, where the collection is
the host's first label. The scp-style SSH form carries no `_git` and is recognised by its
`v3/` marker instead.

`/_git/` is also what identifies an ADO remote at all. Guessing a bridge from a URL is
ordinarily the surprise an explicit scheme exists to avoid, and a fully bare `git issue
pull` is the one case where there was no scheme to write — see `LooksLikeADO`. A host under
`dev.azure.com` or `visualstudio.com` says so directly; on-prem says so with `/_git/`, which
appears in essentially no other tracker's clone URLs.

**Casing.** ADO compares project names case-insensitively, so two spellings of one project
would fork the origin ledger into two trackers. The canonical `name` from
`GET {API}/projects/{project}` is resolved on first contact and used from then on.

## Identity, and where it is recorded

This bridge writes **no namespaced operations at all**. A work item's correspondence with
an entity here is not a fact about the issue — see "The origin ledger" in
[storage-model.md](storage-model.md) — so it is recorded there, under the tracker's own
name:

```
issue    4f2a1c9…  ado:dev.azure.com/contoso#1234
url      4f2a1c9…  https://dev.azure.com/contoso/MyProj/_workitems/edit/1234
comment  4f2a1c9…  8b31e07…  ado:dev.azure.com/contoso#1234/comments/7
```

The tracker name — the ledger's path — is the host, virtual directory, collection and
project: `dev.azure.com/contoso/MyProj`, or `corp.local/tfs/DefaultCollection/MyProj`. The
slashes become subdirectories for free.

**The work item id is the identity; the URL is only a locator.** ADO has no opaque node id,
so the id serves as one — and it is a genuinely good one, because a work item id is unique
per *collection* and survives being moved between projects, between areas, and between
iterations. Only the URL changes.

That is why the identity string is keyed on the **collection**, not the project:

```
K = <host><virtual-dir>/<collection>      lowercased
    dev.azure.com/contoso
    corp.local/tfs/DefaultCollection
```

A work item moved from `MyProj` to `OtherProj` in the same collection keeps its id and
therefore keeps its entity. Keying on the project would fork it into two.

### Nonces

Per [storage-model.md](storage-model.md), every imported event's `n` is derived from the
upstream object's own stable identity, so a re-import produces byte-identical events and
converges instead of duplicating. The exact input string is part of this specification
rather than an implementation detail — two bridges that hash different strings fork the
identity of every issue they both import.

Writing `W` for `ado:K#<id>`:

| Event stands for | `n` is the first 16 hex of SHA-256 of |
| --- | --- |
| The work item itself (`create`) | `W` |
| A scalar changed by a revision | `W/revisions/<rev>/<op>` |
| A list member added or removed by a revision | `W/revisions/<rev>/<op>:<value>` |
| The work item as filed — a scalar | `W:<op>` |
| The work item as filed — a list member | `W:<op>:<value>` |
| A comment | `W/comments/<cid>` |
| An edit of that comment | `W/comments/<cid>/versions/<n>` |
| That comment's deletion | `W/comments/<cid>:removed` |

The string stored in the origin ledger is byte-identical to the create event's nonce input,
so the ledger and the hash cannot drift apart.

Two of these need their reason stated, because a shorter form looks sufficient and is not.

**The operation is always in a revision's nonce**, not only when a revision happens to
change more than one field. One revision routinely yields several events — a state change
and a reassignment saved together — and they would otherwise collide into one. Deciding per
revision whether to add the discriminator would make an event's nonce depend on which
*other* fields that revision touched, which is the same class of mistake as deriving `c`
from a position in the feed: add a field to that revision's payload and every event in it
gets a new id.

**A list member's value is in the nonce too**, for the same reason one step down: a single
revision can add two tags, and `W/revisions/7/label.add` cannot tell them apart.

For the events standing for the work item as filed, the nonce deliberately does *not*
mention a revision. Those values come from the creation revision where there is one and
from the earliest revision that changed the field where there is not, so keying them on the
work item and the operation is what keeps them identical whether or not the revision feed
was readable.

### Clocks

`c` is the upstream Unix timestamp — `revisedDate` for a revision-derived event,
`createdDate` for a comment. Never a position in the feed: delete a comment upstream and
every later position shifts, silently duplicating every event after it on the next import.

One quirk has to be handled rather than passed through. A revision is modelled as valid
until its successor supersedes it, so the newest revision of every work item comes back
with a `revisedDate` of **`9999-01-01`** — it has no successor yet. Taken literally that
becomes a Lamport clock eight thousand years in the future, permanently sorting the newest
event above everything else in the entity, and changing on the next import as soon as
another revision arrives. The revision's own `System.ChangedDate` is the honest timestamp
and is what `c` takes.

## Author identity

ADO identities carry a `displayName`, a `uniqueName` — usually a UPN or email address, but
`DOMAIN\user` on older on-prem installs — and a GUID `id`. Bridged events set `a` to:

```
ado:<uniqueName>        lowercased, falling back to ado:<id> when there is no uniqueName
```

The scheme prefix is what keeps an upstream identity from colliding with a local email, and
it matches `github:<login>`. `a` is hashed into every event's id, so this spelling is part
of the specification.

The consequence is the same one GitHub's bridge has: the same person is `ado:jane@corp.com`
upstream and an email locally, so `--author` sees two people. The identity map that fixes
it belongs in the origin ledger and is queued in `TODO.md`.

### Commit byline

A bridge maps upstream identity onto a commit author as well, and that is a different
mapping: `a` is hashed into event ids, while a byline is hashed into nothing. The byline
uses the `displayName` and the `uniqueName` as an address when it looks like one, so git's
own tooling has something to work with.

## Field mapping

ADO's `updates` feed is richer than GitHub's timeline. Every revision carries `oldValue` and
`newValue` for each field it touched, plus `revisedBy` and `revisedDate`, so most fields
reconstruct as genuine attributed history rather than collapsing to one event at import
time.

| ADO | Becomes |
| --- | --- |
| `System.Title` | `title`, one per revision that changed it |
| `System.Description` (see below) | `description`, one per revision that changed it |
| `System.State` | `status`, **verbatim** |
| `System.WorkItemType` | `type`, lowercased |
| `System.Tags` | `label.add` / `label.remove` |
| `System.AssignedTo` | `assignee.add` / `assignee.remove` |
| `System.IterationPath` | `milestone` |
| `System.LinkTypes.Hierarchy-Reverse` | `rel.add` / `rel.remove`, kind `parent` |
| `System.LinkTypes.Dependency-Reverse` | `rel.add` / `rel.remove`, kind `blocked-by` |
| `System.LinkTypes.Duplicate-Reverse` | `rel.add` / `rel.remove`, kind `duplicate-of` |
| `System.LinkTypes.Related` | `rel.add` / `rel.remove`, kind `related` |
| comments | `comment`, `comment.edit`, `comment.remove` |
| work item id, `_workitems/edit/<id>` | `issue` and `url` in the origin ledger |
| `System.AreaPath` | **nothing** — see "The area is scope" |

### `status` is verbatim, and open/closed is deferred

ADO's states depend on the process template: Agile has New/Active/Resolved/Closed, Scrum has
New/Approved/Committed/Done, Basic has To Do/Doing/Done, and a customised process has
whatever it was given. `status` carries `System.State` exactly as ADO spells it.

[issues.md](issues.md) requires an unrecognised status to be preserved and treated as
non-terminal, so this is within the format. The consequence is worth stating plainly: an
issue in Scrum's `Done` folds to a status this tracker does not recognise as terminal, and
stays listed as open. `Terminal` compares case-insensitively, which covers Agile's `Closed`
but nothing else.

A canonical mapping is **deliberately deferred** until a third bridge shows what shape it
should take. The mechanism is not the hard part — ADO groups every state into a category
(`Proposed`, `InProgress`, `Resolved`, `Completed`, `Removed`), and `Completed` and
`Removed` are the terminal ones. Where the mapping should live, so that GitHub and ADO agree
without either bridge's vocabulary leaking into the other's, is the open question.

Those same categories are what an open-only import is scoped by, and the two uses are worth
telling apart. **Scoping asks the server a question; mapping would write an answer down.**
The query says

```sql
AND [System.State] NOT IN GROUP 'Completed'
AND [System.State] NOT IN GROUP 'Removed'
```

so `--all` versus the default is decided entirely inside the WIQL, on ADO's own definition
of which of *its* states are terminal. Nothing about that reaches an event, which is why it
can be settled now while the mapping stays deferred.

### The body field is not always `System.Description`

Bug work items on the Agile process put their body in
`Microsoft.VSTS.TCM.ReproSteps`, not `System.Description`. The body field is resolved per
work item type; assuming `System.Description` silently imports every bug with an empty
description.

Unlike GitHub, which exposes only a body's current content, ADO's revisions carry the old
and new value of the body itself — so **descriptions have complete edit history here**. The
asymmetry is a property of the two platforms, not of the format.

### `System.Tags` is one string, and is diffed

Tags arrive as a single semicolon-separated string, and a revision reports the whole old
string and the whole new one. Diffing the two yields the tags added and removed by that
revision, each becoming a `label.add` or a `label.remove` attributed to `revisedBy`. A
remove names the id of the specific `label.add` it retracts, per the OR-Set rule in
[blob-format.md](blob-format.md) — never the tag's text, since a concurrent add of the same
tag from another clone must survive.

### Assignees are a list here and a scalar there

ADO's `System.AssignedTo` holds one identity. [issues.md](issues.md) makes assignees a list
because multiple assignees are common elsewhere and because a scalar turns two people
assigning themselves into a silent overwrite. A revision that changes the assignee therefore
imports as a pair — `assignee.remove` naming the live add, then `assignee.add` — so the list
holds at most one ADO assignee and the OR-Set semantics stay intact.

A push sends the first member and reports any others as skipped. There is no way to
represent two assignees upstream, and dropping the second silently would be worse than
saying so.

### `System.History` must not be replayed

`System.History` appears in the `updates` feed as an ordinary field change, and it *is* the
comment text — ADO's comments and its history field are the same store, surfaced two ways.
The comments API is the authoritative view, because only it gives each comment a stable id
to derive a nonce from. A bridge that replays both files every comment twice.

### Comments do not thread

Work item comments are flat. ADO has no `in_reply_to` equivalent, so no `ref` is written and
every comment is a root entry. Locally authored replies still nest, and a push flattens them.

Comment **edits do have history**: `GET .../comments/{id}/versions` returns each version with
its own author and timestamp, so an edited comment imports as a `comment` plus one
`comment.edit` per later version. `isDeleted` becomes `comment.remove`.

One trap is worth naming, because the obvious reading of the API is wrong. A comment's
`text` field is its **current** text, not what was posted — an edited comment reports its
latest version there. The `comment` event must carry the *first* version's text, or the
same string is filed twice, once as the comment and once as an edit of itself, and the
comment's original wording is lost entirely.

### Which work item types are imported

Every type **except those in `Microsoft.HiddenCategory`**, asked for as a category group in
the query itself:

```sql
AND [System.WorkItemType] NOT IN GROUP 'Microsoft.HiddenCategory'
```

That category is ADO's own record of the types it keeps off backlogs and boards — on the
stock processes, Test Case, Test Suite, Test Plan, Shared Steps, Shared Parameter, and the
code-review and feedback request/response types; on a customised process, whatever that
project hid. Asking for the group rather than reading
`GET {API}/wit/workitemtypecategories` and expanding it locally means the answer is the
server's, costs no extra round trip, and cannot go stale.

Excluding them is the difference between a usable import and an unusable one: test
artifacts routinely outnumber tracked work ten to one, their content lives in
`Microsoft.VSTS.TCM.Steps` as an XML blob with no representation in the issue vocabulary,
and they are churned by tooling rather than by people.

`--type` replaces the group with an explicit list, and is therefore also how someone who
genuinely wants test cases asks for them.

### Links

Azure DevOps models every link the same way — a `rel` type and a URL — which is
also how [issues.md](issues.md) models a relation, so the mapping is a table
rather than a field each. `Epic → Feature → User Story → Task` imports as `type`
values plus real `parent` relations, and the hierarchy survives. **All four
kinds [issues.md](issues.md) defines map**, which makes this the more capable of
the two bridges on links: GitHub can write two of them.

**Only the reverse half of each directional family is mapped**, and that is the
whole of the direction question. ADO stores an edge on *both* work items —
`Hierarchy-Reverse` on the child and `Hierarchy-Forward` on the parent,
`Dependency-Reverse` on the blocked item and `Dependency-Forward` on the blocker,
`Duplicate-Reverse` on the duplicate and `Duplicate-Forward` on the canonical —
while a relation here is stored once, on the dependent end. The forward halves
are therefore dropped rather than imported: the same edge arrives with the other
work item, from the end that owns it. Importing both would file the epic under
its own child.

One rule fixes which half is which: ADO spells a directional link from the
dependent end, and `Reverse` is always the way up — the parent, the predecessor,
the item this one duplicates. `Related` is symmetric, sits on both work items
under one reference name, and so has no direction to get wrong.

**`Related` is therefore the one kind that imports a member at each end**, since
each work item genuinely carries it and an import is a pure function of the one
work item in front of it. [issues.md](issues.md) allows exactly this — two
members, one link — and pays for it by making a retraction reach both. Choosing
one end at import time instead is not open: a push reconstructs its base by
replaying the importer, so an import that declined a link the entity locally
holds would report it missing upstream on every run, push it again each time, and
duplicate the edge. The other three kinds are strictly one-ended, exactly as
GitHub's are.

A link is written only when its target is itself mapped in the origin ledger,
because a relation holds an *entity* id — the hash of that issue's own create
event — which cannot be derived from a work item id without fetching it. A
target outside the import's scope is reported, not guessed at. This is the same
cross-repository question [storage-model.md](storage-model.md) is blocked on;
ADO makes it sharper, since a link may cross to another project of the same
collection.

A target *inside* the import's scope is a different matter, and one an import
must not lose: an epic and its children arrive in the same batch, and the ledger
learns where each is filed only as the batch is imported. So the batch is filed
first and imported second, exactly as
[bridge-github.md](bridge-github.md) describes — an import reports how many
links it could not write, and the ones that reported any are read again for
free, from responses already in hand.

ADO reports links twice over — as the revisions that added and removed them, and
as the array a work item currently holds — and both are read. The feed is what
attributes a link to whoever made it; the array is what makes the import right
anyway when the feed is unavailable or does not reach far enough back. Whatever
the replay did not account for is reconciled at the end of the import, the same
way a tag the revisions never mentioned is, and a link the replay saw *retracted*
stays retracted.

### Fields that need no operation

- **`System.CreatedBy` and `System.CreatedDate`** are the `a` and `ts` of the create event.
- **`System.ChangedDate`** is the import watermark, not state; it is derived from the
  events already held.
- **`System.CommentCount`** is a count of data the blob holds. Never store a count that can
  be recomputed: it is one more thing to keep consistent under merge, and it cannot be.

### Still unrepresented

- **`Microsoft.VSTS.Common.Priority`, `Severity`, `Microsoft.VSTS.Scheduling.*`** — no core
  operation covers effort, priority or story points. A namespaced scalar would fit the
  format but has not been justified by a use yet.
- **Attachments.** The entity model has no attachment concept at all, for any bridge.
- **Link comments.** ADO carries free text on a link, as a relation's
  `attributes.comment`. `rel.note` is exactly where it goes; the API layer does
  not decode it yet.
- **Reactions on comments.** ADO has them; they are unimported here for the same reason
  they are unimported from GitHub, and therefore unpushed.
- **Boards, columns and swimlanes**, which are per-team views rather than facts about a
  work item.

## The area is scope, and only scope

A work item's area path is a fact about the work item **in ADO**, not about the entity here.
Apply the namespacing test from [blob-format.md](blob-format.md) in its harsher form — stop
caring about this tracker, and does the field still mean anything? An area path is a
taxonomy whose shape exists only inside one ADO project; it does not survive the platform.

So the area is stored nowhere: not as an event, not in the origin ledger, not in an index.
It exists in exactly two places, both of them configuration rather than state:

- the `#` scope on a target, for one run;
- `issue.<tracker>.area` in git config, as the saved default.

And it does exactly two things:

- it is the `[System.AreaPath] UNDER '…'` predicate on a pull;
- it is `System.AreaPath` when a push **creates** a work item.

**A push to an existing work item never writes `System.AreaPath`.** That is not a rule the
push logic enforces; it follows from there being no local area to produce a delta from.
Moving a work item between areas is done in ADO.

> Why iteration maps to `milestone` while area maps to nothing, when both are ADO paths:
> an iteration is a scheduling fact that survives leaving the platform — "this is for Sprint
> 3" means something on any tracker, and `milestone` is defined as a name rather than a
> reference. `milestone` carries the iteration path below the project root rather than the
> leaf name, so a "Sprint 3" under two different releases does not collide.

The cost is accepted rather than hidden: a clone that pulled a whole project cannot tell a
`Web` issue from a `Mobile` one, and a work item moved out of the pulled area silently stops
updating. The second is the same staleness window an open-only import has, and has the same
remedy — the ledger holds every upstream id, so a refresh can name them directly.

### Choosing one

A pull with no area saved and none written renders the project's area tree and asks, once,
per clone. Choosing the root means the whole project, so "no area" needs no special answer.
When stdin is not a terminal nothing is prompted: the import covers the whole project and
says how to scope it.

## Import scope and resuming

The watermark is `System.ChangedDate`, and the query orders by it ascending so that a run
cut short by `--limit` has read a prefix: every work item it did not reach has a
`ChangedDate` at or after the watermark, and the next run asks for precisely those. The
comparison is inclusive, so the boundary item is read twice rather than lost, and
re-reading converges.

Keep one watermark **per scope**, and the area is part of the scope:

```
ado:<tracker>:<area or "">:<"open"|"all">
```

The rule between scopes is that **a wider run may advance a narrower run's watermark, never
the reverse.** A whole-project `--all` run has genuinely read everything a `#Web/Auth`
open-only run would have, so it advances that key too. A subtree run has not read its
ancestors and must not touch theirs.

The watermark is local, derived and disposable, exactly like any other index. It says what
*this* clone has read, which is true of nowhere else; losing it costs one full re-import and
nothing more.

> WIQL refuses a result set above 20,000 ids (`VS402337`). Narrowing the area or passing
> `--since` is the answer, and the error says so rather than surfacing raw.

## Write-back

The mechanics are platform-neutral and specified in "Pushing to an external tracker" in
[storage-model.md](storage-model.md): the base is reconstructed by replaying the importer
against upstream's current state, so nothing records what was pushed last time.

### Mutations

Everything is one JSON-Patch document per work item, `application/json-patch+json`:

| Change | Patch |
| --- | --- |
| `title` | `/fields/System.Title` |
| `description` | `/fields/System.Description`, or the type's body field |
| `status` | `/fields/System.State` |
| `milestone` | `/fields/System.IterationPath` |
| `label` | `/fields/System.Tags`, rewritten whole |
| `assignee` | `/fields/System.AssignedTo`, first member only |
| `rel.add` | `add` on `/relations/-`, `{rel, url}` |
| `rel.remove` | `remove` on `/relations/<index>` |
| create | `POST .../workitems/${Type}`, plus `System.AreaPath` |

Comments go through the comments API — `POST`, `PATCH` and `DELETE` on
`.../workItems/{id}/comments[/{cid}]` — and each mapping earned is journalled before the
next call, so a run that fails part-way leaves everything before it recorded.

**Every patch opens with a `test` on `/rev`:**

```json
[ {"op":"test","path":"/rev","value":17},
  {"op":"add","path":"/fields/System.Title","value":"…"} ]
```

ADO rejects the whole document if the work item has moved since the plan read it, so a
concurrent upstream edit fails the write instead of silently clobbering it. GitHub's API
offers no equivalent, and this is the one place where ADO's write path is the stronger of
the two.

### Writing a link

**A removal names a relation by its index, and by nothing else.** There is no
"remove the link whose rel and url are these", so the document can only be built
against the relations array as it was read — which is why the client is handed
the work item rather than its id. Two consequences follow:

- **Removals go before additions, and descending among themselves.** JSON-Patch
  applies operations in order and removing an element closes the array up behind
  it, so ascending removals delete the wrong links from the second one on.
- **The `test` on `/rev` is what makes the indexes safe.** A relations array that
  has changed under the patch belongs to a revision that has changed, and the
  test refuses it. The concurrency check and the addressing scheme are the same
  guarantee.

A link is matched to its index by link type and target *id*, not by comparing
URLs as strings: ADO returns a link spelled against the collection and this
client writes one spelled against the project. A link that is no longer in the
array produces no operation — upstream has already dropped it.

Everything a work item is created with goes in the create document, links
included, for the reason the fields do: one created bare and then linked gains a
revision for a link it was born with, and the next import reads that back as
history that never happened. A whole tree therefore seeds in one run, because a
push applies each delta after the ones it links to.

### What is not written back

Reported as skipped rather than dropped silently:

- a **link whose target is not on this tracker** — a relation names an entity and
  a patch names a work item by URL, so an issue that has never been pushed cannot
  be linked to. Pushing it first is the whole fix, and the message says so. This
  is the same cross-project question the import side is blocked on;
- a **link of a kind ADO has no link type for** — every kind
  [issues.md](issues.md) defines has one, so what is left is a kind another
  bridge carried in, and guessing at a link type would be worse than saying so;
- a **second assignee**, which ADO cannot hold;
- a **`type` change**, which ADO restricts — changing `System.WorkItemType` is not a plain
  field write and can fail on rules the client cannot see;
- **reactions and attachments**, which are unimported and so have nothing to push;
- **`System.AreaPath` on an existing work item**, per "The area is scope" above.

### Attribution

The receiving platform attributes every write to whoever's PAT made it, because no ADO API
lets one post as someone else. The event's own `a` stays correct locally, and that is the
record that matters.

## Pull requests

`git review pull` reads Azure DevOps pull requests into the [review entity](reviews.md).
The mapping is a sibling of the work-item one — same identity discipline, same "pure
function of what the API returned" rule — living in `internal/bridge/ado/review` over
`internal/bridge/ado/api`. Both directions exist: `git review pull ado:<repo>` imports, and
`git review push ado:<repo>` writes local changes back — see "Push", below.

### The target names a repository

A pull request is per-repository, not per-project, so the target must resolve to one:

| Written | Means |
| --- | --- |
| `ado:` / `ado:origin` | the remote's clone URL, whose `/_git/<repo>` segment names the repo |
| `ado:https://dev.azure.com/org/proj/_git/repo` | an explicit repository URL |
| `ado:org/proj` | **rejected** — a collection/project slug names no repository |

There is no `#` scope: an area path is a work-item concept.

### Identity

```
ado:<host>/<collection>/<repo>/pullRequests/<n>
```

The nonce input for the review's `create` event and the id the origin ledger files it
under. The collection alone keys a work item, because a work-item id survives a move
between projects; a pull request id is unique only within its repository, so the repository
is part of its identity. Comments are `…/threads/<t>/comments/<c>`, threads `…/threads/<t>`.

### Field mapping

| Review field | Azure DevOps |
| --- | --- |
| `title`, `description` | `title`, `description` |
| `base` | `targetRefName`, `refs/heads/` stripped |
| `head` | `sourceRefName` stripped; prefixed `<forkRepo>/` when `forkSource` names another repo |
| `head.sha` | `lastMergeSourceCommit.commitId` |
| `status` | `active` → open (no event); `completed` → `Completed`; `abandoned` → `Abandoned` — verbatim, classified client-side like the work-item states |
| `draft` | `isDraft` |
| `label.add` | `labels[].name` |
| `assignee.add` | `reviewers[]` — a reviewer is who was asked to read, which is what the review's assignee list means |
| `rel.add closes` | linked work items (`/workitems`), resolved through the ledger; an unheld one is left unwritten and counted, exactly as on the work-item side |
| `verdict.add` | reviewer `vote`: `10`/`5` → `approve`, `-10` → `request-changes`, cast against `head.sha` |
| comments | thread comments with `commentType` `text`; a thread with a `threadContext` is a review thread and its first comment carries the `comment.anchor` |
| `comment.resolve` | thread `status` of `fixed`, `closed`, `wontFix` or `byDesign` |
| checks (own ref) | pull request `statuses` → `<genre>/<name>` keyed by `head.sha` |

The anchor's revision is the commit of the **iteration** the thread was left against
(`pullRequestThreadContext.iterationContext.secondComparingIteration` → `/iterations`),
falling back to `head.sha`. Azure DevOps re-bases a thread's displayed position as later
iterations land; importing that would rewrite where the reviewer was looking.

### What is not imported

- **Vote history.** Azure DevOps keeps only the current vote, so a verdict imports as
  current state against the current head — there is no per-revision history and no
  dismissal.
- **A `-5` vote** ("waiting for the author") is a soft "not yet" rather than a position on
  the change, so it imports as no verdict. Only an explicit approval or rejection counts.
- **Policy evaluations** (build validation, required reviewers as a gate). Only the
  simpler pull request *statuses* become checks; branch-policy evaluations are a later
  refinement.
- **Timeline replay.** Labels, reviewers and the title import as state at creation time,
  not as history — the same gap the GitHub review bridge has.
- **`status.reason`.** A pull request has no close reason.

### Incremental pull

Azure DevOps has no "updated since" filter for pull requests — `searchCriteria` takes only
Created and Closed time ranges. So `--since` and the watermark drop *closed* pull requests
older than the cutoff before their threads are fetched, but an active one is re-read in
full every run. The active set is small, so this costs little; a first `--all` import is
the expensive case and `--since` bounds it.

### Push

`git review push ado:<repo>` writes local changes back, through the same
plan/confirm/apply/journal sequence the GitHub review push uses
(`cmd/git-review/push.go` `runBridgePush`). The three-way comparison is
`review.Diff`, unchanged and platform-free; `internal/bridge/ado/review/push.go`
is only the two ends — reading the pull request's current state by replaying the
importer against it, and mapping a delta onto pull request mutations
(`internal/bridge/ado/api/pullmutations.go`).

| Review change | Azure DevOps |
| --- | --- |
| `title`, `description` | `PATCH .../pullrequests/<n>` |
| `base` | `PATCH` `targetRefName` |
| `draft` | `PATCH` `isDraft` |
| `status` open ↔ `closed`/`Abandoned` | `PATCH` `status` `active` / `abandoned` |
| `label` add / remove | `POST` / `DELETE .../labels` — Azure DevOps creates an unknown label, unlike GitHub |
| a plain comment | a new thread with no `threadContext` |
| an anchored comment | a new thread carrying `threadContext` for the line range and side |
| a reply | a comment appended to the thread, **flattened** — Azure DevOps' own model, per "Comments do not thread" above |
| `comment.edit` / `comment.remove` | `PATCH` / `DELETE .../threads/<t>/comments/<c>` |
| `comment.resolve` / reopen | `PATCH .../threads/<t>` `status` `closed` / `active` |
| `verdict.add` `approve` / `request-changes` | `PUT .../reviewers/<self>` `vote` `10` / `-10` |
| a verdict's message | its own discussion thread — a vote carries no body |
| `verdict.remove` | `PUT .../reviewers/<self>` `vote` `0` |

A locally opened review is filed with `POST .../pullrequests` once its head
branch is on the remote; the branch push stays the caller's `git push`, and a
review with no `head` is refused with a message saying so.

**What is reported unsent** rather than guessed at:

- **`status` `merged` / `Completed`** — a completion is the server's to perform.
- **`status.reason`** — a pull request has no close reason (unimported too).
- **`milestone`** — a pull request has no milestone.
- **reviewer (`assignee`) changes** — resolving a name to an Azure DevOps
  identity id is a `vssps` host round-trip this bridge does not make yet.
- **`closes` links** — Azure DevOps ties a pull request to a work item with a
  `vstfs` artifact link, a different mechanism from the work-item relation
  patches the issue bridge writes. Deferred; reported per link.

**Verdict convergence.** Azure DevOps keeps no vote object and no vote history, so
a pushed verdict is recorded on the origin ledger against the key a faithful
re-import derives from it — `<pr origin>:vote:<self author>`, the `self` identity
coming from `connectionData`. The re-import claims it there instead of the push
casting it again every run. If the `connectionData` account spelling and the
reviewer's `uniqueName` diverge on some install, the worst case is the idempotent
`PUT` running once per push rather than a lost or duplicated verdict.

## Authentication

In order, stopping at the first that answers:

1. `--token`
2. `AZURE_DEVOPS_EXT_PAT`, `AZURE_DEVOPS_PAT`, `SYSTEM_ACCESSTOKEN`
3. `az account get-access-token --resource 499b84ac-1321-427f-aa17-267ca6975798`
4. `git credential fill` for the host

`az` runs before git's credential helpers for the same reason `gh` does: the helpers can
fall back to an interactive prompt, and a CLI that is installed and logged in should answer
first. An ADO clone URL authenticates with a PAT as the password, so a repository already
cloned over https usually needs nothing configured at all.

A PAT is sent as `Authorization: Basic base64(":" + pat)`; an Entra access token, which is
what `az` returns, is sent as `Bearer`. The two are told apart by shape — an access token is
a JWT, three base64 segments separated by dots, and a PAT never is.

On-prem installs that accept only NTLM or Kerberos are **out of scope**. The failure says so
and points at a PAT rather than retrying.

## Rate limits

Azure DevOps bills throughput in TSTUs and answers a saturated account with `Retry-After`,
alongside `X-RateLimit-Remaining` and `X-RateLimit-Reset`. Those are waited out rather than
failed on, capped so that a pathological reset time cannot hang a pull indefinitely.

Two request shapes carry most of a run's cost and are batched accordingly: work items are
fetched 200 at a time through `workitemsbatch`, which is that endpoint's hard limit, and the
initial WIQL query returns ids only, so the field data is never fetched twice.

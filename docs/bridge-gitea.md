# Bridge: Gitea

Mapping between Gitea issues and the [issue entity](issues.md). Generic bridge
mechanics — deriving nonces so re-import converges, one commit per upstream
event — are in [storage-model.md](storage-model.md).

This document exists because the tracker's purpose is portability: an issue must
survive the platform it came from. Nothing Gitea-specific belongs in
[issues.md](issues.md), and nothing here may be required to read an issue. A
client that has never heard of Gitea folds a Gitea-bridged issue into complete
state.

One bridge covers **Gitea**, **Forgejo** and **Codeberg** alike: the parts of
the REST API an issue import touches are the same across all three. The mapping
is closest to the [GitHub bridge](bridge-github.md) — a repo-local issue number,
a body with no history, comments with stable ids, and a timeline of typed
entries — and this document only spells out where Gitea differs.

## The target

```
gitea:<location>
```

There is no `#` scope: Gitea issues are a flat per-repository resource.

| Written | Means |
| --- | --- |
| `gitea:` | the current branch's remote, else `origin` |
| `gitea:origin` | that remote |
| `gitea:https://gitea.example.com/acme/proj` | an explicit URL, no git remote needed |

**A bare `gitea:acme/proj` slug is refused.** GitHub is always `github.com` and
Azure DevOps Services is always `dev.azure.com`, so an owner/name slug resolves
against a known host. Gitea is self-hosted on an arbitrary hostname and has no
canonical one, so there is nothing for a slug to resolve against — name a git
remote, or a full URL.

A subpath-hosted install (`https://example.com/gitea/acme/proj`) is supported:
the last two path segments are the owner and the repository, and whatever
precedes them is part of the base URL, the same rule the Azure DevOps bridge
uses for a virtual directory.

### Recognising a Gitea remote

A fully bare `git issue pull` has no scheme to go on and decides for itself
whether the resolved remote is a tracker's. GitHub and Azure DevOps have
definitive URL markers; Gitea does not. Two things stand in:

- Hostnames containing `gitea` or `forgejo`, and the two large public instances
  `codeberg.org` and `gitea.com`, are recognised outright.
- Any other http(s) remote that is not GitHub's or Azure DevOps' is probed once
  with `GET <base>/api/v1/version`. A Gitea host answers with a version; nothing
  else does.

Naming `gitea:` explicitly skips all of this.

## Identity, and where it is recorded

This bridge writes **no namespaced operations**. An issue's correspondence with
its Gitea counterpart is recorded on the origin ledger
(`refs/git-issue/origins`), under the tracker's own name:

```
issue    4f2a1c9…  gitea:gitea.example.com/acme/proj#42
url      4f2a1c9…  https://gitea.example.com/acme/proj/issues/42
comment  4f2a1c9…  8b31e07…  gitea:gitea.example.com/acme/proj#42/comments/7
```

The identity string is `gitea:<host>/<owner>/<repo>#<number>`, and the create
event's nonce is the first 16 hex characters of its `SHA-256`. Other imported
events derive `n` from their own upstream id — the comment id, or
`<identity>/timeline/<entry id>` — and reconciled current-state events from
`SHA-256("<identity>:<op>")` (or `":<op>:<value>"` for a list member), the same
three rules the [GitHub bridge](bridge-github.md) lists.

**The number is the identity.** Gitea's REST API also exposes a global `id`, but
nothing addresses an issue by it — every endpoint takes the repo-local
`{index}` — so keying identity on the global id would mean it could never be
re-fetched. An issue's number is stable for its whole life in one repository. An
issue **moved to another repository** is renumbered by Gitea and imports there
as a new entity; this is a rare operation that already discards the old number,
and is left as documented behaviour rather than worked around.

## Author identity

`a` is `gitea:<login>`, and it is hashed into every event, so the spelling is
part of the specification. A deleted account is Gitea's `ghost`.

The **commit byline** is `<login> <login@host>`. Gitea often exposes no usable
email, so the address is synthesised — enough for `git shortlog`, `git log
--author` and a `.mailmap` to work on the tracker's history. It is a
bridge-asserted label, not proof of anything; the authoritative record of who
wrote an event is the `a` field inside it.

Nothing about a pushed change is attributed to its local author: Gitea stamps a
created issue or comment with the token's user and the current time. This is the
same limitation the other two bridges have.

## Field mapping

| Issue field | Gitea | Notes |
| --- | --- | --- |
| `title` | issue title | Full rename history; the earliest `change_title` names the original. |
| `description` | issue body | Import-once, like GitHub — Gitea keeps no body history. |
| `status` | `open` / `closed` | Open is implied and never written as an event. No terminal `status.reason`: Gitea issues carry none. |
| `milestone` | milestone title | Resolved to an id on write. A milestone the repo does not have is **reported and dropped**, like an unknown label; detaching a milestone is always writable. |
| `label` (list) | labels | Resolved to an id — the repository's own labels and its organization's — on write. A label neither namespace has is **reported and dropped**, not an error: creating one would give it a colour and description nobody chose. The rest of the push proceeds. |
| `assignee` (list) | assignees | A list on both sides. Gitea replaces the whole assignee list on write, so a push reads the current one first. An assignee this repository cannot assign — someone from the source tracker who is not a member here — is **reported and dropped**, checked against `GET /repos/{o}/{r}/assignees`; Gitea would otherwise reject the whole write with a 422. A *removal* is never dropped: taking a since-departed collaborator off an issue has to go through. |
| `locked`, `pinned` | lock / pin timeline entries | Imported for display; not pushable. |
| `type` | — | Gitea has no issue types. A type change is reported unsent. |

### Links: `blocked-by` only

Gitea's issue-dependencies API is the one cross-issue relationship this bridge
can write. `GET /issues/{index}/dependencies` lists the issues that block one,
which maps to `blocked-by`; `POST` / `DELETE` on the same path adds and removes.
The write body names the blocking issue as `{index, owner, repo}`, all three
always sent: Gitea compares the `owner`/`repo` against the issue in the path and
falls through to a cross-repository lookup on any mismatch — an omitted field
included — which then resolves the empty repository to `404 repository does not
exist [id: 0, ...]`. The repo name goes under `repo`, not `name`: Gitea binds
that field as `json:"repo"` and ignores `name`.

The other kinds are declared unsupported and reported rather than half-attempted:

- **`parent`** — Gitea has no issue hierarchy.
- **`duplicate-of`** — Gitea has no duplicate marking.
- **`related`** — not a Gitea link.
- Any kind another bridge carried in — no mutation to guess at.

A `blocked-by` whose target has never been pushed to this repository is
reported too, with the fix named: push the target first.

Dependencies can be disabled per repository by an administrator. When they are,
the dependency feed comes back empty and a write fails with the server's own
error.

**The link direction is not read from the timeline.** Gitea writes an identical
`add_dependency` entry to *both* issues of a pair, so the timeline cannot say
which end is blocked. The direction is taken from the current dependency list
instead, reconciled at creation time without attribution or removal history —
the same treatment GitHub's `duplicateOf` gets.

### Comments

Comments import from the comments feed, each with a stable id. Gitea exposes no
edit history for a comment through the API, so an edited comment imports as its
current body — the same as an issue body, and the same as the GitHub bridge.

The timeline's own `comment` entries are ignored; the comments feed is
authoritative.

## Write-back

`Plan` first reads `GET /repos/{owner}/{repo}` — a repository that is missing,
private to the token, or has its issue tracker switched off is one clear error
before anything is filed, rather than a per-issue 404 with some issues already
created. An all-creates push makes no other read, so without this check the
first failure would come from the middle of the run.

It then reads each **mapped** issue with its feeds (the same worker pool as a
pull, and the same skips — see "Speed"), replays the importer against it, folds
the result, and diffs it against the local blob exactly as
[storage-model.md](storage-model.md) describes. A conflicted scalar skips its
whole issue.

**Where the issue list is the cheaper way to those issues, it is used instead of
a request each** — see "Speed". The entries it returns carry every field a
per-issue read does that the importer looks at, so which one a plan came through
is invisible in its result.

Each issue is diffed and discarded as it arrives, not collected first: a mirror
maps thousands of issues, and holding every one with its comment and timeline
feeds in memory to compare them was enough to get the process OOM-killed on a
small host. The plan itself — one delta, skip or conflict per issue — is all
that is kept, and it is assembled in candidate order regardless of which feed
finished first.

`Apply` sends, per issue:

1. `PATCH /issues/{index}` — title, body, state, milestone, and the whole
   assignee list in one call.
2. `POST` / `DELETE /issues/{index}/labels[/{id}]` — label membership.
3. `POST` / `DELETE /issues/{index}/dependencies` — `blocked-by` links.
4. `POST` / `PATCH` / `DELETE` on comments.

A create files everything the issue is born with — title, body, labels,
milestone, assignees, closed state — in the one `POST /issues` call, so no
timeline entries are raised for changes that never happened. Deleting a comment
is the one irreversible thing a push does, and is reached only from a local
`comment.remove` naming an entry this repository posted.

### Conflicts

Gitea has no optimistic-concurrency token on an issue, so a push cannot refuse a
concurrent upstream edit the way the Azure DevOps bridge's `test` on `/rev`
does. The three-way diff in `Plan` is the whole of the protection: a field both
sides moved is reported as a conflict and its issue is left untouched.

## Authentication

A token is taken from `--token`, then `GITEA_TOKEN` or `FORGEJO_TOKEN`, then
git's credential helpers for the host. It is sent as `Authorization: token
<token>`. Nothing here stores a token; a credential that works is handed back to
git's helper to record, the same contract `git credential fill` starts.

## Speed

Gitea has no endpoint that reads a set of issues in full, so a large import is
thousands of small requests — one page of issues, then up to three per issue for
comments, timeline and dependencies. Three things keep that from taking minutes:

- The per-issue feeds are fetched by a **pool of workers** (twelve by default),
  not one at a time.
- An issue whose `updated_at` still equals its `created_at` **and** whose
  comment count is zero has no history to fetch, so all three calls are skipped.
  One whose comment count is zero skips the comments call regardless. Current
  state — labels, assignees, milestone, closed — is still reconstructed by the
  importer's reconcile pass from what the list response already carried.
- The first `dependencies` call that comes back `404`/`403` (the feature is off
  for this repository) sets a flag, and the rest of the import does not ask
  again.

A push's `Apply` reads the issue it is about to edit **without** its feeds
(`GET /issues/{index}` alone), since it needs only the scalar fields and the
current assignee list.

### The list is the batch endpoint a push wants

A `Plan` against a mirror reads every mapped issue, and every locally created
issue stays mapped and a candidate for the rest of its life — so this is the
cost of a push where nothing changed, not of a rare one. Gitea has no endpoint
that takes a set of issue numbers, but the paged list is a batch read by another
name: fifty issues per request, carrying the same fields the per-issue read
does.

Confirmed empirically against a 1002-issue repository, all of them mapped and
none of them changed:

| Reading the mapped issues | Requests | Time |
| --- | --- | --- |
| One `GET /issues/{index}` each, twelve workers | 1002 | 12.6s |
| The paged list, hydrating what has history | 21 | 0.9s |

The cost is per request and on the server rather than on the wire — an
authenticated request measured ~3.7× an anonymous one on the same host, and
raising the pool from twelve workers to thirty-two made the read *slower*
(21s). So fewer requests is the only lever, and the list is a 14× one.

It is not always cheaper. The list cannot be narrowed to a set of numbers, so it
costs a page per fifty issues **in the repository** whether they were asked for
or not; `push <id>` against a large tracker would pay for all of them. The first
page carries `X-Total-Count`, so the size that decides it is read from the
server rather than guessed: the list is used when it takes no more requests than
it replaces, and that first page is the first page of the walk when it wins. A
server that sends no total, or a list that fails, falls back to a request per
issue.

Pages are hydrated and handed on one at a time, so the OOM this streaming exists
to prevent stays prevented: a page's worth of feeds is live at once, not a
mirror's.

### Not every endpoint paginates

`GET /issues/{index}/comments` **ignores `page` and `limit`** and answers every
request with the entire thread — confirmed empirically against Gitea 1.27.3, where
three requests for successive pages of a 392-comment issue each returned the
same 392 comments, with `X-Total-Count: 392` and no `Link` header. The issue
list, the timeline and the label feeds do paginate.

So a walk cannot end on a short page alone. It ends on any of three:

- a page shorter than the limit that was asked for — the ordinary last page;
- a page **longer** than that limit, which can only mean the server ignored it
  and sent the whole collection;
- having reached the `X-Total-Count` the server itself reported.

Ending on the first test alone is an unbounded read of a bounded thread: the
walk asks for page 2, 3, 4 … and appends the same comments every time, until the
process is killed. Any issue with at least a page of comments was unreadable
that way, on a pull as much as on a push.

## Rate limits

Gitea's rate limit, where one is configured, answers with `429` and a
`Retry-After`; a `5xx` is retried on a short backoff. Both are bounded. An
import resumes from the newest `updated_at` it has seen, so a repeat pull asks
only for what changed.

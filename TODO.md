# TODO

## Coverage matrix

Where each part of the issue model is actually reachable. Every row is a real
field or operation in the blob (docs/issues.md, docs/blob-format.md); the
columns are the surfaces that can read or write it.

| | read (`show`/`list`) | write (`add`/`edit`) | git peer | GitHub pull | GitHub push | ADO pull | ADO push |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `title` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `description` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `status` (open/closed) | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `status.reason` | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ | ✗ |
| `type` | ✓ | ✓ | ✓ | ✓ | ✗ | ✓ | ⚠ create only |
| `milestone` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `label` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `assignee` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ⚠ first only |
| `locked` / `lock.reason` | ✓ | ✗ | ✓ | ✓ | ✗ | ✗ | ✗ |
| `pinned` | ✗ | ✗ | ✓ | ✓ | ✗ | ✗ | ✗ |
| `draft` | ✗ | ✗ | ✓ | ✗ | ✗ | ✗ | ✗ |
| rel `parent` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| rel `blocked-by` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| rel `duplicate-of` | ✓ | ✓ | ✓ | ✓ | ✗ | ✓ | ✓ |
| rel `related` | ✓ | ✓ | ✓ | ✗ | ✗ | ✓ | ✓ |
| rel unknown / namespaced kind | ✓ | ✓ | ✓ | – | ✗ | – | ✗ |
| `rel.note` | ✓ | ✗ | ✓ | ✗ | ✗ | ✗ | ✗ |
| `comment` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `comment.edit` | ✓ | ✓ | ✓ | ✗ | ✓ | ✓ | ✓ |
| `comment.remove` | ✓ | ✓ | ✓ | ✗ | ✓ | ✓ | ✓ |
| comment reply (`ref` parent) | ✓ | ✗ | ✓ | ✓ | ⚠ flattened | ✗ | ⚠ flattened |
| `react` / `react.remove` | ✓ | ✗ | ✓ | ✗ | ✗ | ✗ | ✗ |

✓ supported · ✗ not · ⚠ partial · – not applicable (the kind cannot originate on that platform)

What the matrix says, in prose:

- **A git-to-git peer loses nothing.** Every blob field syncs with full fidelity
  because it is an ordinary notes merge. The only non-convergent operation is
  `remove`, which unlists an issue by deleting a tree entry rather than writing a
  tombstone event — a concurrent edit anywhere resurrects it. The origin ledger
  syncs separately, on `refs/git-issue/origins`.
- **The local write surface is still thin.** `locked`, `lock.reason`, `pinned`
  and `draft` have no `add`/`edit` path — they can only arrive from a bridge.
  `status` and `status.reason` now have one: `git issue close` / `reopen`, and
  `edit --status` / `--status-reason` for the raw values.
- **`draft` is vocabulary-only.** Nothing reads, writes, imports or pushes it.
  `pinned` imports from the GitHub timeline but is never rendered or pushed.
- **`type` cannot be written back.** It round-trips through git and imports from
  both platforms, but GitHub push drops it (needs a repo-level type id) and ADO
  push only sets it when creating a work item.
- **GitHub import covers `comment` only** — edits, removals and reactions are
  dropped. ADO import covers edits and removals.
- **docs/bridge-github.md still lists `draft`, `pinned` and reactions as
  imported.** Only `pinned` is. Reconcile the doc with the code.

## Open work

### Local write surface

- [ ] a write command for the remaining fields that only make sense once an
      issue exists: `locked` / `lock.reason`, `pinned`, `draft`. `status` is
      done — `close` / `reopen` verbs, plus `edit --status` / `--status-reason`
      for the raw values a bridge round-trips.
- [ ] `add --closed` / `add --status` — filing an already-resolved issue is a
      one-liner in the model (`Create` writes the `status` event) but has no
      flag; deferred with the rest of `add`'s status story.
- [ ] `rel.note` — a link can carry why it is there and nothing writes one.
      Wants a spelling first: a flag on `--rel`, or its own verb.
- [ ] `react` / `react.remove` on a comment or an issue, once there is a
      vocabulary for picking a reaction on the command line. Reads already
      render.
- [ ] `--no-rel related:<id>` on the end that holds no member of its own says
      "nothing changed", because the link it displays is the far end's and
      docs/issues.md lets only the storing end retract one. Defensible, but a
      reader who sees `Related:` on this issue expects to drop it. Wants either a
      message pointing at the other end, or a whole-ref scan to find who holds
      it.
- [ ] decide whether `pinned` / `draft` are display-only or worth rendering and
      wiring end to end. Right now they are imported and invisible.

### Rendering and output

- [ ] author column on `list` — blocked on the identity map, since the same
      person is `github:login` upstream and an email locally.
- [ ] honour git's own `color.ui`; only `NO_COLOR`, `TERM` and isatty are read
      now.
- [ ] relative dates (`3 days ago`) in `--format medium`?
- [ ] `list` / `show` JSON output.
- [ ] markdown rendering in `show`.
- [ ] a `log --format` of our own for the cases git log's shapes do not cover?
- [ ] `git issue blame <id>` over the event lines, once the log is used enough
      to know whether it is wanted.

### GitHub bridge

- [ ] import reactions and comment edits / removals — currently only the initial
      `comment` body is imported.
- [ ] import `draft` (only `pinned` is imported today).
- [ ] push `type` — needs a repository issue-type id, and the read path already
      degrades around servers that reject the field.
- [ ] push reactions and comment edits — blocked on importing them, since a push
      compares against a fresh import.
- [ ] `duplicate-of` has no push path: GitHub offers `unmarkIssueAsDuplicate`
      and no `markIssueAsDuplicate`, so a link can be undone through the API and
      never made. Reported per link rather than attempted. ADO writes it fine.
- [ ] cross-repo sub-issue targets — GitHub supports them, the model has no
      representation, so such a link neither imports nor pushes.

### Azure DevOps bridge

- [ ] `status.reason` — neither imported (only `System.State` is read) nor
      pushed.
- [ ] reactions on comments, attachments, and the scheduling fields (priority,
      severity, story points) — no core operation, unimported.
- [ ] comment replies are flattened; locally authored replies nest but a push
      posts them flat.
- [ ] an exact-node area form, if the parent-team case ever comes up. Today
      `#Web` always means the subtree.

### New bridges

- [x] **Gitea bridge** — `internal/bridge/gitea/api` + `internal/bridge/gitea/issue`, `git
      issue pull|push gitea:<remote>`. Issues, comments, timeline history,
      labels, assignees, milestone, open/closed, and `blocked-by` via the
      issue-dependencies API. Covers Forgejo and Codeberg unchanged. No issue
      types, no `parent`/`duplicate-of` (Gitea has no API); those are reported
      unsent. Identity is the repo-local number. Stateful fake in
      `internal/bridge/gitea/issue/fake_test.go`; `docs/bridge-gitea.md` is the spec.
- [ ] GitLab bridge (`internal/bridge/gitlab/{api,issue}`).

### Cross-cutting

- [ ] place closed issues on `refs/notes/issues/archived` — the ref exists and
      is documented; nothing moves entities onto it yet.
- [ ] identity map: the same person is `github:login` upstream and an email
      locally, so `--author` sees two people. Belongs in the origin ledger — a
      `user <local-email> <upstream-login>` line needs no format change, since
      unknown kinds are already preserved.
- [ ] a canonical open/closed mapping across bridges. `issue.Statuses` now
      classifies the ADO process states client-side (Proposed/InProgress/
      Resolved non-terminal, Completed/Removed terminal), so `Done` and
      `Removed` answer to `--state closed` and dot purple. That is a client's
      best-effort list of states it happens to know, not a mapping: *where the
      mapping lives*, so two bridges agree without either one's vocabulary
      leaking into the other's, is still the open question. A customised process
      with a novel state name still reads as open until added to the list.
      Deferred until a third bridge shows its shape.
- [ ] `pull` flag to refresh issues already held. `ghapi.FetchNodes` reads
      exactly the ids the origin ledger holds, so an open-only import's staleness
      window is closable without a search — push uses it, pull does not offer it.
- [ ] cross-repo references, now confined to one field: a relation's `ref`. An
      anchor SHA has no meaning across repositories. Candidate spellings: a
      qualified `ref` naming a repository alongside the id, or a namespaced
      companion event annotating the `rel.add` with an upstream URL. Neither
      chosen.
- [ ] author as a git-style name rather than a bare email?

## git-review

The second entity type, `internal/review` + `cmd/git-review`, specified in
`docs/reviews.md`. The local surface and the GitHub bridge are both written;
what is left is listed below.

- [x] **Azure DevOps import** — `internal/bridge/ado/api/pulls.go` +
      `internal/bridge/ado/review`, `git review pull ado:<remote>`. Fields,
      labels, reviewers as reviewers, conversation and review-thread comments
      with their anchors (against the iteration commit) and resolution, votes as
      verdicts against the head, `closes` links to work items via the ledger,
      and pull request statuses as check runs onto the checks ref. Mirrors the
      GitHub import; `docs/bridge-ado.md` § "Pull requests" is the spec.
- [x] **Azure DevOps push** — `internal/bridge/ado/api/pullmutations.go` +
      `internal/bridge/ado/review/push.go`, `git review push ado:<repo>`. Fields,
      labels, comments in all three shapes, review threads with their anchors and
      resolution, votes as verdicts (with the message posted as its own thread),
      and `POST` for a locally opened review. The push pipeline types moved from
      `ghreview` to `internal/review` (`Candidate`/`Plan`/`Result`/`Ledger`) and
      `cmd/git-review` drives both bridges through one `reviewPusher`. Stateful
      fake in `internal/bridge/ado/review/fake_test.go`; `docs/bridge-ado.md`
      § "Push" is the spec. Still deferred: reviewer changes (needs `vssps`
      identity resolution), `closes` artifact links, `milestone`.
- [x] **GitHub import** — `internal/bridge/github/api/pulls.go` + `internal/bridge/github/review`.
      Fields, labels, review requests as reviewers, comments, review threads
      with their anchors and resolution, verdicts with the revision they were
      cast against, `closes` links, and check runs onto the checks ref.
- [x] **GitHub push** — `internal/review/delta.go` (the review-shaped three-way
      comparison, no platform in it) + `internal/bridge/github/api/pullmutations.go` +
      `internal/bridge/github/review/push.go`. Fields, labels, review requests,
      comments in all three upstream shapes, review threads with their anchors,
      thread resolution, verdicts with the revision they were cast against, and
      dismissals. `internal/bridge` was left alone: it is typed on `issue.Delta`,
      and `ghreview` carries its own `Candidate`/`Plan`/`Result` so the issue
      vocabulary stays off `cmd/git-review`'s import graph.
- [ ] **what a pull request cannot take** is reported per field rather than
      dropped, and each has a different reason. None of these is a gap a later
      version closes; they are what GitHub does not offer:
      - `closes` — GitHub derives closing links from keywords in the body, so
        writing one would mean editing somebody's prose. `parent` and
        `blocked-by` are issue-only mutations; `related` is not a GitHub link.
      - `status.reason` — a pull request has no close reason.
      - `status: merged` — a merge is performed upstream. See `merge`, below.
      - `head` / `head.sha` — the tracker learns these from the git remote, and
        a bridge that wrote them would claim to have moved code it never sent.
- [ ] a review-thread reply whose root this clone has no mapping for is refused
      rather than posted at the top level, where it would be detached from what
      it answers. It becomes pushable once the root is.
- [ ] review requests are stated as a whole set, because `requestReviews`
      replaces and there is no remove mutation. A reviewer name that resolves to
      no GitHub user — a team slug looks exactly like a login — is left out and
      reported rather than failing the push. Teams are not resolved.
- [ ] **import gaps** — no timeline replay, so a review's labels, reviewers,
      milestone and title import as current state at creation time rather than
      as history. Issues get add/remove pairs from `timelineItems`; a pull
      request could, and does not yet.
- [ ] **connection paging** — comments and review threads page; reviews, review
      thread comments, labels and review requests take one page of 100 and
      would silently truncate past it. Raising any of those is not free: GitHub
      caps a query at 500,000 *possible* nodes, counted from the `first:` values
      in the query text, and `reviewThreads → comments` is nested two deep. The
      pull request connection is at 25 per page to buy room for it, and
      `TestQueriesFitTheNodeLimit` measures the whole budget.
- [ ] only the head commit's checks are read. Earlier commits have their own
      and are not imported.
- [ ] **`push --with-head`** — a bridge cannot open a PR for a head branch the
      remote has never heard of, so a locally created review either refuses or
      pushes the branch first. Refusing is the default; the flag is unwritten.
- [ ] **`merge`** — a bridge verb that asks the forge to merge and lets the
      next pull bring back the real status. `merged` is never written locally.
- [ ] **cross-repo `closes`** — the common case for this type, and the same
      unsolved question as anywhere else: an id means nothing in another
      repository's object store.
- [ ] `list --format medium` renders check runs but not verdicts.
- [ ] `checkout` reaches a fork's head through GitHub's `refs/pull/<n>/head`,
      taking the number from the URL the origin ledger recorded at import. A
      review that arrived without that line — pulled from a peer's notes ref
      rather than from the forge — has no number and no third route, and no
      other forge's spelling of the ref (GitLab's `refs/merge-requests/<n>/head`)
      is tried.
- [ ] no `react` writer, same as issues.
- [ ] `git issue destroy` clears state the reviews still need. It removes the
      whole origin ledger and every import watermark, both of which are shared:
      the reviews lose their mappings, so the next `git review push` re-creates
      every pull request upstream as a second copy. `git review destroy` forgets
      only its own share (`Ledger.Forget`, `SyncState.Forget`); the issue side
      should do the same rather than the reverse.
- [ ] check runs are never pushed. They are facts a CI system reports about a
      commit, and this client is not that system; the ref rides along with the
      git leg in both directions.

## Design notes, not yet scheduled

- **Reactions** — the model has `react` / `react.remove` and `show` renders
  them; only the CLI writer and the bridge coverage are missing (above). A
  reaction targets an event id, so per-comment reactions already work
  structurally.
- **Personal / non-shared state** (watching, notification prefs, read/unread) —
  should *not* go in the shared notes. "Am I subscribed" is not collaborative
  state, and forcing it in makes every watch/unwatch tracker-wide noise. Better
  as a local unpushed ref, or outside git as a client setting. Worth deciding
  explicitly which concepts are shared versus personal.
- **Kanban boards / custom fields** (GitHub Projects–style) — out of scope for
  now. User-defined fields, saved views and cross-repo aggregation need a more
  general extensible-schema mechanism than "one more event type".

## UI
- keep git-issues lean and ui projects separate
- gh dash clone
- nvim status line integration
- bash completions

## Done

### add

- editor support — opens when there is no title, as `git commit` does without
  `-m`; `--edit` forces it, and anything given on the command line is filled in
  first.
- `-t` / `--title`, `-d` / `--description`, `--type`, `-l` / `--label`,
  `-a` / `--assignee`, `-m` / `--milestone`.
- `add <sha>` files the new issue under that one — a positional id is "what this
  command hangs off", the same slot `list <sha>` uses.
- `add --rel <kind>:<sha>` — any link an issue is born with; the positional is
  the same field said the way this command says it.

### edit

- field flags, so a change needs no editor: `-t`, `-d`, `--type`, `-l`, `-a`,
  `--milestone`, `--parent`, `--rel` / `--no-rel`, plus `--remove-label` /
  `--remove-assignee` for taking one member off a list. No flag opens the
  editor; `-e` opens it anyway, prefilled. Every change is written as one event
  per field that actually changed.
- `-m` stays the comment's message here and the milestone is spelled in full —
  the collision `comment`'s own verb exists to avoid.
- `none` clears any field with a cleared state, the same word the filters use
  for "has none". `--no-rel <kind>` drops every link of a kind.
- `--no-rel` on a symmetric kind lets go at both ends, and the command reports
  the other issue it wrote to. `related` may be written by either end, so two
  clones — or an ADO pull, since ADO stores `Related` on both work items — leave
  two members standing for one link.

### close / reopen

- `close` / `reopen` verbs, sugar over the one `status` scalar `edit --status`
  writes. Bare `close` is one event, no reason. Idempotent — a `close` of a
  closed issue prints `nothing changed`.
- `close --as completed|not-planned|duplicate` writes `status.reason`; the
  hyphen normalises to the blob's `not_planned`, and an unknown reason is
  refused rather than written.
- `close <id> --as duplicate <of-id>` also writes the `duplicate-of` link, in
  one action.
- `reopen` leaves `status.reason` and any `duplicate-of` link alone — the stale
  pairing docs/issues.md accepts.
- `edit --status <value>` / `--status-reason <value>` for the raw values a
  bridge round-trips; `--status` has no `none` (an issue with no event is open).
- `issue.Statuses` classifies the ADO process states so `Done` / `Removed` are
  terminal; `show` no longer prints `Reason:` for a non-terminal status.

### list

- output aligned with git: compact sha, state, type, title, labels, comment
  count. `--format medium` for a `git log`-shaped block per issue.
- status shown as a green/purple dot on a terminal; a wrapping title is
  shortened without dropping metadata.
- `--state`, `--type`, `--label`, `--author`, `--assignee` filters.
- nesting under each issue's parent is the default on a terminal and off down a
  pipe; `--tree` / `--no-tree` force it either way. The spine sits in the title
  column so the id/status/type grid survives; a root whose parent is not in the
  listing is marked `↑`.
- `list <sha>` — the whole subtree filed under that one; `list none` for the
  ones filed under nothing. No `--parent` flag: the positional is the slot
  `add <sha>` uses.

### show

- `--edit` — the issue goes into the editor and what changed comes back as one
  event per field; list fields diff into `.add` / `.remove` pairs naming event
  ids.
- a second id shows one comment and the replies under it, by the same walk the
  full issue uses.
- `Parent:` resolved to a title, and a `Children:` block, so a link reads from
  both ends. `show` folds the whole ref for it.
- `--web` opens the issue's page on the tracker it came from, via
  `git web--browse`.

### comment

- `comment <id> [-m]` — its own verb, not `add <id>`.
- editor when there is no `-m`, as `git commit` opens one.
- `edit <id> <comment>` and `remove <id> <comment>` — one argument is the issue,
  two are a comment inside it.
- comments addressed by abbreviated event id, never by ordinal — a thread orders
  by `(c, id)`, so an ordinal from a stale listing edits the wrong text.
- `comment <id> -i <comment>` / `--in-reply-to <comment>` nests a reply under
  another entry; the `comment` op's `ref` and the fold's `Forest` already
  carried this, so the flag just resolves a prefix and passes it through.

### pull

- `--all` (open issues by default; `--all` includes closed), `--since`,
  `--limit`, `--token`.
- `git:origin` / `github:origin` / `ado:origin` / `gitea:origin` remote schemes;
  git-style fetch overview of changes.
- GitHub bridge: issues, comments, timeline history, idempotent re-import.
  Relations: `parent` and `blocked-by` from the timeline, `duplicate-of` from
  current state; `related` is not a GitHub link. A link whose target this clone
  does not hold is left unwritten, cross-repo included.
- Azure DevOps bridge (`internal/bridge/ado/issue` + `internal/bridge/ado/api`), Services and
  on-prem Server alike; area as scope only, states verbatim. All four relation
  kinds, from the revision feed and reconciled against the links a work item
  currently holds.
- Gitea bridge (`internal/bridge/gitea/issue` + `internal/bridge/gitea/api`), Gitea / Forgejo /
  Codeberg alike; open/closed states, `blocked-by` from the current dependency
  list. A bare `git issue pull` recognises a Gitea remote by hostname or a
  `/api/v1/version` probe.
- the last sync per remote is remembered, so `--since` defaults to it; `--full`
  overrides.

### push

- git-style A/M/D overview, confirmation, `--dry-run`, `-y`.
- refuses a non-fast-forward on the notes ref and the origin ledger rather than
  forcing.
- GitHub write-back: fields, labels, assignees, milestone, status, comments
  posted / edited / deleted, and `createIssue` for unlinked issues.
- the origin ledger on `refs/git-issue/origins` — one blob per tracker, so one
  issue can be linked to an upstream and a fork at once, and a tracker can be
  dropped without rewriting a single issue. A bridge push writes nothing to the
  notes ref; mappings are journalled and committed in one commit per run.
- relations to GitHub: `issue.Diff` compares links as an OR-Set of
  (kind, target) pairs; `parent` and `blocked-by` go through
  `addSubIssue` / `addBlockedBy`, a created issue carries its parent in
  `createIssue`, and a push applies each delta after the ones it links to, so a
  whole tree seeds in one run.
- relations to Azure DevOps: all four kinds, as JSON Patch operations on the
  work item's relations in the same document as the fields. Removals address a
  relation by array index, so they run descending with a `test` on `/rev`.
- relations to Gitea: `blocked-by` only, through the issue-dependencies API;
  `parent` / `duplicate-of` / `related` are reported unsent. Gitea has no
  optimistic-concurrency token, so the three-way diff is the whole conflict
  guard.

### log

- `git log` over the notes ref, with git's own flags passed through.
- descriptive commit messages: one commit per action, with the author and date
  of whoever did it. Local writes go through the same commit writer as the
  bridge.
- automatic repack after a bulk import — fast-import chains a note's blob
  versions oldest-first, so a listing replays each issue's whole history until
  `git repack -adf` re-picks the bases; plain `git gc` does not, and
  `gc.auto = 0` opts out.

### tweaks

- `show` / `list` re-added to `git issue --help`; `--help` on `rm`.
- `--version` only, no `version` subcommand.
- `show` opens the pager when the issue is too long.
- emoji mapping of bug / task / feature.

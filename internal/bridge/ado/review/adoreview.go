// Package adoreview maps Azure DevOps pull requests onto the review vocabulary.
//
// It is the only package that knows both what a review is and what Azure DevOps
// is. internal/bridge/ado/api below it knows Azure DevOps and nothing else;
// internal/review below that knows reviews and nothing else. It is a sibling of
// internal/bridge/ado/issue and internal/bridge/github/review rather than part of either —
// the two entity types are siblings, and so are their bridges — which keeps
// cmd/git-review clear of the issue vocabulary entirely.
//
// Everything here is a pure function of what Azure DevOps returned. That is a
// correctness requirement, not a style: an import that depended on local state
// would produce different bytes on a second run, and since an event's id is the
// hash of its own bytes, a re-import would duplicate the entity instead of
// converging on it.
package adoreview

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/review"
)

// Lookup answers the questions a response cannot: which upstream objects this
// repository already holds, and as what. It is review.Lookup — the same
// questions every review bridge asks the origin ledger.
type Lookup = review.Lookup

// CommentOrigin ties one local event to the upstream object it corresponds to.
type CommentOrigin = review.CommentOrigin

// Entity is one imported review: the id its create event hashes to, every
// action the import produced, and the mappings it establishes.
type Entity struct {
	ID      string
	Actions []entity.Action

	Origin string
	URL    string

	Comments []CommentOrigin
	// Threads ties each local thread-root event to the upstream thread it came
	// from, which is what a push needs in order to resolve one.
	Threads []CommentOrigin
	// Verdicts ties each local verdict event to the vote key it was imported
	// from, so a vote this clone pushed is claimed on re-import rather than
	// pushed again every run.
	Verdicts []CommentOrigin

	// Checks are the head commit's runs. They are not part of this entity and
	// never reach its blob: a check is a fact about a commit and lives on its
	// own ref, keyed by commit sha.
	Checks []review.CommitCheck

	// Unresolved is how many relation events this import could not write because
	// the ledger does not hold the entity they name.
	Unresolved int
}

// Events is every event the import produced, in the order the actions carry
// them.
func (e Entity) Events() []entity.Event {
	var out []entity.Event
	for _, a := range e.Actions {
		out = append(out, a.Events...)
	}
	return out
}

// Blob is the entity's events as a note body: one canonical line each.
func (e Entity) Blob() []byte {
	var b strings.Builder
	for _, ev := range e.Events() {
		b.Write(ev.Raw)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// Import maps one Azure DevOps pull request onto actions, each a group of events
// that one person did at one moment.
func Import(format entity.ObjectFormat, t adoapi.Target, pr adoapi.PullRequest, known Lookup) (Entity, error) {
	b := &builder{format: format, target: t, pr: pr, known: known, origin: t.PullRequestOrigin(pr.ID)}
	b.build()
	if b.err != nil {
		return Entity{}, b.err
	}
	return Entity{
		ID:         b.id,
		Actions:    b.actions(),
		Origin:     b.origin,
		URL:        b.webURL(),
		Comments:   b.comments,
		Threads:    b.threads,
		Verdicts:   b.verdictOrigins,
		Checks:     b.checks(),
		Unresolved: b.unresolved,
	}, nil
}

type group struct {
	at     time.Time
	who    adoapi.Identity
	seq    int
	events []entity.Event
}

type builder struct {
	format entity.ObjectFormat
	target adoapi.Target
	pr     adoapi.PullRequest
	known  Lookup
	origin string

	groups []*group
	cur    *group
	id     string
	err    error

	comments       []CommentOrigin
	threads        []CommentOrigin
	verdictOrigins []CommentOrigin
	unresolved     int
}

func (b *builder) begin(at time.Time, who adoapi.Identity) *group {
	g := &group{at: at, who: who, seq: len(b.groups)}
	b.groups = append(b.groups, g)
	b.cur = g
	return g
}

func (b *builder) build() {
	created := b.pr.CreatedAt
	author := b.pr.CreatedBy.EventAuthor()

	b.begin(created, b.pr.CreatedBy)

	// The create event first, and its id is the entity's. Its nonce is SHA-256
	// of the identity string exactly as the origin ledger spells it — the one
	// derivation that fixes the identity of every pull request ever imported by
	// any implementation.
	create := b.emit(created, author, "create", entity.Str(review.Type), "", b.origin)
	b.id = create.ID

	b.emitField(created, author, "title", entity.Str(b.pr.Title))
	if b.pr.Description != "" {
		b.emitField(created, author, "description", entity.Str(b.pr.Description))
	}
	if b.pr.TargetRef != "" {
		b.emitField(created, author, "base", entity.Str(b.pr.TargetRef))
	}
	if head := b.head(); head != "" {
		b.emitField(created, author, "head", entity.Str(head))
	}
	if b.pr.HeadCommit != "" {
		b.emitField(created, author, "head.sha", entity.Str(b.pr.HeadCommit))
	}
	// No `status` event for an active pull request: open is the implied status
	// of a review that has none, and writing it explicitly would tie on `c`
	// with a status reconciled from current state.
	if status := Status(b.pr.Status); status != review.StatusOpen {
		b.emitField(created, author, "status", entity.Str(status))
	}
	if b.pr.IsDraft {
		b.emitField(created, author, "draft", entity.Bool(true))
	}

	for _, l := range b.pr.Labels {
		b.emitList(created, author, "label.add", l)
	}
	// A reviewer is who was asked to read this, which is what the assignee list
	// means on a review — not who is responsible for landing it.
	for _, r := range b.pr.Reviewers {
		if who := r.Identity.EventAuthor(); who != "" {
			b.emitList(created, author, "assignee.add", who)
		}
	}

	b.closes(created, author)
	b.threadsAndComments()
	b.verdicts()
}

// head is the review's head branch, remote-qualified when it is a fork.
//
// The repository name rather than a remote name, because a remote name is local
// to one clone and this string is shared: `fork-repo/feature-x` says whose fork
// the branch is in, which is what a reader needs and what a fetch can be built
// from. Same rule as ghreview's head().
func (b *builder) head() string {
	if b.pr.SourceRef == "" {
		return ""
	}
	if b.pr.ForkRepo == "" || b.pr.ForkRepo == b.target.Repo {
		return b.pr.SourceRef
	}
	return b.pr.ForkRepo + "/" + b.pr.SourceRef
}

func (b *builder) webURL() string {
	if b.pr.URL != "" {
		return b.pr.URL
	}
	return b.target.PullRequestWebURL(b.pr.ID)
}

// closes writes the links to the work items Azure DevOps says this pull request
// resolves.
//
// A target this clone does not hold is left unwritten rather than guessed at,
// and counted: an id has no meaning until the entity it names is here, and a
// cross-repository one may never be. Re-import still converges — the blob is a
// grow-only set and the missing link is simply added when it becomes
// resolvable. Nothing here closes a work item; that is docs/reviews.md's rule.
func (b *builder) closes(at time.Time, author string) {
	for _, id := range b.pr.WorkItems {
		upstream := b.target.WorkItemOrigin(id)
		target, ok := b.entity(upstream)
		if !ok {
			b.unresolved++
			continue
		}
		b.emit(at, author, review.RelAdd, entity.Str(review.KindCloses), target, b.origin+":closes:"+strconv.Itoa(id))
	}
}

func (b *builder) entity(upstream string) (string, bool) {
	if b.known == nil || upstream == "" {
		return "", false
	}
	return b.known.Entity(upstream)
}

// threadsAndComments imports every conversation on the pull request.
//
// A thread's first importable comment becomes the root and the rest become
// replies under it, which mirrors what a thread is and makes resolution — which
// addresses the root — line up with Azure DevOps' own per-thread status. A
// thread with a file context also gets an anchor on its root; one without is the
// pull request's general discussion, and an unanchored render is still complete.
//
// Only a code thread's resolution is imported. A general-discussion thread is
// often auto-closed by the server on merge, which is not a reviewer saying "this
// is addressed" — the meaning `comment.resolve` carries.
func (b *builder) threadsAndComments() {
	for _, th := range b.pr.Threads {
		if th.IsDeleted {
			continue
		}
		anchored := th.FilePath != ""

		var root string
		first := true
		for _, c := range th.Comments {
			if !b.importable(c) {
				continue
			}
			upstream := b.target.PullRequestCommentOrigin(b.pr.ID, th.ID, c.ID)
			if b.known != nil && b.known.ClaimedComment(upstream) {
				if first {
					if id, ok := b.entity(upstream); ok {
						root = id
					}
				}
				first = false
				continue
			}

			parent := ""
			if !first {
				parent = root
			}
			by := c.Author.EventAuthor()
			b.begin(c.PublishedAt, c.Author)
			e := b.emit(c.PublishedAt, by, "comment", entity.Str(c.Content), parent, upstream)
			b.comments = append(b.comments, CommentOrigin{EventID: e.ID, Upstream: upstream})

			if first {
				root = e.ID
				threadUp := b.target.PullRequestThreadOrigin(b.pr.ID, th.ID)
				b.threads = append(b.threads, CommentOrigin{EventID: e.ID, Upstream: threadUp})
				if a, ok := anchorOf(b.pr, th); ok {
					b.emit(c.PublishedAt, by, review.AnchorOp, entity.Str(a.String()), e.ID, upstream+":anchor")
				}
			}
			first = false
		}

		// Resolution is current state — Azure DevOps raises no event this bridge
		// can replay for it — so it is written at the thread's own moment and
		// attributed to whoever opened it. Only when resolved: an active thread
		// needs no event saying so.
		if anchored && root != "" && resolvedStatus(th.Status) {
			at, who := b.threadOpened(th)
			b.begin(at, who)
			b.emit(at, who.EventAuthor(), review.ResolveOp, entity.Bool(true), root,
				b.target.PullRequestThreadOrigin(b.pr.ID, th.ID)+":resolved")
		}
	}
}

// threadOpened is when the thread's first importable comment was posted and by
// whom, which is what the resolution event is attributed to.
func (b *builder) threadOpened(th adoapi.PRThread) (time.Time, adoapi.Identity) {
	for _, c := range th.Comments {
		if b.importable(c) {
			return c.PublishedAt, c.Author
		}
	}
	return th.PublishedAt, b.pr.CreatedBy
}

// importable reports whether a comment is one a person wrote — Azure DevOps logs
// votes and ref updates as `system` comments in the same threads.
func (b *builder) importable(c adoapi.PRComment) bool {
	if c.IsDeleted || strings.TrimSpace(c.Content) == "" {
		return false
	}
	return c.CommentType == "" || strings.EqualFold(c.CommentType, "text")
}

// anchorOf builds the anchor for a review thread's root comment.
//
// The revision is the commit of the iteration the thread was left against, so
// the anchor names where the person was actually looking rather than wherever
// the branch has moved to since. Azure DevOps re-bases a thread's displayed
// position as later iterations arrive; importing that would silently rewrite
// what somebody said.
func anchorOf(pr adoapi.PullRequest, th adoapi.PRThread) (review.Anchor, bool) {
	path := strings.TrimPrefix(th.FilePath, "/")
	if path == "" {
		return review.Anchor{}, false
	}

	first, last := th.RightStart, th.RightEnd
	side := review.SideRight
	if first == 0 && th.LeftStart != 0 {
		first, last, side = th.LeftStart, th.LeftEnd, review.SideLeft
	}
	if first == 0 {
		return review.Anchor{}, false
	}
	if last < first {
		last = first
	}

	revision := pr.HeadCommit
	if sha, ok := pr.Iteration(th.IterationID); ok {
		revision = sha
	}
	if revision == "" {
		return review.Anchor{}, false
	}

	a := review.Anchor{Revision: revision, Path: path, First: first, Last: last}
	if side == review.SideLeft {
		a.Side = review.SideLeft
	}
	return a, true
}

// verdicts imports each reviewer's current vote as their position on the review.
//
// Azure DevOps keeps no vote history — only the vote as it stands now — so a
// verdict is written at the pull request's own last-activity moment against its
// current head, and there is no dismissal to replay.
func (b *builder) verdicts() {
	for _, r := range b.pr.Reviewers {
		value := Verdict(r.Vote)
		if value == "" {
			continue
		}
		author := r.Identity.EventAuthor()
		upstream := b.origin + ":vote:" + author
		if b.known != nil {
			if _, ok := b.known.ClaimedVerdict(upstream); ok {
				continue
			}
		}
		at := b.pr.UpdatedAt
		if at.IsZero() {
			at = b.pr.CreatedAt
		}
		b.begin(at, r.Identity)
		e := b.emit(at, author, review.VerdictAdd, entity.Str(value), b.pr.HeadCommit, upstream)
		b.verdictOrigins = append(b.verdictOrigins, CommentOrigin{EventID: e.ID, Upstream: upstream})
	}
}

// checks turns the pull request's statuses into what the caller writes on the
// checks ref, keyed by the head commit they were posted against.
func (b *builder) checks() []review.CommitCheck {
	if b.pr.HeadCommit == "" {
		return nil
	}
	out := make([]review.CommitCheck, 0, len(b.pr.Statuses))
	for _, s := range b.pr.Statuses {
		name := s.Name
		if s.Genre != "" {
			name = s.Genre + "/" + s.Name
		}
		name = strings.Join(strings.Fields(name), "-")
		if name == "" {
			continue
		}
		at := s.CreatedAt
		if at.IsZero() {
			at = b.pr.UpdatedAt
		}
		out = append(out, review.CommitCheck{
			Commit: b.pr.HeadCommit,
			At:     at,
			Author: b.pr.CreatedBy.EventAuthor(),
			Run: review.Check{
				Name:       name,
				Conclusion: Conclusion(s.State),
				URL:        s.TargetURL,
			},
		})
	}
	return out
}

// actions turns the collected groups into the units Apply commits, oldest
// first, each carrying the title the review answered to at the time.
func (b *builder) actions() []entity.Action {
	groups := make([]*group, len(b.groups))
	copy(groups, b.groups)
	sort.SliceStable(groups, func(i, j int) bool {
		if !groups[i].at.Equal(groups[j].at) {
			return groups[i].at.Before(groups[j].at)
		}
		return groups[i].seq < groups[j].seq
	})

	var trailers []review.Trailer
	if url := b.webURL(); url != "" {
		trailers = []review.Trailer{{Key: "Origin", Value: url}}
	}

	title := ""
	out := make([]entity.Action, 0, len(groups))
	for _, g := range groups {
		if len(g.events) == 0 {
			continue
		}
		out = append(out, entity.Action{
			Author: g.who.CommitIdentity(g.at),
			Message: review.Action{
				ID:       b.id,
				Title:    title,
				Events:   g.events,
				Trailers: trailers,
			}.Message(),
			Events: g.events,
		})
		for _, e := range g.events {
			if e.Op == "title" {
				title = e.Val.Display()
			}
		}
	}
	return out
}

// emit builds one event.
//
// `c` is the upstream timestamp, not a position in the feed. That is forced:
// re-import must reproduce every event's bytes exactly, so `c` has to be a pure
// function of one upstream event and never of its neighbours.
func (b *builder) emit(at time.Time, author, op string, val entity.Value, ref, nonceInput string) entity.Event {
	if b.err != nil {
		return entity.Event{}
	}
	unix := at.Unix()
	e, err := entity.NewEventWithNonce(b.format, entity.Event{
		V:   entity.FormatVersion,
		C:   unix,
		TS:  unix,
		A:   author,
		Op:  op,
		N:   Nonce(nonceInput),
		Ref: ref,
		Val: val,
	})
	if err != nil {
		b.err = fmt.Errorf("%s: %w", op, err)
		return entity.Event{}
	}
	b.cur.events = append(b.cur.events, e)
	return e
}

// emitField is emit for an event synthesised from the pull request's current
// state rather than from an upstream event of its own.
func (b *builder) emitField(at time.Time, author, op string, val entity.Value) entity.Event {
	return b.emit(at, author, op, val, "", b.origin+":"+op)
}

// emitList is emitField for a list member, whose nonce includes the value so
// that two members of one field do not collide into one event.
func (b *builder) emitList(at time.Time, author, op, val string) entity.Event {
	return b.emit(at, author, op, entity.Str(val), "", b.origin+":"+op+":"+val)
}

// Vocabulary is this bridge's contribution to the fold, and it is empty: the
// pull request id and the URL live on the origin ledger. It stays declared
// because that is the seam a bridge with genuine namespaced state would use.
var Vocabulary = entity.Vocabulary{}

// Nonce derives an event's nonce from stable upstream identity: the first 16 hex
// characters of its SHA-256.
func Nonce(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:16]
}

// Status maps Azure DevOps' pull request status onto docs/reviews.md's
// vocabulary.
//
// The value is carried through in Azure DevOps' own spelling, capitalised — the
// same `Active` / `Completed` / `Abandoned` internal/review already classifies,
// so a bridged review dots the same way a local close does. `completed` is the
// one value a local writer may never assert, and this import is entitled to:
// Azure DevOps performed the merge and is reporting it.
func Status(status string) string {
	switch strings.ToLower(status) {
	case adoapi.PRCompleted:
		return "Completed"
	case adoapi.PRAbandoned:
		return "Abandoned"
	default:
		return review.StatusOpen
	}
}

// Verdict maps an Azure DevOps reviewer vote onto the verdict vocabulary.
//
// A vote of -5, "waiting for the author", is a soft "not yet" rather than a
// position on the change, so it imports as no verdict. 0 — no vote — likewise.
// Only an explicit approval or rejection is a verdict.
func Verdict(vote int) string {
	switch {
	case vote >= adoapi.VoteApprovedWithSuggest:
		return review.VerdictApprove
	case vote <= adoapi.VoteRejected:
		return review.VerdictRequestChanges
	default:
		return ""
	}
}

// Conclusion maps Azure DevOps' status state onto docs/reviews.md's check
// vocabulary. The vocabulary is open on both sides, so anything unrecognised
// passes through lowercased rather than being dropped or coerced.
func Conclusion(state string) string {
	switch strings.ToLower(state) {
	case "succeeded":
		return review.ConclusionPass
	case "failed", "error":
		return review.ConclusionFail
	case "notapplicable":
		return review.ConclusionSkipped
	case "pending", "notset", "":
		return review.ConclusionPending
	default:
		return strings.ToLower(state)
	}
}

// resolvedStatus reports whether a thread status means the conversation was
// addressed. `active` and `pending` are open; the rest are closed one way or
// another.
func resolvedStatus(status string) bool {
	switch strings.ToLower(status) {
	case "fixed", "closed", "wontfix", "bydesign":
		return true
	default:
		return false
	}
}

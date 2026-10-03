// Package ghreview maps GitHub pull requests onto the review vocabulary.
//
// It is the only package that knows both what a review is and what GitHub is.
// internal/bridge/github/api below it knows GitHub and nothing else; internal/review below
// that knows reviews and nothing else. It is a sibling of internal/bridge/github/issue
// rather than part of it, which keeps `cmd/git-review` clear of the issue
// vocabulary entirely: the two types are siblings, and their bridges are too.
//
// Everything here is a pure function of what GitHub returned. That is a
// correctness requirement, not a style: an import that depended on local state
// would produce different bytes on a second run, and since an event's id is the
// hash of its own bytes, a re-import would duplicate the entity instead of
// converging on it.
package ghreview

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	ghapi "github.com/hdweiss/git-issue/internal/bridge/github/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/review"
)

// Lookup answers the questions a response cannot: which upstream objects this
// repository already holds, and as what. It is review.Lookup — the same
// questions every review bridge asks the origin ledger.
type Lookup = review.Lookup

// CommentOrigin ties one local thread-entry event to the upstream comment it
// corresponds to.
type CommentOrigin = review.CommentOrigin

// Entity is one imported review: the id its create event hashes to, every
// action the import produced, and the mappings it establishes.
//
// The mappings do not go into the blob — which GitHub object a review
// corresponds to is not a fact about the review.
type Entity struct {
	ID      string
	Actions []entity.Action

	Origin string
	URL    string

	Comments []CommentOrigin

	// Threads ties each local thread-root event to the upstream review thread
	// it came from, which is what a push needs in order to resolve one.
	Threads []CommentOrigin

	// Checks are the head commit's runs. They are *not* part of this entity and
	// never reach its blob: a check is a fact about a commit and lives on its
	// own ref, keyed by commit sha. They are carried here only because the same
	// response supplied them, and the caller writes them elsewhere.
	Checks []Check

	// Unresolved is how many relation events this import could not write
	// because the ledger does not hold the entity they name.
	Unresolved int
}

// Check is one run to record against a commit. The shape is shared with every
// bridge that imports checks, so it is defined once in internal/review.
type Check = review.CommitCheck

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

// Import maps one GitHub pull request onto actions, each a group of events that
// one person did at one moment.
func Import(format entity.ObjectFormat, pr ghapi.PullRequest, known Lookup) (Entity, error) {
	b := &builder{format: format, pr: pr, known: known}
	b.build()
	if b.err != nil {
		return Entity{}, b.err
	}
	return Entity{
		ID:         b.id,
		Actions:    b.actions(),
		Origin:     pr.ID,
		URL:        pr.URL,
		Comments:   b.comments,
		Threads:    b.threads,
		Checks:     b.checks(),
		Unresolved: b.unresolved,
	}, nil
}

type group struct {
	at     time.Time
	login  string
	seq    int
	events []entity.Event
}

type builder struct {
	format entity.ObjectFormat
	pr     ghapi.PullRequest
	groups []*group
	cur    *group
	id     string
	err    error
	known  Lookup

	comments   []CommentOrigin
	threads    []CommentOrigin
	unresolved int
}

func (b *builder) begin(at time.Time, login string) *group {
	g := &group{at: at, login: login, seq: len(b.groups)}
	b.groups = append(b.groups, g)
	b.cur = g
	return g
}

func (b *builder) build() {
	created := b.pr.CreatedAt
	author := Author(b.pr.Author)

	b.begin(created, login(b.pr.Author))

	// The create event first, and its id is the entity's. Its nonce is SHA-256
	// of the node id — the one derivation that fixes the identity of every
	// GitHub pull request ever imported by any implementation.
	create := b.emit(created, author, "create", entity.Str(review.Type), "", b.pr.ID)
	b.id = create.ID

	b.emitField(created, author, "title", entity.Str(b.pr.Title))
	if b.pr.Body != "" {
		b.emitField(created, author, "description", entity.Str(b.pr.Body))
	}
	if b.pr.BaseRef != "" {
		b.emitField(created, author, "base", entity.Str(b.pr.BaseRef))
	}
	if head := b.head(); head != "" {
		b.emitField(created, author, "head", entity.Str(head))
	}
	if b.pr.HeadOID != "" {
		b.emitField(created, author, "head.sha", entity.Str(b.pr.HeadOID))
	}
	if b.pr.Milestone != "" {
		b.emitField(created, author, "milestone", entity.Str(b.pr.Milestone))
	}
	// No `status: open` event. Open is the implied status of a review that has
	// none, and writing it explicitly would sit at the same `c` as a status
	// reconciled from current state — leaving the tie-break on id to decide
	// whether a merged pull request imports as merged.
	if status := Status(b.pr.State); status != review.StatusOpen {
		b.emitField(created, author, "status", entity.Str(status))
	}
	if b.pr.Draft {
		b.emitField(created, author, "draft", entity.Bool(true))
	}
	if b.pr.Locked {
		b.emitField(created, author, "locked", entity.Bool(true))
	}

	for _, l := range b.pr.Labels {
		b.emitList(created, author, "label.add", l)
	}
	// A review request is who was asked to read this, which is what the
	// assignee list means on a review. GitHub's own `assignees` field on a pull
	// request means something else — who is responsible for landing it — and
	// conflating the two would file the author as their own reviewer.
	for _, r := range b.pr.Reviewers {
		b.emitList(created, author, "assignee.add", Author(r))
	}

	b.closes(created, author)
	b.issueComments()
	b.reviewThreads()
	b.verdicts()
}

// head is the review's head branch, remote-qualified.
//
// The owner rather than a remote name, because a remote name is local to one
// clone and this string is shared: `acme/feature-x` says whose fork the branch
// is in, which is what a reader needs and what a fetch can be built from. A
// pull request whose head repository has been deleted — routine once a fork
// goes away — reports none, and the bare branch name is the honest remainder.
func (b *builder) head() string {
	if b.pr.HeadRef == "" {
		return ""
	}
	if b.pr.HeadOwner == "" {
		return b.pr.HeadRef
	}
	return b.pr.HeadOwner + "/" + b.pr.HeadRef
}

// closes writes the links GitHub says this pull request would close.
//
// A target this clone does not hold is left unwritten rather than guessed at,
// and counted: an id has no meaning until the entity it names is here, and a
// cross-repository one may never be. Re-import still converges, because the
// blob is a grow-only set and the missing link is simply added when it becomes
// resolvable.
//
// The link is written and nothing else happens. Nothing here closes an issue,
// now or on merge — that is docs/reviews.md's rule, and it is also the rule
// that keeps this package from ever writing to the issue refs.
func (b *builder) closes(at time.Time, author string) {
	for _, node := range b.pr.Closes {
		target, ok := b.entity(node)
		if !ok {
			b.unresolved++
			continue
		}
		b.emit(at, author, review.RelAdd, entity.Str(review.KindCloses), target, b.pr.ID+":closes:"+node)
	}
}

func (b *builder) entity(node string) (string, bool) {
	if b.known == nil || node == "" {
		return "", false
	}
	return b.known.Entity(node)
}

// issueComments imports the pull request's own conversation — the comments that
// are not attached to a line.
func (b *builder) issueComments() {
	for _, c := range b.pr.Comments {
		if b.known != nil && b.known.ClaimedComment(c.ID) {
			continue
		}
		b.begin(c.CreatedAt, login(c.Author))
		e := b.emit(c.CreatedAt, Author(c.Author), "comment", entity.Str(c.Body), "", c.ID)
		b.comments = append(b.comments, CommentOrigin{EventID: e.ID, Upstream: c.ID})
	}
}

// reviewThreads imports each conversation anchored to a place in the diff.
//
// A thread's first comment becomes the root and carries the anchor; the rest
// become replies. That mirrors what the thread is, and it is what makes
// resolution — which addresses the root — line up with GitHub's own
// per-thread `isResolved`.
func (b *builder) reviewThreads() {
	for _, t := range b.pr.Threads {
		if len(t.Comments) == 0 {
			continue
		}

		var root string
		for i, c := range t.Comments {
			if b.known != nil && b.known.ClaimedComment(c.ID) {
				// A claimed root still has to anchor the replies under it, so
				// its local event id is recovered from the ledger rather than
				// re-emitted.
				if i == 0 {
					if id, ok := b.entity(c.ID); ok {
						root = id
					}
				}
				continue
			}

			parent := ""
			if i > 0 {
				parent = root
			}
			b.begin(c.CreatedAt, login(c.Author))
			e := b.emit(c.CreatedAt, Author(c.Author), "comment", entity.Str(c.Body), parent, c.ID)
			b.comments = append(b.comments, CommentOrigin{EventID: e.ID, Upstream: c.ID})

			if i == 0 {
				root = e.ID
				b.threads = append(b.threads, CommentOrigin{EventID: e.ID, Upstream: t.ID})
				if a, ok := anchorOf(t, c); ok {
					b.emit(c.CreatedAt, Author(c.Author), review.AnchorOp, entity.Str(a.String()), e.ID, c.ID+":anchor")
				}
			}
		}

		// Resolution is current state — GitHub raises no event for it that this
		// bridge can replay — so it is written at the thread's own moment and
		// attributed to whoever opened it. Only when true: a thread nobody
		// resolved needs no event saying so, and writing `false` would put a
		// resolution event on every thread on every import.
		if t.Resolved && root != "" {
			at := t.Comments[0].CreatedAt
			b.begin(at, login(t.Comments[0].Author))
			b.emit(at, Author(t.Comments[0].Author), review.ResolveOp, entity.Bool(true), root, t.ID+":resolved")
		}
	}
}

// anchorOf builds the anchor for a thread's root comment.
//
// It reads `originalCommit` and `originalLine` rather than `commit` and `line`.
// GitHub re-anchors a thread onto later revisions as the branch moves, and
// importing that would silently rewrite where somebody was looking — the one
// thing docs/reviews.md forbids doing to an anchor. What GitHub calls outdated
// is then derived here rather than imported, from the same comparison every
// other anchor gets.
func anchorOf(t ghapi.ReviewThread, c ghapi.ReviewComment) (review.Anchor, bool) {
	path := c.Path
	if path == "" {
		path = t.Path
	}
	line, start := c.OriginalLine, c.OriginalStartLine
	if line == 0 {
		line, start = t.Line, t.StartLine
	}
	if path == "" || line == 0 || c.OriginalCommitOID == "" {
		return review.Anchor{}, false
	}

	a := review.Anchor{
		Revision: c.OriginalCommitOID,
		Path:     path,
		First:    line,
		Last:     line,
	}
	if start > 0 && start < line {
		a.First = start
	}
	// The side comes from the thread. GitHub puts `diffSide` on
	// PullRequestReviewThread and not on the comments inside it, which is the
	// right shape anyway: a thread is attached to one place in the code, so its
	// entries cannot disagree about which half of a split diff they are on.
	if strings.EqualFold(t.DiffSide, "LEFT") {
		a.Side = review.SideLeft
	}
	return a, true
}

// verdicts imports each submitted review as one person's position.
//
// A review with a body writes a comment too, in the same action: on GitHub the
// two are one thing somebody did, and splitting them across actions would put a
// verdict and its explanation in different commits.
//
// DISMISSED reviews are imported as the verdicts they were and then retracted,
// which is what a dismissal is here — the add stays in the blob, so the record
// that somebody once approved survives, exactly as it does for a local
// dismissal.
func (b *builder) verdicts() {
	for _, r := range b.pr.Reviews {
		value := Verdict(r.State)
		if value == "" {
			continue
		}
		author := Author(r.Author)
		b.begin(r.CreatedAt, login(r.Author))

		if r.Body != "" && !(b.known != nil && b.known.ClaimedComment(r.ID)) {
			e := b.emit(r.CreatedAt, author, "comment", entity.Str(r.Body), "", r.ID)
			b.comments = append(b.comments, CommentOrigin{EventID: e.ID, Upstream: r.ID})
		}

		// A review this clone submitted is already in the blob as the local
		// verdict that produced it. Writing it again would leave the review with
		// two positions from what is really one person — the local author and the
		// GitHub login are different spellings — so the local event id is
		// recovered from the ledger instead, which is what a dismissal needs.
		addID := ""
		if b.known != nil {
			addID, _ = b.known.ClaimedVerdict(r.ID)
		}
		if addID == "" {
			// The revision the verdict was cast against goes in `ref`, which is
			// what makes staleness derivable without consulting a clock. A review
			// GitHub reports no commit for gets none, and reads as never stale
			// rather than as stale against nothing.
			addID = b.emit(r.CreatedAt, author, review.VerdictAdd, entity.Str(value), r.CommitOID, r.ID+":verdict").ID
		}

		if strings.EqualFold(r.State, ghapi.ReviewDismissed) {
			b.emit(r.CreatedAt, author, review.VerdictRemove, entity.Value{}, addID, r.ID+":dismissed")
		}
	}
}

// checks turns the head commit's runs into what the caller writes on the checks
// ref.
//
// They are returned rather than emitted: a check is a fact about a commit, so
// it belongs on a ref keyed by commit sha and never in this entity's blob.
func (b *builder) checks() []Check {
	out := make([]Check, 0, len(b.pr.Checks))
	for _, run := range b.pr.Checks {
		if run.CommitOID == "" {
			continue
		}
		at := run.StartedAt
		if at.IsZero() {
			at = b.pr.UpdatedAt
		}
		out = append(out, Check{
			Commit: run.CommitOID,
			At:     at,
			Author: Author(b.pr.Author),
			Run: review.Check{
				Name:       run.Name,
				Conclusion: Conclusion(run.Conclusion),
				URL:        run.URL,
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
	if b.pr.URL != "" {
		trailers = []review.Trailer{{Key: "Origin", Value: b.pr.URL}}
	}

	title := ""
	out := make([]entity.Action, 0, len(groups))
	for _, g := range groups {
		if len(g.events) == 0 {
			continue
		}
		out = append(out, entity.Action{
			Author: Identity(g.login, g.at),
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
	return b.emit(at, author, op, val, "", b.pr.ID+":"+op)
}

// emitList is emitField for a list member, whose nonce has to include the value
// so that two members of one field do not collide into one event.
func (b *builder) emitList(at time.Time, author, op, val string) entity.Event {
	return b.emit(at, author, op, entity.Str(val), "", b.pr.ID+":"+op+":"+val)
}

// Vocabulary is this bridge's contribution to the fold, and it is empty: the
// node id and the URL live on the origin ledger, where a fact about this
// repository's relationship with one tracker belongs. It stays declared because
// that is the seam a bridge with genuine namespaced state would use.
var Vocabulary = entity.Vocabulary{}

// Nonce derives an event's nonce from stable upstream identity: the first 16
// hex characters of its SHA-256.
func Nonce(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:16]
}

func login(s string) string {
	if s == "" {
		return "ghost"
	}
	return s
}

// Author maps a GitHub login onto an event author. The scheme prefix matters as
// much as the login: `a` is part of the event's hashed bytes, so two
// implementations spelling the same author differently would produce different
// ids for the same event and stop converging.
func Author(l string) string { return "github:" + login(l) }

// Identity maps a GitHub login onto the author of a commit. Nothing hashes
// this, unlike Author.
func Identity(l string, at time.Time) gitx.Identity {
	l = login(l)
	return gitx.Identity{Name: l, Email: l + "@users.noreply.github.com", When: at}
}

// Status maps GitHub's pull request state onto docs/reviews.md's vocabulary.
//
// MERGED is the one value a local writer may never assert, and this is the
// import that is entitled to: GitHub performed the merge and is reporting it.
func Status(state string) string {
	switch strings.ToUpper(state) {
	case ghapi.StateMerged:
		return review.StatusMerged
	case ghapi.StateClosed:
		return review.StatusClosed
	default:
		return review.StatusOpen
	}
}

// Verdict maps a GitHub review state onto the verdict vocabulary.
//
// PENDING has no counterpart and must not get one: an unsubmitted review is
// visible only to its author, and importing it would publish a draft nobody
// sent. DISMISSED maps to the verdict it was, and the caller retracts it.
func Verdict(state string) string {
	switch strings.ToUpper(state) {
	case ghapi.ReviewApproved:
		return review.VerdictApprove
	case ghapi.ReviewChangesRequested:
		return review.VerdictRequestChanges
	case ghapi.ReviewCommented:
		return review.VerdictComment
	case ghapi.ReviewDismissed:
		// A dismissed review was an approval or a request for changes, and
		// GitHub no longer says which. Recording it as a comment is the honest
		// remainder: somebody read this, and their position was withdrawn.
		return review.VerdictComment
	default:
		return ""
	}
}

// Conclusion maps GitHub's check vocabulary onto docs/reviews.md's.
//
// The vocabulary is open on both sides, so anything unrecognised passes through
// lowercased rather than being dropped or coerced — a client that does not know
// a word treats it as neither failed nor pending, which is the safe reading.
func Conclusion(s string) string {
	switch strings.ToUpper(s) {
	case "SUCCESS", "NEUTRAL":
		return review.ConclusionPass
	case "FAILURE", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "ERROR":
		return review.ConclusionFail
	case "CANCELLED":
		return review.ConclusionCancelled
	case "SKIPPED", "STALE":
		return review.ConclusionSkipped
	case "QUEUED", "IN_PROGRESS", "PENDING", "WAITING", "REQUESTED", "EXPECTED":
		return review.ConclusionPending
	case "":
		return review.ConclusionPending
	default:
		return strings.ToLower(s)
	}
}

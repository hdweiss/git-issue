package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/render"
)

// Sync reports what a pull or a push moved, in the shape `git pull` reports a
// fetch.
//
// The two things it cannot work out for itself are what to call an entity and
// how to draw one as a row — both of which are a type's vocabulary. Everything
// else here is the same for every type, and lives here rather than in each
// command so the two cannot drift into two different-looking syncs of the same
// object store.
type Sync struct {
	// Noun is the singular word for one entity: "issue", "review".
	Noun string
	// Row draws one entity as a listing line.
	Row func(id string, st entity.State) render.Row
}

// Report prints the source, the ref-update line git's own fetch prints, whether
// the local ref moved by fast-forward or by a merge, one line per changed
// entity in a listing's columns, and a git-style tally.
//
// An up-to-date pull prints only "Already up to date." — no "From" header —
// because that is exactly what git does when a fetch brought back nothing.
func (s Sync) Report(pull entity.Pull, header string, uniq render.Unique) error {
	if pull.UpToDate() {
		fmt.Println("Already up to date.")
		return nil
	}

	changes, err := s.Changes(pull)
	if err != nil {
		return err
	}

	fmt.Println(header)
	if line := FetchLine(pull); line != "" {
		fmt.Println(line)
	}
	switch {
	case pull.FF:
		fmt.Printf("Updating %s..%s\nFast-forward\n", ShortSHA(pull.Old), ShortSHA(pull.New))
	case pull.Old != "":
		// A notes merge is a set union of lines; name the strategy the way git
		// names 'ort' after a real merge.
		fmt.Println("Merge made by the 'cat_sort_uniq' strategy.")
	}

	// The ref moved and nothing anybody can see did. That is exactly what a
	// bridge import of a change this clone pushed looks like: the tracker
	// recorded an event of its own for it, and the import wrote that event
	// faithfully, saying what the blob already said. The ref lines above stay,
	// because the ref really did move; what would mislead is listing an entity
	// as updated and sending someone looking for a difference that is not there.
	if len(changes) == 0 {
		fmt.Println(s.NothingChanged())
		return nil
	}
	render.WriteChanges(os.Stdout, changes, uniq)
	fmt.Println(s.Tally(changes))
	return nil
}

// NothingChanged is what a sync that moved the ref without moving any entity
// says instead of a change list.
func (s Sync) NothingChanged() string { return "No " + s.Noun + " changed." }

// Changes are the entities whose folded state actually differs.
//
// An update whose before and after say the same thing is dropped. The blob did
// grow — the commits are on the ref and `log` shows them — but nothing a reader
// can see moved, and a listing that claims otherwise is noise on every pull
// that follows a push.
func (s Sync) Changes(pull entity.Pull) ([]render.Change, error) {
	before, err := pull.Previous()
	if err != nil {
		return nil, err
	}
	var changes []render.Change
	err = pull.Each(func(c entity.Change, st entity.State) error {
		if prev, ok := before[c.ID]; ok && prev.SameAs(st) {
			return nil
		}
		changes = append(changes, render.Change{Status: byte(c.Kind), Row: s.Row(c.ID, st)})
		return nil
	})
	return changes, err
}

// Tally is the summary line, shaped like git's "N files changed, X
// insertions(+), Y deletions(-)": a category with no entities in it is left
// out, and the +/- signs echo git's where the sense lines up.
//
// Counted over what is actually reported rather than over every path the diff
// named, so the words and the lines above them agree.
func (s Sync) Tally(changes []render.Change) string {
	count := func(kind entity.ChangeKind) int {
		n := 0
		for _, c := range changes {
			if c.Status == byte(kind) {
				n++
			}
		}
		return n
	}
	parts := []string{Plural(len(changes), s.Noun) + " changed"}
	if n := count(entity.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("%d added(+)", n))
	}
	if n := count(entity.Updated); n > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", n))
	}
	if n := count(entity.Removed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d removed(-)", n))
	}
	return strings.Join(parts, ", ")
}

// Plural counts things the way a sentence does: "1 issue", "3 issues".
func Plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// FetchLine is git fetch's own ref-update line — the one printed under
// "From <url>" that says where the tracking ref moved. Only a git-mode pull has
// a tracking ref; a bridge import fetches no ref, so it has no such line.
func FetchLine(p entity.Pull) string {
	if p.Tracking == "" {
		return ""
	}
	summary, flag := ShortSHA(p.Old)+".."+ShortSHA(p.New), " "
	if p.Old == "" {
		summary, flag = "[new ref]", "*"
	}
	return fmt.Sprintf(" %s %-17s %s -> %s", flag, summary, PrettyRef(p.Ref), PrettyRef(p.Tracking))
}

// PrettyRef drops the prefix git drops when it prints a ref. A notes ref keeps
// its refs/notes/ prefix, which is git's behaviour too.
func PrettyRef(ref string) string {
	for _, p := range []string{"refs/remotes/", "refs/heads/", "refs/tags/"} {
		if rest, ok := strings.CutPrefix(ref, p); ok {
			return rest
		}
	}
	return ref
}

// ShortSHA abbreviates a commit to the width git's ref-update lines use.
func ShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Progress draws a one-line transfer indicator, redrawn in place as work lands
// and closed with ", done." — the shape git's own "Receiving objects" line
// uses. It shows a percentage while the total can be trusted, and a plain
// running count when it cannot: a count the run has already passed, or one it
// never reached once there was nothing left, is not a count worth a percentage.
type Progress struct {
	W io.Writer
	// Verb leads the line: "Reading" while a fetch pulls entities in, "Pushing"
	// while a bridge writes them out. Empty reads as "Reading", since that is
	// what most callers are doing.
	Verb string
	// Noun is what is being moved, since a bridge's own word for it is the one
	// the rest of its output uses.
	Noun           string
	fetched, total int
	width          int // widest line drawn, so a shorter redraw clears the last
	drawn          bool
}

func (p *Progress) Update(fetched, total int) {
	p.fetched, p.total = fetched, total
	p.draw(false)
}

// Note writes a line that has to outlive the progress line, which is redrawn
// over itself and would otherwise erase it: the drawn line is cleared, the note
// is written where it will stay, and the progress line is drawn again under it.
func (p *Progress) Note(s string) {
	if !p.drawn {
		fmt.Fprintln(p.W, s)
		return
	}
	fmt.Fprintf(p.W, "\r%s\r", strings.Repeat(" ", p.width))
	fmt.Fprintln(p.W, s)
	p.draw(false)
}

func (p *Progress) Done() {
	if p.drawn {
		p.draw(true)
		fmt.Fprintln(p.W)
	}
}

// Stop ends a run that did not finish — an aborted transfer. It leaves the last
// frame it drew untouched (no ", done.", no collapse to a bare count) and moves
// off the line, so the error printed next starts clean and the count still reads
// as "got this far out of the total".
func (p *Progress) Stop() {
	if p.drawn {
		fmt.Fprintln(p.W)
	}
}

func (p *Progress) draw(done bool) {
	verb := p.Verb
	if verb == "" {
		verb = "Reading"
	}
	noun := p.Noun
	if noun == "" {
		noun = "entities"
	}
	line := fmt.Sprintf("%s %s: %d", verb, noun, p.fetched)
	if p.total > 0 && p.fetched <= p.total && !(done && p.fetched < p.total) {
		line = fmt.Sprintf("%s %s: %d%% (%d/%d)", verb, noun, p.fetched*100/p.total, p.fetched, p.total)
	}
	if done {
		line += ", done."
	}
	if len(line) < p.width {
		line += strings.Repeat(" ", p.width-len(line))
	}
	p.width = max(p.width, len(line))
	fmt.Fprintf(p.W, "\r%s", line)
	p.drawn = true
}

// AutoRepackAt is the number of commits in one run past which an import
// repacks the object store on its way out.
//
// The problem it fixes only shows up in bulk. An import that replays whole
// histories writes several versions of each note blob, and fast-import chains
// them in write order — oldest version as the delta base, newest at the deep
// end. Every listing reads exactly the newest version of every note, so it
// replays each note's whole history.
const AutoRepackAt = 1000

// Repack puts the object store back in shape after a large import.
//
// This runs without being asked, which is a real cost. It is worth it because
// the alternative is worse and silent: the import leaves every subsequent read
// several times slower, in a way nothing about the output suggests, and the
// obvious remedy does not work — `git gc` reuses the deltas it already has.
//
// `gc.auto = 0` turns it off. Someone who has told git never to maintain this
// repository on its own has said the thing that matters.
//
// A repack that fails is reported and swallowed. The import is already on the
// ref by this point, so what a failure costs is reading speed, not data.
func Repack(repo *gitx.Repo, commits int, w io.Writer) {
	if commits < AutoRepackAt || repo.ConfigDefault("gc.auto") == "0" {
		return
	}
	fmt.Fprintf(w, "Repacking after %d commits; this is a one-off and may take a while.\n", commits)
	if err := repo.Repack(w); err != nil {
		fmt.Fprintf(w, "warning: repack failed: %s\n", err)
	}
}

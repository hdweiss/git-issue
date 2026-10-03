package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/hdweiss/git-issue/internal/bridge"
	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
	adoissue "github.com/hdweiss/git-issue/internal/bridge/ado/issue"
	giteaapi "github.com/hdweiss/git-issue/internal/bridge/gitea/api"
	giteaissue "github.com/hdweiss/git-issue/internal/bridge/gitea/issue"
	ghapi "github.com/hdweiss/git-issue/internal/bridge/github/api"
	ghissue "github.com/hdweiss/git-issue/internal/bridge/github/issue"
	"github.com/hdweiss/git-issue/internal/cli"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/issue"
	"github.com/hdweiss/git-issue/internal/origins"
	"github.com/hdweiss/git-issue/internal/remote"
	"github.com/hdweiss/git-issue/internal/render"
)

// pushBridge writes local changes back to an external tracker.
//
// Everything platform-specific is behind bridge.Bridge; what is here is the
// order of operations, which is where the safety lives: read, plan, show, ask,
// then write, journalling every mapping as it is earned and committing them once
// at the end.
func pushBridge(s *entity.Store, spec remote.Spec, opts pushOptions) error {
	dest, err := resolveBridge(s, spec, opts)
	if err != nil {
		return err
	}
	tracker := dest.tracker

	led := origins.NewStore(s.Repo)
	ledger, err := led.Load(tracker)
	if err != nil {
		return err
	}
	for _, w := range ledger.Warnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}

	// A run that died before committing its mappings left them here. Commit
	// them before anything else, or this run will create upstream duplicates of
	// everything the last one already filed.
	journal, err := led.OpenJournal(tracker)
	if err != nil {
		return err
	}
	if !journal.Empty() {
		fmt.Fprintf(os.Stderr, "Recovering %s from an interrupted push\n", plural(len(journal.Lines()), "mapping"))
		journal.Into(ledger)
		if err := led.Save(ledger, recoveredMessage(tracker, len(journal.Lines()))); err != nil {
			return err
		}
		journal.Done()
	}

	// The bridge is built only now, because it is handed the ledger: a bridge
	// that resolves links has to answer them the same way the pull did.
	br := dest.open(ledger)

	candidates, err := collect(s, ledger, dest.prefix, opts.ids)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		fmt.Println("Everything up-to-date")
		return nil
	}

	fmt.Fprintf(os.Stderr, "Reading %s from %s\n", plural(len(candidates), "issue"), tracker)
	plan, err := br.Plan(candidates, ledger)
	if err != nil {
		return err
	}

	if err := reportPlan(s, tracker, dest.prefix, plan); err != nil {
		return err
	}
	if len(plan.Deltas) == 0 {
		return conflictExit(plan)
	}
	if opts.dryRun {
		return conflictExit(plan)
	}
	if !opts.yes {
		ok, err := confirm(fmt.Sprintf("\nPush %s to %s?", plural(len(plan.Deltas), "issue"), tracker))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Nothing was pushed.")
			return nil
		}
	}

	prog := cli.Progress{W: os.Stderr, Verb: "Pushing", Noun: "issues"}
	pushed, applyErr := applyDeltas(br, candidates, plan, ledger, journal, prog.Update)
	if applyErr != nil {
		prog.Stop()
	} else {
		prog.Done()
	}

	// The ledger is written whatever happened. A mapping earned and then
	// dropped costs a duplicate issue upstream that nothing undoes, so it is
	// committed before any error is reported.
	if !journal.Empty() {
		if err := led.Save(ledger, pushedMessage(tracker, pushed)); err != nil {
			return err
		}
		journal.Done()
	}
	if applyErr != nil {
		// The run is resumable: every issue filed so far has a mapping now, so
		// a re-run plans only what is left. Say so, rather than leaving someone
		// to guess whether pushing again duplicates the first half.
		if pushed > 0 {
			fmt.Fprintf(os.Stderr, "%s pushed to %s before the error; re-run the same command to continue.\n",
				plural(pushed, "issue"), tracker)
		}
		return applyErr
	}

	fmt.Printf("%s pushed to %s\n", plural(pushed, "issue"), tracker)
	if s.Repo.RefSHA(led.Ref) != "" {
		fmt.Printf("Run 'git issue push %s' to share the origin ledger with other clones.\n", remote.Default)
	}
	if opts.refresh {
		refresh := pullOptions{all: true, full: true, token: opts.token}
		switch spec.Scheme {
		case remote.SchemeADO:
			return pullADO(s, spec, refresh)
		case remote.SchemeGitea:
			return pullGitea(s, spec, refresh)
		default:
			return pullGitHub(s, spec, refresh)
		}
	}
	return conflictExit(plan)
}

// destination is a resolved push target.
//
// The bridge is a constructor rather than a value because building one needs
// the origin ledger, and loading the ledger needs the tracker name that
// resolving the target produces. Splitting the two is what breaks that circle,
// and it is also where the author prefix comes from — the scheme is known
// here, so nothing downstream has to guess a platform from a tracker name.
type destination struct {
	tracker string
	// prefix is how this bridge spells an author in `a`, which is what lets a
	// push tell an issue with something local to say from one that is only an
	// echo of the tracker it came from.
	prefix string
	open   func(bridge.Lookup) bridge.Bridge
}

// resolveBridge resolves a target for a push. A second scheme is a second case
// here and nothing else.
func resolveBridge(s *entity.Store, spec remote.Spec, opts pushOptions) (destination, error) {
	switch spec.Scheme {
	case remote.SchemeGitHub:
		target, err := ghapi.ParseTarget(spec.URL)
		if err != nil {
			return destination{}, fmt.Errorf("%s: %w", spec.Target, err)
		}
		token, _, _ := ghapi.Token(s.Repo, target.Host, opts.token)
		client := ghapi.New(target.Endpoint(), token)
		return destination{
			tracker: target.String(),
			prefix:  "github:",
			open: func(known bridge.Lookup) bridge.Bridge {
				return ghissue.NewPusher(client, target, s.Format(), s.Vocab, known)
			},
		}, nil

	case remote.SchemeGitea:
		target, err := giteaapi.ParseTarget(spec.URL)
		if err != nil {
			return destination{}, fmt.Errorf("%s: %w", spec.Target, err)
		}
		token, _, _ := giteaapi.Token(s.Repo, target.Host, opts.token)
		client := giteaapi.New(target.API(), token)
		return destination{
			tracker: target.String(),
			prefix:  "gitea:",
			open: func(known bridge.Lookup) bridge.Bridge {
				return giteaissue.NewPusher(client, target, s.Format(), s.Vocab, known)
			},
		}, nil

	case remote.SchemeADO:
		target, err := adoapi.ParseTarget(spec.URL)
		if err != nil {
			return destination{}, fmt.Errorf("%s: %w", spec.Target, err)
		}
		token, _, _ := adoapi.Token(s.Repo, adoHost(target), opts.token)
		client := adoapi.New(token)
		if name, err := client.Project(target); err == nil {
			target.Project = name
		}
		// A push files new work items in the area this clone is scoped to, and
		// nowhere else: an update never writes System.AreaPath at all.
		target.Area = pushArea(s.Repo, target, spec)
		return destination{
			tracker: target.String(),
			prefix:  "ado:",
			open: func(known bridge.Lookup) bridge.Bridge {
				return adoissue.NewPusher(client, target, s.Format(), s.Vocab, known)
			},
		}, nil

	default:
		return destination{}, fmt.Errorf("cannot push to '%s'", spec.Scheme)
	}
}

// pushArea is where a push files a work item it creates: the area written on
// the target, else the one this clone saved, else the project root.
//
// Nothing is prompted here. A push is not where somebody should be asked to
// choose a default, and a pull has already asked by the time there is anything
// to push.
func pushArea(repo *gitx.Repo, target adoapi.Target, spec remote.Spec) string {
	if spec.Scope != "" {
		return normalizeArea(spec.Scope)
	}
	return normalizeArea(savedArea(repo, target.String()))
}

// collect gathers the issues worth reading upstream state for.
//
// The prefilter is what keeps a push from fetching the whole tracker: an issue
// whose every event was authored by this bridge has nothing local to say. Every
// locally written event takes its author from user.email, including one that
// arrived from another clone over the git leg, so this never skips an issue that
// had something to push. It is per-bridge on purpose — pushing to a second
// tracker must not skip the first tracker's events, since those are exactly what
// a mirror carries across.
func collect(s *entity.Store, ledger *origins.Ledger, prefix string, ids []string) ([]bridge.Candidate, error) {
	if len(ids) > 0 {
		var out []bridge.Candidate
		for _, want := range ids {
			id, st, err := s.Find(want, issue.Type)
			if err != nil {
				return nil, err
			}
			upstream, _ := ledger.Upstream(id)
			out = append(out, bridge.Candidate{ID: id, Events: st.Events, State: st, Upstream: upstream})
		}
		return out, nil
	}

	notes, err := s.Notes()
	if err != nil {
		return nil, err
	}
	var out []bridge.Candidate
	err = s.Each(notes, func(id string, st entity.State) error {
		upstream, mapped := ledger.Upstream(id)
		if mapped && !hasLocalWrite(st, prefix) {
			return nil
		}
		out = append(out, bridge.Candidate{ID: id, Events: st.Events, State: st, Upstream: upstream})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Newest first, matching every other listing, so the plan lines up with the
	// `list` someone just ran.
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// hasLocalWrite reports whether any event came from somewhere other than this
// bridge. An empty prefix means the bridge is unknown, so nothing is skipped.
func hasLocalWrite(st entity.State, prefix string) bool {
	if prefix == "" {
		return true
	}
	for _, ev := range st.Events {
		if !strings.HasPrefix(ev.A, prefix) {
			return true
		}
	}
	return false
}

// applyDeltas sends each delta, journalling what it earns as it goes.
//
// One issue at a time, and the journal is flushed to disk after every one. That
// is what makes seeding a fork resumable: interrupt it, run it again, and only
// the issues with no mapping are created.
//
// report is called with the running count after each delta lands, so a large
// seed draws the same pushed/total bar a pull draws while fetching.
func applyDeltas(br bridge.Bridge, candidates []bridge.Candidate, plan bridge.Plan, ledger *origins.Ledger, journal *origins.Journal, report func(done, total int)) (int, error) {
	byID := map[string]bridge.Candidate{}
	for _, c := range candidates {
		byID[c.ID] = c
	}

	deltas := inLinkOrder(plan.Deltas)
	pushed := 0
	report(pushed, len(deltas))
	for _, d := range deltas {
		result, err := br.Apply(byID[d.ID], d)
		// Record before reporting: Apply returns whatever it managed to create
		// even when it then failed, and an unrecorded creation is the one loss
		// that cannot be undone.
		if jerr := journalResult(journal, ledger, result); jerr != nil {
			return pushed, jerr
		}
		if err != nil {
			return pushed, fmt.Errorf("%s: %w", short(d.ID), err)
		}
		pushed++
		report(pushed, len(deltas))
	}
	return pushed, nil
}

// inLinkOrder puts each delta after the ones it links to.
//
// A link is written by naming the upstream object its target is filed as, and
// an issue this run is creating has no such object until it has been created.
// Applying the epic before the issues filed under it is what lets a whole tree
// be seeded in one push rather than two — the mapping is journalled the moment
// each create returns, so the next delta can resolve it.
//
// Only new links order anything. A retraction names a link that already exists
// upstream, so both ends are there by definition. Ties and cycles keep the
// order they came in: a parent that is somehow its own ancestor is a
// broken tracker, not a reason to drop a delta on the floor.
func inLinkOrder(deltas []issue.Delta) []issue.Delta {
	byID := make(map[string]issue.Delta, len(deltas))
	for _, d := range deltas {
		byID[d.ID] = d
	}

	out := make([]issue.Delta, 0, len(deltas))
	state := make(map[string]int, len(deltas)) // 1 while being placed, 2 once placed
	var place func(d issue.Delta)
	place = func(d issue.Delta) {
		if state[d.ID] != 0 {
			return
		}
		state[d.ID] = 1
		for _, r := range d.RelationsAdded {
			if target, ok := byID[r.Target]; ok && state[target.ID] == 0 {
				place(target)
			}
		}
		state[d.ID] = 2
		out = append(out, d)
	}
	for _, d := range deltas {
		place(d)
	}
	return out
}

func journalResult(journal *origins.Journal, ledger *origins.Ledger, r bridge.Result) error {
	if r.Upstream != "" {
		if err := journal.Append(origins.KindIssue, r.ID, r.Upstream); err != nil {
			return err
		}
		ledger.Add(origins.KindIssue, r.ID, r.Upstream)
	}
	if r.URL != "" {
		if err := journal.Append(origins.KindURL, r.ID, r.URL); err != nil {
			return err
		}
		ledger.Add(origins.KindURL, r.ID, r.URL)
	}
	for _, c := range r.Comments {
		if err := journal.Append(origins.KindComment, r.ID, c.EventID, c.Upstream); err != nil {
			return err
		}
		ledger.Add(origins.KindComment, r.ID, c.EventID, c.Upstream)
	}
	return nil
}

// reportPlan prints what the push intends to do, in a listing's own columns.
func reportPlan(s *entity.Store, tracker, scheme string, plan bridge.Plan) error {
	if len(plan.Deltas) == 0 && len(plan.Conflicts) == 0 && len(plan.Skipped) == 0 {
		fmt.Println("Everything up-to-date")
		return nil
	}

	fmt.Println("To " + tracker)

	conflicted := map[string][]string{}
	for _, c := range plan.Conflicts {
		conflicted[c.ID] = append(conflicted[c.ID], c.Field)
	}

	var rows []render.Change
	notes := map[string]string{}
	for _, d := range plan.Deltas {
		status := byte(entity.Updated)
		// A create needs no note: the " A " and the title next to it already
		// say "new issue", and a page of identical "new issue" lines under a
		// fork seed is exactly the noise `git push` never prints.
		note := strings.Join(d.Fields(), ", ")
		if d.Create {
			status, note = byte(entity.Added), ""
		}
		st, err := stateOf(s, d.ID)
		if err != nil {
			return err
		}
		rows = append(rows, render.Change{Status: status, Row: issue.Row(d.ID, st)})
		notes[d.ID] = note
	}
	for id, fields := range conflicted {
		st, err := stateOf(s, id)
		if err != nil {
			return err
		}
		rows = append(rows, render.Change{Status: '!', Row: issue.Row(id, st)})
		notes[id] = strings.Join(fields, ", ") + " changed on both sides"
	}

	render.WriteChanges(os.Stdout, rows, uniqueIDs(s))
	for _, r := range rows {
		if note := notes[r.Row.ID]; note != "" {
			fmt.Printf("   %s  %s\n", short(r.Row.ID), note)
		}
	}

	reportSkips(plan.Skipped)

	summary := []string{plural(len(plan.Deltas), "issue") + " to push"}
	if n := len(conflicted); n > 0 {
		summary = append(summary, fmt.Sprintf("%d conflicted", n))
	}
	fmt.Println(strings.Join(summary, ", "))

	if len(conflicted) > 0 {
		fmt.Println("hint: pull, look at what the other side did, then edit:")
		fmt.Printf("hint:   git issue pull %s%s\n", scheme, tracker)
	}
	return nil
}

func stateOf(s *entity.Store, id string) (entity.State, error) {
	_, st, err := s.Find(id, issue.Type)
	return st, err
}

// reportSkips prints what a bridge declined to send, one line per distinct
// reason rather than one per issue.
//
// A mirror seeded from another tracker names the same missing label, milestone
// or person on issue after issue, and a line each buries the plan. Skips that
// share a reason collapse to a single line with a count; a reason that touched
// one issue still names it.
func reportSkips(skips []bridge.Skip) {
	type group struct {
		field, reason string
		ids           []string
	}
	var order []string
	groups := map[string]*group{}
	for _, sk := range skips {
		key := sk.Field + "\x00" + sk.Reason
		g, ok := groups[key]
		if !ok {
			g = &group{field: sk.Field, reason: sk.Reason}
			groups[key] = g
			order = append(order, key)
		}
		g.ids = append(g.ids, sk.ID)
	}

	for _, key := range order {
		g := groups[key]
		label := "skipped"
		if g.field != "" {
			label = g.field + " not pushed"
		}
		if len(g.ids) == 1 {
			fmt.Printf("   %s  %s: %s\n", short(g.ids[0]), label, g.reason)
		} else {
			fmt.Printf("   %s: %s (%s)\n", label, g.reason, plural(len(g.ids), "issue"))
		}
	}
}

// conflictExit makes a partly-refused push visible to a script. Everything that
// could be pushed was; the non-zero status is what says something was not.
func conflictExit(plan bridge.Plan) error {
	if len(plan.Conflicts) == 0 {
		return nil
	}
	return exitCode(1)
}

func recoveredMessage(tracker string, n int) string {
	return fmt.Sprintf("Recover %s for %s\n\nMappings earned by a push that did not finish recording them.\n\nTracker: %s\n",
		plural(n, "mapping"), tracker, tracker)
}

func pushedMessage(tracker string, n int) string {
	return fmt.Sprintf("Link %s to %s\n\nWhere each one now lives upstream, so a later import converges on the\nissue already here instead of filing a second one.\n\nTracker: %s\n",
		plural(n, "issue"), tracker, tracker)
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
	adoissue "github.com/hdweiss/git-issue/internal/bridge/ado/issue"
	"github.com/hdweiss/git-issue/internal/cli"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/origins"
	"github.com/hdweiss/git-issue/internal/remote"
)

// pullADO imports work items from Azure DevOps.
//
// The shape mirrors the GitHub path deliberately, which mirrors the git one:
// fetch, union into the notes ref, then report what moved in the columns
// `list` uses. What differs is only where the events come from, and that this
// bridge has an area to resolve first.
func pullADO(s *entity.Store, spec remote.Spec, opts pullOptions) error {
	target, err := adoapi.ParseTarget(spec.URL)
	if err != nil {
		return fmt.Errorf("%s: %w", spec.Target, err)
	}

	token, source, cred := adoapi.Token(s.Repo, adoHost(target), opts.token)
	client := adoapi.New(token)

	// Azure DevOps compares project names case-insensitively, so two spellings
	// of one project would be two paths in the ledger and two watermarks. This
	// is also the first request of the run, which makes it where a bad
	// credential is reported rather than three calls later.
	name, err := client.Project(target)
	if err != nil {
		return importErrorADO(target, source, err)
	}
	target.Project = name

	target, err = resolveArea(s.Repo, client, target, spec, os.Stderr)
	if err != nil {
		return importErrorADO(target, source, err)
	}

	// Open work items only unless asked otherwise. The consequence is worth
	// knowing: an item closed upstream since the last import is not in the
	// result set, so its local copy keeps saying what it last said — a filter
	// cannot report the absence of what it filtered out. Scoping by area has
	// exactly the same shape, and the same remedy.
	filter := adoapi.Filter{Open: !opts.all, Since: opts.since, Limit: opts.limit, Types: opts.types}
	scope := "open work items"
	if opts.all {
		scope = "all work items"
	}

	state, err := s.SyncState()
	if err != nil {
		return err
	}
	// A date the caller asked for wins, and does not disturb the watermark:
	// --since is a question, not a checkpoint. --full asks for everything, and
	// does move the watermark, because it genuinely read everything.
	if !opts.sinceGiven && !opts.full {
		filter.Since = state.Since(adoSyncKey(target, opts.all))
	}

	fmt.Fprintf(os.Stderr, "Importing %s from %s%s%s\n", scope, target, areaNote(target), sinceNote(filter.Since))
	prog := cli.Progress{W: os.Stderr, Noun: "work items"}
	items, err := client.Fetch(target, filter, prog.Update)
	prog.Done()
	if err != nil {
		return importErrorADO(target, source, err)
	}
	// The credential worked, so let git's helper record it — that is the other
	// half of the contract `git credential fill` starts.
	if cred != nil {
		s.Repo.CredentialApprove(*cred)
	}

	// The ledger answers three questions the blob cannot: which local entity a
	// work item is already filed as, which upstream comments are already here
	// as entries this repository posted, and which entity a link names.
	led := origins.NewStore(s.Repo)
	ledger, err := led.Load(target.String())
	if err != nil {
		return err
	}
	for _, w := range ledger.Warnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}

	// Filed first, imported second, for the reason spelled out over the same
	// two passes in pull_github.go: a link can only be written once the ledger
	// holds the work item it names, and a batch routinely contains both ends.
	imported := make([]adoissue.Entity, len(items))
	ids := make([]string, len(items))
	for i, w := range items {
		e, err := adoissue.Import(s.Format(), target, w, ledger)
		if err != nil {
			return fmt.Errorf("importing work item %d: %w", w.ID, err)
		}
		// An entity the ledger already names is filed under the id it has here,
		// not under the one the import derived.
		id := e.ID
		if claimed, ok := ledger.Entity(e.Origin); ok {
			id = claimed
		}
		imported[i], ids[i] = e, id
		recordADOIssue(ledger, id, e)
	}

	incoming := make([]entity.Incoming, 0, len(items))
	for i, w := range items {
		e := imported[i]
		if e.Unresolved > 0 {
			again, err := adoissue.Import(s.Format(), target, w, ledger)
			if err != nil {
				return fmt.Errorf("importing work item %d: %w", w.ID, err)
			}
			e = again
		}
		incoming = append(incoming, entity.Incoming{ID: ids[i], Actions: e.Actions})
		recordADO(ledger, ids[i], e)
	}

	pull, err := s.Apply(target.String(), incoming)
	if err != nil {
		return err
	}

	// After the events are safely on the ref, for the same reason the watermark
	// waits: a mapping recorded for a write that failed would tell the next run
	// that a work item is upstream when it is not.
	if err := led.Save(ledger, ledgerMessage(target.String(), len(items))); err != nil {
		return err
	}
	if !opts.sinceGiven {
		if err := state.Record(adoWatermark(items), adoSyncKeys(state, target, opts.all)...); err != nil {
			return err
		}
	}
	if err := report(pull, fmt.Sprintf("From %s", target), uniqueIDs(s)); err != nil {
		return err
	}
	cli.Repack(s.Repo, pull.Commits, os.Stderr)
	return nil
}

// recordADO files everything an import established about where an entity lives
// upstream. Adding a mapping that is already there changes nothing, so a
// re-import leaves the ledger — and its ref — exactly where it was.
//
// The area is not among them. It is a fact about the work item in Azure
// DevOps, and the ledger holds this repository's relationship with a tracker
// rather than a copy of the tracker.
func recordADO(ledger *origins.Ledger, id string, e adoissue.Entity) {
	recordADOIssue(ledger, id, e)
	for _, c := range e.Comments {
		ledger.Add(origins.KindComment, id, c.EventID, c.Upstream)
	}
}

// recordADOIssue files where the work item itself lives, and nothing about its
// comments — which is what the first of the two import passes may safely say.
func recordADOIssue(ledger *origins.Ledger, id string, e adoissue.Entity) {
	ledger.Add(origins.KindIssue, id, e.Origin)
	if e.URL != "" {
		ledger.Add(origins.KindURL, id, e.URL)
	}
}

// adoHost is the host a credential is looked up under, which is the API's
// host rather than the project's full path.
func adoHost(t adoapi.Target) string {
	host := strings.TrimPrefix(strings.TrimPrefix(t.Base, "https://"), "http://")
	if slash := strings.Index(host, "/"); slash >= 0 {
		host = host[:slash]
	}
	return host
}

func areaNote(t adoapi.Target) string {
	if t.Area == "" {
		return ""
	}
	return " under " + t.Area
}

// adoWatermark is the newest ChangedDate among the work items just imported.
//
// Safe to advance to even when --limit truncated the run. Items arrive ordered
// by ChangedDate ascending, so a truncated result is a prefix: every item left
// unread has a ChangedDate at or after this one, and the next run asks for
// exactly them.
func adoWatermark(items []adoapi.WorkItem) time.Time {
	var newest time.Time
	for _, w := range items {
		if w.ChangedAt.After(newest) {
			newest = w.ChangedAt
		}
	}
	return newest
}

// adoSyncKey names one project at one scope.
//
// The scope has two axes here rather than GitHub's one, and the area is the
// second: an import scoped to Web\Auth has read every open item under that
// area and says nothing whatever about Mobile.
func adoSyncKey(t adoapi.Target, all bool) string {
	scope := "open"
	if all {
		scope = "all"
	}
	return "ado:" + t.String() + ":" + t.Area + ":" + scope
}

// adoSyncKeys are the watermarks one run may advance.
//
// The rule is that a wider run may advance a narrower one's watermark and
// never the reverse. An --all run has necessarily seen every open item too,
// and a run over the whole project has seen everything a run over one of its
// areas would have — so both widenings advance the keys they cover. A narrower
// run advances only its own, because a later wider run starting from a point
// it never reached would silently skip everything changed before it.
//
// Existing keys are walked rather than enumerated, because the areas this
// clone has ever pulled are not knowable in advance. A key for an area nobody
// has pulled does not exist and needs no advancing.
func adoSyncKeys(state *entity.SyncState, t adoapi.Target, all bool) []string {
	keys := []string{adoSyncKey(t, all)}
	prefix := "ado:" + t.String() + ":"

	for key := range state.Sources {
		if key == keys[0] || !strings.HasPrefix(key, prefix) {
			continue
		}
		area, scope, ok := strings.Cut(strings.TrimPrefix(key, prefix), ":")
		if !ok {
			continue
		}
		// A narrower run never advances a wider key.
		if !under(area, t.Area) {
			continue
		}
		// An open-only run never advances an all-states key.
		if scope == "all" && !all {
			continue
		}
		keys = append(keys, key)
	}
	return keys
}

// under reports whether area is at or below root, the same containment the
// query's UNDER expresses. An empty root is the whole project and contains
// everything.
func under(area, root string) bool {
	if root == "" {
		return true
	}
	return strings.EqualFold(area, root) ||
		strings.HasPrefix(strings.ToLower(area), strings.ToLower(root)+"/")
}

// importErrorADO says which credential was used, so that a failure on a
// project somebody cannot see reads as an authorization problem rather than a
// typo, and turns the one API error a caller can act on into advice.
func importErrorADO(target adoapi.Target, source adoapi.TokenSource, err error) error {
	var e *adoapi.Error
	if !errors.As(err, &e) {
		return err
	}
	switch {
	case e.TooManyResults():
		return fmt.Errorf("%s: %w\n       narrow the area with 'ado:<remote>#<area>', or pass --since", target, e)
	case source == adoapi.FromNowhere:
		return fmt.Errorf("%s: %w\n       no Azure DevOps token found; set AZURE_DEVOPS_EXT_PAT, or run: az login", target, e)
	case e.Unauthorized():
		return fmt.Errorf("%s: %w\n       using the token from %s", target, e, source)
	default:
		return fmt.Errorf("%s: %w", target, e)
	}
}

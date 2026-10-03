package main

import (
	"fmt"

	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
	adoreview "github.com/hdweiss/git-issue/internal/bridge/ado/review"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/remote"
)

// pushADO writes local review changes back to Azure DevOps pull requests.
//
// The shape mirrors pushGitHub: read the tracker, recover an interrupted run,
// collect the candidates, build the pusher, then hand off to runBridgePush for
// the plan/confirm/apply/journal sequence both bridges share. What differs is
// only where the mutations land, and that a pull request is per-repository — so
// the target has to name one.
func pushADO(s *entity.Store, spec remote.Spec, opts pushOptions) error {
	if spec.ScopeSet {
		return fmt.Errorf("ado: a review push has no '#' scope; an area path is a work-item concept")
	}
	target, err := adoapi.ParseTarget(spec.URL)
	if err != nil {
		return fmt.Errorf("%s: %w", spec.Target, err)
	}
	if target.Repo == "" {
		return fmt.Errorf("%s names no repository; a review push needs the /_git/<repo> URL, not a collection/project slug", spec.Target)
	}
	tracker := target.String()

	token, source, cred := adoapi.Token(s.Repo, adoHost(target), opts.token)
	client := adoapi.New(token)

	// Azure DevOps compares project names case-insensitively, so this resolves
	// the canonical spelling the ledger is keyed under, and is also the first
	// request of the run — where a bad credential is reported rather than
	// several calls later.
	name, err := client.Project(target)
	if err != nil {
		return importErrorADOReview(target, source, err)
	}
	target.Project = name

	led, ledger, journal, err := recoverLedger(s, tracker)
	if err != nil {
		return err
	}

	candidates, err := collect(s, ledger, opts.ids)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		fmt.Println("Everything up-to-date")
		return nil
	}

	// Who the token authenticates as: the identity a vote is cast under, and the
	// key a pushed verdict is recorded on the ledger against.
	self, err := client.ConnectionData(target)
	if err != nil {
		return importErrorADOReview(target, source, err)
	}
	if cred != nil {
		s.Repo.CredentialApprove(*cred)
	}

	pusher := adoreview.NewPusher(client, target, s.Format(), s.Vocab, ledger, self)
	return runBridgePush(s, tracker, led, ledger, journal, candidates, pusher, opts, func() error {
		return pullADO(s, spec, pullOptions{all: true, full: true, token: opts.token})
	})
}

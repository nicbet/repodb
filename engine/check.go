package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/common/storage"
)

// CheckOptions selects the data commits Check verifies: the data head by
// default, Revision when set, or with All every commit in the data history.
type CheckOptions struct {
	Revision string
	All      bool
}

// CheckReport lists the outcome for each checked commit, in check order.
type CheckReport struct {
	Commits []CommitCheck
}

// CommitCheck is the outcome for one commit. Problems are integrity failures;
// Warnings are harmless anomalies, such as inventory objects no table reaches.
type CommitCheck struct {
	Commit   string
	Problems []string
	Warnings []string
}

// Problems counts the problems across every commit.
func (r CheckReport) Problems() int {
	total := 0
	for _, commit := range r.Commits {
		total += len(commit.Problems)
	}
	return total
}

// FailedCommits counts the commits with at least one problem.
func (r CheckReport) FailedCommits() int {
	failed := 0
	for _, commit := range r.Commits {
		if len(commit.Problems) != 0 {
			failed++
		}
	}
	return failed
}

// Check verifies data commits without trusting anything checked earlier in
// the process. For each commit it repeats the inventory checks of an open,
// reads every object by Git object ID and checks it against its name, and
// validates every table as ValidateSnapshot does. It bypasses the snapshot
// memo and the process-wide validation caches. Within one run, a Git object
// is read at most once, and a table or subtree validated for an earlier
// commit is reused only for a commit that provides the same Git objects.
//
// Corruption is reported in the CheckReport, and checking continues; the
// error reports failures to run the check, such as an unknown revision or a
// cancelled context. The uncheckpointed journal is not checked.
func Check(ctx context.Context, repo *repository.Repository, options CheckOptions) (CheckReport, error) {
	if repo == nil {
		return CheckReport{}, errors.New("repository is required")
	}
	commits, err := checkCommits(ctx, repo, options)
	if err != nil {
		return CheckReport{}, err
	}
	run := checkRun{repo: repo, cache: newValidationCache(), verified: make(map[string]struct{})}
	report := CheckReport{Commits: make([]CommitCheck, 0, len(commits))}
	for _, commit := range commits {
		result, err := run.commit(ctx, commit)
		if err != nil {
			return CheckReport{}, err
		}
		report.Commits = append(report.Commits, result)
	}
	return report, nil
}

func checkCommits(ctx context.Context, repo *repository.Repository, options CheckOptions) ([]string, error) {
	switch {
	case options.All && options.Revision != "":
		return nil, errors.New("check takes a revision or all commits, not both")
	case options.All:
		return repo.DataHistory(ctx)
	case options.Revision != "":
		commit, err := repo.ResolveCommit(ctx, options.Revision)
		if errors.Is(err, repodbgit.ErrRefNotFound) {
			return nil, fmt.Errorf("unknown revision %q", options.Revision)
		}
		if err != nil {
			return nil, err
		}
		return []string{commit}, nil
	default:
		head, err := repo.Head(ctx)
		if err != nil {
			return nil, err
		}
		if head == "" {
			return nil, repository.ErrNotInitialized
		}
		return []string{head}, nil
	}
}

// checkRun is the state one Check shares across commits.
type checkRun struct {
	repo  *repository.Repository
	cache *validationCache
	// verified holds the Git object IDs whose content was checked this run.
	verified map[string]struct{}
}

func (r *checkRun) commit(ctx context.Context, commit string) (CommitCheck, error) {
	result := CommitCheck{Commit: commit}
	snapshot, err := r.repo.LoadSnapshotUncached(ctx, commit)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		// Without a trustworthy inventory there is nothing to check
		// objects or tables against.
		result.Problems = append(result.Problems, err.Error())
		return result, nil
	}
	problems, err := snapshot.VerifyObjects(ctx, func(oid string) bool {
		_, ok := r.verified[oid]
		return ok
	})
	if err != nil {
		return result, err
	}
	failed := make(map[storage.Hash]struct{}, len(problems))
	for _, problem := range problems {
		failed[problem.Hash] = struct{}{}
		result.Problems = append(result.Problems, problem.String())
	}
	for _, ref := range snapshot.ObjectRefs(snapshot.Manifest.Objects) {
		if _, bad := failed[ref.Hash]; !bad {
			r.verified[ref.OID] = struct{}{}
		}
	}

	names := make([]string, 0, len(snapshot.Manifest.Tables))
	for name := range snapshot.Manifest.Tables {
		names = append(names, name)
	}
	sort.Strings(names)
	reached := make(map[storage.Hash]struct{}, len(snapshot.Manifest.Objects))
	complete := true
	for _, name := range names {
		objects, err := r.cache.validateTable(ctx, snapshot, name, snapshot.Manifest.Tables[name])
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			result.Problems = append(result.Problems, err.Error())
			complete = false
			continue
		}
		for _, hash := range objects {
			reached[hash] = struct{}{}
		}
	}
	// A failed table's reachable objects are unknown, so unreferenced objects
	// are reported only when every table was walked.
	if complete {
		for _, hash := range snapshot.Manifest.Objects {
			if _, ok := reached[hash]; !ok {
				result.Warnings = append(result.Warnings, fmt.Sprintf("unreferenced object %s", hash))
			}
		}
	}
	return result, nil
}

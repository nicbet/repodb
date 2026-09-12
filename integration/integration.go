// Package integration configures and synchronizes RepoDB's dedicated Git data
// history. It never modifies source branches, worktrees, indexes, or hooks.
package integration

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

var (
	ErrRemoteRequired = errors.New("an explicit RepoDB remote is required")
	ErrNotEnabled     = errors.New("RepoDB integration is not enabled for this remote")
)

type Status struct {
	Remote      string
	TrackingRef string
	LocalHead   string
	RemoteHead  string
	Action      string
}

var remoteName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func TrackingRef(remote string) (string, error) {
	if !remoteName.MatchString(remote) {
		return "", fmt.Errorf("invalid Git remote name %q for RepoDB tracking ref", remote)
	}
	return "refs/repodb/remotes/" + remote + "/data", nil
}

func Enable(ctx context.Context, start, remote string) (Status, error) {
	tracking, err := requireRemote(remote)
	if err != nil {
		return Status{}, err
	}
	cli := repodbgit.CLI{}
	info, err := cli.Discover(ctx, start)
	if err != nil {
		return Status{}, err
	}
	if _, err := cli.RemoteURL(ctx, info.TopLevel, remote); err != nil {
		return Status{}, err
	}
	spec := "+" + repository.DataRef + ":" + tracking
	values, err := cli.ConfigValues(ctx, info.TopLevel, "remote."+remote+".fetch")
	if err != nil {
		return Status{}, err
	}
	if !slices.Contains(values, spec) {
		if err := cli.AddConfig(ctx, info.TopLevel, "remote."+remote+".fetch", spec); err != nil {
			return Status{}, err
		}
	}
	if err := cli.SetConfig(ctx, info.TopLevel, "repodb.remote", remote); err != nil {
		return Status{}, err
	}

	remoteHead, exists, err := fetch(ctx, cli, info.TopLevel, remote, tracking)
	if err != nil {
		return Status{}, err
	}
	repo, err := repository.Discover(ctx, info.TopLevel)
	if err != nil {
		return Status{}, err
	}
	localHead, err := repo.Head(ctx)
	if err != nil {
		return Status{}, err
	}
	action := "enabled"
	if exists {
		remoteSnapshot, err := repo.SnapshotAt(ctx, tracking)
		if err != nil {
			return Status{}, fmt.Errorf("validate fetched RepoDB snapshot: %w", err)
		}
		if err := engine.ValidateSnapshot(ctx, remoteSnapshot); err != nil {
			return Status{}, fmt.Errorf("validate fetched RepoDB SQL graph: %w", err)
		}
		if localHead == "" {
			err = repo.WithPublicationLock(ctx, func(publication *repository.LockedPublication) error {
				_, err := publication.FastForwardSnapshot(ctx, "", remoteSnapshot)
				return err
			})
			if err != nil {
				return Status{}, err
			}
			localHead, action = remoteHead, "adopted-remote"
		}
	} else if localHead == "" {
		repo, err = repository.Init(ctx, info.TopLevel)
		if err != nil {
			return Status{}, err
		}
		local, err := repo.Current(ctx)
		if err != nil {
			return Status{}, err
		}
		localHead, action = local.Commit, "initialized-empty"
	}
	return Status{Remote: remote, TrackingRef: tracking, LocalHead: localHead, RemoteHead: remoteHead, Action: action}, nil
}

func Sync(ctx context.Context, start, remote string) (Status, error) {
	tracking, err := requireRemote(remote)
	if err != nil {
		return Status{}, err
	}
	cli := repodbgit.CLI{}
	info, err := cli.Discover(ctx, start)
	if err != nil {
		return Status{}, err
	}
	if _, err := cli.RemoteURL(ctx, info.TopLevel, remote); err != nil {
		return Status{}, err
	}
	spec := "+" + repository.DataRef + ":" + tracking
	values, err := cli.ConfigValues(ctx, info.TopLevel, "remote."+remote+".fetch")
	if err != nil {
		return Status{}, err
	}
	if !slices.Contains(values, spec) {
		return Status{}, fmt.Errorf("%w: run repodb enable --remote %s", ErrNotEnabled, remote)
	}
	repo, openedSnapshot, err := repository.OpenWithSnapshot(ctx, info.TopLevel)
	if err != nil {
		return Status{}, err
	}
	const maxAttempts = 3
	var last Status
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		localHead, err := repo.Head(ctx)
		if err != nil {
			return Status{}, err
		}
		remoteHead, remoteExists, err := fetch(ctx, cli, info.TopLevel, remote, tracking)
		last = Status{Remote: remote, TrackingRef: tracking, LocalHead: localHead, RemoteHead: remoteHead}
		if err != nil {
			last.Action = "fetch-failed"
			return last, err
		}
		if !remoteExists || localHead == remoteHead {
			if localHead == remoteHead {
				localSnapshot := openedSnapshot
				if localSnapshot == nil || localSnapshot.Commit != localHead {
					localSnapshot, err = repo.SnapshotAt(ctx, localHead)
					if err != nil {
						return last, fmt.Errorf("validate local RepoDB snapshot: %w", err)
					}
				}
				if err := engine.ValidateSnapshot(ctx, localSnapshot); err != nil {
					return last, fmt.Errorf("validate local RepoDB SQL graph: %w", err)
				}
				last.Action = "up-to-date"
				if err := clearConflictSet(repo, remote); err != nil {
					last.Action = "conflict-cleanup-failed"
					return last, err
				}
				return last, nil
			}
			err = pushExpected(ctx, repo, cli, info.TopLevel, remote, localHead)
			if err == nil {
				last.RemoteHead, _, err = fetch(ctx, cli, info.TopLevel, remote, tracking)
				if err != nil {
					last.Action = "pushed-local-fetch-failed"
					return last, err
				}
				last.Action = "pushed"
				if err := clearConflictSet(repo, remote); err != nil {
					last.Action = "conflict-cleanup-failed"
					return last, err
				}
				return last, nil
			}
			lastErr, last.Action = err, "push-retry"
			continue
		}

		remoteBehind, err := cli.IsAncestor(ctx, info.TopLevel, remoteHead, localHead)
		if err != nil {
			return last, err
		}
		if remoteBehind {
			if err := pushExpected(ctx, repo, cli, info.TopLevel, remote, localHead); err != nil {
				lastErr, last.Action = err, "push-retry"
				continue
			}
			last.RemoteHead, _, err = fetch(ctx, cli, info.TopLevel, remote, tracking)
			if err != nil {
				last.Action = "pushed-local-fetch-failed"
				return last, err
			}
			last.Action = "pushed"
			if err := clearConflictSet(repo, remote); err != nil {
				last.Action = "conflict-cleanup-failed"
				return last, err
			}
			return last, nil
		}
		localBehind, err := cli.IsAncestor(ctx, info.TopLevel, localHead, remoteHead)
		if err != nil {
			return last, err
		}
		if localBehind {
			remoteSnapshot, err := repo.SnapshotAt(ctx, remoteHead)
			if err != nil {
				return last, fmt.Errorf("validate fetched RepoDB snapshot: %w", err)
			}
			if err := engine.ValidateSnapshot(ctx, remoteSnapshot); err != nil {
				return last, fmt.Errorf("validate fetched RepoDB SQL graph: %w", err)
			}
			err = repo.WithPublicationLock(ctx, func(publication *repository.LockedPublication) error {
				_, err := publication.FastForwardSnapshot(ctx, localHead, remoteSnapshot)
				return err
			})
			if errors.Is(err, repository.ErrConflict) {
				lastErr, last.Action = err, "publication-retry"
				continue
			}
			if err != nil {
				last.Action = "publication-failed"
				return last, err
			}
			last.LocalHead, last.Action = remoteHead, "fast-forwarded-local"
			if err := clearConflictSet(repo, remote); err != nil {
				last.Action = "conflict-cleanup-failed"
				return last, err
			}
			return last, nil
		}

		baseHead, err := repo.MergeBase(ctx, localHead, remoteHead)
		if err != nil {
			return last, err
		}
		baseSnapshot, err := repo.SnapshotAt(ctx, baseHead)
		if err != nil {
			return last, err
		}
		localSnapshot := openedSnapshot
		if localSnapshot == nil || localSnapshot.Commit != localHead {
			localSnapshot, err = repo.SnapshotAt(ctx, localHead)
			if err != nil {
				return last, err
			}
		}
		remoteSnapshot, err := repo.SnapshotAt(ctx, remoteHead)
		if err != nil {
			return last, err
		}
		for _, item := range []struct {
			label    string
			snapshot *repository.Snapshot
		}{{"base", baseSnapshot}, {"local", localSnapshot}, {"remote", remoteSnapshot}} {
			if err := engine.ValidateSnapshot(ctx, item.snapshot); err != nil {
				return last, fmt.Errorf("validate %s RepoDB SQL graph: %w", item.label, err)
			}
		}
		writer, err := repo.BeginMergeSnapshots(localSnapshot, remoteSnapshot)
		if err != nil {
			return last, err
		}
		resolutions := map[string]engine.Resolution{}
		if prior, loadErr := loadConflictSet(repo, remote); loadErr == nil && prior.BaseHead == baseHead && prior.LocalHead == localHead && prior.RemoteHead == remoteHead {
			resolutions = prior.Resolutions
		}
		manifest, hashes, conflicts, err := engine.MergeSnapshots(ctx, writer, baseSnapshot, localSnapshot, remoteSnapshot, resolutions)
		if err != nil {
			last.Action = "merge-invalid"
			return last, err
		}
		if len(conflicts) != 0 {
			set := ConflictSet{Version: 1, Remote: remote, BaseHead: baseHead, LocalHead: localHead, RemoteHead: remoteHead, Conflicts: conflicts, Resolutions: resolutions}
			if err := saveConflictSet(repo, set); err != nil {
				return last, err
			}
			last.Action = "conflicts"
			return last, &MergeConflictError{Set: set}
		}
		if err := writer.RetainOnly(hashes); err != nil {
			return last, err
		}
		result, err := writer.CommitWithOutcome(ctx, manifest)
		if errors.Is(err, repository.ErrConflict) {
			lastErr, last.Action = err, "publication-retry"
			continue
		}
		if err != nil {
			last.Action = "publication-failed"
			return last, err
		}
		localHead = result.Commit
		last.LocalHead = localHead
		if err := pushExpected(ctx, repo, cli, info.TopLevel, remote, localHead); err != nil {
			lastErr, last.Action = err, "push-retry"
			continue
		}
		last.RemoteHead, _, err = fetch(ctx, cli, info.TopLevel, remote, tracking)
		if err != nil {
			last.Action = "merged-local-fetch-failed"
			return last, err
		}
		last.Action = "merged"
		if err := clearConflictSet(repo, remote); err != nil {
			last.Action = "conflict-cleanup-failed"
			return last, err
		}
		return last, nil
	}
	last.Action = "retry-exhausted"
	return last, fmt.Errorf("RepoDB synchronization did not stabilize after %d attempts; both local and tracking histories are preserved: %w", maxAttempts, lastErr)
}

func pushExpected(ctx context.Context, repo *repository.Repository, cli repodbgit.CLI, root, remote, expected string) error {
	return repo.WithPublicationLock(ctx, func(publication *repository.LockedPublication) error {
		actual, err := publication.Head(ctx)
		if err != nil {
			return err
		}
		if actual != expected {
			return repository.ErrConflict
		}
		return cli.PushCommit(ctx, root, remote, expected, repository.DataRef)
	})
}

func requireRemote(remote string) (string, error) {
	if remote == "" {
		return "", ErrRemoteRequired
	}
	return TrackingRef(remote)
}

func fetch(ctx context.Context, cli repodbgit.CLI, root, remote, tracking string) (string, bool, error) {
	_, err := cli.RemoteRef(ctx, root, remote, repository.DataRef)
	if errors.Is(err, repodbgit.ErrRemoteRefNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if err := cli.FetchRef(ctx, root, remote, repository.DataRef, tracking); err != nil {
		return "", false, err
	}
	head, err := cli.ResolveRef(ctx, root, tracking)
	return head, err == nil, err
}

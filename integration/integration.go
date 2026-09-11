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

type DivergenceError struct {
	Remote, LocalHead, RemoteHead, TrackingRef string
}

func (e *DivergenceError) Error() string {
	return fmt.Sprintf("RepoDB data histories diverged: local %s and %s %s; both refs were preserved (M4 reconciliation is required)", e.LocalHead, e.Remote, e.RemoteHead)
}

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
				_, err := publication.FastForward(ctx, "", remoteHead)
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
	repo, err := repository.Open(ctx, info.TopLevel)
	if err != nil {
		return Status{}, err
	}
	initialLocal, err := repo.Head(ctx)
	if err != nil {
		return Status{}, err
	}
	remoteHead, remoteExists, err := fetch(ctx, cli, info.TopLevel, remote, tracking)
	if err != nil {
		return Status{Remote: remote, TrackingRef: tracking, LocalHead: initialLocal, Action: "fetch-failed"}, err
	}
	if remoteExists {
		remoteSnapshot, err := repo.SnapshotAt(ctx, tracking)
		if err != nil {
			return Status{}, fmt.Errorf("validate fetched RepoDB snapshot: %w", err)
		}
		if err := engine.ValidateSnapshot(ctx, remoteSnapshot); err != nil {
			return Status{}, fmt.Errorf("validate fetched RepoDB SQL graph: %w", err)
		}
	}
	action := "up-to-date"
	localHead := ""
	pushed := false
	err = repo.WithPublicationLock(ctx, func(publication *repository.LockedPublication) error {
		var err error
		localHead, err = publication.Head(ctx)
		if err != nil {
			return err
		}
		if !remoteExists {
			if err := cli.PushRef(ctx, info.TopLevel, remote, repository.DataRef, repository.DataRef); err != nil {
				action = "push-failed"
				return err
			}
			action, pushed = "pushed", true
			return nil
		}
		if localHead == remoteHead {
			return nil
		}
		remoteBehind, err := cli.IsAncestor(ctx, info.TopLevel, remoteHead, localHead)
		if err != nil {
			return err
		}
		if remoteBehind {
			if err := cli.PushRef(ctx, info.TopLevel, remote, repository.DataRef, repository.DataRef); err != nil {
				action = "push-failed"
				return fmt.Errorf("remote changed or rejected RepoDB fast-forward: %w", err)
			}
			action, pushed = "pushed", true
			return nil
		}
		localBehind, err := cli.IsAncestor(ctx, info.TopLevel, localHead, remoteHead)
		if err != nil {
			return err
		}
		if localBehind {
			if _, err := publication.FastForward(ctx, localHead, remoteHead); err != nil {
				action = "publication-failed"
				return err
			}
			localHead, action = remoteHead, "fast-forwarded-local"
			return nil
		}
		action = "diverged"
		return &DivergenceError{Remote: remote, LocalHead: localHead, RemoteHead: remoteHead, TrackingRef: tracking}
	})
	if err != nil {
		return Status{Remote: remote, TrackingRef: tracking, LocalHead: localHead, RemoteHead: remoteHead, Action: action}, err
	}
	if pushed {
		remoteHead, _, err = fetch(ctx, cli, info.TopLevel, remote, tracking)
		if err != nil {
			return Status{Remote: remote, TrackingRef: tracking, LocalHead: localHead, Action: "pushed-local-fetch-failed"}, err
		}
	}
	return Status{Remote: remote, TrackingRef: tracking, LocalHead: localHead, RemoteHead: remoteHead, Action: action}, nil
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

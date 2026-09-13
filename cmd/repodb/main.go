package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/nicbet/repodb/client"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
	"github.com/nicbet/repodb/integration"
	repodbserver "github.com/nicbet/repodb/server"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "repodb:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: repodb <init|status|diff|commit|import-legacy|enable|sync|conflicts|resolve|start|sql>")
	}
	switch args[0] {
	case "init":
		set := flag.NewFlagSet("init", flag.ContinueOnError)
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		path := "."
		if set.NArg() > 0 {
			path = set.Arg(0)
		}
		repo, err := repository.Init(ctx, path)
		if err != nil {
			return err
		}
		snapshot, err := repo.Current(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("initialized RepoDB at %s (%s)\n", repository.DataRef, snapshot.Commit)
		return nil
	case "status":
		repo, err := repository.Open(ctx, ".")
		if err != nil {
			return err
		}
		snapshot, err := repo.Current(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("RepoDB data head: %s\n", snapshot.Commit)
		fmt.Printf("Format: %d, objects: %d, tables: %d\n", snapshot.Manifest.FormatVersion, len(snapshot.Manifest.Objects), len(snapshot.Manifest.Tables))
		working, _ := repository.OpenWorkingState(repo)
		if working.Exists() {
			state, err := working.Status(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("Working generation: %d (%s)\n", state.Generation, map[bool]string{true: "dirty", false: "clean"}[state.Dirty])
		}
		return nil
	case "diff":
		repo, err := repository.Open(ctx, ".")
		if err != nil {
			return err
		}
		working, _ := repository.OpenWorkingState(repo)
		changes, err := working.Diff(ctx)
		if err != nil {
			return err
		}
		if len(changes) == 0 {
			fmt.Println("No uncommitted data changes.")
			return nil
		}
		for _, change := range changes {
			fmt.Printf("%s\t%s\n", change.Change, change.Table)
		}
		return nil
	case "commit":
		set := flag.NewFlagSet("commit", flag.ContinueOnError)
		message := set.String("m", "", "data commit message")
		repoPath := set.String("repo", ".", "path inside the Git worktree")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if strings.TrimSpace(*message) == "" {
			return errors.New("commit requires -m <message>")
		}
		repo, err := repository.Open(ctx, *repoPath)
		if err != nil {
			return err
		}
		working, _ := repository.OpenWorkingState(repo)
		result, err := working.Checkpoint(ctx, *message)
		if err != nil {
			return err
		}
		fmt.Printf("RepoDB data commit: %s\n", result.Commit)
		return nil
	case "snapshot":
		return errors.New("snapshot is obsolete; RepoDB transactions publish data commits automatically")
	case "import-legacy":
		set := flag.NewFlagSet("import-legacy", flag.ContinueOnError)
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		path := "."
		if set.NArg() > 0 {
			path = set.Arg(0)
		}
		_, snapshot, err := repository.ImportLegacy(ctx, path)
		if err != nil {
			return err
		}
		fmt.Printf("imported legacy .repodb state at %s (%s); legacy files were retained\n", repository.DataRef, snapshot.Commit)
		return nil
	case "sql":
		return runSQL(ctx, args[1:])
	case "enable":
		set := flag.NewFlagSet("enable", flag.ContinueOnError)
		remote := set.String("remote", "", "explicit Git remote name")
		repoPath := set.String("repo", ".", "path inside the Git worktree")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		status, err := integration.Enable(ctx, *repoPath, *remote)
		if err != nil {
			return err
		}
		fmt.Printf("RepoDB enabled for %s: %s\nlocal: %s\ntracking: %s", status.Remote, status.Action, status.LocalHead, status.TrackingRef)
		if status.RemoteHead != "" {
			fmt.Printf(" (%s)", status.RemoteHead)
		}
		fmt.Println()
		return nil
	case "sync":
		set := flag.NewFlagSet("sync", flag.ContinueOnError)
		remote := set.String("remote", "", "explicit Git remote name")
		repoPath := set.String("repo", ".", "path inside the Git worktree")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		status, err := integration.Sync(ctx, *repoPath, *remote)
		if err != nil {
			if status.LocalHead != "" || status.RemoteHead != "" {
				fmt.Fprintf(os.Stderr, "local: %s\nremote tracking: %s (%s)\n", status.LocalHead, status.TrackingRef, status.RemoteHead)
			}
			return err
		}
		fmt.Printf("RepoDB sync with %s: %s\nlocal: %s\nremote tracking: %s (%s)\n", status.Remote, status.Action, status.LocalHead, status.TrackingRef, status.RemoteHead)
		return nil
	case "conflicts":
		set := flag.NewFlagSet("conflicts", flag.ContinueOnError)
		remote := set.String("remote", "", "explicit Git remote name")
		repoPath := set.String("repo", ".", "path inside the Git worktree")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		conflicts, err := integration.Conflicts(ctx, *repoPath, *remote)
		if err != nil {
			return err
		}
		fmt.Printf("RepoDB conflicts with %s (base %s, local %s, remote %s)\n", conflicts.Remote, conflicts.BaseHead, conflicts.LocalHead, conflicts.RemoteHead)
		for _, conflict := range conflicts.Conflicts {
			choice := "unresolved"
			if resolved, ok := conflicts.Resolutions[conflict.ID]; ok {
				choice = string(resolved)
			}
			fmt.Printf("%s\tkind=%s\ttable=%s\tkey=%s\t%s\n  base=%s\n  local=%s\n  remote=%s\n",
				conflict.ID, conflict.Kind, conflict.Table, conflict.Key, choice,
				conflictValue(conflict.BasePresent, conflict.Base),
				conflictValue(conflict.LocalPresent, conflict.Local),
				conflictValue(conflict.RemotePresent, conflict.Remote))
		}
		return nil
	case "resolve":
		set := flag.NewFlagSet("resolve", flag.ContinueOnError)
		remote := set.String("remote", "", "explicit Git remote name")
		repoPath := set.String("repo", ".", "path inside the Git worktree")
		id := set.String("id", "", "conflict ID from repodb conflicts")
		take := set.String("take", "", "resolution: local, remote, base, or delete")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if *id == "" || *take == "" {
			return errors.New("resolve requires --id and --take")
		}
		status, err := integration.Resolve(ctx, *repoPath, *remote, *id, engine.Resolution(*take))
		if err != nil {
			var conflictErr *integration.MergeConflictError
			if errors.As(err, &conflictErr) && len(conflictErr.Set.Unresolved()) > 0 {
				fmt.Printf("recorded resolution; %d conflict(s) remain\n", len(conflictErr.Set.Unresolved()))
				return nil
			}
			return err
		}
		fmt.Printf("RepoDB conflict resolved and synchronized: %s (%s)\n", status.Action, status.LocalHead)
		return nil
	case "start":
		set := flag.NewFlagSet("start", flag.ContinueOnError)
		address := set.String("addr", "127.0.0.1:3306", "MySQL listen address")
		repoPath := set.String("repo", ".", "path inside the Git worktree")
		persistence := set.String("persistence", string(engine.PersistenceNativeGit), "persistence mode: native-git or journal")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		repo, err := repository.Open(ctx, *repoPath)
		if err != nil {
			return err
		}
		srv, err := repodbserver.New(repodbserver.Config{Address: *address, Repository: repo, Persistence: engine.PersistenceMode(*persistence)})
		if err != nil {
			return err
		}
		fmt.Printf("RepoDB listening on %s (repository %s)\n", srv.Address(), repo.Root)
		return srv.Serve(ctx)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func conflictValue(present bool, value []byte) string {
	if !present {
		return "<absent>"
	}
	return string(value)
}

func runSQL(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("sql", flag.ContinueOnError)
	address := set.String("addr", "127.0.0.1:3306", "server address")
	database := set.String("database", "repodb", "database name")
	user := set.String("user", "root", "MySQL user")
	password := set.String("password", "", "MySQL password")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() == 0 {
		return errors.New("SQL statement is required")
	}
	cli, err := client.Open(client.Config{Address: *address, Database: *database, User: *user, Password: *password})
	if err != nil {
		return err
	}
	defer cli.Close()
	result, err := cli.Query(ctx, strings.Join(set.Args(), " "))
	if err != nil {
		return err
	}
	fmt.Println(strings.Join(result.Columns, "\t"))
	for _, row := range result.Rows {
		values := make([]string, len(row))
		for i, value := range row {
			if value == nil {
				values[i] = "NULL"
			} else {
				values[i] = *value
			}
		}
		fmt.Println(strings.Join(values, "\t"))
	}
	return nil
}

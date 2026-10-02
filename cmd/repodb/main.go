package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/nicbet/repodb/client"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
	"github.com/nicbet/repodb/integration"
	repodbserver "github.com/nicbet/repodb/server"
	"golang.org/x/term"
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
		return errors.New("usage: repodb <init|status|diff|commit|check|enable|sync|conflicts|resolve|start|sql>")
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
		result, err := checkpoint(ctx, *repoPath, *message)
		if err != nil {
			return err
		}
		fmt.Printf("RepoDB data commit: %s\n", result.Commit)
		return nil
	case "check":
		set := flag.NewFlagSet("check", flag.ContinueOnError)
		repoPath := set.String("repo", ".", "path inside the Git worktree")
		revision := set.String("revision", "", "data commit to check (default: the data head)")
		all := set.Bool("all", false, "check every commit in the data history")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if *all && *revision != "" {
			return errors.New("check takes --revision or --all, not both")
		}
		repo, err := repository.Open(ctx, *repoPath)
		if err != nil {
			return err
		}
		report, err := engine.Check(ctx, repo, engine.CheckOptions{Revision: *revision, All: *all})
		if err != nil {
			return err
		}
		return printCheckReport(os.Stdout, report)
	case "snapshot":
		return errors.New("snapshot is obsolete; RepoDB transactions publish data commits automatically")
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
		commit := set.Bool("commit", false, "commit (checkpoint) uncommitted journal changes before syncing, retrying if more arrive")
		message := set.String("m", "", "data commit message for --commit")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		var options integration.SyncOptions
		switch {
		case *commit:
			if strings.TrimSpace(*message) == "" {
				return errors.New("sync --commit requires -m <message>")
			}
			options.Checkpoint = checkpointFunc(*repoPath, *message)
		case *message != "":
			return errors.New("-m requires --commit")
		case term.IsTerminal(int(os.Stdin.Fd())):
			confirmed, err := promptCheckpointIfDirty(ctx, *repoPath)
			if err != nil {
				return err
			}
			if confirmed {
				options.Checkpoint = checkpointFunc(*repoPath, "checkpoint before sync")
			}
		}
		status, err := integration.SyncWithOptions(ctx, *repoPath, *remote, options)
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
		opts, err := parseStartOptions(args[1:])
		if err != nil {
			return err
		}
		repo, err := repository.Open(ctx, opts.repoPath)
		if err != nil {
			return err
		}
		srv, err := repodbserver.New(repodbserver.Config{Address: opts.address, Repository: repo, Persistence: opts.persistence, Durability: opts.durability})
		if err != nil {
			return err
		}
		fmt.Printf("RepoDB listening on %s (repository %s, %s persistence, %s durability)\n", srv.Address(), repo.Root, opts.persistence, opts.durability)
		return srv.Serve(ctx)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// printCheckReport prints one line per problem and warning, an ok line for
// each commit without problems, and a summary. It returns an error if any
// commit has problems.
func printCheckReport(out io.Writer, report engine.CheckReport) error {
	for _, commit := range report.Commits {
		for _, problem := range commit.Problems {
			fmt.Fprintf(out, "%s\t%s\n", commit.Commit, problem)
		}
		for _, warning := range commit.Warnings {
			fmt.Fprintf(out, "%s\twarning: %s\n", commit.Commit, warning)
		}
		if len(commit.Problems) == 0 {
			fmt.Fprintf(out, "ok %s\n", commit.Commit)
		}
	}
	if problems := report.Problems(); problems != 0 {
		fmt.Fprintf(out, "checked %d commit(s): %d problem(s) in %d commit(s)\n", len(report.Commits), problems, report.FailedCommits())
		return fmt.Errorf("check failed: %d problem(s) in %d commit(s)", problems, report.FailedCommits())
	}
	fmt.Fprintf(out, "checked %d commit(s): no problems\n", len(report.Commits))
	return nil
}

func conflictValue(present bool, value []byte) string {
	if !present {
		return "<absent>"
	}
	return string(value)
}

type startOptions struct {
	address, repoPath string
	persistence       engine.PersistenceMode
	durability        repository.Durability
}

func parseStartOptions(args []string) (startOptions, error) {
	set := flag.NewFlagSet("start", flag.ContinueOnError)
	address := set.String("addr", "127.0.0.1:3306", "MySQL listen address")
	repoPath := set.String("repo", ".", "path inside the Git worktree")
	persistence := set.String("persistence", string(engine.PersistenceJournal), "persistence mode: journal (default) or native-git (audit mode: every transaction is a Git commit)")
	durability := set.String("durability", string(repository.DurabilityNormal), "journal commit durability: normal (default; survives process and OS crashes), full (also survives power loss) or off (survives process crashes)")
	if err := set.Parse(args); err != nil {
		return startOptions{}, err
	}
	level, err := repository.ParseDurability(*durability)
	if err != nil {
		return startOptions{}, err
	}
	return startOptions{address: *address, repoPath: *repoPath, persistence: engine.PersistenceMode(*persistence), durability: level}, nil
}

// promptCheckpointIfDirty asks whether to commit a dirty journal before
// syncing. It reports true when the user agreed; a clean journal needs no
// answer.
func promptCheckpointIfDirty(ctx context.Context, repoPath string) (bool, error) {
	repo, err := repository.Open(ctx, repoPath)
	if err != nil {
		return false, err
	}
	working, err := repository.OpenWorkingState(repo)
	if err != nil {
		return false, err
	}
	if !working.Exists() {
		return false, nil
	}
	ws, err := working.Status(ctx)
	if err != nil {
		return false, err
	}
	if !ws.Dirty {
		return false, nil
	}
	fmt.Fprintf(os.Stderr, "Journal has uncommitted changes at generation %d. Checkpoint before syncing? [y/N] ", ws.Generation)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, dirtySyncError(ws.Generation)
	}
	if answer := strings.TrimSpace(line); answer != "y" && answer != "Y" {
		return false, dirtySyncError(ws.Generation)
	}
	return true, nil
}

func dirtySyncError(generation uint64) error {
	return fmt.Errorf("%w at generation %d; run repodb sync --commit -m <message>, or repodb diff and repodb commit -m <message> first", integration.ErrWorkingDirty, generation)
}

// checkpointFunc returns a checkpoint step for SyncWithOptions that reports
// each data commit it makes.
func checkpointFunc(repoPath, message string) func(context.Context) error {
	return func(ctx context.Context) error {
		result, err := checkpoint(ctx, repoPath, message)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "RepoDB data commit: %s\n", result.Commit)
		return nil
	}
}

// checkpoint publishes the journal through the engine, which materializes
// pending typed row edits into Prolly trees before committing.
func checkpoint(ctx context.Context, repoPath, message string) (repository.CommitResult, error) {
	eng, err := engine.OpenWithOptions(ctx, repoPath, engine.Options{Persistence: engine.PersistenceJournal})
	if err != nil {
		return repository.CommitResult{}, err
	}
	result, err := eng.Checkpoint(ctx, message)
	return result, errors.Join(err, eng.Close())
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

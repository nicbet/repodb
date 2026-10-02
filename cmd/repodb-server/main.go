package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
	repodbserver "github.com/nicbet/repodb/server"
)

type options struct {
	address, repoPath string
	persistence       engine.PersistenceMode
	durability        repository.Durability
}

func parseOptions(args []string) (options, error) {
	set := flag.NewFlagSet("repodb-server", flag.ContinueOnError)
	address := set.String("addr", "127.0.0.1:3306", "MySQL listen address")
	repoPath := set.String("repo", ".", "path inside the Git worktree")
	persistence := set.String("persistence", string(engine.PersistenceJournal), "persistence mode: journal (default) or native-git (audit mode: every transaction is a Git commit)")
	durability := set.String("durability", string(repository.DurabilityNormal), "journal commit durability: normal (default; survives process and OS crashes), full (also survives power loss) or off (survives process crashes)")
	if err := set.Parse(args); err != nil {
		return options{}, err
	}
	level, err := repository.ParseDurability(*durability)
	if err != nil {
		return options{}, err
	}
	return options{address: *address, repoPath: *repoPath, persistence: engine.PersistenceMode(*persistence), durability: level}, nil
}

func main() {
	opts, err := parseOptions(os.Args[1:])
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "repodb-server:", err)
		}
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	repo, err := repository.Open(ctx, opts.repoPath)
	if err != nil {
		log.Fatal(err)
	}
	srv, err := repodbserver.New(repodbserver.Config{
		Address:     opts.address,
		Repository:  repo,
		Persistence: opts.persistence,
		Durability:  opts.durability,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("RepoDB listening on %s (repository %s, %s persistence, %s durability)\n", srv.Address(), repo.Root, opts.persistence, opts.durability)
	if err := srv.Serve(ctx); err != nil {
		log.Fatal(err)
	}
}

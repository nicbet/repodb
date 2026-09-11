package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/nicbet/repodb/common/repository"
	repodbserver "github.com/nicbet/repodb/server"
)

func main() {
	address := flag.String("addr", "127.0.0.1:3306", "MySQL listen address")
	database := flag.String("database", "repodb", "default database name")
	repoPath := flag.String("repo", ".", "path inside the Git worktree")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	repo, err := repository.Open(ctx, *repoPath)
	if err != nil {
		log.Fatal(err)
	}
	srv, err := repodbserver.New(repodbserver.Config{
		Address:      *address,
		DatabaseName: *database,
		Repository:   repo,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("RepoDB listening on %s (repository %s)\n", srv.Address(), repo.Root)
	if err := srv.Serve(ctx); err != nil {
		log.Fatal(err)
	}
}

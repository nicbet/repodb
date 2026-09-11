package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/nicbet/repodb/client"
	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/repository"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "repodb:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: repodb <init|status|snapshot|sql>")
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
		fmt.Println("initialized RepoDB in", repo.Dir)
		return nil
	case "status":
		repo, err := repository.Open(ctx, ".")
		if err != nil {
			return err
		}
		status, err := (repodbgit.CLI{}).Status(ctx, repo.Root)
		if err != nil {
			return err
		}
		if status == "" {
			fmt.Println("RepoDB state is clean")
		} else {
			fmt.Print(status)
		}
		return nil
	case "snapshot":
		set := flag.NewFlagSet("snapshot", flag.ContinueOnError)
		message := set.String("m", "", "Git commit message")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		repo, err := repository.Open(ctx, ".")
		if err != nil {
			return err
		}
		git := repodbgit.CLI{}
		if err := git.StageRepoDB(ctx, repo.Root); err != nil {
			return err
		}
		return git.Commit(ctx, repo.Root, *message)
	case "sql":
		return runSQL(ctx, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
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

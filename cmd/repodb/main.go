package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/nicbet/repodb/client"
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
		return errors.New("usage: repodb <init|status|import-legacy|sql>")
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

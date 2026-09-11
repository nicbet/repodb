package server_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicbet/repodb/client"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
	"github.com/nicbet/repodb/server"
)

func TestMySQLWireRoundTrip(t *testing.T) {
	root := t.TempDir()
	git := exec.Command("git", "init", "--quiet", "-b", "main")
	git.Dir = root
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := repository.Init(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{
		Address:      "127.0.0.1:0",
		DatabaseName: "repodb",
		Repository:   repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Start() }()
	t.Cleanup(func() { _ = srv.Close() })

	cli, err := client.Open(client.Config{Address: srv.Address(), Database: "repodb"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Exec(ctx, "CREATE TABLE people (id BIGINT PRIMARY KEY, name TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Exec(ctx, "INSERT INTO people VALUES (?, ?)", 1, "Ada"); err != nil {
		t.Fatal(err)
	}
	result, err := cli.Query(ctx, "SELECT name FROM people WHERE id = ?", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] == nil || *result.Rows[0][0] != "Ada" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if err := cli.Close(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := repository.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := server.New(server.Config{Address: "127.0.0.1:0", Repository: reopened})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = restarted.Start() }()
	defer restarted.Close()
	restartClient, err := client.Open(client.Config{Address: restarted.Address(), Database: "repodb"})
	if err != nil {
		t.Fatal(err)
	}
	defer restartClient.Close()
	result, err = restartClient.Query(ctx, "SELECT name FROM people WHERE id = ?", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] == nil || *result.Rows[0][0] != "Ada" {
		t.Fatalf("unexpected result after restart: %#v", result)
	}
}

func TestEmbeddedWriteIsReadableAfterServerRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	git := exec.Command("git", "init", "--quiet", "-b", "main")
	git.Dir = root
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	repo, err := repository.Init(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := engine.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := embedded.NewSession()
	if err := session.Exec(ctx, "CREATE TABLE issues (id BIGINT PRIMARY KEY, title TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := session.Exec(ctx, "INSERT INTO issues VALUES (?, ?)", 7, "from embedded"); err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	_ = embedded.Close()

	first, err := server.New(server.Config{Address: "127.0.0.1:0", Repository: repo})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = first.Start() }()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := repository.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := server.New(server.Config{Address: "127.0.0.1:0", Repository: reopened})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = second.Start() }()
	defer second.Close()
	cli, err := client.Open(client.Config{Address: second.Address(), Database: "repodb"})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	result, err := cli.Query(ctx, "SELECT title FROM issues WHERE id = ?", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] == nil || *result.Rows[0][0] != "from embedded" {
		t.Fatalf("rows = %#v", result.Rows)
	}
}

package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/nicbet/repodb/client"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/server"
)

func TestMySQLWireRoundTrip(t *testing.T) {
	repo := &repository.Repository{Root: t.TempDir(), Dir: t.TempDir()}
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
	defer cli.Close()
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
}

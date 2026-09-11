package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	repodbgit "github.com/nicbet/repodb/common/git"
	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

type measurement struct {
	Rows                     int     `json:"rows"`
	Updates                  int     `json:"updates"`
	OpenMilliseconds         float64 `json:"open_ms"`
	BeginMilliseconds        float64 `json:"begin_ms"`
	InsertCommitMilliseconds float64 `json:"insert_commit_ms"`
	UpdateMilliseconds       float64 `json:"updates_ms"`
	HeapBytes                uint64  `json:"heap_bytes"`
	Objects                  int     `json:"live_objects"`
	GitProcesses             uint64  `json:"git_processes"`
}

func main() {
	root := flag.String("root", "/tmp/repodb-m2", "benchmark Git worktree")
	rows := flag.Int("rows", 100, "rows in initial transaction")
	updates := flag.Int("updates", 10, "repeated autocommit updates")
	flag.Parse()
	if err := run(*root, *rows, *updates); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(root string, rows, updates int) error {
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	cmd := exec.Command("git", "init", "--quiet", "-b", "main")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, output)
	}
	ctx := context.Background()
	if _, err := repository.Init(ctx, root); err != nil {
		return err
	}
	repodbgit.ResetProcessCount()
	started := time.Now()
	eng, err := engine.Open(ctx, root)
	if err != nil {
		return err
	}
	openTime := time.Since(started)
	defer eng.Close()
	session, _ := eng.NewSession()
	defer session.Close()
	if err := session.Exec(ctx, "CREATE TABLE bench (id BIGINT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		return err
	}
	started = time.Now()
	tx, err := session.Begin(ctx)
	if err != nil {
		return err
	}
	beginTime := time.Since(started)
	for i := 0; i < rows; i++ {
		if err := tx.Exec(ctx, "INSERT INTO bench VALUES (?, ?)", i, fmt.Sprintf("value-%d", i)); err != nil {
			return err
		}
	}
	started = time.Now()
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	insertTime := time.Since(started)
	started = time.Now()
	for i := 0; i < updates; i++ {
		if err := session.Exec(ctx, "UPDATE bench SET value = ? WHERE id = 0", fmt.Sprintf("revision-%d", i)); err != nil {
			return err
		}
	}
	updateTime := time.Since(started)
	snapshot, err := eng.Repository().Current(ctx)
	if err != nil {
		return err
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	result := measurement{Rows: rows, Updates: updates, OpenMilliseconds: ms(openTime), BeginMilliseconds: ms(beginTime), InsertCommitMilliseconds: ms(insertTime), UpdateMilliseconds: ms(updateTime), HeapBytes: memory.HeapAlloc, Objects: len(snapshot.Manifest.Objects), GitProcesses: repodbgit.ProcessCount()}
	data, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(data))
	return nil
}

func ms(duration time.Duration) float64 { return float64(duration.Microseconds()) / 1000 }

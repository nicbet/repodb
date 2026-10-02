package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicbet/repodb/engine"
	"github.com/nicbet/repodb/integration"
)

// tracedGitCommands runs fn with Git's trace2 event log enabled and returns how
// often each Git subcommand started.
func tracedGitCommands(t *testing.T, fn func()) map[string]int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace2.json")
	t.Setenv("GIT_TRACE2_EVENT", path)
	fn()
	os.Unsetenv("GIT_TRACE2_EVENT")
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]int{}
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	counts := map[string]int{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var event struct {
			Event string   `json:"event"`
			SID   string   `json:"sid"`
			Argv  []string `json:"argv"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		// Child processes Git starts itself carry a nested session ID.
		if event.Event != "start" || strings.Contains(event.SID, "/") {
			continue
		}
		args := event.Argv[1:]
		for len(args) > 1 && args[0] == "-c" {
			args = args[2:]
		}
		if len(args) > 0 {
			counts[args[0]]++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return counts
}

// Sync learns the remote head from its own push, and only fetches when the
// remote has moved past the tracking ref.
func TestSyncTrackingRefWithoutRedundantFetches(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	git(t, root, "init", "--quiet", "--bare", remote)
	git(t, root, "init", "--quiet", "-b", "main", a)
	git(t, a, "remote", "add", "origin", remote)
	if _, err := integration.Enable(ctx, a, "origin"); err != nil {
		t.Fatal(err)
	}
	eng, err := engine.OpenWithOptions(ctx, a, engine.Options{Persistence: engine.PersistenceNativeGit})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	session, _ := eng.NewSession()
	if err := session.Exec(ctx, "CREATE TABLE notes (id BIGINT PRIMARY KEY, body TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}

	var status integration.Status
	pushed := tracedGitCommands(t, func() {
		status, err = integration.Sync(ctx, a, "origin")
	})
	if err != nil || status.Action != "pushed" {
		t.Fatalf("sync = %#v, %v", status, err)
	}
	if pushed["fetch"] != 0 {
		t.Fatalf("pushing sync ran %d fetches, want 0", pushed["fetch"])
	}
	tracking := git(t, a, "rev-parse", "refs/repodb/remotes/origin/data")
	onRemote := git(t, remote, "rev-parse", "refs/repodb/data")
	if status.RemoteHead != status.LocalHead || tracking != status.LocalHead || onRemote != status.LocalHead {
		t.Fatalf("after push: status %#v, tracking %s, remote %s", status, tracking, onRemote)
	}

	upToDate := tracedGitCommands(t, func() {
		status, err = integration.Sync(ctx, a, "origin")
	})
	if err != nil || status.Action != "up-to-date" {
		t.Fatalf("second sync = %#v, %v", status, err)
	}
	if upToDate["fetch"] != 0 || upToDate["ls-remote"] != 1 {
		t.Fatalf("up-to-date sync ran fetch %d times and ls-remote %d times, want 0 and 1", upToDate["fetch"], upToDate["ls-remote"])
	}

	// A remote that moved is still fetched.
	git(t, root, "clone", "--quiet", remote, b)
	if _, err := integration.Enable(ctx, b, "origin"); err != nil {
		t.Fatal(err)
	}
	if err := session.Exec(ctx, "INSERT INTO notes VALUES (1, 'moved')"); err != nil {
		t.Fatal(err)
	}
	if status, err := integration.Sync(ctx, a, "origin"); err != nil || status.Action != "pushed" {
		t.Fatalf("sync a = %#v, %v", status, err)
	}
	pulled := tracedGitCommands(t, func() {
		status, err = integration.Sync(ctx, b, "origin")
	})
	if err != nil || status.Action != "fast-forwarded-local" {
		t.Fatalf("sync b = %#v, %v", status, err)
	}
	if pulled["fetch"] != 1 {
		t.Fatalf("sync after the remote moved ran %d fetches, want 1", pulled["fetch"])
	}
	if got := git(t, b, "rev-parse", "refs/repodb/data"); got != git(t, remote, "rev-parse", "refs/repodb/data") {
		t.Fatalf("b data head %s differs from the remote", got)
	}
}

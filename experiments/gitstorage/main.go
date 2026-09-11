// Command gitstorage runs RepoDB's M0 Git integration experiment.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	dataRef     = "refs/repodb/data"
	trackingRef = "refs/repodb/remotes/origin/data"
)

type experiment struct {
	root string
	out  io.Writer
}

func main() {
	root := flag.String("root", "/tmp/repodb-m0", "workspace to recreate for the experiment")
	flag.Parse()
	if err := run(*root, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gitstorage:", err)
		os.Exit(1)
	}
}

func run(root string, out io.Writer) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if root == string(filepath.Separator) || root == "/tmp" || root == os.TempDir() {
		return fmt.Errorf("refusing unsafe experiment root %q", root)
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("reset workspace: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	e := &experiment{root: root, out: out}
	if err := e.execute(); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nM0 Git storage experiment passed.\nWorkspace: %s\n", root)
	return nil
}

func (e *experiment) execute() error {
	remote := filepath.Join(e.root, "remote.git")
	seed := filepath.Join(e.root, "seed")
	cloneA := filepath.Join(e.root, "clone-a")
	cloneB := filepath.Join(e.root, "clone-b")
	cloneC := filepath.Join(e.root, "clone-after-gc")
	linked := filepath.Join(e.root, "clone-a-linked")

	if _, err := e.git(e.root, nil, "init", "--bare", remote); err != nil {
		return err
	}
	if _, err := e.git(e.root, nil, "init", "-b", "main", seed); err != nil {
		return err
	}
	if err := e.identity(seed); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("source history\n"), 0o644); err != nil {
		return err
	}
	if _, err := e.git(seed, nil, "add", "README.md"); err != nil {
		return err
	}
	if _, err := e.git(seed, nil, "commit", "-m", "initial source"); err != nil {
		return err
	}
	if _, err := e.git(seed, nil, "remote", "add", "origin", remote); err != nil {
		return err
	}
	if _, err := e.git(seed, nil, "push", "-u", "origin", "main"); err != nil {
		return err
	}
	if _, err := e.git(remote, nil, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return err
	}
	for _, clone := range []string{cloneA, cloneB} {
		if _, err := e.git(e.root, nil, "clone", remote, clone); err != nil {
			return err
		}
		if err := e.identity(clone); err != nil {
			return err
		}
	}
	e.pass("created a bare remote and two ordinary clones")

	sourceHead, err := e.git(cloneA, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	statusBefore, err := e.git(cloneA, nil, "status", "--porcelain=v1")
	if err != nil {
		return err
	}
	first, _, err := e.snapshot(cloneA, "", "synthetic row chunk v1", "initial database")
	if err != nil {
		return err
	}
	if _, err := e.git(cloneA, nil, "update-ref", dataRef, first, strings.Repeat("0", 40)); err != nil {
		return fmt.Errorf("create data ref with compare-and-swap: %w", err)
	}
	if err := e.assertSourceUnchanged(cloneA, sourceHead, statusBefore); err != nil {
		return err
	}
	if _, err := e.git(cloneA, nil, "push", "origin", dataRef+":"+dataRef); err != nil {
		return err
	}
	e.pass("published a complete synthetic snapshot without changing source HEAD, index, or worktree")

	// A stale writer builds from the same parent but cannot replace a newer head.
	winner, winnerChunkPath, err := e.snapshot(cloneA, first, "winner row chunk", "winning transaction")
	if err != nil {
		return err
	}
	loser, _, err := e.snapshot(cloneA, first, "loser row chunk", "stale transaction")
	if err != nil {
		return err
	}
	if _, err := e.git(cloneA, nil, "update-ref", dataRef, winner, first); err != nil {
		return err
	}
	if _, err := e.git(cloneA, nil, "update-ref", dataRef, loser, first); err == nil {
		return errors.New("stale compare-and-swap unexpectedly succeeded")
	}
	e.pass("rejected a stale compare-and-swap ref publication")

	if _, err := e.git(cloneA, nil, "worktree", "add", "-b", "m0-linked", linked); err != nil {
		return err
	}
	commonA, err := e.commonDir(cloneA)
	if err != nil {
		return err
	}
	commonLinked, err := e.commonDir(linked)
	if err != nil {
		return err
	}
	if commonA != commonLinked {
		return fmt.Errorf("linked worktree common dir mismatch: %q != %q", commonA, commonLinked)
	}
	linkedHead, err := e.git(linked, nil, "rev-parse", dataRef)
	if err != nil || linkedHead != winner {
		return fmt.Errorf("linked worktree did not see repository data ref")
	}
	e.pass("discovered shared repository state and refs from a linked worktree")

	// Preserve a pre-existing refspec, add only RepoDB's tracking refspec, and
	// leave push selection alone because explicit pushes override its defaults.
	preexisting := "+refs/tags/*:refs/tags/*"
	if _, err := e.git(cloneB, nil, "config", "--add", "remote.origin.fetch", preexisting); err != nil {
		return err
	}
	if err := e.enableFetch(cloneB); err != nil {
		return err
	}
	if err := e.enableFetch(cloneB); err != nil {
		return err
	}
	fetchSpecs, err := e.git(cloneB, nil, "config", "--get-all", "remote.origin.fetch")
	if err != nil {
		return err
	}
	if strings.Count(fetchSpecs, trackingRef) != 1 || !strings.Contains(fetchSpecs, preexisting) {
		return fmt.Errorf("enable did not preserve and idempotently extend fetch refspecs: %q", fetchSpecs)
	}
	if _, err := e.git(cloneB, nil, "fetch", "origin"); err != nil {
		return err
	}
	tracked, err := e.git(cloneB, nil, "rev-parse", trackingRef)
	if err != nil || tracked != first {
		return fmt.Errorf("configured fetch did not populate tracking ref")
	}
	e.pass("idempotently extended fetch configuration while preserving existing refspecs")

	hooksDir := commonAFor(cloneB)
	hooksDir = filepath.Join(hooksDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	prePush := filepath.Join(hooksDir, "pre-push")
	if err := os.WriteFile(prePush, []byte("#!/bin/sh\nprintf 'existing\\n' >> \"$REPODB_M0_HOOK_LOG\"\n"), 0o755); err != nil {
		return err
	}
	if err := e.composeHooks(cloneB); err != nil {
		return err
	}
	hookLog := filepath.Join(e.root, "hook-order.log")
	if _, err := e.git(cloneB, []string{"REPODB_M0_HOOK_LOG=" + hookLog}, "push", "origin", "main"); err != nil {
		return fmt.Errorf("run composed hook through git push: %w", err)
	}
	logData, err := os.ReadFile(hookLog)
	if err != nil {
		return err
	}
	if string(logData) != "existing\nrepodb\n" {
		return fmt.Errorf("unexpected composed hook order: %q", logData)
	}
	if err := e.composeHooks(cloneB); err != nil {
		return err
	}
	e.pass("composed and preserved a pre-existing pre-push hook idempotently")

	// Plain and explicitly scoped source pushes omit the custom data ref.
	if err := appendCommit(cloneA, "README.md", "plain push\n", "plain source push"); err != nil {
		return err
	}
	if _, err := e.git(cloneA, nil, "push"); err != nil {
		return err
	}
	remoteData, err := e.git(remote, nil, "rev-parse", dataRef)
	if err != nil {
		return err
	}
	if remoteData != first {
		return errors.New("plain push unexpectedly transported the data ref")
	}
	if err := appendCommit(cloneA, "README.md", "explicit branch push\n", "explicit source push"); err != nil {
		return err
	}
	if _, err := e.git(cloneA, nil, "push", "origin", "main"); err != nil {
		return err
	}
	remoteData, err = e.git(remote, nil, "rev-parse", dataRef)
	if err != nil || remoteData != first {
		return errors.New("explicit branch push unexpectedly transported the data ref")
	}
	e.pass("confirmed plain and explicit branch pushes omit RepoDB data")

	// Pull --no-rebase invokes fetch and therefore updates the data tracking ref.
	if _, err := e.git(cloneB, nil, "pull", "--no-rebase"); err != nil {
		return err
	}
	if _, err := e.git(cloneA, nil, "push", "origin", dataRef+":"+dataRef); err != nil {
		return err
	}
	if err := appendCommit(cloneA, "README.md", "rebase pull\n", "remote source for rebase"); err != nil {
		return err
	}
	if _, err := e.git(cloneA, nil, "push", "origin", "main"); err != nil {
		return err
	}
	if err := appendCommit(cloneB, "LOCAL.md", "local source\n", "local source before rebase"); err != nil {
		return err
	}
	if _, err := e.git(cloneB, nil, "pull", "--rebase"); err != nil {
		return err
	}
	tracked, err = e.git(cloneB, nil, "rev-parse", trackingRef)
	if err != nil || tracked != winner {
		return errors.New("pull --rebase did not fetch the configured data ref")
	}
	e.pass("confirmed fetch and pull (merge/rebase) update only the remote-tracking data ref")

	// A failed transport does not affect the locally durable data snapshot.
	remoteURL, err := e.git(cloneA, nil, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	missing := filepath.Join(e.root, "offline-remote.git")
	if _, err := e.git(cloneA, nil, "remote", "set-url", "origin", missing); err != nil {
		return err
	}
	if _, err := e.git(cloneA, nil, "fetch", "origin"); err == nil {
		return errors.New("offline fetch unexpectedly succeeded")
	}
	localData, err := e.git(cloneA, nil, "show", dataRef+":"+winnerChunkPath)
	if err != nil || localData == "" {
		return errors.New("local database became unreadable after transport failure")
	}
	if _, err := e.git(cloneA, nil, "remote", "set-url", "origin", remoteURL); err != nil {
		return err
	}
	e.pass("kept local data readable while the remote was unavailable")

	// Repack/prune the remote, fetch into a fresh clone, remove an unrelated
	// rebuildable cache, repack locally, and prove every snapshot blob is reachable.
	if _, err := e.git(remote, nil, "repack", "-Ad"); err != nil {
		return err
	}
	if _, err := e.git(remote, nil, "prune", "--expire=now"); err != nil {
		return err
	}
	if _, err := e.git(e.root, nil, "clone", remote, cloneC); err != nil {
		return err
	}
	if err := e.enableFetch(cloneC); err != nil {
		return err
	}
	if _, err := e.git(cloneC, nil, "fetch", "origin"); err != nil {
		return err
	}
	cache := filepath.Join(cloneC, ".git", "repodb", "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cache, "discardable"), []byte("cache"), 0o644); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(cloneC, ".git", "repodb")); err != nil {
		return err
	}
	if _, err := e.git(cloneC, nil, "gc", "--prune=now"); err != nil {
		return err
	}
	if _, err := e.git(cloneC, nil, "fsck", "--full", "--no-dangling"); err != nil {
		return err
	}
	if _, err := e.git(cloneC, nil, "show", trackingRef+":manifest.json"); err != nil {
		return fmt.Errorf("manifest unreachable after gc: %w", err)
	}
	if _, err := e.git(cloneC, nil, "show", trackingRef+":objects/sha256/"+hashPath("winner row chunk")); err != nil {
		return fmt.Errorf("chunk unreachable after gc and cache deletion: %w", err)
	}
	e.pass("reconstructed the complete data snapshot after remote/local GC and cache deletion")

	for _, repo := range []string{cloneA, cloneB, cloneC, linked} {
		status, err := e.git(repo, nil, "status", "--porcelain=v1")
		if err != nil {
			return err
		}
		if status != "" {
			return fmt.Errorf("source state is dirty in %s: %s", repo, status)
		}
	}
	e.pass("finished with every source worktree and index clean")
	return nil
}

func (e *experiment) snapshot(repo, parent, chunk, message string) (string, string, error) {
	sum := sha256.Sum256([]byte(chunk))
	digest := hex.EncodeToString(sum[:])
	chunkPath := "objects/sha256/" + digest[:2] + "/" + digest[2:]
	manifest := fmt.Sprintf("{\"format_version\":1,\"data_root\":%q}\n", digest)
	chunkOID, err := e.gitInput(repo, []byte(chunk), nil, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", "", err
	}
	manifestOID, err := e.gitInput(repo, []byte(manifest), nil, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", "", err
	}
	index := filepath.Join(e.root, ".snapshot-index")
	_ = os.Remove(index)
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := e.git(repo, env, "read-tree", "--empty"); err != nil {
		return "", "", err
	}
	for path, oid := range map[string]string{"manifest.json": manifestOID, chunkPath: chunkOID} {
		if _, err := e.git(repo, env, "update-index", "--add", "--cacheinfo", "100644,"+oid+","+path); err != nil {
			return "", "", err
		}
	}
	tree, err := e.git(repo, env, "write-tree")
	_ = os.Remove(index)
	if err != nil {
		return "", "", err
	}
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit, err := e.gitInput(repo, []byte(message+"\n"), nil, args...)
	return commit, chunkPath, err
}

func (e *experiment) enableFetch(repo string) error {
	spec := "+" + dataRef + ":" + trackingRef
	existing, err := e.gitAllowExit(repo, nil, "config", "--get-all", "remote.origin.fetch")
	if err != nil && !errors.Is(err, errExitOne) {
		return err
	}
	for _, line := range strings.Split(existing, "\n") {
		if line == spec {
			return nil
		}
	}
	_, err = e.git(repo, nil, "config", "--add", "remote.origin.fetch", spec)
	return err
}

func (e *experiment) composeHooks(repo string) error {
	hooks := filepath.Join(commonAFor(repo), "hooks")
	if err := os.MkdirAll(filepath.Join(hooks, "pre-push.d"), 0o755); err != nil {
		return err
	}
	target := filepath.Join(hooks, "pre-push")
	existing := filepath.Join(hooks, "pre-push.user")
	if _, err := os.Stat(existing); errors.Is(err, os.ErrNotExist) {
		if current, readErr := os.ReadFile(target); readErr == nil {
			if !bytes.Contains(current, []byte("RepoDB M0 hook dispatcher")) {
				if err := os.Rename(target, existing); err != nil {
					return err
				}
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
	}
	repodb := filepath.Join(hooks, "pre-push.d", "repodb")
	if err := os.WriteFile(repodb, []byte("#!/bin/sh\nprintf 'repodb\\n' >> \"$REPODB_M0_HOOK_LOG\"\n"), 0o755); err != nil {
		return err
	}
	dispatcher := "#!/bin/sh\n# RepoDB M0 hook dispatcher\nset -e\nuser_hook=\"$(dirname \"$0\")/pre-push.user\"\n[ ! -x \"$user_hook\" ] || \"$user_hook\" \"$@\"\nfor hook in \"$(dirname \"$0\")\"/pre-push.d/*; do\n  [ ! -x \"$hook\" ] || \"$hook\" \"$@\"\ndone\n"
	return os.WriteFile(target, []byte(dispatcher), 0o755)
}

func (e *experiment) commonDir(repo string) (string, error) {
	path, err := e.git(repo, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Clean(path), nil
}

func commonAFor(repo string) string {
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return filepath.Join(repo, ".git")
	}
	return filepath.Clean(strings.TrimSpace(string(out)))
}

func (e *experiment) identity(repo string) error {
	if _, err := e.git(repo, nil, "config", "user.name", "RepoDB M0"); err != nil {
		return err
	}
	_, err := e.git(repo, nil, "config", "user.email", "m0@repodb.invalid")
	return err
}

func appendCommit(repo, name, content, message string) error {
	file, err := os.OpenFile(filepath.Join(repo, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	cmd := exec.Command("git", "add", "--", name)
	cmd.Dir = repo
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w: %s", err, output)
	}
	cmd = exec.Command("git", "commit", "-m", message)
	cmd.Dir = repo
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, output)
	}
	return nil
}

func hashPath(content string) string {
	sum := sha256.Sum256([]byte(content))
	digest := hex.EncodeToString(sum[:])
	return digest[:2] + "/" + digest[2:]
}

func (e *experiment) assertSourceUnchanged(repo, head, status string) error {
	afterHead, err := e.git(repo, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	afterStatus, err := e.git(repo, nil, "status", "--porcelain=v1")
	if err != nil {
		return err
	}
	if head != afterHead || status != afterStatus {
		return errors.New("data publication changed source HEAD, index, or worktree")
	}
	return nil
}

func (e *experiment) pass(message string) { fmt.Fprintln(e.out, "PASS", message) }

var errExitOne = errors.New("command exited with status 1")

func (e *experiment) git(dir string, env []string, args ...string) (string, error) {
	return e.gitInput(dir, nil, env, args...)
}

func (e *experiment) gitInput(dir string, input []byte, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s in %s: %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (e *experiment) gitAllowExit(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if exit := new(exec.ExitError); errors.As(err, &exit) && exit.ExitCode() == 1 {
		return strings.TrimSpace(stdout.String()), errExitOne
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

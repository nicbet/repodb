// Package git isolates calls to the Git CLI. Keeping this boundary small makes
// it possible to replace it with a pure-Go implementation later.
package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

var processCount atomic.Uint64

func ProcessCount() uint64 { return processCount.Load() }
func ResetProcessCount()   { processCount.Store(0) }

type CLI struct{}

var (
	ErrRefNotFound = errors.New("Git ref not found")
	ErrRefConflict = errors.New("Git ref changed")
)

type RepositoryInfo struct {
	TopLevel     string
	CommonDir    string
	ObjectFormat string
}

type TreeEntry struct {
	Path     string
	ObjectID string
}

func (CLI) Discover(ctx context.Context, start string) (RepositoryInfo, error) {
	top, err := run(ctx, start, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return RepositoryInfo{}, fmt.Errorf("find Git worktree: %w", err)
	}
	top = filepath.Clean(strings.TrimSpace(top))
	common, err := run(ctx, top, nil, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return RepositoryInfo{}, fmt.Errorf("find common Git directory: %w", err)
	}
	format, err := run(ctx, top, nil, nil, "rev-parse", "--show-object-format")
	if err != nil {
		return RepositoryInfo{}, fmt.Errorf("find Git object format: %w", err)
	}
	return RepositoryInfo{
		TopLevel:     top,
		CommonDir:    filepath.Clean(strings.TrimSpace(common)),
		ObjectFormat: strings.TrimSpace(format),
	}, nil
}

func (CLI) ResolveRef(ctx context.Context, root, ref string) (string, error) {
	out, err := run(ctx, root, nil, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "", ErrRefNotFound
		}
		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}
	return strings.TrimSpace(out), nil
}

func (CLI) IsAncestor(ctx context.Context, root, ancestor, descendant string) (bool, error) {
	_, err := run(ctx, root, nil, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check Git ancestry: %w", err)
}

func (CLI) HashObject(ctx context.Context, root string, data []byte) (string, error) {
	out, err := run(ctx, root, data, nil, durableArgs("hash-object", "-w", "--stdin")...)
	if err != nil {
		return "", fmt.Errorf("write Git blob: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (CLI) WriteTree(ctx context.Context, root, commonDir string, entries []TreeEntry) (string, error) {
	tmpDir := filepath.Join(commonDir, "repodb", "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", err
	}
	index, err := os.CreateTemp(tmpDir, "index-*")
	if err != nil {
		return "", err
	}
	indexPath := index.Name()
	if err := index.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(indexPath); err != nil {
		return "", err
	}
	defer os.Remove(indexPath)
	env := []string{"GIT_INDEX_FILE=" + indexPath}
	if _, err := run(ctx, root, nil, env, "read-tree", "--empty"); err != nil {
		return "", fmt.Errorf("initialize temporary Git tree: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	var input strings.Builder
	for _, entry := range entries {
		if entry.Path == "" || strings.HasPrefix(entry.Path, "/") || strings.Contains(entry.Path, "..") || strings.ContainsRune(entry.Path, '\x00') {
			return "", fmt.Errorf("invalid Git tree path %q", entry.Path)
		}
		fmt.Fprintf(&input, "100644 %s\t%s\n", entry.ObjectID, entry.Path)
	}
	if _, err := run(ctx, root, []byte(input.String()), env, "update-index", "--index-info"); err != nil {
		return "", fmt.Errorf("populate temporary Git tree: %w", err)
	}
	out, err := run(ctx, root, nil, env, durableArgs("write-tree")...)
	if err != nil {
		return "", fmt.Errorf("write Git tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (CLI) CommitTree(ctx context.Context, root, tree, parent, message string) (string, error) {
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	env := []string{
		"GIT_AUTHOR_NAME=RepoDB",
		"GIT_AUTHOR_EMAIL=repodb@localhost",
		"GIT_COMMITTER_NAME=RepoDB",
		"GIT_COMMITTER_EMAIL=repodb@localhost",
	}
	out, err := run(ctx, root, []byte(message+"\n"), env, durableArgs(args...)...)
	if err != nil {
		return "", fmt.Errorf("write RepoDB commit: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (CLI) UpdateRef(ctx context.Context, root, ref, newValue, oldValue string) error {
	_, err := run(ctx, root, nil, nil, durableArgs("update-ref", ref, newValue, oldValue)...)
	if err != nil {
		return fmt.Errorf("update %s: %w", ref, err)
	}
	return nil
}

func (CLI) ReadTreeFile(ctx context.Context, root, commit, path string) ([]byte, error) {
	out, err := run(ctx, root, nil, nil, "show", commit+":"+path)
	if err != nil {
		return nil, fmt.Errorf("read %s from snapshot %s: %w", path, commit, err)
	}
	return []byte(out), nil
}

func (CLI) ListTree(ctx context.Context, root, commit string) ([]string, error) {
	out, err := run(ctx, root, nil, nil, "ls-tree", "-r", "--name-only", "-z", commit)
	if err != nil {
		return nil, fmt.Errorf("list snapshot %s: %w", commit, err)
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
}

// ListTreeObjects returns every blob path and object ID using one Git process.
func (CLI) ListTreeObjects(ctx context.Context, root, commit string) (map[string]string, error) {
	out, err := run(ctx, root, nil, nil, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return nil, fmt.Errorf("list snapshot %s: %w", commit, err)
	}
	objects := make(map[string]string)
	for _, record := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		if record == "" {
			continue
		}
		header, path, ok := strings.Cut(record, "\t")
		if !ok {
			return nil, fmt.Errorf("invalid ls-tree record %q", record)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[1] != "blob" {
			return nil, fmt.Errorf("invalid RepoDB tree entry %q", record)
		}
		objects[path] = fields[2]
	}
	return objects, nil
}

// ReadObjects reads a set of blobs through one cat-file batch process.
func (CLI) ReadObjects(ctx context.Context, root string, objectIDs []string) (map[string][]byte, error) {
	if len(objectIDs) == 0 {
		return map[string][]byte{}, nil
	}
	input := strings.Join(objectIDs, "\n") + "\n"
	out, err := run(ctx, root, []byte(input), nil, "cat-file", "--batch")
	if err != nil {
		return nil, fmt.Errorf("batch read Git objects: %w", err)
	}
	reader := bufio.NewReader(strings.NewReader(out))
	objects := make(map[string][]byte, len(objectIDs))
	for range objectIDs {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read cat-file header: %w", err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[1] != "blob" {
			return nil, fmt.Errorf("invalid cat-file header %q", strings.TrimSpace(header))
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, err
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, err
		}
		if delimiter, err := reader.ReadByte(); err != nil || delimiter != '\n' {
			return nil, errors.New("invalid cat-file object delimiter")
		}
		objects[fields[0]] = data
	}
	return objects, nil
}

func durableArgs(args ...string) []string {
	return append([]string{"-c", "core.fsync=committed", "-c", "core.fsyncMethod=fsync"}, args...)
}

func run(ctx context.Context, dir string, stdin []byte, env []string, args ...string) (string, error) {
	processCount.Add(1)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return "", fmt.Errorf("%w: %s", err, message)
		}
		return "", err
	}
	return stdout.String(), nil
}

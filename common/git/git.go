// Package git isolates calls to the Git CLI. Keeping this boundary small makes
// it possible to replace it with a pure-Go implementation later.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

type CLI struct{}

func (CLI) TopLevel(ctx context.Context, start string) (string, error) {
	out, err := run(ctx, start, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("find Git worktree: %w", err)
	}
	return filepath.Clean(strings.TrimSpace(out)), nil
}

func (CLI) StageRepoDB(ctx context.Context, root string) error {
	_, err := run(ctx, root, "add", "--", ".repodb")
	if err != nil {
		return fmt.Errorf("stage RepoDB state: %w", err)
	}
	return nil
}

func (CLI) Commit(ctx context.Context, root, message string) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("commit message is required")
	}
	if _, err := run(ctx, root, "commit", "-m", message, "--", ".repodb"); err != nil {
		return fmt.Errorf("commit RepoDB state: %w", err)
	}
	return nil
}

func (CLI) Status(ctx context.Context, root string) (string, error) {
	out, err := run(ctx, root, "status", "--short", "--", ".repodb")
	if err != nil {
		return "", fmt.Errorf("read RepoDB status: %w", err)
	}
	return out, nil
}

func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
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

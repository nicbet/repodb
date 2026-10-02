package git

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// readerIdleTimeout closes a reader nobody has used for a while, so short-lived
// callers that never call CloseReaders don't leave Git processes behind.
var readerIdleTimeout = 5 * time.Second

// errObjectMissing marks a request cat-file reported as missing.
var errObjectMissing = errors.New("git object not found")

// objectReader keeps one `git cat-file --batch` process per worktree root, so
// object reads cost no process start. Requests are serialized. Any I/O or
// protocol error, or a cancelled context, discards the process; the next
// request starts a fresh one.
type objectReader struct {
	root string

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	idle   *time.Timer
}

var readers = struct {
	sync.Mutex
	byRoot map[string]*objectReader
}{byRoot: make(map[string]*objectReader)}

func readerFor(root string) *objectReader {
	readers.Lock()
	defer readers.Unlock()
	r, ok := readers.byRoot[root]
	if !ok {
		r = &objectReader{root: root}
		readers.byRoot[root] = r
	}
	return r
}

// CloseReaders stops the long-lived object reader of the worktree at root, if
// one is running. Engines call it on Close; on Windows an open reader keeps
// pack files open, which blocks removing the repository.
func CloseReaders(root string) {
	readers.Lock()
	r, ok := readers.byRoot[root]
	readers.Unlock()
	if ok {
		r.mu.Lock()
		r.closeLocked()
		r.mu.Unlock()
	}
}

// CloseAllReaders stops every long-lived object reader.
func CloseAllReaders() {
	readers.Lock()
	all := make([]*objectReader, 0, len(readers.byRoot))
	for _, r := range readers.byRoot {
		all = append(all, r)
	}
	readers.Unlock()
	for _, r := range all {
		r.mu.Lock()
		r.closeLocked()
		r.mu.Unlock()
	}
}

// objectResult is one reply. Missing is set instead of Data when cat-file
// cannot resolve the request.
type objectResult struct {
	OID     string
	Type    string
	Data    []byte
	Missing bool
}

// read resolves each request (an object ID or any revision such as
// <commit>:<path>) and returns the replies in request order.
func (r *objectReader) read(ctx context.Context, requests []string) ([]objectResult, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	for _, request := range requests {
		if request == "" || strings.ContainsAny(request, "\n\r") {
			return nil, fmt.Errorf("invalid cat-file request %q", request)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.idle != nil {
		r.idle.Stop()
	}
	reused := r.cmd != nil
	results, err := r.attemptLocked(ctx, requests)
	if err != nil && reused && ctx.Err() == nil {
		// The idle process may have died since its last request (killed, or
		// the system reaped it). Reads are idempotent: retry once on a fresh one.
		results, err = r.attemptLocked(ctx, requests)
	}
	if err != nil {
		return nil, err
	}
	r.idle = time.AfterFunc(readerIdleTimeout, r.closeIdle)
	return results, nil
}

func (r *objectReader) attemptLocked(ctx context.Context, requests []string) ([]objectResult, error) {
	if r.cmd == nil {
		if err := r.startLocked(); err != nil {
			return nil, err
		}
	}
	cmd := r.cmd
	stop := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
	results, err := r.exchangeLocked(requests)
	if !stop() {
		err = errors.Join(ctx.Err(), err)
	}
	if err != nil {
		r.closeLocked()
		return nil, err
	}
	return results, nil
}

func (r *objectReader) exchangeLocked(requests []string) ([]objectResult, error) {
	// Write from a goroutine: cat-file answers as it reads, so a large batch
	// would fill both pipes if requests and replies were not interleaved.
	written := make(chan error, 1)
	stdin := r.stdin
	go func() {
		w := bufio.NewWriter(stdin)
		for _, request := range requests {
			if _, err := w.WriteString(request + "\n"); err != nil {
				written <- err
				return
			}
		}
		written <- w.Flush()
	}()
	results := make([]objectResult, len(requests))
	var readErr error
	for i := range requests {
		result, err := readReply(r.stdout)
		if err != nil {
			readErr = err
			break
		}
		results[i] = result
	}
	if readErr != nil {
		// Unblock the writer before waiting for it.
		_ = r.cmd.Process.Kill()
		<-written
		return nil, fmt.Errorf("read Git objects: %w", readErr)
	}
	if err := <-written; err != nil {
		return nil, fmt.Errorf("write cat-file requests: %w", err)
	}
	return results, nil
}

func readReply(reader *bufio.Reader) (objectResult, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return objectResult{}, fmt.Errorf("read cat-file header: %w", err)
	}
	header = strings.TrimSuffix(header, "\n")
	if rest, ok := strings.CutSuffix(header, " missing"); ok {
		return objectResult{OID: rest, Missing: true}, nil
	}
	if rest, ok := strings.CutSuffix(header, " ambiguous"); ok {
		return objectResult{OID: rest, Missing: true}, nil
	}
	fields := strings.Fields(header)
	if len(fields) != 3 {
		return objectResult{}, fmt.Errorf("invalid cat-file header %q", header)
	}
	size, err := strconv.Atoi(fields[2])
	if err != nil || size < 0 {
		return objectResult{}, fmt.Errorf("invalid cat-file header %q", header)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return objectResult{}, err
	}
	if delimiter, err := reader.ReadByte(); err != nil || delimiter != '\n' {
		return objectResult{}, errors.New("invalid cat-file object delimiter")
	}
	return objectResult{OID: fields[0], Type: fields[1], Data: data}, nil
}

func (r *objectReader) startLocked() error {
	processCount.Add(1)
	cmd := exec.Command("git", "cat-file", "--batch")
	cmd.Dir = r.root
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git cat-file: %w", err)
	}
	r.cmd, r.stdin, r.stdout = cmd, stdin, bufio.NewReaderSize(stdout, 64<<10)
	return nil
}

// closeLocked ends the process: closing stdin lets cat-file exit on its own,
// and Kill covers one that is stuck or mid-reply.
func (r *objectReader) closeLocked() {
	if r.idle != nil {
		r.idle.Stop()
		r.idle = nil
	}
	if r.cmd == nil {
		return
	}
	_ = r.stdin.Close()
	_ = r.cmd.Process.Kill()
	_ = r.cmd.Wait()
	r.cmd, r.stdin, r.stdout = nil, nil, nil
}

func (r *objectReader) closeIdle() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeLocked()
}

// running reports whether a cat-file process is up, for tests.
func (r *objectReader) running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmd != nil
}

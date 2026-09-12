package git

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type FsyncMethod string

const (
	FsyncMethodFsync FsyncMethod = "fsync"
	FsyncMethodBatch FsyncMethod = "batch"
)

type DurabilityCommandMetrics struct {
	Invocations      uint64
	LatencyNanos     uint64
	ObjectsCreated   uint64
	ObjectBytes      uint64
	HardwareFlushes  uint64
	WriteoutRequests uint64
}

type DurabilityMetrics struct {
	HashObject DurabilityCommandMetrics
	WriteTree  DurabilityCommandMetrics
	CommitTree DurabilityCommandMetrics
	UpdateRef  DurabilityCommandMetrics
}

type DurabilityAudit struct {
	commonDir string
	method    FsyncMethod
	mu        sync.Mutex
	metrics   DurabilityMetrics
}

type durabilityAuditKey struct{}

// WithDurabilityAudit enables per-command Trace2 and loose-object accounting.
// It is intended for controlled benchmarks: filesystem inventory happens
// outside the recorded Git command latency but still adds observer overhead to
// the enclosing operation.
func WithDurabilityAudit(ctx context.Context, commonDir string, method FsyncMethod) (context.Context, *DurabilityAudit, error) {
	if method != FsyncMethodFsync && method != FsyncMethodBatch {
		return nil, nil, fmt.Errorf("unsupported Git fsync method %q", method)
	}
	audit := &DurabilityAudit{commonDir: commonDir, method: method}
	return context.WithValue(ctx, durabilityAuditKey{}, audit), audit, nil
}

func (a *DurabilityAudit) Metrics() DurabilityMetrics {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.metrics
}

func durabilityMethod(ctx context.Context) FsyncMethod {
	if audit, _ := ctx.Value(durabilityAuditKey{}).(*DurabilityAudit); audit != nil {
		return audit.method
	}
	return FsyncMethodFsync
}

func durabilityAudit(ctx context.Context) *DurabilityAudit {
	audit, _ := ctx.Value(durabilityAuditKey{}).(*DurabilityAudit)
	return audit
}

func (a *DurabilityAudit) record(command string, elapsed time.Duration, before, after map[string]int64, trace []byte) {
	metric := DurabilityCommandMetrics{Invocations: 1, LatencyNanos: uint64(elapsed)}
	for oid, size := range after {
		if _, existed := before[oid]; !existed {
			metric.ObjectsCreated++
			metric.ObjectBytes += uint64(size)
		}
	}
	for _, line := range strings.Split(string(trace), "\n") {
		var event struct {
			Event    string `json:"event"`
			Category string `json:"category"`
			Name     string `json:"name"`
			Count    uint64 `json:"count"`
		}
		if json.Unmarshal([]byte(line), &event) != nil || event.Event != "counter" || event.Category != "fsync" {
			continue
		}
		switch event.Name {
		case "hardware-flush":
			metric.HardwareFlushes += event.Count
		case "writeout-only":
			metric.WriteoutRequests += event.Count
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var destination *DurabilityCommandMetrics
	switch command {
	case "hash-object":
		destination = &a.metrics.HashObject
	case "write-tree":
		destination = &a.metrics.WriteTree
	case "commit-tree":
		destination = &a.metrics.CommitTree
	case "update-ref":
		destination = &a.metrics.UpdateRef
	default:
		return
	}
	destination.Invocations += metric.Invocations
	destination.LatencyNanos += metric.LatencyNanos
	destination.ObjectsCreated += metric.ObjectsCreated
	destination.ObjectBytes += metric.ObjectBytes
	destination.HardwareFlushes += metric.HardwareFlushes
	destination.WriteoutRequests += metric.WriteoutRequests
}

func (a *DurabilityAudit) looseObjects() map[string]int64 {
	objects := make(map[string]int64)
	entries, err := os.ReadDir(filepath.Join(a.commonDir, "objects"))
	if err != nil {
		return objects
	}
	for _, prefix := range entries {
		if !prefix.IsDir() || len(prefix.Name()) != 2 || !isHex(prefix.Name()) {
			continue
		}
		children, err := os.ReadDir(filepath.Join(a.commonDir, "objects", prefix.Name()))
		if err != nil {
			continue
		}
		for _, child := range children {
			if child.IsDir() || !isHex(child.Name()) {
				continue
			}
			info, err := child.Info()
			if err == nil {
				objects[prefix.Name()+child.Name()] = info.Size()
			}
		}
	}
	return objects
}

func isHex(value string) bool {
	for _, char := range value {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return value != ""
}

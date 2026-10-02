// Package storage contains content-addressed storage primitives shared by the
// SQL server and repository tooling.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

var ErrNotFound = errors.New("object not found")

// Hash is the SHA-256 identity of an immutable object.
type Hash string

func Sum(data []byte) Hash {
	sum := sha256.Sum256(data)
	return Hash(hex.EncodeToString(sum[:]))
}

// Valid reports whether h is a hex-encoded SHA-256.
func (h Hash) Valid() bool {
	if len(h) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(h); i++ {
		if c := h[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// Store is deliberately smaller than a filesystem or Git abstraction. Prolly
// trees only need immutable blobs addressed by content.
//
// Get returns the stored bytes themselves, which may be shared with the store
// and other readers: callers must not modify them. Put copies its input.
type Store interface {
	Get(context.Context, Hash) ([]byte, error)
	Put(context.Context, []byte) (Hash, error)
}

// Prefetcher is implemented by stores whose reads are cheaper in batches.
// Prefetch is a hint: it loads what it can so later Gets are served from
// memory, and leaves any failure for Get to report.
type Prefetcher interface {
	Prefetch(context.Context, []Hash)
}

// Prefetch hints store to load hashes if it supports batched reads.
func Prefetch(ctx context.Context, store Store, hashes []Hash) {
	if p, ok := store.(Prefetcher); ok && len(hashes) != 0 {
		p.Prefetch(ctx, hashes)
	}
}

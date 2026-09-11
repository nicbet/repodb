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

func (h Hash) Valid() bool {
	if len(h) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(string(h))
	return err == nil
}

// Store is deliberately smaller than a filesystem or Git abstraction. Prolly
// trees only need immutable blobs addressed by content.
type Store interface {
	Get(context.Context, Hash) ([]byte, error)
	Put(context.Context, []byte) (Hash, error)
}

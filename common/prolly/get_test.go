package prolly_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nicbet/repodb/common/prolly"
	"github.com/nicbet/repodb/common/storage"
)

// Get binary-searches cached item offsets; it must agree with the iterator for
// every stored key and report ErrNotFound around them, on multi-level trees.
func TestGetAgreesWithIterator(t *testing.T) {
	ctx := context.Background()
	for _, count := range []int{1, 2, 255, 20_000} {
		t.Run(fmt.Sprintf("entries=%d", count), func(t *testing.T) {
			store := storage.NewMemory()
			entries := make([]prolly.Entry, count)
			for i := range entries {
				// Odd keys are stored; even keys fall between them.
				entries[i] = prolly.Entry{Key: []byte(fmt.Sprintf("k%06d", 2*i+1)), Value: bytes.Repeat([]byte{byte(i)}, i%300)}
			}
			tree, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
			if err != nil {
				t.Fatal(err)
			}
			it, err := tree.Iterator(ctx)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for {
				entry, ok, err := it.Next()
				if err != nil {
					t.Fatal(err)
				}
				if !ok {
					break
				}
				n++
				got, err := tree.Get(ctx, entry.Key)
				if err != nil || !bytes.Equal(got, entry.Value) {
					t.Fatalf("Get(%q) = %d bytes, %v; want %d bytes", entry.Key, len(got), err, len(entry.Value))
				}
			}
			if n != count {
				t.Fatalf("iterated %d entries, want %d", n, count)
			}
			missing := [][]byte{nil, []byte("a"), []byte("k"), []byte("k000000"), []byte(fmt.Sprintf("k%06d", 2*count)), []byte("z")}
			for i := 0; i < count; i += 97 {
				missing = append(missing, []byte(fmt.Sprintf("k%06d", 2*i)), []byte(fmt.Sprintf("k%06d0", 2*i+1)))
			}
			for _, key := range missing {
				if _, err := tree.Get(ctx, key); !errors.Is(err, prolly.ErrNotFound) {
					t.Fatalf("Get(%q) error = %v, want ErrNotFound", key, err)
				}
			}
		})
	}
}

func TestGetRejectsMalformedNodes(t *testing.T) {
	ctx := context.Background()
	// 'P', codec version 1, leaf, one item: key "a", value "b".
	valid := []byte{'P', 1, 0, 1, 1, 'a', 1, 'b'}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"trailing-bytes", append(append([]byte(nil), valid...), 0)},
		{"truncated", []byte{'P', 1, 0, 2, 1, 'a', 1, 'b'}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := storage.NewMemory()
			hash, err := store.Put(ctx, tc.data)
			if err != nil {
				t.Fatal(err)
			}
			tree, err := prolly.Open(store, hash)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tree.Get(ctx, []byte("a")); err == nil || errors.Is(err, prolly.ErrNotFound) {
				t.Fatalf("Get on malformed node error = %v, want a decode error", err)
			}
		})
	}
	store := storage.NewMemory()
	hash, err := store.Put(ctx, valid)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := prolly.Open(store, hash)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := tree.Get(ctx, []byte("a")); err != nil || string(got) != "b" {
		t.Fatalf("Get on valid node = %q, %v", got, err)
	}
}

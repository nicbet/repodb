package prolly_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nicbet/repodb/common/prolly"
	"github.com/nicbet/repodb/common/storage"
)

// mapNodeCache is a NodeCache without eviction or trust checks.
type mapNodeCache map[storage.Hash]prolly.ValidatedNode

func (c mapNodeCache) Subtree(hash storage.Hash) ([]prolly.NodeCount, bool) {
	var nodes []prolly.NodeCount
	for stack := []storage.Hash{hash}; len(stack) != 0; {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		node, ok := c[next]
		if !ok {
			return nil, false
		}
		nodes = append(nodes, prolly.NodeCount{Hash: next, Count: node.Count})
		stack = append(stack, node.Children...)
	}
	return nodes, true
}

func (c mapNodeCache) Remember(hash storage.Hash, node prolly.ValidatedNode) { c[hash] = node }

// countingStore counts Get calls.
type countingStore struct {
	storage.Store
	gets int
}

func (s *countingStore) Get(ctx context.Context, hash storage.Hash) ([]byte, error) {
	s.gets++
	return s.Store.Get(ctx, hash)
}

func validateCounting(t *testing.T, store storage.Store, root storage.Hash, cache prolly.NodeCache) ([]storage.Hash, int) {
	t.Helper()
	leafEntries := 0
	hashes, err := prolly.Validate(context.Background(), store, root, cache, func(entries []prolly.Entry) error {
		leafEntries += len(entries)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hashes, leafEntries
}

func TestValidateSkipsCachedSubtrees(t *testing.T) {
	ctx := context.Background()
	store := &countingStore{Store: storage.NewMemory()}
	entries := make([]prolly.Entry, 5000)
	for i := range entries {
		entries[i] = prolly.Entry{Key: []byte(fmt.Sprintf("key-%05d", i)), Value: []byte("v")}
	}
	tree, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	cache := mapNodeCache{}
	hashes, leafEntries := validateCounting(t, store, tree.Root(), cache)
	if leafEntries != len(entries) {
		t.Fatalf("first validation saw %d entries, want %d", leafEntries, len(entries))
	}
	if len(cache) != len(hashes) {
		t.Fatalf("cache remembered %d nodes, tree has %d", len(cache), len(hashes))
	}

	// Unchanged tree: nothing is read.
	store.gets = 0
	again, leafEntries := validateCounting(t, store, tree.Root(), cache)
	if leafEntries != 0 || store.gets != 0 || !slices.Equal(again, hashes) {
		t.Fatalf("revalidating an unchanged tree saw %d entries, read %d nodes", leafEntries, store.gets)
	}

	edited, err := prolly.Apply(ctx, store, tree, []prolly.Edit{{Key: []byte("key-02500"), Value: []byte("changed")}})
	if err != nil {
		t.Fatal(err)
	}
	want, err := prolly.Reachable(ctx, store, edited.Root())
	if err != nil {
		t.Fatal(err)
	}
	store.gets = 0
	got, leafEntries := validateCounting(t, store, edited.Root(), cache)
	if !slices.Equal(got, want) {
		t.Fatalf("Validate with cache returned %d hashes, Reachable %d", len(got), len(want))
	}
	if leafEntries == 0 || leafEntries > prolly.DefaultOptions.MaxChunkEntries {
		t.Fatalf("one-key edit decoded %d leaf entries, want one leaf", leafEntries)
	}
	if store.gets > 4 {
		t.Fatalf("one-key edit read %d nodes, want only the changed path", store.gets)
	}
}

// A cached subtree's count still has to match the parent's child link.
func TestValidateChecksCachedSubtreeCounts(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	entries := make([]prolly.Entry, 2000)
	for i := range entries {
		entries[i] = prolly.Entry{Key: []byte(fmt.Sprintf("key-%05d", i)), Value: []byte("v")}
	}
	tree, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	cache := mapNodeCache{}
	hashes, _ := validateCounting(t, store, tree.Root(), cache)
	for _, hash := range hashes {
		if node := cache[hash]; hash != tree.Root() && node.Children == nil {
			node.Count++
			cache[hash] = node
			break
		}
	}
	delete(cache, tree.Root())
	if _, err := prolly.Validate(ctx, store, tree.Root(), cache, nil); err == nil || !strings.Contains(err.Error(), "counts") {
		t.Fatalf("Validate accepted a cached subtree with a wrong count: %v", err)
	}
}

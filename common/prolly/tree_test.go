package prolly_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nicbet/repodb/common/prolly"
	"github.com/nicbet/repodb/common/storage"
)

func TestBuildIsDeterministicAndReadable(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	entries := make([]prolly.Entry, 300)
	for i := range entries {
		entries[i] = prolly.Entry{
			Key:   []byte(fmt.Sprintf("key-%04d", 299-i)),
			Value: []byte(fmt.Sprintf("value-%04d", 299-i)),
		}
	}
	first, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	if first.Root() != second.Root() {
		t.Fatalf("roots differ: %s != %s", first.Root(), second.Root())
	}
	got, err := first.Get(ctx, []byte("key-0123"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "value-0123" {
		t.Fatalf("unexpected value %q", got)
	}
	if _, err := first.Get(ctx, []byte("missing")); !errors.Is(err, prolly.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestBuildRejectsDuplicateKeys(t *testing.T) {
	_, err := prolly.Build(context.Background(), storage.NewMemory(), []prolly.Entry{
		{Key: []byte("same"), Value: []byte("one")},
		{Key: []byte("same"), Value: []byte("two")},
	}, prolly.DefaultOptions)
	if err == nil {
		t.Fatal("expected duplicate-key error")
	}
}

func TestSortedBuilderMatchesBuildAndIterator(t *testing.T) {
	ctx := context.Background()
	for _, count := range []int{0, 1, 31, 32, 127, 128, 129, 300, 10_000} {
		t.Run(fmt.Sprintf("entries=%d", count), func(t *testing.T) {
			entries := make([]prolly.Entry, count)
			for i := range entries {
				entries[i] = prolly.Entry{Key: []byte(fmt.Sprintf("key-%08d", i)), Value: []byte(fmt.Sprintf("value-%08d", i))}
			}
			store := storage.NewMemory()
			want, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
			if err != nil {
				t.Fatal(err)
			}
			builder, err := prolly.NewSortedBuilder(ctx, store, prolly.DefaultOptions)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if err := builder.Add(entry); err != nil {
					t.Fatal(err)
				}
			}
			got, err := builder.Finish()
			if err != nil {
				t.Fatal(err)
			}
			if got.Root() != want.Root() {
				t.Fatalf("roots differ: got %s want %s", got.Root(), want.Root())
			}
			iterator, err := got.Iterator(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer iterator.Close()
			for i, wantEntry := range entries {
				entry, ok, err := iterator.Next()
				if err != nil || !ok {
					t.Fatalf("entry %d: ok=%v err=%v", i, ok, err)
				}
				if string(entry.Key) != string(wantEntry.Key) || string(entry.Value) != string(wantEntry.Value) {
					t.Fatalf("entry %d = %q/%q, want %q/%q", i, entry.Key, entry.Value, wantEntry.Key, wantEntry.Value)
				}
			}
			if _, ok, err := iterator.Next(); err != nil || ok {
				t.Fatalf("iterator end: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestSortedBuilderRejectsUnorderedEntries(t *testing.T) {
	builder, err := prolly.NewSortedBuilder(context.Background(), storage.NewMemory(), prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Add(prolly.Entry{Key: []byte("b")}); err != nil {
		t.Fatal(err)
	}
	if err := builder.Add(prolly.Entry{Key: []byte("a")}); err == nil {
		t.Fatal("expected unordered-entry error")
	}
}

func TestApplyMatchesCanonicalBuild(t *testing.T) {
	ctx := context.Background()
	for _, count := range []int{0, 1, 127, 128, 1_000, 10_000} {
		t.Run(fmt.Sprintf("entries=%d", count), func(t *testing.T) {
			store := storage.NewMemory()
			entries := make([]prolly.Entry, count)
			for i := range entries {
				entries[i] = prolly.Entry{Key: []byte(fmt.Sprintf("key-%08d", i*2)), Value: []byte(fmt.Sprintf("value-%08d", i))}
			}
			base, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
			if err != nil {
				t.Fatal(err)
			}
			edits := []prolly.Edit{
				{Key: []byte("key-00000001"), Value: []byte("inserted")},
				{Key: []byte("key-00000004"), Value: []byte("updated")},
				{Key: []byte("key-00000006"), Delete: true},
			}
			if count < 4 {
				edits = edits[:1]
			}
			got, err := prolly.Apply(ctx, store, base, edits)
			if err != nil {
				t.Fatal(err)
			}
			values := make(map[string][]byte, len(entries)+1)
			for _, entry := range entries {
				values[string(entry.Key)] = entry.Value
			}
			for _, edit := range edits {
				if edit.Delete {
					delete(values, string(edit.Key))
				} else {
					values[string(edit.Key)] = edit.Value
				}
			}
			wantEntries := make([]prolly.Entry, 0, len(values))
			for key, value := range values {
				wantEntries = append(wantEntries, prolly.Entry{Key: []byte(key), Value: value})
			}
			want, err := prolly.Build(ctx, store, wantEntries, prolly.DefaultOptions)
			if err != nil {
				t.Fatal(err)
			}
			if got.Root() != want.Root() {
				t.Fatalf("roots differ: got %s want %s", got.Root(), want.Root())
			}
		})
	}
}

func TestApplyDistantEditsMatchesCanonicalBuild(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	entries := make([]prolly.Entry, 10_000)
	for i := range entries {
		entries[i] = prolly.Entry{Key: []byte(fmt.Sprintf("key-%08d", i*2)), Value: []byte(fmt.Sprintf("value-%08d", i))}
	}
	base, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	edits := []prolly.Edit{
		{Key: []byte("key-00000002"), Value: []byte("first")},
		{Key: []byte("key-00010000"), Delete: true},
		{Key: []byte("key-00019998"), Value: []byte("last")},
		{Key: []byte("key-00020001"), Value: []byte("after")},
	}
	got, err := prolly.Apply(ctx, store, base, edits)
	if err != nil {
		t.Fatal(err)
	}
	entries[1].Value = []byte("first")
	entries[5_000].Key = nil
	entries[9_999].Value = []byte("last")
	wantEntries := make([]prolly.Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.Key != nil {
			wantEntries = append(wantEntries, entry)
		}
	}
	wantEntries = append(wantEntries, prolly.Entry{Key: []byte("key-00020001"), Value: []byte("after")})
	want, err := prolly.Build(ctx, store, wantEntries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root() != want.Root() {
		t.Fatalf("roots differ: got %s want %s", got.Root(), want.Root())
	}
}

func TestIteratorFromExactKey(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	entries := make([]prolly.Entry, 300)
	for i := range entries {
		entries[i] = prolly.Entry{Key: []byte(fmt.Sprintf("key-%04d", i)), Value: []byte(fmt.Sprintf("val-%04d", i))}
	}
	tree, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	it, err := tree.IteratorFrom(ctx, []byte("key-0150"))
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	entry, ok, err := it.Next()
	if err != nil || !ok {
		t.Fatalf("expected entry, got ok=%v err=%v", ok, err)
	}
	if string(entry.Key) != "key-0150" {
		t.Fatalf("got key %q, want key-0150", entry.Key)
	}
	if string(entry.Value) != "val-0150" {
		t.Fatalf("got value %q, want val-0150", entry.Value)
	}
}

func TestIteratorFromBetweenKeys(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	entries := make([]prolly.Entry, 100)
	for i := range entries {
		entries[i] = prolly.Entry{Key: []byte(fmt.Sprintf("key-%04d", i*2)), Value: []byte(fmt.Sprintf("val-%04d", i*2))}
	}
	tree, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	it, err := tree.IteratorFrom(ctx, []byte("key-0099"))
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	entry, ok, err := it.Next()
	if err != nil || !ok {
		t.Fatalf("expected entry, got ok=%v err=%v", ok, err)
	}
	if string(entry.Key) != "key-0100" {
		t.Fatalf("got key %q, want key-0100", entry.Key)
	}
}

func TestIteratorFromPastEnd(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	entries := []prolly.Entry{
		{Key: []byte("aaa"), Value: []byte("1")},
		{Key: []byte("bbb"), Value: []byte("2")},
	}
	tree, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	it, err := tree.IteratorFrom(ctx, []byte("zzz"))
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	_, ok, err := it.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected done, got entry")
	}
}

func TestIteratorFromEmptyTree(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	tree, err := prolly.Build(ctx, store, nil, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	it, err := tree.IteratorFrom(ctx, []byte("anything"))
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	_, ok, err := it.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected done on empty tree")
	}
}

func TestIteratorFromEmptyStart(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	entries := make([]prolly.Entry, 50)
	for i := range entries {
		entries[i] = prolly.Entry{Key: []byte(fmt.Sprintf("key-%04d", i)), Value: []byte(fmt.Sprintf("val-%04d", i))}
	}
	tree, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	it, err := tree.IteratorFrom(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	entry, ok, err := it.Next()
	if err != nil || !ok {
		t.Fatalf("expected entry, got ok=%v err=%v", ok, err)
	}
	if string(entry.Key) != "key-0000" {
		t.Fatalf("got key %q, want key-0000", entry.Key)
	}
}

func TestIteratorFromSingleEntry(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	tree, err := prolly.Build(ctx, store, []prolly.Entry{
		{Key: []byte("only"), Value: []byte("one")},
	}, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}

	it, err := tree.IteratorFrom(ctx, []byte("only"))
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	entry, ok, err := it.Next()
	if err != nil || !ok {
		t.Fatalf("expected entry, got ok=%v err=%v", ok, err)
	}
	if string(entry.Key) != "only" || string(entry.Value) != "one" {
		t.Fatalf("got %q/%q", entry.Key, entry.Value)
	}
	_, ok, _ = it.Next()
	if ok {
		t.Fatal("expected done after single entry")
	}
}

func TestIteratorFromWalksRemainingEntries(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	entries := make([]prolly.Entry, 300)
	for i := range entries {
		entries[i] = prolly.Entry{Key: []byte(fmt.Sprintf("key-%04d", i)), Value: []byte(fmt.Sprintf("val-%04d", i))}
	}
	tree, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	startAt := 200
	it, err := tree.IteratorFrom(ctx, []byte(fmt.Sprintf("key-%04d", startAt)))
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	count := 0
	for {
		entry, ok, err := it.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		want := fmt.Sprintf("key-%04d", startAt+count)
		if string(entry.Key) != want {
			t.Fatalf("entry %d: got key %q, want %q", count, entry.Key, want)
		}
		count++
	}
	if count != 300-startAt {
		t.Fatalf("got %d entries, want %d", count, 300-startAt)
	}
}

func TestIteratorFromPrefixScanPattern(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	entries := []prolly.Entry{
		{Key: []byte("user-001-email"), Value: []byte("a@b.com")},
		{Key: []byte("user-001-name"), Value: []byte("Alice")},
		{Key: []byte("user-001-role"), Value: []byte("admin")},
		{Key: []byte("user-002-email"), Value: []byte("b@c.com")},
		{Key: []byte("user-002-name"), Value: []byte("Bob")},
		{Key: []byte("user-003-email"), Value: []byte("c@d.com")},
	}
	tree, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte("user-001-")
	it, err := tree.IteratorFrom(ctx, prefix)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	var matched []string
	for {
		entry, ok, err := it.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if len(entry.Key) < len(prefix) || string(entry.Key[:len(prefix)]) != string(prefix) {
			break
		}
		matched = append(matched, string(entry.Key))
	}
	if len(matched) != 3 {
		t.Fatalf("got %d matches %v, want 3", len(matched), matched)
	}
}

// newNodeStore counts the nodes a write adds that the store didn't already
// hold.
type newNodeStore struct {
	storage.Store
	added map[storage.Hash]struct{}
}

func (s *newNodeStore) Put(ctx context.Context, data []byte) (storage.Hash, error) {
	hash := storage.Sum(data)
	if _, err := s.Store.Get(ctx, hash); err != nil {
		s.added[hash] = struct{}{}
	}
	return s.Store.Put(ctx, data)
}

// Chunk boundaries depend only on each entry's key (and each link's max key),
// so an edit rewrites its own leaf and that leaf's path, not the chunks after
// it.
func TestApplyRewritesOnlyTouchedChunks(t *testing.T) {
	ctx := context.Background()
	base := storage.NewMemory()
	const n = 10_000
	entries := make([]prolly.Entry, n)
	for i := range entries {
		entries[i] = prolly.Entry{Key: []byte(fmt.Sprintf("key-%06d", i)), Value: []byte(fmt.Sprintf("value-%06d-0123456789abcdef0123456789abcdef", i))}
	}
	tree, err := prolly.Build(ctx, base, entries, prolly.DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	// 10k entries of ~80 per leaf: leaves, one interior level and a root.
	const depth = 3
	apply := func(edits []prolly.Edit) int {
		t.Helper()
		store := &newNodeStore{Store: base, added: map[storage.Hash]struct{}{}}
		applied, err := prolly.Apply(ctx, store, tree, edits)
		if err != nil {
			t.Fatal(err)
		}
		// The result must still be the canonical tree for its contents.
		want := append([]prolly.Entry(nil), entries...)
		for _, edit := range edits {
			i := sortSearchKey(want, edit.Key)
			switch {
			case edit.Delete:
				want = append(want[:i], want[i+1:]...)
			case i < len(want) && string(want[i].Key) == string(edit.Key):
				want[i].Value = edit.Value
			default:
				want = append(want[:i], append([]prolly.Entry{{Key: edit.Key, Value: edit.Value}}, want[i:]...)...)
			}
		}
		rebuilt, err := prolly.Build(ctx, storage.NewMemory(), want, prolly.DefaultOptions)
		if err != nil {
			t.Fatal(err)
		}
		if applied.Root() != rebuilt.Root() {
			t.Fatalf("Apply root differs from a fresh Build")
		}
		return len(store.added)
	}
	for _, i := range []int{0, n / 2, n - 1} {
		key := entries[i].Key
		if got := apply([]prolly.Edit{{Key: key, Value: []byte("updated")}}); got > depth {
			t.Errorf("update at %d wrote %d nodes, want at most %d", i, got, depth)
		}
		// Inserts and deletes change a chunk's size, which can move the
		// size-based cuts up to the next key-hash boundary: a few leaves at
		// most (measured: median 3 nodes, worst 8 over 271 positions).
		if got := apply([]prolly.Edit{{Key: key, Delete: true}}); got > 3*depth {
			t.Errorf("delete at %d wrote %d nodes, want at most %d", i, got, 3*depth)
		}
		if got := apply([]prolly.Edit{{Key: append(append([]byte(nil), key...), '+'), Value: []byte("inserted")}}); got > 3*depth {
			t.Errorf("insert after %d wrote %d nodes, want at most %d", i, got, 3*depth)
		}
	}
	for _, k := range []int{2, 10, 100} {
		edits := make([]prolly.Edit, k)
		for i := range edits {
			edits[i] = prolly.Edit{Key: entries[i].Key, Value: []byte("updated")}
		}
		if got := apply(edits); got > k*depth {
			t.Errorf("updating %d adjacent keys wrote %d nodes, want at most %d", k, got, k*depth)
		}
	}
}

func sortSearchKey(entries []prolly.Entry, key []byte) int {
	lo, hi := 0, len(entries)
	for lo < hi {
		mid := (lo + hi) / 2
		if string(entries[mid].Key) < string(key) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func TestCountMatchesEntries(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	for _, count := range []int{0, 1, 128, 129, 10_000} {
		entries := make([]prolly.Entry, count)
		for i := range entries {
			entries[i] = prolly.Entry{Key: []byte(fmt.Sprintf("key-%05d", i)), Value: []byte("v")}
		}
		built, err := prolly.Build(ctx, store, entries, prolly.DefaultOptions)
		if err != nil {
			t.Fatal(err)
		}
		builder, err := prolly.NewSortedBuilder(ctx, store, prolly.DefaultOptions)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if err := builder.Add(entry); err != nil {
				t.Fatal(err)
			}
		}
		sorted, err := builder.Finish()
		if err != nil {
			t.Fatal(err)
		}
		// Delete every third key and insert as many new ones.
		var edits []prolly.Edit
		for i := 0; i < count; i += 3 {
			edits = append(edits, prolly.Edit{Key: []byte(fmt.Sprintf("key-%05d", i)), Delete: true}, prolly.Edit{Key: []byte(fmt.Sprintf("key-%05d+", i)), Value: []byte("n")})
		}
		applied, err := prolly.Apply(ctx, store, built, edits)
		if err != nil {
			t.Fatal(err)
		}
		for name, tree := range map[string]*prolly.Tree{"build": built, "sorted": sorted, "apply": applied} {
			got, err := tree.Count(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != uint64(count) {
				t.Fatalf("%d entries, %s: Count = %d", count, name, got)
			}
			if _, err := prolly.Reachable(ctx, store, tree.Root()); err != nil {
				t.Fatalf("%d entries, %s: %v", count, name, err)
			}
		}
	}
}

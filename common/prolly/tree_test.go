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

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

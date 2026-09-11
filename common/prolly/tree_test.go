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

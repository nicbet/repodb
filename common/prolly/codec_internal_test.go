package prolly

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nicbet/repodb/common/storage"
)

func sampleNodes(t *testing.T) []node {
	t.Helper()
	leaf := node{Level: 0, Entries: []Entry{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte{}},
		{Key: bytes.Repeat([]byte("k"), 300), Value: bytes.Repeat([]byte("v"), 1000)},
	}}
	interior := node{Level: 2, Children: []link{
		{MaxKey: []byte("m"), Count: 64, Hash: storage.Sum([]byte("one"))},
		{MaxKey: []byte("z"), Count: 1 << 40, Hash: storage.Sum([]byte("two"))},
	}}
	return []node{leaf, interior, {Level: 0}}
}

func TestNodeCodecRoundTrip(t *testing.T) {
	for i, n := range sampleNodes(t) {
		data, err := encodeNode(n)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeNode("h", data)
		if err != nil {
			t.Fatalf("node %d: %v", i, err)
		}
		if fmt.Sprint(got) != fmt.Sprint(n) {
			t.Fatalf("node %d: decoded %v, want %v", i, got, n)
		}
		if again, _ := encodeNode(got); !bytes.Equal(again, data) {
			t.Fatalf("node %d: re-encoding differs", i)
		}
	}
}

func TestNodeCodecRejectsMalformedNodes(t *testing.T) {
	for i, n := range sampleNodes(t) {
		data, err := encodeNode(n)
		if err != nil {
			t.Fatal(err)
		}
		for cut := 0; cut < len(data); cut++ {
			if _, err := decodeNode("h", data[:cut]); err == nil {
				t.Fatalf("node %d truncated to %d bytes decoded", i, cut)
			}
		}
		if _, err := decodeNode("h", append(append([]byte(nil), data...), 0)); err == nil || !strings.Contains(err.Error(), "trailing bytes") {
			t.Fatalf("node %d with a trailing byte: %v", i, err)
		}
	}
	data, _ := encodeNode(node{Level: 0})
	for _, bad := range [][]byte{
		append([]byte{'{'}, data[1:]...),          // a JSON node from format 4
		append([]byte{'P', 2}, data[2:]...),       // unknown codec version
		{'P', 1, 0, 0xff, 0xff, 0xff, 0xff, 0x0f}, // count beyond the input
	} {
		if _, err := decodeNode("h", bad); err == nil {
			t.Fatalf("decoded malformed node %x", bad)
		}
	}
	if _, err := encodeNode(node{Level: 1, Children: []link{{Hash: "not-a-hash"}}}); err == nil {
		t.Fatal("encoded a child link with an invalid hash")
	}
}

func TestDecodedEntriesCannotOverwriteNeighbors(t *testing.T) {
	data, _ := encodeNode(sampleNodes(t)[0])
	n, err := decodeNode("h", data)
	if err != nil {
		t.Fatal(err)
	}
	_ = append(n.Entries[0].Key, 'X')
	_ = append(n.Entries[0].Value, 'X')
	if again, _ := encodeNode(sampleNodes(t)[0]); !bytes.Equal(again, data) {
		t.Fatal("appending to a decoded entry modified the node bytes")
	}
}

func TestReachableRejectsWrongChildCount(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	entries := make([]Entry, 500)
	for i := range entries {
		entries[i] = Entry{Key: []byte(fmt.Sprintf("key-%04d", i)), Value: []byte("v")}
	}
	tree, err := Build(ctx, store, entries, DefaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	root, err := readNode(ctx, store, tree.Root())
	if err != nil || root.Level == 0 {
		t.Fatalf("expected an interior root: %v", err)
	}
	root.Children[0].Count++
	bad, err := writeNode(ctx, store, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reachable(ctx, store, bad); err == nil || !strings.Contains(err.Error(), "counts") {
		t.Fatalf("Reachable accepted a wrong child count: %v", err)
	}
}

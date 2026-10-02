package repository

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// pendingModel is the reference: the newest edit per key.
type pendingModel map[string]TypedRowEdit

func (m pendingModel) with(edits []TypedRowEdit) pendingModel {
	next := make(pendingModel, len(m)+len(edits))
	for k, v := range m {
		next[k] = v
	}
	for _, edit := range edits {
		next[string(edit.Key)] = edit
	}
	return next
}

func checkPending(t *testing.T, label string, p PendingRows, model pendingModel) {
	t.Helper()
	keys := make([]string, 0, len(model))
	for k := range model {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var got []TypedRowEdit
	for it := p.Iter(nil, nil); ; {
		edit, ok := it.Next()
		if !ok {
			break
		}
		got = append(got, edit)
	}
	if len(got) != len(keys) {
		t.Fatalf("%s: iterated %d edits, want %d", label, len(got), len(keys))
	}
	for i, key := range keys {
		want := model[key]
		if string(got[i].Key) != key || !bytes.Equal(got[i].Value, want.Value) || got[i].Delete != want.Delete {
			t.Fatalf("%s: edit %d = %q/%q/%v, want %q/%q/%v", label, i, got[i].Key, got[i].Value, got[i].Delete, key, want.Value, want.Delete)
		}
		edit, ok := p.Get([]byte(key))
		if !ok || !bytes.Equal(edit.Value, want.Value) || edit.Delete != want.Delete {
			t.Fatalf("%s: Get(%q) = %q/%v/%v, want %q/%v", label, key, edit.Value, edit.Delete, ok, want.Value, want.Delete)
		}
	}
	if _, ok := p.Get([]byte("absent")); ok {
		t.Fatalf("%s: Get found an absent key", label)
	}
}

func TestPendingRowsMatchesModel(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var p PendingRows
	model := pendingModel{}
	type version struct {
		p     PendingRows
		model pendingModel
	}
	var versions []version
	for batch := 0; batch < 300; batch++ {
		edits := make([]TypedRowEdit, 1+rng.Intn(20))
		for i := range edits {
			key := []byte(fmt.Sprintf("k%04d", rng.Intn(500)))
			if rng.Intn(4) == 0 {
				edits[i] = TypedRowEdit{Key: key, Delete: true}
			} else {
				edits[i] = TypedRowEdit{Key: key, Value: []byte(fmt.Sprintf("v%d-%d", batch, i))}
			}
		}
		p, model = p.With(edits), model.with(edits)
		if batch%10 == 0 {
			versions = append(versions, version{p, model})
		}
		if len(p.runs) > 12 {
			t.Fatalf("batch %d: %d runs, want O(log n)", batch, len(p.runs))
		}
	}
	checkPending(t, "final", p, model)
	// Earlier versions are unchanged by later With calls.
	for i, v := range versions {
		checkPending(t, fmt.Sprintf("version %d", i), v.p, v.model)
	}
}

func TestPendingRowsLastEditInBatchWins(t *testing.T) {
	p := PendingRows{}.With([]TypedRowEdit{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("a"), Value: []byte("2")},
		{Key: []byte("b"), Value: []byte("x")},
		{Key: []byte("a"), Delete: true},
	})
	checkPending(t, "batch", p, pendingModel{"a": {Key: []byte("a"), Delete: true}, "b": {Key: []byte("b"), Value: []byte("x")}})
	if p.With(nil).Len() != p.Len() {
		t.Fatal("With(nil) changed the set")
	}
}

func TestPendingRowsIterInterval(t *testing.T) {
	var p PendingRows
	for i := 0; i < 50; i++ {
		p = p.With([]TypedRowEdit{{Key: []byte(fmt.Sprintf("k%02d", i)), Value: []byte{byte(i)}}})
	}
	p = p.With([]TypedRowEdit{{Key: []byte("k10"), Delete: true}})
	var keys []string
	for it := p.Iter([]byte("k10"), []byte("k13")); ; {
		edit, ok := it.Next()
		if !ok {
			break
		}
		keys = append(keys, fmt.Sprintf("%s:%v", edit.Key, edit.Delete))
	}
	if got, want := fmt.Sprint(keys), "[k10:true k11:false k12:false]"; got != want {
		t.Fatalf("interval = %s, want %s", got, want)
	}
}

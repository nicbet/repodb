package main

import (
	"testing"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

func TestPersistenceDefaultsToJournal(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want engine.PersistenceMode
	}{
		{nil, engine.PersistenceJournal},
		{[]string{"-persistence", "native-git"}, engine.PersistenceNativeGit},
		{[]string{"-persistence", "journal"}, engine.PersistenceJournal},
	} {
		opts, err := parseOptions(tc.args)
		if err != nil || opts.persistence != tc.want {
			t.Errorf("parseOptions(%v) = %v, %v; want %s", tc.args, opts.persistence, err, tc.want)
		}
	}
}

func TestDurabilityDefaultsToNormal(t *testing.T) {
	opts, err := parseOptions(nil)
	if err != nil || opts.durability != repository.DurabilityNormal {
		t.Fatalf("default durability = %q, %v", opts.durability, err)
	}
	if opts, err := parseOptions([]string{"-durability", "full"}); err != nil || opts.durability != repository.DurabilityFull {
		t.Fatalf("-durability full = %q, %v", opts.durability, err)
	}
	if _, err := parseOptions([]string{"-durability", "fast"}); err == nil {
		t.Fatal("parseOptions accepted -durability fast")
	}
}

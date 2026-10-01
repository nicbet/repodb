package main

import (
	"testing"

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

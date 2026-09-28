# Walkthrough: dbbench survives auto-gc during size measurement

**Symptom.** Native-git scorecards stopped each fixture after `clone_enable_open`. The four sync workloads (sync unchanged, edit/sync roundtrip, divergent merge, conflict resolve) were never measured, and the report ended with `lstat …/.git/objects/xx/…: no such file or directory`. The 2026-09-13 run hit the same thing, as its `failure` field shows.

**Cause.** `measure` records fixture growth by summing file sizes with `directoryBytes` before and after each workload. The sync workloads fetch and push, which makes Git start `gc --auto` detached in the background. That gc prunes loose objects while `filepath.WalkDir` is iterating. An object listed by `ReadDir` and gone by the time `DirEntry.Info()` runs its `lstat` returned `ENOENT`, which aborted the fixture.

**Fix.** `directoryBytes` ignores `fs.ErrNotExist` passed to the walk callback (an entry or directory that vanished). The new `entryBytes(d)` returns 0 for a vanished entry and the size otherwise. Any other error still fails the measurement. Growth was already documented as an approximate logical size, so a pruned object counting as zero is within that definition.

**Test.** `TestEntryBytesToleratesVanishedFile` lists a directory, deletes the file, then calls `entryBytes` on the stale entry; it also checks that a missing root measures 0. On macOS/Linux, `os.ReadDir` entries `lstat` lazily, which makes the race deterministic in the test.

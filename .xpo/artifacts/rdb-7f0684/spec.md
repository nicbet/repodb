# Drop the object inventory from `manifest.json` (format 7)

## What
`manifest.json` no longer lists objects. A data commit's Git tree (`objects/sha256/xx/…`) **is** the inventory. The storage format goes from 6 to 7, with no migration (alpha).

## Why: measurement (2026-10-02)
Measured with dbbench `mutable`, journal mode, 100k issues, 3 growth rounds, on macOS; the fixture held 5,736 objects. New objects per sync round of 600 scattered edits:

| Kind | Count | Raw | On disk (loose) |
|---|---|---|---|
| Data blobs (Prolly nodes) | ~600 | 18–24 MB | 5.0–6.6 MB |
| Bucket trees `objects/sha256/xx` | ~252 | 0.51 MB | 0.38 MB |
| `manifest.json` | 1 | 0.38 MB | 0.22 MB |

- Push (thin pack) for one round: 4.36 MB, of which the blobs alone are 4.12 MB. Metadata is ~5% of a push and ~10% of local writes.
- The issue's ~2 MB per round estimate predates format 6's larger chunks.
- The inventory is purely redundant. `loadSnapshot` already lists the tree and requires it to match the inventory exactly. Dropping it saves ~0.2 MB per commit (in history too) and the strict decode of a ~385 KB JSON file on every snapshot open.

**Out of scope (decided with user):**
- **Bucket layout.** A two-level fan-out saves ~0.2 MB per round but adds trees; not worth it.
- **Leaf write amplification.** About 30 KB of new blob per scattered row edit, which is the real byte cost. Filed separately.

## How
### 1. `common/repository/repository.go`
- `Manifest` loses `Objects`. Persisted fields: `format_version`, `default_database`, `tables`. `FormatVersion = 7`.
- `Writer.Commit`: build the sorted inventory locally (`sortedHashSet(available)`). Use it for the tree entries and hash-object; it is not written into the manifest.
- `loadSnapshot`: decode the manifest strictly, as now. Then build `objectSet`/`objectOIDs` from `ls-tree -r`. Every entry must be either `manifest.json` or a path equal to `objectPath(hash)` for a valid hash; anything else is `ErrCorrupt` ("unexpected tree entry"). `validateManifestInventory` (table roots ⊆ objects) then runs against the tree-derived set.
- `verifySnapshotTree`: compare entry count with `len(snapshot.objectSet)+1`.
- New accessor `Snapshot.ObjectCount() int`, used by `repodb status` (`cmd/repodb/main.go`) in place of `len(Manifest.Objects)`. Also add `Snapshot.Objects() []storage.Hash` (sorted) if a caller needs the list.

### 2. `common/repository/working.go`
- `takeWorkingWriter`, `applyTypedEditsToSnapshot` and `Checkpoint` use `objectSet` (or the sorted list) instead of `Manifest.Objects`.
- The legacy `prepare` journal record still needs its inventory. Add `Inventory []storage.Hash \`json:"inventory,omitempty"\`` to `journalRecord`. Replay builds `available` from it and runs the same checks (sorted, unique, valid, covers table roots).
- Bump `workingFormatVersion` to 4: old journals sit on format-6 bases, which are refused anyway.

### 3. Tests
- Update `engine/engine_test.go:1289` and any other `Manifest.Objects` users.
- New repository tests:
  - a published `manifest.json` has no `objects` key;
  - a tree with an extra non-object entry, or a malformed object path, fails to open with `ErrCorrupt`;
  - a table root missing from the tree fails to open with `ErrCorrupt`;
  - a format-6 manifest is refused with "unsupported RepoDB format".
- Existing journal replay and recovery tests pass, including legacy `prepare` with an inventory.

### 4. Docs
- `docs/architecture.md` § Snapshots: format 7, the manifest field list without `objects`, and the open checks ("every tree entry is the manifest or a well-formed object path; every table root is present").
- Fix any `v6` / `format_version 6` mentions in `docs/` (grep).

## Acceptance criteria
- [ ] `manifest.json` contains no object list; format 7; format-6 data is refused with the existing message.
- [ ] Snapshot open derives the inventory from the tree and rejects unexpected or malformed entries and missing table roots.
- [ ] Journal legacy `prepare` keeps an inventory in the record; replay validates it.
- [ ] `go test ./...` passes (with `-tags gms_pure_go` as the Makefile uses).
- [ ] Re-measure one 100k `mutable` round: `manifest.json` is a few KB; bucket and blob bytes are unchanged.
- [ ] Docs updated.

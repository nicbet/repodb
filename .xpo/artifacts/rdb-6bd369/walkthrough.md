# Walkthrough: PendingRows.With sort

Fixed in the rdb-a0d510 change (commit `rdb-a0d510: … fixes rdb-6bd369`).

**Problem.** `PendingRows.With` (rdb-a6a4d2) built each new run with `sort.SliceStable` over `TypedRowEdit` structs (56 bytes each). Go's stable sort is insertion sort plus merging by rotation, O(n log² n) element moves. A 50k-row bulk commit spent about 0.3 s of CPU there, and in-process bulk load rose from ~138 to ~166 ms.

**Fix.** Sort a `[]int32` permutation of the edits with `slices.SortFunc` (pdqsort), comparing keys and breaking ties by position. With ties broken by position the result is the stable order, so "last edit of a key wins" holds. The deduplicated run is then built in one pass over the permutation. Swaps move 4-byte indices, and the sort does O(n log n) work.

**Result.** In-process bulk load is 139–142 ms (3 × 5 runs), back at the `062f8b3` level. The `PendingRows` model, batch and interval tests pass unchanged; they cover last-in-batch-wins.

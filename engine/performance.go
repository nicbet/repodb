package engine

import "sync/atomic"

// PerformanceCounters expose structural work for benchmarks and regression
// tests. They are process-wide, monotonic between resets, and not persisted.
type PerformanceCounters struct {
	RowsDecoded        uint64
	RowsScanned        uint64
	PointKeysVisited   uint64
	TablesRebuilt      uint64
	TablesReused       uint64
	UndoRowsCaptured   uint64
	TreeMutationNanos  uint64
	ReachabilityNanos  uint64
	SnapshotBuildNanos uint64
}

var performanceCounters struct {
	rowsDecoded        atomic.Uint64
	rowsScanned        atomic.Uint64
	pointKeysVisited   atomic.Uint64
	tablesRebuilt      atomic.Uint64
	tablesReused       atomic.Uint64
	undoRowsCaptured   atomic.Uint64
	treeMutationNanos  atomic.Uint64
	reachabilityNanos  atomic.Uint64
	snapshotBuildNanos atomic.Uint64
}

func ResetPerformanceCounters() {
	performanceCounters.rowsDecoded.Store(0)
	performanceCounters.rowsScanned.Store(0)
	performanceCounters.pointKeysVisited.Store(0)
	performanceCounters.tablesRebuilt.Store(0)
	performanceCounters.tablesReused.Store(0)
	performanceCounters.undoRowsCaptured.Store(0)
	performanceCounters.treeMutationNanos.Store(0)
	performanceCounters.reachabilityNanos.Store(0)
	performanceCounters.snapshotBuildNanos.Store(0)
}

func ReadPerformanceCounters() PerformanceCounters {
	return PerformanceCounters{
		RowsDecoded:        performanceCounters.rowsDecoded.Load(),
		RowsScanned:        performanceCounters.rowsScanned.Load(),
		PointKeysVisited:   performanceCounters.pointKeysVisited.Load(),
		TablesRebuilt:      performanceCounters.tablesRebuilt.Load(),
		TablesReused:       performanceCounters.tablesReused.Load(),
		UndoRowsCaptured:   performanceCounters.undoRowsCaptured.Load(),
		TreeMutationNanos:  performanceCounters.treeMutationNanos.Load(),
		ReachabilityNanos:  performanceCounters.reachabilityNanos.Load(),
		SnapshotBuildNanos: performanceCounters.snapshotBuildNanos.Load(),
	}
}

package repository

import "sync/atomic"

type PublicationMetrics struct {
	InventoryNanos    uint64
	LockWaitNanos     uint64
	LockHoldNanos     uint64
	ObjectWriteNanos  uint64
	TreeCommitNanos   uint64
	RefUpdateNanos    uint64
	VerificationNanos uint64
}

var publicationMetrics struct {
	inventory, lockWait, lockHold, objectWrite, treeCommit, refUpdate, verification atomic.Uint64
}

func ResetPublicationMetrics() {
	publicationMetrics.inventory.Store(0)
	publicationMetrics.lockWait.Store(0)
	publicationMetrics.lockHold.Store(0)
	publicationMetrics.objectWrite.Store(0)
	publicationMetrics.treeCommit.Store(0)
	publicationMetrics.refUpdate.Store(0)
	publicationMetrics.verification.Store(0)
}

func ReadPublicationMetrics() PublicationMetrics {
	return PublicationMetrics{
		InventoryNanos: publicationMetrics.inventory.Load(), LockWaitNanos: publicationMetrics.lockWait.Load(),
		LockHoldNanos: publicationMetrics.lockHold.Load(), ObjectWriteNanos: publicationMetrics.objectWrite.Load(),
		TreeCommitNanos: publicationMetrics.treeCommit.Load(), RefUpdateNanos: publicationMetrics.refUpdate.Load(),
		VerificationNanos: publicationMetrics.verification.Load(),
	}
}

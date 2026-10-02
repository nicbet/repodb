package repository

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nicbet/repodb/common/robustio"
)

// ErrWorkingHistoryTruncated reports a transaction older than the journal
// history that compaction retains, whose outcome can no longer be told.
var ErrWorkingHistoryTruncated = errors.New("RepoDB journal transaction is older than the retained journal history")

// journalHeaderSize is the size of a frame header: "RDBJ", payload length and
// CRC-32C. The first frame's header identifies a journal file (see
// readJournalFrames).
const journalHeaderSize = 12

// encodeFrame frames one journal record.
func encodeFrame(record journalRecord) ([]byte, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	frame := make([]byte, journalHeaderSize, journalHeaderSize+len(data))
	copy(frame[:4], "RDBJ")
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(data)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.Checksum(data, journalCRC))
	return append(frame, data...), nil
}

// compactLocked replaces the journal with a single anchor frame after a
// checkpoint has been recorded in it. Everything before that checkpoint is
// already published in Git; replay starts from the anchor. The anchor keeps
// the IDs of the transactions it drops, so RecoverTransaction can still answer
// for them (see recoverFromAnchor). The caller holds working.lock.
//
// Compaction is best effort: the checkpoint it follows is already published,
// so a failure leaves the old journal, which is valid, and the next
// checkpoint compacts it.
func (w *WorkingState) compactLocked(checkpoint journalRecord) (os.FileInfo, error) {
	records, err := readJournal(w.JournalPath())
	if err != nil {
		return nil, err
	}
	anchor := checkpoint
	anchor.CheckpointedTxIDs = nil
	if len(records) > 0 && records[0].Kind == "checkpoint" {
		anchor.FromGeneration = records[0].Generation
	}
	for _, record := range records {
		if record.Kind == "commit" {
			anchor.CheckpointedTxIDs = append(anchor.CheckpointedTxIDs, record.TxID)
		}
	}
	frame, err := encodeFrame(anchor)
	if err != nil {
		return nil, err
	}
	tmp := w.JournalPath() + ".tmp"
	if err := writeSynced(tmp, frame); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if w.fault != nil {
		if err := w.fault(BeforeJournalCompactionRename); err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
	}
	if err := robustio.Rename(tmp, w.JournalPath()); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := syncDir(filepath.Dir(w.JournalPath())); err != nil {
		return nil, err
	}
	if w.fault != nil {
		if err := w.fault(AfterJournalCompactionRename); err != nil {
			return nil, err
		}
	}
	return os.Stat(w.JournalPath())
}

func writeSynced(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := flushFile(file, DurabilityFull); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// compactAfterCheckpoint runs compactLocked and, on success, points this
// instance's caches at the new file. It never fails the checkpoint.
func (w *WorkingState) compactAfterCheckpoint(checkpoint journalRecord, view workingView) {
	info, err := w.compactLocked(checkpoint)
	if err != nil {
		w.metrics.compactionFailures.Add(1)
		// The cache may describe the old file or a half-applied state; the
		// next load re-reads whatever is on disk.
		w.cache = nil
		return
	}
	w.metrics.compactions.Add(1)
	first, err := journalFirstHeader(w.JournalPath())
	if err != nil {
		w.cache = nil
		return
	}
	w.cache = &workingCache{view: view, info: info, offset: info.Size(), first: first}
	w.dirtyScan = journalDirtyScan{}
}

// newTransactionID returns an ID carrying the generation the transaction
// would commit at, so RecoverTransaction can tell whether compaction may have
// dropped it.
func newTransactionID(generation uint64) (string, error) {
	random, err := randomID()
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(generation, 10) + "-" + random, nil
}

func transactionGeneration(id string) (uint64, bool) {
	prefix, _, found := strings.Cut(id, "-")
	if !found {
		return 0, false
	}
	generation, err := strconv.ParseUint(prefix, 10, 64)
	return generation, err == nil
}

// recoverFromAnchor settles a transaction ID that the current journal does not
// contain. An anchor lists the transactions committed in the journal it
// replaced, which covered generations above its FromGeneration; anything older
// is unknown.
func recoverFromAnchor(anchor *journalRecord, result WorkingCommitResult) (WorkingCommitResult, error) {
	if anchor == nil {
		// Full history is present: the transaction never committed.
		return result, nil
	}
	for _, id := range anchor.CheckpointedTxIDs {
		if id == result.TransactionID {
			result.Outcome = OutcomeCommitted
			result.Generation, _ = transactionGeneration(id)
			return result, nil
		}
	}
	generation, ok := transactionGeneration(result.TransactionID)
	if !ok || generation <= anchor.FromGeneration {
		result.Outcome = OutcomeUnknown
		return result, fmt.Errorf("%w: compacted at generation %d", ErrWorkingHistoryTruncated, anchor.FromGeneration)
	}
	return result, nil
}

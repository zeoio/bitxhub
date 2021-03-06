package ledger

import (
	"fmt"
	"sync"

	"github.com/meshplus/bitxhub-kit/types"
	"github.com/meshplus/bitxhub-model/pb"
	"github.com/meshplus/bitxhub/internal/repo"
	"github.com/meshplus/bitxhub/pkg/storage"
	"github.com/meshplus/bitxhub/pkg/storage/leveldb"
	"github.com/sirupsen/logrus"
)

var _ Ledger = (*ChainLedger)(nil)

var (
	ErrorRollbackToHigherNumber  = fmt.Errorf("rollback to higher blockchain height")
	ErrorRollbackWithoutJournal  = fmt.Errorf("rollback to blockchain height without journal")
	ErrorRollbackTooMuch         = fmt.Errorf("rollback too much block")
	ErrorRemoveJournalOutOfRange = fmt.Errorf("remove journal out of range")
)

type ChainLedger struct {
	logger          logrus.FieldLogger
	blockchainStore storage.Storage
	ldb             storage.Storage
	minJnlHeight    uint64
	maxJnlHeight    uint64
	events          map[string][]*pb.Event
	accounts        map[string]*Account
	prevJnlHash     types.Hash

	chainMutex sync.RWMutex
	chainMeta  *pb.ChainMeta
}

// New create a new ledger instance
func New(repoRoot string, blockchainStore storage.Storage, logger logrus.FieldLogger) (*ChainLedger, error) {
	ldb, err := leveldb.New(repo.GetStoragePath(repoRoot, "ledger"))
	if err != nil {
		return nil, fmt.Errorf("create tm-leveldb: %w", err)
	}

	chainMeta, err := loadChainMeta(blockchainStore)
	if err != nil {
		return nil, fmt.Errorf("load chain meta: %w", err)
	}

	minJnlHeight, maxJnlHeight := getJournalRange(ldb)

	if maxJnlHeight < chainMeta.Height {
		// TODO(xcc): how to handle this case
		panic("state tree height is less than blockchain height")
	}

	prevJnlHash := types.Hash{}
	if maxJnlHeight != 0 {
		blockJournal := getBlockJournal(maxJnlHeight, ldb)
		prevJnlHash = blockJournal.ChangedHash
	}

	return &ChainLedger{
		logger:          logger,
		chainMeta:       chainMeta,
		blockchainStore: blockchainStore,
		ldb:             ldb,
		minJnlHeight:    minJnlHeight,
		maxJnlHeight:    maxJnlHeight,
		events:          make(map[string][]*pb.Event, 10),
		accounts:        make(map[string]*Account),
		prevJnlHash:     prevJnlHash,
	}, nil
}

// Rollback rollback ledger to history version
func (l *ChainLedger) Rollback(height uint64) error {
	if l.maxJnlHeight < height {
		return ErrorRollbackToHigherNumber
	}

	if l.minJnlHeight > height {
		return ErrorRollbackTooMuch
	}

	if l.maxJnlHeight == height {
		return nil
	}

	// clean cache account
	l.Clear()

	for i := l.maxJnlHeight; i > height; i-- {
		batch := l.ldb.NewBatch()

		blockJournal := getBlockJournal(i, l.ldb)
		if blockJournal == nil {
			return ErrorRollbackWithoutJournal
		}

		for _, journal := range blockJournal.Journals {
			journal.revert(batch)
		}

		batch.Delete(compositeKey(journalKey, i))
		batch.Put(compositeKey(journalKey, maxHeightStr), marshalHeight(i-1))
		batch.Commit()
	}

	journal := getBlockJournal(height, l.ldb)

	l.maxJnlHeight = height
	l.prevJnlHash = journal.ChangedHash

	return nil
}

// RemoveJournalsBeforeBlock removes ledger journals whose block number < height
func (l *ChainLedger) RemoveJournalsBeforeBlock(height uint64) error {
	if height > l.maxJnlHeight {
		return ErrorRemoveJournalOutOfRange
	}

	if height <= l.minJnlHeight {
		return nil
	}

	batch := l.ldb.NewBatch()
	for i := l.minJnlHeight; i < height; i++ {
		batch.Delete(compositeKey(journalKey, i))
	}
	batch.Put(compositeKey(journalKey, minHeightStr), marshalHeight(height))
	batch.Commit()

	l.minJnlHeight = height

	return nil
}

// AddEvent add ledger event
func (l *ChainLedger) AddEvent(event *pb.Event) {
	hash := event.TxHash.Hex()
	l.events[hash] = append(l.events[hash], event)
}

// Events return ledger events
func (l *ChainLedger) Events(txHash string) []*pb.Event {
	return l.events[txHash]
}

// Close close the ledger instance
func (l *ChainLedger) Close() {
	l.ldb.Close()
}

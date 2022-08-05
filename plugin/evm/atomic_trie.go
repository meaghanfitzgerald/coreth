// (c) 2020-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package evm

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/ava-labs/avalanchego/chains/atomic"
	"github.com/ava-labs/avalanchego/codec"
	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/database/prefixdb"
	"github.com/ava-labs/avalanchego/database/versiondb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/utils/wrappers"

	"github.com/ava-labs/coreth/core"
	"github.com/ava-labs/coreth/core/types"
	"github.com/ava-labs/coreth/ethdb"
	syncclient "github.com/ava-labs/coreth/sync/client"
	"github.com/ava-labs/coreth/trie"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

const (
	progressLogUpdate          = 30 * time.Second
	atomicKeyLength            = wrappers.LongLen + common.HashLength
	sharedMemoryApplyBatchSize = 10_000 // specifies the number of atomic operations to batch progress updates

	atomicTrieTipBufferSize = 1
	atomicTrieMemoryCap     = 64 * units.MiB
)

var (
	_                            AtomicTrie         = &atomicTrie{}
	_                            AtomicTrieSnapshot = &atomicTrieSnapshot{}
	lastCommittedKey                                = []byte("atomicTrieLastCommittedBlock")
	appliedSharedMemoryCursorKey                    = []byte("atomicTrieLastAppliedToSharedMemory")
)

// AtomicTrie maintains an index of atomic operations by blockchainIDs for every block
// height containing atomic transactions. The backing data structure for this index is
// a Trie. The keys of the trie are block heights and the values (leaf nodes)
// are the atomic operations applied to shared memory while processing the block accepted
// at the corresponding height.
type AtomicTrie interface {
	// OpenTrie returns a modifiable instance of the atomic trie backed by trieDB
	// opened at hash.
	OpenTrie(hash common.Hash) (AtomicTrieSnapshot, error)

	// Iterator returns an AtomicTrieIterator to iterate the trie at the given
	// root hash starting at [cursor].
	Iterator(hash common.Hash, cursor []byte) (AtomicTrieIterator, error)

	// LastCommitted returns the last committed hash and corresponding block height
	LastCommitted() (common.Hash, uint64)

	// TrieDB returns the underlying trie database
	TrieDB() *trie.Database

	// Root returns hash if it exists at specified height
	// if trie was not committed at provided height, it returns
	// common.Hash{} instead
	Root(height uint64) (common.Hash, error)

	// ApplyToSharedMemory applies the atomic operations that have been indexed into the trie
	// but not yet applied to shared memory for heights less than or equal to [lastAcceptedBlock].
	// This executes operations in the range [cursorHeight+1, lastAcceptedBlock].
	// The cursor is initially set by  MarkApplyToSharedMemoryCursor to signal to the atomic trie
	// the range of operations that were added to the trie without being executed on shared memory.
	ApplyToSharedMemory(lastAcceptedBlock uint64) error

	// MarkApplyToSharedMemoryCursor marks the atomic trie as containing atomic ops that
	// have not been executed on shared memory starting at [previousLastAcceptedHeight+1].
	// This is used when state sync syncs the atomic trie, such that the atomic operations
	// from [previousLastAcceptedHeight+1] to the [lastAcceptedHeight] set by state sync
	// will not have been executed on shared memory.
	MarkApplyToSharedMemoryCursor(previousLastAcceptedHeight uint64) error

	// Syncer creates and returns a new Syncer object that can be used to sync the
	// state of the atomic trie from peers
	Syncer(client syncclient.LeafClient, targetRoot common.Hash, targetHeight uint64) (Syncer, error)

	// LastAcceptedRoot returns the most recent accepted root of the atomic trie,
	// or the root it was initialized to if no new tries were accepted yet.
	LastAcceptedRoot() common.Hash

	// InsertTrie adds a reference for root in the trieDB. Once InsertTrie
	// is called, it is expected either AcceptTrie or RejectTrie be called.
	InsertTrie(root common.Hash) error

	// AcceptTrie marks root as the last accepted atomic trie root, and
	// commits the trie to persistent storage if height is divisible by
	// the commit interval.
	AcceptTrie(height uint64, root common.Hash) error

	// RejectTrie dereferences root from the trieDB, freeing memory.
	RejectTrie(root common.Hash) error
}

// TODO: rename this
// AtomicTrieSnapshot represents a modifyable atomic trie
type AtomicTrieSnapshot interface {
	// UpdateTrie updates the trie to inlude atomicOps for height.
	UpdateTrie(height uint64, atomicOps map[ids.ID]*atomic.Requests) error

	// TryUpdate updates the underlying trie with the raw key/value bytes.
	// Used in syncing.
	TryUpdate(key []byte, val []byte) error

	// Root hashes the changes and generates a root without writing to
	// the trieDB.
	Root() common.Hash

	// Commit hashes the changes and generates a root, committing modified
	// trie nodes to the trieDB.
	Commit() (common.Hash, error)
}

// AtomicTrieIterator is a stateful iterator that iterates the leafs of an AtomicTrie
type AtomicTrieIterator interface {
	// Next advances the iterator to the next node in the atomic trie and
	// returns true if there are more leaves to iterate
	Next() bool

	// Key returns the current database key that the iterator is iterating
	// returned []byte can be freely modified
	Key() []byte

	// BlockNumber returns the current block number
	BlockNumber() uint64

	// BlockchainID returns the current blockchain ID at the current block number
	BlockchainID() ids.ID

	// AtomicOps returns a map of blockchainIDs to the set of atomic requests
	// for that blockchainID at the current block number
	AtomicOps() *atomic.Requests

	// Error returns error, if any encountered during this iteration
	Error() error
}

// atomicTrie implements the AtomicTrie interface
type atomicTrie struct {
	commitInterval      uint64              // commit interval, same as commitHeightInterval by default
	db                  *versiondb.Database // Underlying database
	bonusBlocks         map[uint64]ids.ID   // Map of height to blockID for blocks to skip indexing
	metadataDB          database.Database   // Underlying database containing the atomic trie metadata
	atomicTrieDB        database.Database   // Underlying database containing the atomic trie
	trieDB              *trie.Database      // Trie database
	repo                AtomicTxRepository
	lastCommittedRoot   common.Hash // trie root of the most recent commit
	lastCommittedHeight uint64      // index height of the most recent commit
	lastAcceptedRoot    common.Hash // most recent trie root passed to accept trie or the root of the atomic trie on intialization.
	codec               codec.Manager
	log                 log.Logger // struct logger
	sharedMemory        atomic.SharedMemory
	memoryCap           common.StorageSize
	tipBuffer           *core.BoundedBuffer
}

// atomicTrieSnapshot implements the AtomicTrieSnapshot interface
type atomicTrieSnapshot struct {
	trie       *trie.Trie
	atomicTrie *atomicTrie
}

// NewAtomicTrie returns a new instance of a atomicTrie with the default commitHeightInterval.
// Initializes the trie before returning it.
// If the cursor set by MarkApplyToSharedMemoryCursor exists, the atomic operations are applied synchronously
// during initialization (blocks until ApplyToSharedMemory completes).
func NewAtomicTrie(
	db *versiondb.Database, sharedMemory atomic.SharedMemory,
	bonusBlocks map[uint64]ids.ID, repo AtomicTxRepository, codec codec.Manager, lastAcceptedHeight uint64, commitHeightInterval uint64,
) (AtomicTrie, error) {
	return newAtomicTrie(db, sharedMemory, bonusBlocks, repo, codec, lastAcceptedHeight, commitHeightInterval)
}

// newAtomicTrie returns a new instance of a atomicTrie with a configurable commitHeightInterval, used in testing.
// Initializes the trie before returning it.
func newAtomicTrie(
	db *versiondb.Database, sharedMemory atomic.SharedMemory,
	bonusBlocks map[uint64]ids.ID, repo AtomicTxRepository, codec codec.Manager,
	lastAcceptedHeight uint64, commitHeightInterval uint64,
) (*atomicTrie, error) {
	atomicTrieDB := prefixdb.New(atomicTrieDBPrefix, db)
	metadataDB := prefixdb.New(atomicTrieMetaDBPrefix, db)
	root, height, err := lastCommittedRootIfExists(metadataDB)
	if err != nil {
		return nil, err
	}
	// If the last committed height is above the last accepted height, then we fall back to
	// the last commit below the last accepted height.
	if height > lastAcceptedHeight {
		height = nearestCommitHeight(lastAcceptedHeight, commitHeightInterval)
		root, err = getRoot(metadataDB, height)
		if err != nil {
			return nil, err
		}
	}

	trieDB := trie.NewDatabaseWithConfig(
		Database{atomicTrieDB},
		&trie.Config{
			Cache:     64,    // Allocate 64MB of memory for clean cache
			Preimages: false, // Keys are not hashed, so there is no need for preimages
		},
	)
	atomicTrie := &atomicTrie{
		commitInterval:      commitHeightInterval,
		db:                  db,
		bonusBlocks:         bonusBlocks,
		atomicTrieDB:        atomicTrieDB,
		metadataDB:          metadataDB,
		trieDB:              trieDB,
		repo:                repo,
		codec:               codec,
		lastCommittedRoot:   root,
		lastCommittedHeight: height,
		tipBuffer:           core.NewBoundedBuffer(atomicTrieTipBufferSize, trieDB.Dereference),
		log:                 log.New("c", "atomicTrie"),
		sharedMemory:        sharedMemory,
		memoryCap:           atomicTrieMemoryCap,
	}

	// We call ApplyToSharedMemory here to ensure that if the node was shut down in the middle
	// of applying atomic operations from state sync, we finish the operation to ensure we never
	// return an atomic trie that is out of sync with shared memory.
	// In normal operation, the cursor is not set, such that this call will be a no-op.
	if err := atomicTrie.ApplyToSharedMemory(lastAcceptedHeight); err != nil {
		return nil, err
	}
	return atomicTrie, atomicTrie.initialize(lastAcceptedHeight)
}

// lastCommittedRootIfExists returns the last committed trie root and height if it exists
// else returns empty common.Hash{} and 0
// returns error only if there are issues with the underlying data store
// or if values present in the database are not as expected
func lastCommittedRootIfExists(db database.Database) (common.Hash, uint64, error) {
	// read the last committed entry if it exists and set the root hash
	lastCommittedHeightBytes, err := db.Get(lastCommittedKey)
	switch {
	case err == database.ErrNotFound:
		return common.Hash{}, 0, nil
	case err != nil:
		return common.Hash{}, 0, err
	case len(lastCommittedHeightBytes) != wrappers.LongLen:
		return common.Hash{}, 0, fmt.Errorf("expected value of lastCommittedKey to be %d but was %d", wrappers.LongLen, len(lastCommittedHeightBytes))
	}
	height := binary.BigEndian.Uint64(lastCommittedHeightBytes)
	hash, err := db.Get(lastCommittedHeightBytes)
	if err != nil {
		return common.Hash{}, 0, fmt.Errorf("committed hash does not exist for committed height: %d: %w", height, err)
	}
	return common.BytesToHash(hash), height, nil
}

// nearestCommitheight returns the nearest multiple of commitInterval less than or equal to blockNumber
func nearestCommitHeight(blockNumber uint64, commitInterval uint64) uint64 {
	return blockNumber - (blockNumber % commitInterval)
}

// initializes the atomic trie using the atomic repository height index.
// Iterating from the last indexed height to lastAcceptedBlockNumber, making a single commit at the
// most recent height divisible by the commitInterval.
// Subsequent updates to this trie are made using the Index call as blocks are accepted.
// Note: this method assumes no atomic txs are applied at genesis.
func (a *atomicTrie) initialize(lastAcceptedBlockNumber uint64) error {
	start := time.Now()
	a.log.Info("initializing atomic trie", "lastAcceptedBlockNumber", lastAcceptedBlockNumber)
	// finalCommitHeight is the highest block that can be committed i.e. is divisible by b.commitHeightInterval
	// Txs from heights greater than commitHeight are to be included in the trie corresponding to the block at
	// finalCommitHeight+b.commitHeightInterval, which has not been accepted yet.
	finalCommitHeight := nearestCommitHeight(lastAcceptedBlockNumber, a.commitInterval)
	uncommittedOpsMap := make(map[uint64]map[ids.ID]*atomic.Requests, lastAcceptedBlockNumber-finalCommitHeight)

	// iterate by height, from [a.lastCommittedHeight+1] to [lastAcceptedBlockNumber]
	iter := a.repo.IterateByHeight(a.lastCommittedHeight + 1)
	defer iter.Release()

	preCommitBlockIndexed := 0
	postCommitTxIndexed := 0
	lastUpdate := time.Now()

	// open the atomic trie at the last committed root
	tr, err := a.OpenTrie(a.lastCommittedRoot)
	if err != nil {
		return err
	}

	lastHeight := a.lastCommittedHeight // track the last committed height
	for iter.Next() {
		// Get the height and transactions for this iteration (from the key and value, respectively)
		// iterate over the transactions, indexing them if the height is < commit height
		// otherwise, add the atomic operations from the transaction to the uncommittedOpsMap
		height := binary.BigEndian.Uint64(iter.Key())
		txs, err := ExtractAtomicTxs(iter.Value(), true, a.codec)
		if err != nil {
			return err
		}

		// combine atomic operations from all transactions at this block height
		combinedOps, err := mergeAtomicOps(txs)
		if err != nil {
			return err
		}

		// Check whether we have crossed over a commitHeight.
		// If so, make a commit with the current root before modifying the trie.
		commitHeight := nearestCommitHeight(height, a.commitInterval)
		for commitHeight > lastHeight && height > commitHeight {
			nextCommitHeight := lastHeight + a.commitInterval

			root, err := tr.Commit()
			if err != nil {
				return err
			}
			if err := a.AcceptTrie(nextCommitHeight, root); err != nil {
				return err
			}
			// Write to versiondb to persist changes to disk.
			if err := a.db.Commit(); err != nil {
				return err
			}

			lastHeight = nextCommitHeight
		}

		if _, skipBonusBlock := a.bonusBlocks[height]; skipBonusBlock {
			// If [height] is a bonus block, do not index the atomic operations into the trie
		} else if height > finalCommitHeight {
			// if height is greater than commit height, add it to the map so that we can write it later
			// this is to ensure we have all the data before the commit height so that we can commit the
			// trie
			uncommittedOpsMap[height] = combinedOps
		} else {
			if err := tr.UpdateTrie(height, combinedOps); err != nil {
				return err
			}
			preCommitBlockIndexed++
		}

		if time.Since(lastUpdate) > progressLogUpdate {
			a.log.Info("imported entries into atomic trie pre-commit", "heightsIndexed", preCommitBlockIndexed)
			lastUpdate = time.Now()
		}
	}
	if err := iter.Error(); err != nil {
		return err
	}

	// Note: we should never create a commit at the genesis block (should not contain any atomic txs)
	if lastAcceptedBlockNumber == 0 {
		a.lastAcceptedRoot = types.EmptyRootHash
		return nil
	}
	// now that all heights less than [finalCommitHeight] have been processed
	// commit the trie. If [a.lastCommittedHeight] is already the same as
	// [finalCommitHeight] (or higher, which can occur if we resume a state sync)
	// we skip this commit.
	for finalCommitHeight > lastHeight {
		nextCommitHeight := lastHeight + a.commitInterval

		root, err := tr.Commit()
		if err != nil {
			return err
		}
		if err := a.AcceptTrie(nextCommitHeight, root); err != nil {
			return err
		}
		// Write to versiondb to persist changes to disk.
		if err := a.db.Commit(); err != nil {
			return err
		}

		lastHeight = nextCommitHeight
	}

	// process uncommitted ops for heights > finalCommitHeight
	for height, ops := range uncommittedOpsMap {
		if err := tr.UpdateTrie(height, ops); err != nil {
			return fmt.Errorf("failed to update trie at height %d: %w", height, err)
		}

		postCommitTxIndexed++
		if time.Since(lastUpdate) > progressLogUpdate {
			a.log.Info("imported entries into atomic trie post-commit", "entriesIndexed", postCommitTxIndexed)
			lastUpdate = time.Now()
		}
	}
	a.lastAcceptedRoot, err = tr.Commit()
	if err != nil {
		return err
	}
	if err := a.InsertTrie(a.lastAcceptedRoot); err != nil {
		return err
	}
	if err := a.AcceptTrie(lastAcceptedBlockNumber, a.lastAcceptedRoot); err != nil {
		return err
	}

	a.log.Info(
		"finished initializing atomic trie",
		"lastAcceptedBlockNumber", lastAcceptedBlockNumber,
		"lastAcceptedAtomicRoot", a.lastAcceptedRoot,
		"preCommitEntriesIndexed", preCommitBlockIndexed,
		"postCommitEntriesIndexed", postCommitTxIndexed,
		"lastCommittedRoot", a.lastCommittedRoot,
		"lastCommittedHeight", a.lastCommittedHeight,
		"time", time.Since(start),
	)
	return nil
}

func (a *atomicTrie) OpenTrie(root common.Hash) (AtomicTrieSnapshot, error) {
	tr, err := trie.New(common.Hash{}, root, a.trieDB)
	if err != nil {
		return nil, err
	}
	return &atomicTrieSnapshot{
		trie:       tr,
		atomicTrie: a,
	}, nil
}

// Index updates the trie with entries in atomicOps
// This function updates the following:
// - heightBytes => trie root hash (if the trie was committed)
// - lastCommittedBlock => height (if the trie was committed)
func (a *atomicTrie) index(snapshot AtomicTrieSnapshot, height uint64, atomicOps map[ids.ID]*atomic.Requests) error {
	if err := snapshot.UpdateTrie(height, atomicOps); err != nil {
		return err
	}

	if height%a.commitInterval != 0 {
		return nil
	}

	root, err := snapshot.Commit()
	if err != nil {
		return err
	}

	return a.commit(height, root)
}

// commit calls commit on the underlying trieDB and updates metadata pointers.
func (a *atomicTrie) commit(height uint64, root common.Hash) error {
	if err := a.trieDB.Commit(root, false, nil); err != nil {
		return err
	}
	a.log.Info("committed atomic trie", "root", root.String(), "height", height)
	return a.updateLastCommitted(root, height)
}

func (a *atomicTrieSnapshot) UpdateTrie(height uint64, atomicOps map[ids.ID]*atomic.Requests) error {
	for blockchainID, requests := range atomicOps {
		valueBytes, err := a.atomicTrie.codec.Marshal(codecVersion, requests)
		if err != nil {
			// highly unlikely but possible if atomic.Element
			// has a change that is unsupported by the codec
			return err
		}

		// key is [height]+[blockchainID]
		keyPacker := wrappers.Packer{Bytes: make([]byte, atomicKeyLength)}
		keyPacker.PackLong(height)
		keyPacker.PackFixedBytes(blockchainID[:])
		if err := a.trie.TryUpdate(keyPacker.Bytes, valueBytes); err != nil {
			return err
		}
	}

	return nil
}

func (a *atomicTrieSnapshot) Root() common.Hash {
	return a.trie.Hash()
}

func (a *atomicTrieSnapshot) Commit() (common.Hash, error) {
	root, _, err := a.trie.Commit(nil)
	return root, err
}

func (a *atomicTrieSnapshot) TryUpdate(key []byte, val []byte) error {
	return a.trie.TryUpdate(key, val)
}

// LastCommitted returns the last committed trie hash and last committed height
func (a *atomicTrie) LastCommitted() (common.Hash, uint64) {
	return a.lastCommittedRoot, a.lastCommittedHeight
}

// updateLastCommitted adds [height] -> [root] to the index and marks it as the last committed
// root/height pair.
func (a *atomicTrie) updateLastCommitted(root common.Hash, height uint64) error {
	heightBytes := make([]byte, wrappers.LongLen)
	binary.BigEndian.PutUint64(heightBytes, height)

	// now save the trie hash against the height it was committed at
	if err := a.metadataDB.Put(heightBytes, root[:]); err != nil {
		return err
	}

	// update lastCommittedKey with the current height
	if err := a.metadataDB.Put(lastCommittedKey, heightBytes); err != nil {
		return err
	}

	a.lastCommittedRoot = root
	a.lastCommittedHeight = height
	return nil
}

// Iterator returns a types.AtomicTrieIterator that iterates the trie from the given
// atomic trie root, starting at the specified [cursor].
func (a *atomicTrie) Iterator(root common.Hash, cursor []byte) (AtomicTrieIterator, error) {
	t, err := trie.New(common.Hash{}, root, a.trieDB)
	if err != nil {
		return nil, err
	}

	iter := trie.NewIterator(t.NodeIterator(cursor))
	return NewAtomicTrieIterator(iter, a.codec), iter.Err
}

func (a *atomicTrie) TrieDB() *trie.Database {
	return a.trieDB
}

// Root returns hash if it exists at specified height
// if trie was not committed at provided height, it returns
// common.Hash{} instead
func (a *atomicTrie) Root(height uint64) (common.Hash, error) {
	return getRoot(a.metadataDB, height)
}

// getRoot is a helper function to return the committed atomic trie root hash at [height]
// from [metadataDB].
func getRoot(metadataDB database.Database, height uint64) (common.Hash, error) {
	if height == 0 {
		// if root is queried at height == 0, return the empty root hash
		// this may occur if peers ask for the most recent state summary
		// and number of accepted blocks is less than the commit interval.
		return types.EmptyRootHash, nil
	}

	heightBytes := make([]byte, wrappers.LongLen)
	binary.BigEndian.PutUint64(heightBytes, height)

	hash, err := metadataDB.Get(heightBytes)
	switch {
	case err == database.ErrNotFound:
		return common.Hash{}, nil
	case err != nil:
		return common.Hash{}, err
	}
	return common.BytesToHash(hash), nil
}

// ApplyToSharedMemory applies the atomic operations that have been indexed into the trie
// but not yet applied to shared memory for heights less than or equal to [lastAcceptedBlock].
// This executes operations in the range [cursorHeight+1, lastAcceptedBlock].
// The cursor is initially set by  MarkApplyToSharedMemoryCursor to signal to the atomic trie
// the range of operations that were added to the trie without being executed on shared memory.
func (a *atomicTrie) ApplyToSharedMemory(lastAcceptedBlock uint64) error {
	sharedMemoryCursor, err := a.metadataDB.Get(appliedSharedMemoryCursorKey)
	if err == database.ErrNotFound {
		return nil
	} else if err != nil {
		return err
	}

	log.Info("applying atomic operations to shared memory", "root", a.lastCommittedRoot, "lastAcceptedBlock", lastAcceptedBlock, "startHeight", binary.BigEndian.Uint64(sharedMemoryCursor[:wrappers.LongLen]))

	it, err := a.Iterator(a.lastCommittedRoot, sharedMemoryCursor)
	if err != nil {
		return err
	}
	lastUpdate := time.Now()
	putRequests, removeRequests := 0, 0
	totalPutRequests, totalRemoveRequests := 0, 0

	// value of sharedMemoryCursor is either a uint64 signifying the
	// height iteration should begin at or is a uint64+blockchainID
	// specifying the last atomic operation that was applied to shared memory.
	// To avoid applying the same operation twice, we call [it.Next()] in the
	// latter case.
	if len(sharedMemoryCursor) > wrappers.LongLen {
		it.Next()
	}

	batchOps := make(map[ids.ID]*atomic.Requests)
	for it.Next() {
		height := it.BlockNumber()
		atomicOps := it.AtomicOps()

		if height > lastAcceptedBlock {
			log.Warn("Found height above last accepted block while applying operations to shared memory", "height", height, "lastAcceptedBlock", lastAcceptedBlock)
			break
		}

		putRequests += len(atomicOps.PutRequests)
		removeRequests += len(atomicOps.RemoveRequests)
		totalPutRequests += len(atomicOps.PutRequests)
		totalRemoveRequests += len(atomicOps.RemoveRequests)
		if time.Since(lastUpdate) > 10*time.Second {
			log.Info("atomic trie iteration", "height", height, "puts", totalPutRequests, "removes", totalRemoveRequests)
			lastUpdate = time.Now()
		}
		mergeAtomicOpsToMap(batchOps, it.BlockchainID(), atomicOps)

		if putRequests+removeRequests > sharedMemoryApplyBatchSize {
			// Update the cursor to the key of the atomic operation being executed on shared memory.
			// If the node shuts down in the middle of this function call, ApplyToSharedMemory will
			// resume operation starting at the key immediately following [it.Key()].
			if err = a.metadataDB.Put(appliedSharedMemoryCursorKey, it.Key()); err != nil {
				return err
			}
			batch, err := a.db.CommitBatch()
			if err != nil {
				return err
			}
			// calling [sharedMemory.Apply] updates the last applied pointer atomically with the shared memory operation.
			if err = a.sharedMemory.Apply(batchOps, batch); err != nil {
				return err
			}
			putRequests, removeRequests = 0, 0
			batchOps = make(map[ids.ID]*atomic.Requests)
		}
	}
	if err := it.Error(); err != nil {
		return err
	}

	if err = a.metadataDB.Delete(appliedSharedMemoryCursorKey); err != nil {
		return err
	}
	batch, err := a.db.CommitBatch()
	if err != nil {
		return err
	}
	if err = a.sharedMemory.Apply(batchOps, batch); err != nil {
		return err
	}
	log.Info("finished applying atomic operations", "puts", totalPutRequests, "removes", totalRemoveRequests)
	return nil
}

// MarkApplyToSharedMemoryCursor marks the atomic trie as containing atomic ops that
// have not been executed on shared memory starting at [previousLastAcceptedHeight+1].
// This is used when state sync syncs the atomic trie, such that the atomic operations
// from [previousLastAcceptedHeight+1] to the [lastAcceptedHeight] set by state sync
// will not have been executed on shared memory.
func (a *atomicTrie) MarkApplyToSharedMemoryCursor(previousLastAcceptedHeight uint64) error {
	// Set the cursor to [previousLastAcceptedHeight+1] so that we begin the iteration at the
	// first item that has not been applied to shared memory.
	return database.PutUInt64(a.metadataDB, appliedSharedMemoryCursorKey, previousLastAcceptedHeight+1)
}

// Syncer creates and returns a new Syncer object that can be used to sync the
// state of the atomic trie from peers
func (a *atomicTrie) Syncer(client syncclient.LeafClient, targetRoot common.Hash, targetHeight uint64) (Syncer, error) {
	return newAtomicSyncer(client, a, targetRoot, targetHeight)
}

func (a *atomicTrie) LastAcceptedRoot() common.Hash {
	return a.lastAcceptedRoot
}

func (a *atomicTrie) InsertTrie(root common.Hash) error {
	a.trieDB.Reference(root, common.Hash{})

	// The use of [Cap] in [insertTrie] prevents exceeding the configured memory
	// limit (and OOM) in case there is a large backlog of processing (unaccepted) blocks.
	nodes, _ := a.trieDB.Size()
	if nodes <= a.memoryCap {
		return nil
	}
	if err := a.trieDB.Cap(a.memoryCap - ethdb.IdealBatchSize); err != nil {
		return fmt.Errorf("failed to cap atomic trie for root %s: %w", root, err)
	}

	return nil
}

func (a *atomicTrie) AcceptTrie(height uint64, root common.Hash) error {
	// Attempt to dereference roots at least [tipBufferSize] old
	//
	// Note: It is safe to dereference roots that have been committed to disk
	// (they are no-ops).
	a.tipBuffer.Insert(root)

	// Commit this root if we have reached the [commitInterval].
	modCommitInterval := height % a.commitInterval
	if modCommitInterval == 0 {
		if err := a.commit(height, root); err != nil {
			return err
		}
	}

	a.lastAcceptedRoot = root
	return nil
}

func (a *atomicTrie) RejectTrie(root common.Hash) error {
	a.trieDB.Dereference(root)
	return nil
}

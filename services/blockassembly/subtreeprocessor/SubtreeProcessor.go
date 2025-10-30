// Package subtreeprocessor provides functionality for processing transaction subtrees in Teranode.
//
// The subtreeprocessor implements an efficient system for organizing transactions into
// hierarchical subtrees that can be quickly assembled into blocks. This approach enables:
//   - Efficient transaction storage and retrieval
//   - Dynamic adjustment of subtree sizes based on transaction volume
//   - Parallelized processing of transaction groups
//   - Optimized block candidate generation
//   - Fast response to blockchain reorganizations
//
// The package works closely with the blockassembly service to maintain transaction
// integrity during chain reorganizations and provides mechanisms for transaction
// deduplication and conflict resolution.

package subtreeprocessor

import (
	"bufio"
	"container/ring"
	"context"
	"encoding/binary"
	"io"
	"log"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	safeconversion "github.com/bsv-blockchain/go-safe-conversion"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	txmap "github.com/bsv-blockchain/go-tx-map"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob"
	"github.com/bsv-blockchain/teranode/stores/blob/options"
	utxostore "github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/retry"
	"github.com/bsv-blockchain/teranode/util/tracing"
	"github.com/ordishs/gocore"
	"golang.org/x/sync/errgroup"
)

// Job represents a mining job with its associated data.
// A Job encapsulates all the information needed for a miner to attempt finding a valid
// proof-of-work solution, including the block template and associated transaction subtrees.
// Each job is uniquely identified to track mining attempts and solutions.

type Job struct {
	ID              *chainhash.Hash        // Unique identifier for the job
	Subtrees        []*subtreepkg.Subtree  // Collection of subtrees for the job
	MiningCandidate *model.MiningCandidate // Mining candidate information
}

// NewSubtreeRequest encapsulates a request to process a new subtree.
// This structure is used to communicate new subtrees between the block assembly service
// and the subtree processor, including an error channel for asynchronous result reporting.

type NewSubtreeRequest struct {
	Subtree          *subtreepkg.Subtree                                     // The subtree to process
	ParentTxMap      *txmap.SyncedMap[chainhash.Hash, subtreepkg.TxInpoints] // Map of parent transactions
	SkipNotification bool                                                    // Whether to skip notification to the network
	ErrChan          chan error                                              // Channel for error reporting
}

// moveBlockRequest represents a request to move a block in the chain.
type moveBlockRequest struct {
	block   *model.Block // The block to move
	errChan chan error   // Channel for error reporting
}

// reorgBlocksRequest represents a request to reorganize blocks in the chain.
type reorgBlocksRequest struct {
	// moveBackBlocks contains blocks that need to be removed from the current chain
	moveBackBlocks []*model.Block

	// moveForwardBlocks contains blocks that need to be added to form the new chain
	moveForwardBlocks []*model.Block

	// errChan receives any error encountered during reorganization
	errChan chan error
}

// resetBlocks encapsulates the data needed for a processor reset operation.
type resetBlocks struct {
	// blockHeader represents the new block header to reset to
	blockHeader *model.BlockHeader

	// moveBackBlocks contains blocks that need to be removed during reset
	moveBackBlocks []*model.Block

	// moveForwardBlocks contains blocks that need to be added during reset
	moveForwardBlocks []*model.Block

	// responseCh receives the reset operation response
	responseCh chan ResetResponse

	// isLegacySync indicates whether this is a legacy synchronization operation
	isLegacySync bool

	// postProcess is an optional function to execute after the reset
	postProcess func() error
}

// ResetResponse encapsulates the response from a reset operation.
type ResetResponse struct {
	// Err contains any error encountered during the reset operation
	Err error
}

// RemainderTransactionParams groups parameters for processRemainderTransactionsAndDequeue
// to comply with SonarQube's parameter count recommendations.
type RemainderTransactionParams struct {
	Block             *model.Block
	ChainedSubtrees   []*subtreepkg.Subtree
	CurrentSubtree    *subtreepkg.Subtree
	TransactionMap    txmap.TxMap
	LosingTxHashesMap txmap.TxMap
	CurrentTxMap      *txmap.SyncedMap[chainhash.Hash, subtreepkg.TxInpoints]
	SkipDequeue       bool
	SkipNotification  bool
}

// SubtreeProcessor manages the processing of transaction subtrees and block assembly.
// It serves as the core component for organizing transactions into efficient structures
// for block creation. The processor maintains the current blockchain state, manages
// transaction queues, and coordinates with the block assembler to create valid block templates.
//
// Key responsibilities include:
//   - Organizing transactions into hierarchical subtrees
//   - Processing newly arrived transactions
//   - Handling chain reorganizations
//   - Managing transaction conflicts
//   - Optimizing subtree sizes based on transaction volume
//   - Providing transaction sets for block candidates

type SubtreeProcessor struct {
	// settings contains the configuration parameters for the processor
	settings *settings.Settings

	// currentItemsPerFile specifies the maximum number of items per subtree file
	currentItemsPerFile int

	// blockStartTime tracks when the current block started
	blockStartTime time.Time

	// subtreesInBlock tracks number of subtrees created in current block
	subtreesInBlock int

	// blockIntervals tracks recent intervals per subtree in previous blocks
	blockIntervals []time.Duration

	// maxBlockSamples is the number of block samples to keep for averaging
	maxBlockSamples int

	// subtreeNodeCounts tracks the actual node count in recent subtrees using a ring buffer
	// With ~10 min blocks, 18 samples = ~3 hours of history for good stability
	subtreeNodeCounts     *ring.Ring
	subtreeNodeCountsSize int // Size of the ring buffer

	// txChan receives transaction batches for processing
	txChan chan *[]TxIDAndFee

	// getSubtreesChan handles requests to retrieve current subtrees
	getSubtreesChan chan chan []*subtreepkg.Subtree

	// getSubtreeHashesChan handles requests to retrieve current subtree hashes
	getSubtreeHashesChan chan chan []chainhash.Hash

	// getTransactionHashesChan handles requests to retrieve transaction hashes
	getTransactionHashesChan chan chan []chainhash.Hash

	// moveForwardBlockChan receives requests to process new blocks
	moveForwardBlockChan chan moveBlockRequest

	// reorgBlockChan handles blockchain reorganization requests
	reorgBlockChan chan reorgBlocksRequest

	// resetCh handles requests to reset the processor state
	resetCh chan *resetBlocks

	// removeTxCh receives transactions to be removed
	removeTxCh chan chainhash.Hash

	// lengthCh receives requests for the current length of the processor
	lengthCh chan chan int

	// checkSubtreeProcessorCh is used to check the subtree processor state
	checkSubtreeProcessorCh chan chan error

	// newSubtreeChan receives notifications about new subtrees
	newSubtreeChan chan NewSubtreeRequest

	// chainedSubtrees stores the ordered list of completed subtrees
	chainedSubtrees []*subtreepkg.Subtree

	// chainedSubtreeCount tracks the number of chained subtrees atomically
	chainedSubtreeCount atomic.Int32

	// currentSubtree represents the subtree currently being built
	currentSubtree *subtreepkg.Subtree

	// currentBlockHeader stores the current block header being processed
	currentBlockHeader *model.BlockHeader

	// txCount tracks the total number of transactions processed
	txCount atomic.Uint64

	// batcher manages transaction batching operations
	batcher *TxIDAndFeeBatch

	// queue manages the transaction processing queue
	queue *LockFreeQueue

	// currentTxMap tracks transactions currently held in the subtree processor
	currentTxMap *txmap.SyncedMap[chainhash.Hash, subtreepkg.TxInpoints]

	// removeMap tracks transactions marked for removal
	removeMap *txmap.SwissMap

	// blockchainClient provides access to blockchain data
	blockchainClient blockchain.ClientI

	// subtreeStore provides persistent storage for subtrees
	subtreeStore blob.Store

	// utxoStore manages UTXO set storage
	utxoStore utxostore.Store

	// logger handles logging operations
	logger ulogger.Logger

	// stats tracks operational statistics
	stats *gocore.Stat

	// currentRunningState tracks the processor's operational state
	currentRunningState atomic.Value
}

type State uint32

// create state strings for the processor
var (
	// StateStarting indicates the processor is starting up
	StateStarting State = 0

	// StateRunning indicates the processor is actively processing
	StateRunning State = 1

	// StateDequeue indicates the processor is dequeuing transactions
	StateDequeue State = 2

	// StateGetSubtrees indicates the processor is retrieving subtrees
	StateGetSubtrees State = 3

	// StateGetSubtreeHashes indicates the processor is retrieving subtree hashes
	StateGetSubtreeHashes State = 4

	// StateGetTransactionHashes indicates the processor is retrieving transaction hashes
	StateGetTransactionHashes State = 5

	// StateReorg indicates the processor is reorganizing blocks
	StateReorg State = 6

	// StateMoveForwardBlock indicates the processor is moving forward a block
	StateMoveForwardBlock State = 7

	// StateResetBlocks indicates the processor is resetting blocks
	StateResetBlocks State = 8

	// StateRemoveTx indicates the processor is removing transactions
	StateRemoveTx State = 9

	// StateCheckSubtreeProcessor indicates the processor is checking its state
	StateCheckSubtreeProcessor State = 11
)

var StateStrings = map[State]string{
	StateStarting:              "starting",
	StateRunning:               "running",
	StateDequeue:               "dequeue",
	StateGetSubtrees:           "getSubtrees",
	StateGetSubtreeHashes:      "getSubtreeHashes",
	StateGetTransactionHashes:  "getTransactionHashes",
	StateReorg:                 "reorg",
	StateMoveForwardBlock:      "moveForwardBlock",
	StateResetBlocks:           "resetBlocks",
	StateRemoveTx:              "removeTx",
	StateCheckSubtreeProcessor: "checkSubtreeProcessor",
}

var (
	// ExpectedNumberOfSubtrees defines the expected number of subtrees in a block.
	// This is calculated based on a subtree being created approximately every second,
	// and is used for initial capacity allocation to optimize memory usage.
	ExpectedNumberOfSubtrees = 1024
)

// NewSubtreeProcessor creates and initializes a new SubtreeProcessor instance.
//
// Parameters:
//   - ctx: Context for cancellation
//   - logger: Logger instance for recording operations
//   - tSettings: Teranode settings configuration
//   - subtreeStore: Storage for subtrees
//   - utxoStore: Storage for UTXOs
//   - newSubtreeChan: Channel for new subtree notifications
//   - options: Optional configuration functions
//
// Returns:
//   - *SubtreeProcessor: Initialized subtree processor
//   - error: Any error encountered during initialization
func NewSubtreeProcessor(ctx context.Context, logger ulogger.Logger, tSettings *settings.Settings, subtreeStore blob.Store,
	blockchainClient blockchain.ClientI, utxoStore utxostore.Store, newSubtreeChan chan NewSubtreeRequest, options ...Options) (*SubtreeProcessor, error) {
	initPrometheusMetrics()

	initialItemsPerFile := tSettings.BlockAssembly.InitialMerkleItemsPerSubtree

	firstSubtree, err := subtreepkg.NewTreeByLeafCount(initialItemsPerFile)
	if err != nil {
		return nil, errors.NewInvalidArgumentError("error creating first subtree", err)
	}

	// We add a placeholder for the coinbase tx because we know this is the first subtree in the chain
	if err = firstSubtree.AddCoinbaseNode(); err != nil {
		return nil, errors.NewInvalidArgumentError("error adding coinbase placeholder to first subtree", err)
	}

	queue := NewLockFreeQueue()

	// Calculate subtree sample size based on expected block time
	// With ~10 min blocks, 18 samples = ~3 hours of history
	// This provides good stability without excessive memory usage
	// - Long enough to smooth out temporary fluctuations
	// - Short enough to adapt to genuine load changes (e.g., day/night cycles)
	// - Small memory footprint (18 * sizeof(int) = 144 bytes)
	const subtreeSampleSize = 18

	stp := &SubtreeProcessor{
		settings:                 tSettings,
		currentItemsPerFile:      initialItemsPerFile,
		blockStartTime:           time.Time{},
		subtreesInBlock:          0,
		blockIntervals:           make([]time.Duration, 0, 10),
		maxBlockSamples:          10,
		subtreeNodeCounts:        ring.New(subtreeSampleSize),
		subtreeNodeCountsSize:    subtreeSampleSize,
		txChan:                   make(chan *[]TxIDAndFee, tSettings.SubtreeValidation.TxChanBufferSize),
		getSubtreesChan:          make(chan chan []*subtreepkg.Subtree),
		getSubtreeHashesChan:     make(chan chan []chainhash.Hash),
		getTransactionHashesChan: make(chan chan []chainhash.Hash),
		moveForwardBlockChan:     make(chan moveBlockRequest),
		reorgBlockChan:           make(chan reorgBlocksRequest),
		resetCh:                  make(chan *resetBlocks),
		removeTxCh:               make(chan chainhash.Hash),
		lengthCh:                 make(chan chan int),
		checkSubtreeProcessorCh:  make(chan chan error),
		newSubtreeChan:           newSubtreeChan,
		chainedSubtrees:          make([]*subtreepkg.Subtree, 0, ExpectedNumberOfSubtrees),
		chainedSubtreeCount:      atomic.Int32{},
		currentSubtree:           firstSubtree,
		batcher:                  NewTxIDAndFeeBatch(tSettings.BlockAssembly.SubtreeProcessorBatcherSize),
		queue:                    queue,
		currentTxMap:             txmap.NewSyncedMap[chainhash.Hash, subtreepkg.TxInpoints](),
		removeMap:                txmap.NewSwissMap(0),
		blockchainClient:         blockchainClient,
		subtreeStore:             subtreeStore,
		utxoStore:                utxoStore,
		logger:                   logger,
		stats:                    gocore.NewStat("subtreeProcessor").NewStat("Add", false),
		currentRunningState:      atomic.Value{},
	}
	stp.setCurrentRunningState(StateStarting)

	// need to make sure first coinbase tx is counted when we start
	stp.setTxCountFromSubtrees()

	for _, opts := range options {
		opts(stp)
	}

	stp.setCurrentRunningState(StateRunning)

	go func() {
		var (
			err error
		)

		for {
			select {
			case getSubtreesChan := <-stp.getSubtreesChan:
				stp.setCurrentRunningState(StateGetSubtrees)

				logger.Debugf("[SubtreeProcessor] get current subtrees")

				chainedCount := stp.chainedSubtreeCount.Load()
				completeSubtrees := make([]*subtreepkg.Subtree, 0, chainedCount)
				completeSubtrees = append(completeSubtrees, stp.chainedSubtrees...)

				// incomplete subtrees ?
				if chainedCount == 0 && stp.currentSubtree.Length() > 1 {
					incompleteSubtree, err := subtreepkg.NewTreeByLeafCount(stp.currentItemsPerFile)
					if err != nil {
						logger.Errorf("[SubtreeProcessor] error creating incomplete subtree: %s", err.Error())
						getSubtreesChan <- nil

						stp.setCurrentRunningState(StateRunning)

						continue
					}

					_ = incompleteSubtree.AddCoinbaseNode()
					for _, node := range stp.currentSubtree.Nodes[1:] {
						_ = incompleteSubtree.AddSubtreeNode(node)
					}

					incompleteSubtree.Fees = stp.currentSubtree.Fees
					completeSubtrees = append(completeSubtrees, incompleteSubtree)

					// store (and announce) new incomplete subtree to other miners
					send := NewSubtreeRequest{
						Subtree:     incompleteSubtree,
						ParentTxMap: stp.currentTxMap,
						ErrChan:     make(chan error),
					}
					newSubtreeChan <- send

					// Wait for a response to ensure proper synchronization.
					// This prevents race conditions when mining initial blocks and running coinbase splitter together.
					// Without this wait, getMiningCandidate creates subtrees in the background while
					// submitMiningSolution tries to setDAH on subtrees that might not yet exist.
					<-send.ErrChan
				}

				getSubtreesChan <- completeSubtrees

				logger.Debugf("[SubtreeProcessor] get current subtrees DONE")

				stp.setCurrentRunningState(StateRunning)

			case getSubtreeHashesChan := <-stp.getSubtreeHashesChan:
				stp.setCurrentRunningState(StateGetSubtreeHashes)
				logger.Debugf("[SubtreeProcessor] get current subtree hashes")
				subtreeHashes := make([]chainhash.Hash, 0, stp.chainedSubtreeCount.Load()+1)

				for _, subtree := range stp.chainedSubtrees {
					subtreeHashes = append(subtreeHashes, *subtree.RootHash())
				}

				if stp.currentSubtree.Length() > 0 {
					subtreeHashes = append(subtreeHashes, *stp.currentSubtree.RootHash())
				}

				getSubtreeHashesChan <- subtreeHashes
				logger.Debugf("[SubtreeProcessor] get current subtree hashes DONE")
				stp.setCurrentRunningState(StateRunning)

			case getTransactionHashesChan := <-stp.getTransactionHashesChan:
				stp.setCurrentRunningState(StateGetTransactionHashes)
				logger.Debugf("[SubtreeProcessor] get current transaction hashes")
				transactionHashes := make([]chainhash.Hash, 0, stp.currentTxMap.Length()+1)
				for _, subtree := range stp.chainedSubtrees {
					for _, node := range subtree.Nodes {
						transactionHashes = append(transactionHashes, node.Hash)
					}
				}
				if stp.currentSubtree.Length() > 0 {
					for _, node := range stp.currentSubtree.Nodes {
						transactionHashes = append(transactionHashes, node.Hash)
					}
				}

				getTransactionHashesChan <- transactionHashes

				logger.Debugf("[SubtreeProcessor] get current transaction hashes DONE")
				stp.setCurrentRunningState(StateRunning)

			case reorgReq := <-stp.reorgBlockChan:
				stp.setCurrentRunningState(StateReorg)
				logger.Infof("[SubtreeProcessor] reorgReq subtree processor: %d, %d", len(reorgReq.moveBackBlocks), len(reorgReq.moveForwardBlocks))

				reorgReq.errChan <- stp.reorgBlocks(ctx, reorgReq.moveBackBlocks, reorgReq.moveForwardBlocks)

				logger.Infof("[SubtreeProcessor] reorgReq subtree processor DONE: %d, %d", len(reorgReq.moveBackBlocks), len(reorgReq.moveForwardBlocks))
				stp.setCurrentRunningState(StateRunning)

			case moveForwardReq := <-stp.moveForwardBlockChan:
				stp.setCurrentRunningState(StateMoveForwardBlock)

				logger.Infof("[SubtreeProcessor][%s] moveForwardBlock subtree processor", moveForwardReq.block.String())

				// create empty map for processed conflicting hashes
				processedConflictingHashesMap := make(map[chainhash.Hash]bool)

				// store current state before attempting to move forward the block
				originalChainedSubtrees := stp.chainedSubtrees
				originalCurrentSubtree := stp.currentSubtree
				originalCurrentTxMap := stp.currentTxMap
				currentBlockHeader := stp.currentBlockHeader

				if _, err = stp.moveForwardBlock(ctx, moveForwardReq.block, false, processedConflictingHashesMap, false, true); err != nil {
					// rollback to previous state
					stp.chainedSubtrees = originalChainedSubtrees
					stp.currentSubtree = originalCurrentSubtree
					stp.currentTxMap = originalCurrentTxMap
					stp.currentBlockHeader = currentBlockHeader

					// recalculate tx count from subtrees
					stp.setTxCountFromSubtrees()
				} else {
					// Finalize block processing
					// this will also set the current block header
					stp.finalizeBlockProcessing(ctx, moveForwardReq.block)
				}

				moveForwardReq.errChan <- err

				logger.Infof("[SubtreeProcessor][%s] moveForwardBlock subtree processor DONE", moveForwardReq.block.String())
				stp.setCurrentRunningState(StateRunning)

			case resetBlocksMsg := <-stp.resetCh:
				stp.setCurrentRunningState(StateResetBlocks)

				err = stp.reset(resetBlocksMsg.blockHeader, resetBlocksMsg.moveBackBlocks, resetBlocksMsg.moveForwardBlocks,
					resetBlocksMsg.isLegacySync, resetBlocksMsg.postProcess)

				if resetBlocksMsg.responseCh != nil {
					resetBlocksMsg.responseCh <- ResetResponse{Err: err}
				}

				stp.setCurrentRunningState(StateRunning)

			case removeTxHash := <-stp.removeTxCh:
				// remove the given transaction from the subtrees
				stp.setCurrentRunningState(StateRemoveTx)

				if err = stp.removeTxFromSubtrees(ctx, removeTxHash); err != nil {
					stp.logger.Errorf("[SubtreeProcessor] error removing tx from subtrees: %s", err.Error())
				}

				stp.setCurrentRunningState(StateRunning)

			case lengthCh := <-stp.lengthCh:
				// return the length of the current subtree
				lengthCh <- stp.currentSubtree.Length()

			case errCh := <-stp.checkSubtreeProcessorCh:
				stp.setCurrentRunningState(StateCheckSubtreeProcessor)

				stp.checkSubtreeProcessor(errCh)

				stp.setCurrentRunningState(StateRunning)

			default:
				stp.setCurrentRunningState(StateDequeue)

				nrProcessed := 0
				mapLength := stp.removeMap.Length()
				// set the validFromMillis to the current time minus the double spend window - so in the past
				validFromMillis := time.Now().Add(-1 * stp.settings.BlockAssembly.DoubleSpendWindow).UnixMilli()

				for {
					node, txInpoints, _, found := stp.queue.dequeue(validFromMillis)
					if !found {
						time.Sleep(1 * time.Millisecond)
						break
					}

					// check if the tx needs to be removed
					if mapLength > 0 && stp.removeMap.Exists(node.Hash) {
						// remove from the map
						if err = stp.removeMap.Delete(node.Hash); err != nil {
							stp.logger.Errorf("[SubtreeProcessor] error removing tx from remove map: %s", err.Error())
						}

						continue
					}

					if node.Hash.Equal(*subtreepkg.CoinbasePlaceholderHash) {
						stp.logger.Errorf("[SubtreeProcessor] error adding node: skipping request to add coinbase tx placeholder")
						continue
					}

					// check if the tx is already in the currentTxMap
					if _, ok := stp.currentTxMap.Get(node.Hash); ok {
						stp.logger.Warnf("[SubtreeProcessor] error adding node: tx %s already in currentTxMap", node.Hash.String())
						continue
					}

					// check txInpoints
					// for _, parent := range txReq.txInpoints {
					// 	if _, ok := stp.currentTxMap.Get(parent); !ok {
					// 		stp.logger.Errorf("[SubtreeProcessor] error adding node: parent %s not found in currentTxMap", parent.String())
					// 		continue
					// 	}
					// }

					if err = stp.addNode(node, &txInpoints, false); err != nil {
						stp.logger.Errorf("[SubtreeProcessor] error adding node: %s", err.Error())
					} else {
						stp.txCount.Add(1)
					}

					nrProcessed++
					if nrProcessed > stp.settings.BlockAssembly.SubtreeProcessorBatcherSize {
						break
					}
				}

				stp.setCurrentRunningState(StateRunning)
			}
		}
	}()

	return stp, nil
}

// setCurrentRunningState updates the current operational state of the processor.
//
// Parameters:
//   - state: New state to set
func (stp *SubtreeProcessor) setCurrentRunningState(state State) {
	stp.currentRunningState.Store(state)
	prometheusSubtreeProcessorCurrentState.Set(float64(state))
}

// GetCurrentRunningState returns the current operational state of the processor.
//
// Returns:
//   - string: Current state description
func (stp *SubtreeProcessor) GetCurrentRunningState() State {
	return stp.currentRunningState.Load().(State)
}

// GetCurrentLength returns the length of the current subtree
//
// Returns:
//   - int: Length of the current subtree
func (stp *SubtreeProcessor) GetCurrentLength() int {
	lengthCh := make(chan int)

	stp.lengthCh <- lengthCh

	return <-lengthCh
}

// Reset resets the processor to a clean state,  removing all subtrees and transactions
// This will be called from the block assembler in a channel select, making sure no other operations are happening
// Note - the queue will still be ingesting transactions
// Parameters:
//   - blockHeader: New block header to reset to
//   - moveBackBlocks: Blocks to move down in the chain
//   - moveForwardBlocks: Blocks to move up in the chain
//   - isLegacySync: Whether this is a legacy sync operation
//
// Returns:
//   - ResetResponse: Response containing any errors encountered
func (stp *SubtreeProcessor) Reset(blockHeader *model.BlockHeader, moveBackBlocks []*model.Block, moveForwardBlocks []*model.Block, isLegacySync bool, postProcess func() error) ResetResponse {
	responseCh := make(chan ResetResponse)
	stp.resetCh <- &resetBlocks{
		blockHeader:       blockHeader,
		moveBackBlocks:    moveBackBlocks,
		moveForwardBlocks: moveForwardBlocks,
		responseCh:        responseCh,
		isLegacySync:      isLegacySync,
		postProcess:       postProcess,
	}

	return <-responseCh
}

func (stp *SubtreeProcessor) reset(blockHeader *model.BlockHeader, moveBackBlocks []*model.Block, moveForwardBlocks []*model.Block,
	isLegacySync bool, postProcess func() error) error {
	_, _, deferFn := tracing.Tracer("subtreeprocessor").Start(context.Background(), "reset",
		tracing.WithParentStat(stp.stats),
		tracing.WithHistogram(prometheusSubtreeProcessorReset),
		tracing.WithLogMessage(stp.logger, "[SubtreeProcessor][reset] Resetting subtree processor with %d moveBackBlocks and %d moveForwardBlocks", len(moveBackBlocks), len(moveForwardBlocks)),
	)

	defer deferFn()

	stp.chainedSubtrees = make([]*subtreepkg.Subtree, 0, ExpectedNumberOfSubtrees)
	stp.chainedSubtreeCount.Store(0)

	stp.currentSubtree, _ = subtreepkg.NewTreeByLeafCount(stp.currentItemsPerFile)
	if err := stp.currentSubtree.AddCoinbaseNode(); err != nil {
		return errors.NewProcessingError("[SubtreeProcessor][Reset] error adding coinbase placeholder to new current subtree", err)
	}

	// clear current tx map
	stp.currentTxMap.Clear()

	// reset tx count
	stp.setTxCountFromSubtrees()

	ctx := context.Background()

	// the processed conflicting hashes map keeps track of all the conflicting hashes we've already processed
	// this is to avoid processing the same conflicting hash multiple times if it appears in multiple blocks
	// the map is only used during the reset process and is not stored in the SubtreeProcessor struct
	processedConflictingHashesMap := make(map[chainhash.Hash]bool)

	for _, block := range moveBackBlocks {
		// delete / unspend all transactions spending the coinbase tx
		if err := stp.removeCoinbaseUtxos(ctx, block); err != nil {
			// no need to error out if the key doesn't exist anyway
			if !errors.Is(err, errors.ErrTxNotFound) {
				return errors.NewProcessingError("[SubtreeProcessor][Reset] error deleting utxos for tx %s", block.CoinbaseTx.String(), err)
			}
		}

		conflictingHashes, err := stp.getConflictingNodes(ctx, block)
		if err != nil {
			return errors.NewProcessingError("[SubtreeProcessor][Reset][%s] error getting conflicting nodes", block.String(), err)
		}

		if len(conflictingHashes) > 0 {
			for _, hash := range conflictingHashes {
				processedConflictingHashesMap[hash] = true
			}
		}
	}

	// optimized version for legacy sync
	if isLegacySync {
		coinbaseTxsAdded := sync.Map{}

		g, gCtx := errgroup.WithContext(context.Background())

		for _, block := range moveForwardBlocks {
			g.Go(func() error {
				if err := stp.processCoinbaseUtxos(gCtx, block); err != nil {
					return err
				}

				coinbaseTxsAdded.Store(block.Hash().String(), block)

				return nil
			})
		}

		if err := g.Wait(); err != nil {
			coinbaseTxsAdded.Range(func(key, value interface{}) bool {
				// remove all the coinbase transactions we added
				block := value.(*model.Block)
				if delErr := stp.removeCoinbaseUtxos(context.Background(), block); delErr != nil {
					stp.logger.Errorf("[SubtreeProcessor][Reset] error deleting utxos for coinbase tx %s: %v", block.CoinbaseTx.String(), delErr)
				}

				return true
			})

			return errors.NewProcessingError("[SubtreeProcessor][Reset] error processing coinbase utxos", err)
		}

		stp.currentBlockHeader = blockHeader
	} else {
		for _, block := range moveForwardBlocks {
			// A block has potentially some conflicting transactions that need to be processed when we move forward the block
			conflictingNodes, err := stp.getConflictingNodes(ctx, block)
			if err != nil {
				return errors.NewProcessingError("[moveForwardBlock][%s] error getting conflicting nodes", block.String(), err)
			}

			if len(conflictingNodes) > 0 {
				losingTxHashesMap, err := utxostore.ProcessConflicting(ctx, stp.utxoStore, conflictingNodes, processedConflictingHashesMap)
				if err != nil {
					return errors.NewProcessingError("[moveForwardBlock][%s] error processing conflicting transactions in Reset()", block.String(), err)
				}

				if losingTxHashesMap.Length() > 0 {
					// mark all the losing txs in the subtrees in the blocks they were mined into as conflicting
					if err = stp.markConflictingTxsInSubtrees(ctx, losingTxHashesMap); err != nil {
						return errors.NewProcessingError("[moveForwardBlock][%s] error marking conflicting transactions", block.String(), err)
					}
				}
			}

			if err = stp.processCoinbaseUtxos(context.Background(), block); err != nil {
				return errors.NewProcessingError("[SubtreeProcessor][Reset] error processing coinbase utxos", err)
			}
		}
	}

	// persist the current state
	if len(moveForwardBlocks) > 0 {
		stp.finalizeBlockProcessing(ctx, moveForwardBlocks[len(moveForwardBlocks)-1])
	} else if len(moveBackBlocks) > 0 {
		// we only moved back, finalize with the parent of the last block we moved back
		block, err := stp.blockchainClient.GetBlock(ctx, moveBackBlocks[len(moveBackBlocks)-1].Header.HashPrevBlock)
		if err != nil {
			return errors.NewProcessingError("[SubtreeProcessor][Reset] error getting parent block of last block we moved back", err)
		}
		stp.finalizeBlockProcessing(ctx, block)
	}

	if postProcess != nil {
		stp.logger.Infof("[SubtreeProcessor][Reset] PostProcessing block headers function called")
		if err := postProcess(); err != nil {
			return errors.NewProcessingError("[SubtreeProcessor][Reset] error in postProcess function", err)
		}
	}

	// dequeue all transactions
	stp.logger.Warnf("[SubtreeProcessor][Reset] Dequeueing all transactions")

	validUntilMillis := time.Now().UnixMilli()

	for {
		_, _, time64, found := stp.queue.dequeue(0)
		if !found || time64 > validUntilMillis {
			// we are done
			break
		}
	}

	return nil
}

func (stp *SubtreeProcessor) getConflictingNodes(ctx context.Context, block *model.Block) ([]chainhash.Hash, error) {
	_, _, deferFn := tracing.Tracer("subtreeprocessor").Start(context.Background(), "getConflictingNodes",
		tracing.WithParentStat(stp.stats),
		tracing.WithDebugLogMessage(stp.logger, "[SubtreeProcessor][getConflictingNodes][%s] getting conflicting nodes", block.String()),
	)
	defer deferFn()

	conflictingNodes := make([]chainhash.Hash, 0, 1024)
	conflictingNodesMu := sync.Mutex{}

	g, gCtx := errgroup.WithContext(ctx)
	util.SafeSetLimit(g, stp.settings.BlockAssembly.MoveBackBlockConcurrency)

	// get the conflicting transactions from the block subtrees
	for _, subtreeHash := range block.Subtrees {
		subtreeHash := subtreeHash

		g.Go(func() error {
			// get the conflicting transactions from the subtree
			subtreeReader, err := stp.subtreeStore.GetIoReader(gCtx, subtreeHash[:], fileformat.FileTypeSubtree)
			if err != nil {
				subtreeReader, err = stp.subtreeStore.GetIoReader(gCtx, subtreeHash[:], fileformat.FileTypeSubtreeToCheck)
				if err != nil {
					return errors.NewProcessingError("[moveForwardBlock][%s] error getting subtree %s from store", block.String(), subtreeHash.String(), err)
				}
			}

			subtreeConflictingNodes, err := subtreepkg.DeserializeSubtreeConflictingFromReader(subtreeReader)
			if err != nil {
				return errors.NewProcessingError("[moveForwardBlock][%s] error deserializing subtree conflicting nodes", block.String(), err)
			}

			if len(subtreeConflictingNodes) > 0 {
				conflictingNodesMu.Lock()
				conflictingNodes = append(conflictingNodes, subtreeConflictingNodes...)
				conflictingNodesMu.Unlock()
			}

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, errors.NewProcessingError("[moveForwardBlock][%s] error getting conflicting nodes", block.String(), err)
	}

	return conflictingNodes, nil
}

// GetCurrentBlockHeader returns the current block header being processed.
//
// Returns:
//   - *model.BlockHeader: Current block header
func (stp *SubtreeProcessor) GetCurrentBlockHeader() *model.BlockHeader {
	return stp.currentBlockHeader
}

// SetCurrentBlockHeader sets the current block header being processed.
//
// Parameters:
//   - blockHeader: New block header to set
func (stp *SubtreeProcessor) SetCurrentBlockHeader(blockHeader *model.BlockHeader) {
	stp.currentBlockHeader = blockHeader
}

// GetCurrentSubtree returns the subtree currently being built.
//
// Returns:
//   - *util.Subtree: Current subtree
func (stp *SubtreeProcessor) GetCurrentSubtree() *subtreepkg.Subtree {
	return stp.currentSubtree
}

// GetCurrentTxMap returns the map of transactions currently held in the subtree processor.
//
// Returns:
//   - *util.SyncedMap[chainhash.Hash, []chainhash.Hash]: Map of transactions
func (stp *SubtreeProcessor) GetCurrentTxMap() *txmap.SyncedMap[chainhash.Hash, subtreepkg.TxInpoints] {
	return stp.currentTxMap
}

// GetRemoveMap returns the map of transactions marked for removal.
// This map is used to track transactions that should be excluded from processing.
//
// Returns:
//   - *txmap.SwissMap: Map of transactions to be removed
func (stp *SubtreeProcessor) GetRemoveMap() *txmap.SwissMap {
	return stp.removeMap
}

// GetChainedSubtrees returns all completed subtrees in the chain.
//
// Returns:
//   - []*util.Subtree: Array of chained subtrees
func (stp *SubtreeProcessor) GetChainedSubtrees() []*subtreepkg.Subtree {
	return stp.chainedSubtrees
}

func (stp *SubtreeProcessor) GetSubtreeHashes() []chainhash.Hash {
	response := make(chan []chainhash.Hash)

	stp.getSubtreeHashesChan <- response

	return <-response
}

func (stp *SubtreeProcessor) GetTransactionHashes() []chainhash.Hash {
	response := make(chan []chainhash.Hash)

	stp.getTransactionHashesChan <- response

	return <-response
}

// GetUtxoStore returns the UTXO store instance.
//
// Returns:
//   - utxostore.Store: UTXO store instance
func (stp *SubtreeProcessor) GetUtxoStore() utxostore.Store {
	return stp.utxoStore
}

// SetCurrentItemsPerFile updates the maximum items per subtree file.
//
// Parameters:
//   - v: New maximum items value
func (stp *SubtreeProcessor) SetCurrentItemsPerFile(v int) {
	stp.currentItemsPerFile = v
}

// TxCount returns the total number of transactions processed.
//
// Returns:
//   - uint64: Total transaction count
func (stp *SubtreeProcessor) TxCount() uint64 {
	return stp.txCount.Load()
}

// QueueLength returns the current length of the transaction queue.
//
// Returns:
//   - int64: Current queue length
func (stp *SubtreeProcessor) QueueLength() int64 {
	return stp.queue.length()
}

// SubtreeCount returns the total number of subtrees.
// This method is primarily used for prometheus statistics.
//
// Returns:
//   - int: Total number of subtrees
func (stp *SubtreeProcessor) SubtreeCount() int {
	// not using len(chainSubtrees) to avoid Race condition
	// should we be using locks around all chainSubtree operations instead?
	// the subtree count isn't mission-critical - it's just for statistics
	return int(stp.chainedSubtreeCount.Load()) + 01
}

// adjustSubtreeSize calculates and sets a new subtree size based on recent block statistics
// to maintain approximately one subtree per second. The size will always be a power of 2
// and not smaller than 1024.
func (stp *SubtreeProcessor) adjustSubtreeSize() {
	if !stp.settings.BlockAssembly.UseDynamicSubtreeSize {
		return
	}

	currentSize := stp.currentItemsPerFile

	// First check if we have actual subtree utilization data
	// Count non-nil values in the ring
	count := 0
	totalNodes := 0
	stp.subtreeNodeCounts.Do(func(v interface{}) {
		if v != nil {
			count++
			totalNodes += v.(int)
		}
	})

	if count > 0 {
		avgNodesPerSubtree := float64(totalNodes) / float64(count)

		// Calculate utilization percentage
		utilization := avgNodesPerSubtree / float64(currentSize)

		stp.logger.Debugf("[adjustSubtreeSize] avgNodesPerSubtree=%.1f, currentSize=%d, utilization=%.2f%%\n",
			avgNodesPerSubtree, currentSize, utilization*100)

		// If subtrees are less than 10% full, we should decrease size
		// If subtrees are more than 80% full, we should increase size
		if utilization < 0.1 {
			// Subtrees are mostly empty, decrease size
			newSize := int(float64(currentSize) * 0.5)
			stp.logger.Debugf("[adjustSubtreeSize] Low utilization (%.2f%%), decreasing size from %d to %d\n",
				utilization*100, currentSize, newSize)

			// Round to power of 2
			newSize = int(math.Pow(2, math.Ceil(math.Log2(float64(newSize)))))

			// Apply minimum size constraint
			minSubtreeSize := stp.settings.BlockAssembly.MinimumMerkleItemsPerSubtree
			if newSize < minSubtreeSize {
				newSize = minSubtreeSize
			}

			if newSize != currentSize {
				stp.logger.Debugf("[adjustSubtreeSize] setting new size from %d to %d (low utilization)\n", currentSize, newSize)
				stp.currentItemsPerFile = newSize
				prometheusSubtreeProcessorDynamicSubtreeSize.Set(float64(newSize))
			}

			// Reset counters for next adjustment
			// Clear the ring buffer
			stp.subtreeNodeCounts = ring.New(stp.subtreeNodeCountsSize)
			stp.blockIntervals = make([]time.Duration, 0)
			return
		} else if utilization > 0.8 {
			// Subtrees are nearly full, might need to increase size
			// But only if we're also creating them too fast AND we have significant volume

			// Don't increase size if average nodes per subtree is small (< 50)
			// This prevents size creep with low transaction volumes
			if avgNodesPerSubtree < 50 {
				stp.logger.Debugf("[adjustSubtreeSize] High utilization (%.2f%%) but low volume (%.1f nodes/subtree), keeping size at %d\n",
					utilization*100, avgNodesPerSubtree, currentSize)
				// Reset counters but don't change size
				stp.blockIntervals = make([]time.Duration, 0)
				return
			}

			stp.logger.Debugf("[adjustSubtreeSize] High utilization (%.2f%%), checking timing...\n", utilization*100)
		} else {
			// Utilization is reasonable (10-80%), keep current size
			stp.logger.Debugf("[adjustSubtreeSize] Utilization is reasonable (%.2f%%), keeping size at %d\n",
				utilization*100, currentSize)
			// Reset counters but don't change size
			stp.blockIntervals = make([]time.Duration, 0)
			return
		}
	}

	// Calculate average interval between subtrees in this block
	if len(stp.blockIntervals) == 0 {
		return
	}

	// Filter out any intervals that are too small (likely spurious) or negative
	validIntervals := make([]time.Duration, 0)

	for _, interval := range stp.blockIntervals {
		if interval > time.Millisecond && interval < time.Hour {
			validIntervals = append(validIntervals, interval)
		}
	}

	if len(validIntervals) == 0 {
		return
	}

	// Calculate average interval
	var sum time.Duration
	for _, interval := range validIntervals {
		sum += interval
	}

	avgInterval := sum / time.Duration(len(validIntervals))

	stp.logger.Debugf("[adjustSubtreeSize] avgInterval=%v, validIntervals=%v\n", avgInterval, validIntervals)

	// Calculate ratio of target to actual interval
	// If we're creating subtrees faster than target, ratio > 1 and size should increase
	targetInterval := time.Second
	ratio := float64(targetInterval) / float64(avgInterval)

	stp.logger.Debugf("[adjustSubtreeSize] ratio=%v, currentSize=%d, newSize before rounding=%d\n",
		ratio, currentSize, int(float64(currentSize)*ratio))

	// Calculate new size based on ratio
	newSize := int(float64(currentSize) * ratio)

	// Round to next power of 2
	newSize = int(math.Pow(2, math.Ceil(math.Log2(float64(newSize)))))
	stp.logger.Debugf("[adjustSubtreeSize] newSize after rounding=%d\n", newSize)

	// Cap the increase to 2x per block to avoid wild swings
	if newSize > currentSize*2 {
		newSize = currentSize * 2
		stp.logger.Debugf("[adjustSubtreeSize] newSize capped at 2x=%d\n", newSize)
	}

	// never go over maximum size
	maxSubtreeSize := stp.settings.BlockAssembly.MaximumMerkleItemsPerSubtree
	if newSize > maxSubtreeSize {
		newSize = maxSubtreeSize
		stp.logger.Debugf("[adjustSubtreeSize] newSize capped at maxSubtreeSize=%d\n", newSize)
	}

	// Never go below minimum size
	minSubtreeSize := stp.settings.BlockAssembly.MinimumMerkleItemsPerSubtree
	if newSize < minSubtreeSize {
		newSize = minSubtreeSize
	}

	// Final check: if we have utilization data, don't increase size beyond what's needed
	// This prevents size increases when transaction volume is low
	maxNodes := 0
	hasData := false
	stp.subtreeNodeCounts.Do(func(v interface{}) {
		if v != nil {
			hasData = true
			if nodeCount := v.(int); nodeCount > maxNodes {
				maxNodes = nodeCount
			}
		}
	})

	if hasData && newSize > currentSize {
		// Only increase if we've actually seen subtrees that would benefit
		// Add some buffer (2x max seen) but round to power of 2
		neededSize := int(math.Pow(2, math.Ceil(math.Log2(float64(maxNodes*2)))))
		if neededSize < newSize {
			stp.logger.Debugf("[adjustSubtreeSize] Limiting size increase based on actual usage: max nodes seen=%d, limiting to %d instead of %d\n",
				maxNodes, neededSize, newSize)
			newSize = neededSize
		}
	}

	if newSize != currentSize {
		stp.logger.Debugf("[adjustSubtreeSize] setting new size from %d to %d\n", currentSize, newSize)
		stp.currentItemsPerFile = newSize
	}

	prometheusSubtreeProcessorDynamicSubtreeSize.Set(float64(newSize))

	// Reset intervals for next block
	stp.blockIntervals = make([]time.Duration, 0)
}

// InitCurrentBlockHeader sets the initial block header.
// This function is not thread-safe.
func (stp *SubtreeProcessor) InitCurrentBlockHeader(blockHeader *model.BlockHeader) {
	stp.logger.Infof("[SubtreeProcessor] initializing current block header to %s", blockHeader.String())

	stp.currentBlockHeader = blockHeader
	stp.blockStartTime = time.Now()
	stp.subtreesInBlock = 0
}

// addNode adds a new transaction node to the current subtree.
//
// Parameters:
//   - node: Transaction node to add
//   - skipNotification: Whether to skip notification of new subtrees
//
// Returns:
//   - error: Any error encountered during addition
func (stp *SubtreeProcessor) addNode(node subtreepkg.SubtreeNode, parents *subtreepkg.TxInpoints, skipNotification bool) (err error) {
	// parent can only be set to nil, when they are already in the map
	if parents == nil {
		if p, ok := stp.currentTxMap.Get(node.Hash); !ok {
			return errors.NewProcessingError("error adding node to subtree: txInpoints not found in currentTxMap for %s", node.Hash.String())
		} else {
			parents = &p // nolint:ineffassign
		}
	} else {
		// SetIfNotExists returns (value, wasSet) where wasSet is true if the key was newly inserted
		if _, wasSet := stp.currentTxMap.SetIfNotExists(node.Hash, *parents); !wasSet {
			// Key already existed, this is a duplicate
			stp.logger.Debugf("[addNode] duplicate transaction ignored %s", node.Hash.String())

			return nil
		}
	}

	if stp.currentSubtree == nil {
		stp.currentSubtree, err = subtreepkg.NewTreeByLeafCount(stp.currentItemsPerFile)
		if err != nil {
			return err
		}

		// This is the first subtree for this block - we need a coinbase placeholder
		err = stp.currentSubtree.AddCoinbaseNode()
		if err != nil {
			return err
		}

		stp.txCount.Add(1)
	}

	err = stp.currentSubtree.AddSubtreeNode(node)
	if err != nil {
		return errors.NewProcessingError("error adding node to subtree", err)
	}

	if stp.currentSubtree.IsComplete() {
		if err = stp.processCompleteSubtree(skipNotification); err != nil {
			return err
		}
	}

	return nil
}

func (stp *SubtreeProcessor) processCompleteSubtree(skipNotification bool) (err error) {
	if !skipNotification {
		stp.logger.Debugf("[%s] append subtree", stp.currentSubtree.RootHash().String())
	}

	// Track the actual number of nodes in this subtree
	// We don't exclude coinbase because:
	// 1. Only the first subtree in a block has a coinbase
	// 2. The coinbase is still a transaction that takes space
	// 3. For sizing decisions, we care about total throughput
	actualNodeCount := len(stp.currentSubtree.Nodes)
	if actualNodeCount > 0 {
		// Add to ring buffer (overwrites oldest value automatically)
		stp.subtreeNodeCounts.Value = actualNodeCount
		stp.subtreeNodeCounts = stp.subtreeNodeCounts.Next()
	}

	// Add the subtree to the chain
	stp.chainedSubtrees = append(stp.chainedSubtrees, stp.currentSubtree)
	stp.chainedSubtreeCount.Add(1)

	stp.subtreesInBlock++ // Track number of subtrees in current block

	oldSubtree := stp.currentSubtree
	oldSubtreeHash := oldSubtree.RootHash()

	// create a new subtree with the same height as the previous subtree
	stp.currentSubtree, err = subtreepkg.NewTree(stp.currentSubtree.Height)
	if err != nil {
		return errors.NewProcessingError("[%s] error creating new subtree", oldSubtreeHash.String(), err)
	}

	// Send the subtree to the newSubtreeChan, including a reference to the parent transactions map
	errCh := make(chan error)

	stp.newSubtreeChan <- NewSubtreeRequest{
		Subtree:          oldSubtree,
		ParentTxMap:      stp.currentTxMap,
		SkipNotification: skipNotification,
		ErrChan:          errCh,
	}

	err = <-errCh
	if err != nil {
		return errors.NewProcessingError("[%s] error sending subtree to newSubtreeChan", oldSubtreeHash.String(), err)
	}

	return nil
}

// Add adds a transaction node to the processor.
//
// Parameters:
//   - node: Transaction node to add
func (stp *SubtreeProcessor) Add(node subtreepkg.SubtreeNode, txInpoints subtreepkg.TxInpoints) {
	stp.queue.enqueue(node, txInpoints)
}

// AddDirectly adds a transaction node directly to the subtree processor without going through the queue.
// It is used for transactions that are already known to be valid and should be added immediately.
// This is useful for transactions that are part of the current block being processed.
//
// Parameters:
//   - node: Transaction node to add
//   - txInpoints: Transaction inpoints for the node
//   - skipNotification: Whether to skip notification of new subtrees
//
// Returns:
//   - error: Any error encountered during addition
func (stp *SubtreeProcessor) AddDirectly(node subtreepkg.SubtreeNode, txInpoints subtreepkg.TxInpoints, skipNotification bool) error {
	if _, ok := stp.currentTxMap.Get(node.Hash); ok {
		return errors.NewInvalidArgumentError("transaction already exists in currentTxMap")
	}

	err := stp.addNode(node, &txInpoints, skipNotification)
	if err != nil {
		return errors.NewProcessingError("error adding node directly to subtree", err)
	}

	stp.txCount.Add(1)

	return nil
}

// Remove prevents a transaction from being processed from the queue into a subtree, and removes it if already present.
// This can only take place before the delay time in the queue has passed.
//
// Parameters:
//   - hash: Hash of the transaction to remove
//
// Returns:
//   - error: Any error encountered during removal
func (stp *SubtreeProcessor) Remove(hash chainhash.Hash) error {
	// add to the removeMap to make sure it gets removed if processing
	// or if it comes in later after cleaning the subtrees
	if err := stp.removeMap.Put(hash); err != nil {
		return errors.NewProcessingError("error adding tx to remove map", err)
	}

	// send a remove request to the subtree processor, making sure it does not block
	go func() {
		stp.removeTxCh <- hash
	}()

	return nil
}

func (stp *SubtreeProcessor) removeTxFromSubtrees(ctx context.Context, hash chainhash.Hash) error {
	_, _, deferFn := tracing.Tracer("subtreeprocessor").Start(ctx, "removeTxFromSubtrees",
		tracing.WithParentStat(stp.stats),
		tracing.WithHistogram(prometheusSubtreeProcessorRemoveTx),
		tracing.WithLogMessage(stp.logger, "[SubtreeProcessor][removeTxFromSubtrees][%s] removing transaction from subtrees", hash),
	)

	defer deferFn()

	// find the transaction in the current and all chained subtrees
	foundIndex := stp.currentSubtree.NodeIndex(hash)
	foundSubtreeIndex := -1

	if foundIndex == -1 {
		// not found in the current subtree, check chained subtrees
		for subtreeIndex, subtree := range stp.chainedSubtrees {
			idx := subtree.NodeIndex(hash)
			if idx >= 0 {
				foundSubtreeIndex = subtreeIndex
				foundIndex = idx
			}
		}
	}

	if foundIndex >= 0 {
		// remove tx from the currentTxMap
		stp.currentTxMap.Delete(hash)

		// we found the transaction in a subtree
		if foundSubtreeIndex == -1 {
			// it was found in the current tree, remove it from there
			// further processing is not needed, as the subtrees in the chainedSubtrees are older than the current subtree
			return stp.currentSubtree.RemoveNodeAtIndex(foundIndex)
		}

		// it was found in a chained subtree, remove it from there and chain the subtrees again from the point it was removed
		// this is a bit more complex, as we need to remove the transaction from the subtree it is in and then make sure
		// the subtrees are chained correctly again
		if err := stp.chainedSubtrees[foundSubtreeIndex].RemoveNodeAtIndex(foundIndex); err != nil {
			return errors.NewProcessingError("[SubtreeProcessor][removeTxFromSubtrees][%s] error removing node from subtree", hash.String(), err)
		}

		// all the chained subtrees should be complete, as we now have a hole in the one we just removed from
		// we need to fill them up all again, including the current subtree
		if err := stp.reChainSubtrees(foundSubtreeIndex); err != nil {
			return errors.NewProcessingError("[SubtreeProcessor][removeTxFromSubtrees][%s] error rechaining subtrees", hash.String(), err)
		}
	}

	return nil
}

// removeTxsFromSubtrees removes multiple transactions from the subtrees.
// It finds each transaction in the current and chained subtrees, removes it, and then rechains the subtrees if necessary.
// This is not thread-safe. You should not be doing other subtree
// operations while this is running.
//
// Parameters:
//   - ctx: Context for the operation
//   - hashes: Slice of transaction hashes to remove
//
// Returns:
//   - error: Any error encountered during removal
func (stp *SubtreeProcessor) removeTxsFromSubtrees(ctx context.Context, hashes []chainhash.Hash) error {
	_, _, deferFn := tracing.Tracer("subtreeprocessor").Start(ctx, "removeTxsFromSubtrees",
		tracing.WithParentStat(stp.stats),
		tracing.WithHistogram(prometheusSubtreeProcessorRemoveTx),
		tracing.WithLogMessage(stp.logger, "[SubtreeProcessor][removeTxsFromSubtrees] removing %d transactions from subtrees", len(hashes)),
	)

	defer deferFn()

	for _, hash := range hashes {
		// find the transaction in the current and all chained subtrees
		foundIndex := stp.currentSubtree.NodeIndex(hash)
		foundSubtreeIndex := -1

		if foundIndex == -1 {
			// not found in the current subtree, check chained subtrees
			for subtreeIndex, subtree := range stp.chainedSubtrees {
				idx := subtree.NodeIndex(hash)
				if idx >= 0 {
					foundSubtreeIndex = subtreeIndex
					foundIndex = idx
				}
			}
		}

		if foundIndex >= 0 {
			// remove tx from the currentTxMap
			stp.currentTxMap.Delete(hash)

			// we found the transaction in a subtree
			if foundSubtreeIndex == -1 {
				// it was found in the current tree, remove it from there
				// further processing is not needed, as the subtrees in the chainedSubtrees are older than the current subtree
				return stp.currentSubtree.RemoveNodeAtIndex(foundIndex)
			}

			// it was found in a chained subtree, remove it from there and chain the subtrees again from the point it was removed
			// this is a bit more complex, as we need to remove the transaction from the subtree it is in and then make sure
			// the subtrees are chained correctly again
			if err := stp.chainedSubtrees[foundSubtreeIndex].RemoveNodeAtIndex(foundIndex); err != nil {
				return errors.NewProcessingError("[SubtreeProcessor][removeTxsFromSubtrees][%s] error removing node from subtree", hash.String(), err)
			}
		}
	}

	// all the chained subtrees should be complete, as we now have a hole in the one we just removed from
	// we need to fill them up all again, including the current subtree
	if err := stp.reChainSubtrees(0); err != nil {
		return errors.NewProcessingError("[SubtreeProcessor][removeTxsFromSubtrees] error rechaining subtrees", err)
	}

	return nil
}

// reChainSubtrees will cycle through all subtrees from the given index and create new subtrees from the nodes
// in the same order as they were before. This is not thread safe. You should not be doing other subtree
// operations while this is running.
//
// Parameters:
//   - fromIndex: Starting index for rechaining
//
// Returns:
//   - error: Any error encountered during rechaining
func (stp *SubtreeProcessor) reChainSubtrees(fromIndex int) error {
	// copy the original subtrees from the given index into a new structure
	originalSubtrees := stp.chainedSubtrees[fromIndex:]
	originalSubtrees = append(originalSubtrees, stp.currentSubtree)

	// reset the chained subtrees and the current subtree
	stp.chainedSubtrees = stp.chainedSubtrees[:fromIndex]

	fromIndexInt32, err := safeconversion.IntToInt32(fromIndex)
	if err != nil {
		return errors.NewProcessingError("error converting fromIndex", err)
	}

	stp.chainedSubtreeCount.Store(fromIndexInt32)

	stp.currentSubtree, err = subtreepkg.NewTreeByLeafCount(stp.currentItemsPerFile)
	if err != nil {
		return errors.NewProcessingError("error creating new current subtree", err)
	}

	if len(originalSubtrees) == 0 {
		// we must add the coinbase tx if we have no original subtrees
		if err = stp.currentSubtree.AddCoinbaseNode(); err != nil {
			return errors.NewProcessingError("error adding coinbase node to new current subtree", err)
		}
	}

	var (
		parents subtreepkg.TxInpoints
		found   bool
	)

	// Process nodes directly from original subtrees without intermediate storage
	// We temporarily remove from currentTxMap and immediately re-add to avoid
	// addNode's duplicate detection while minimizing memory overhead
	for _, subtree := range originalSubtrees {
		for _, node := range subtree.Nodes {
			if node.Hash.Equal(subtreepkg.CoinbasePlaceholderHashValue) {
				continue
			}

			parents, found = stp.currentTxMap.Get(node.Hash)
			if !found {
				// this should not happen, but if it does, we need to add the txInpoints to the currentTxMap
				return errors.NewProcessingError("error getting txInpoints from currentTxMap for %s", node.Hash.String())
			}

			// Remove from currentTxMap so addNode won't skip it as a duplicate
			stp.currentTxMap.Delete(node.Hash)

			// Immediately re-add the node
			if err = stp.addNode(node, &parents, true); err != nil {
				return errors.NewProcessingError("error adding node to subtree", err)
			}
		}
	}

	return nil
}

// CheckSubtreeProcessor checks the integrity of the subtree processor.
// It verifies that all transactions in the current transaction map
// are present in the subtrees and that the size of the current transaction map
// matches the expected transaction count.
//
// Returns:
//   - error: Any error encountered during the check
func (stp *SubtreeProcessor) CheckSubtreeProcessor() error {
	errCh := make(chan error)

	stp.checkSubtreeProcessorCh <- errCh

	return <-errCh
}

// checkSubtreeProcessor performs a check on the subtree processor's state.
func (stp *SubtreeProcessor) checkSubtreeProcessor(errCh chan error) {
	stp.logger.Infof("[SubtreeProcessor] checking subtree processor")

	// check all the transactions in the currentTxMap are in the subtrees
	for subtreeIdx, subtree := range stp.chainedSubtrees {
		for nodeIdx, node := range subtree.Nodes {
			if node.Hash.Equal(subtreepkg.CoinbasePlaceholderHashValue) {
				// check that the coinbase placeholder is in the first subtree
				if subtreeIdx != 0 || nodeIdx != 0 {
					errCh <- errors.NewSubtreeError("[SubtreeProcessor] coinbase placeholder not in first subtree %d", subtreeIdx)

					break
				}

				continue
			}

			if _, ok := stp.currentTxMap.Get(node.Hash); !ok {
				errCh <- errors.NewSubtreeError("[SubtreeProcessor] tx %s from subtree %d not in currentTxMap", node.Hash.String(), subtreeIdx)

				break
			}
		}
	}

	// check all the transactions in the subtrees are in the currentTxMap
	for nodeIdx, node := range stp.currentSubtree.Nodes {
		if _, ok := stp.currentTxMap.Get(node.Hash); !ok {
			if node.Hash.Equal(subtreepkg.CoinbasePlaceholderHashValue) {
				// check that the coinbase placeholder is in the first subtree
				if nodeIdx != 0 {
					errCh <- errors.NewSubtreeError("[SubtreeProcessor] coinbase placeholder not in first node of subtree %d", nodeIdx)

					break
				}

				continue
			}

			errCh <- errors.NewSubtreeError("[SubtreeProcessor] tx %s from currentSubtree not in currentTxMap", node.Hash.String())

			break
		}
	}

	// make sure we have an up2date count of the transactions
	stp.setTxCountFromSubtrees()

	// check that the size of the currentTxMap is equal to the sum of all the subtrees - coinbase placeholder
	currentTxMapSize := stp.currentTxMap.Length()
	txCount := stp.TxCount()

	if currentTxMapSize != int(txCount)-1 { // nolint:gosec
		errCh <- errors.NewSubtreeError("[SubtreeProcessor] currentTxMap size %d does not match txCount %d", currentTxMapSize, txCount-1)
	}

	errCh <- nil

	stp.logger.Infof("[SubtreeProcessor] check subtree processor DONE")
}

// GetCompletedSubtreesForMiningCandidate retrieves all completed subtrees for block mining.
//
// Returns:
//   - []*util.Subtree: Array of completed subtrees
func (stp *SubtreeProcessor) GetCompletedSubtreesForMiningCandidate() []*subtreepkg.Subtree {
	var subtrees []*subtreepkg.Subtree

	subtreesChan := make(chan []*subtreepkg.Subtree)

	// get the subtrees from channel
	stp.getSubtreesChan <- subtreesChan

	subtrees = <-subtreesChan

	return subtrees
}

// MoveForwardBlock updates the subtrees when a new block is found.
//
// Parameters:
//   - block: Block to process
//
// Returns:
//   - error: Any error encountered during processing
func (stp *SubtreeProcessor) MoveForwardBlock(block *model.Block) error {
	errChan := make(chan error)

	stp.moveForwardBlockChan <- moveBlockRequest{
		block:   block,
		errChan: errChan,
	}

	return <-errChan
}

// Reorg handles blockchain reorganization by processing moved blocks.
//
// Parameters:
//   - moveBackBlocks: Blocks to move down in the chain
//   - moveForwardBlocks: Blocks to move up in the chain
//
// Returns:
//   - error: Any error encountered during reorganization
func (stp *SubtreeProcessor) Reorg(moveBackBlocks []*model.Block, moveForwardBlocks []*model.Block) error {
	errChan := make(chan error)
	stp.reorgBlockChan <- reorgBlocksRequest{
		moveBackBlocks:    moveBackBlocks,
		moveForwardBlocks: moveForwardBlocks,
		errChan:           errChan,
	}

	return <-errChan
}

// reorgBlocks performs an incremental blockchain reorganization by processing blocks efficiently.
//
// This is the optimized path for handling small to medium-sized blockchain reorganizations
// (< CoinbaseMaturity blocks). It's more efficient than reset() because it:
// - Keeps existing subtrees intact where possible
// - Only modifies affected transactions incrementally
// - Avoids reloading all transactions from UTXO store
//
// The reorg process:
// 1. Move back: Loads transactions from moveBackBlocks into block assembly
//   - Extracts transactions from blocks no longer on main chain
//   - Adds them to subtrees for re-mining
//   - Tracks which transactions were in moveBack blocks
//
// 2. Move forward: Processes transactions from moveForwardBlocks
//   - Marks transactions (that weren't in moveBack) as ON longest chain (clears unmined_since) - Line 1796
//   - Removes them from block assembly (they're now mined)
//
// 3. Mark remaining block assembly txs as NOT on longest chain (sets unmined_since) - Lines 1816, 1854
//   - These are transactions still unmined after the reorg
//
// This function is MUTUALLY EXCLUSIVE with BlockAssembler.reset():
// - Small/medium successful reorgs: Use reorgBlocks() (this function)
// - Large/failed/invalid reorgs: Use reset() (BlockAssembler)
//
// Parameters:
//   - ctx: Context for cancellation
//   - moveBackBlocks: Blocks being removed from main chain (now on side chain)
//   - moveForwardBlocks: Blocks being added to main chain (new longest chain)
//
// Returns:
//   - error: Any error encountered during reorg (triggers fallback to reset())
func (stp *SubtreeProcessor) reorgBlocks(ctx context.Context, moveBackBlocks []*model.Block, moveForwardBlocks []*model.Block) (err error) {
	if moveBackBlocks == nil {
		return errors.NewProcessingError("you must pass in blocks to move down the chain")
	}

	if moveForwardBlocks == nil {
		return errors.NewProcessingError("you must pass in blocks to move up the chain")
	}

	stp.logger.Infof("reorgBlocks with %d moveBackBlocks and %d moveForwardBlocks", len(moveBackBlocks), len(moveForwardBlocks))

	if len(moveForwardBlocks) == 1 && len(moveBackBlocks) == 0 {
		// wait for the last block to be processed first, mined_set etc.
		ok, err := stp.waitForBlockBeingMined(ctx, moveForwardBlocks[len(moveForwardBlocks)-1].Hash())
		if err != nil {
			return errors.NewProcessingError("[reorgBlocks] error waiting for block being mined", err)
		}

		if !ok {
			return errors.NewProcessingError("[reorgBlocks] timeout waiting for block being mined %s", moveForwardBlocks[len(moveForwardBlocks)-1].Hash().String())
		}
	} else {
		// other operation, wait for all blocks to be processed first, mined_set etc.
		if err = stp.WaitForPendingBlocks(ctx); err != nil {
			return errors.NewProcessingError("[reorgBlocks] error waiting for blocks being mined", err)
		}
	}

	// dequeueDuringBlockMovement all transactions that are in the queue
	if err = stp.dequeueDuringBlockMovement(nil, nil, true); err != nil {
		return errors.NewProcessingError("[reorgBlocks] error dequeueing transactions during block movement", err)
	}

	// store current state for restore in case of error
	originalChainedSubtrees := stp.chainedSubtrees
	originalCurrentSubtree := stp.currentSubtree
	originalCurrentTxMap := stp.currentTxMap
	currentBlockHeader := stp.currentBlockHeader

	defer func() {
		if err != nil {
			// restore the original state
			stp.chainedSubtrees = originalChainedSubtrees
			stp.currentSubtree = originalCurrentSubtree
			stp.currentTxMap = originalCurrentTxMap
			stp.currentBlockHeader = currentBlockHeader

			// recalculate the tx count
			stp.setTxCountFromSubtrees()
		}
	}()

	// During a reorg, we need to ensure proper handling of conflicting transactions
	// When moving back, transactions that were in the winning chain need to have their
	// UTXO spends reverted so they can be properly detected as conflicts when moving forward

	// the processed conflicting hashes map keeps track of all the conflicting hashes we've already processed
	// this is to avoid processing the same conflicting hash multiple times if it appears in multiple blocks
	// the map is only used during the reorg process and is not stored in the SubtreeProcessor struct
	processedConflictingHashesMap := make(map[chainhash.Hash]bool)

	// movedBackBlockTxMap keeps track of all the transactions that were in the blocks we moved back
	// this is used to determine which transactions need to be marked as on the longest chain when moving forward
	// if a transaction was in a block we moved back, it means it was on the longest chain before the reorg
	movedBackBlockTxMap := make(map[chainhash.Hash]bool) // keeps track of all the transactions that were in the blocks we moved back

	for _, block := range moveBackBlocks {
		// move back the block, getting all the transactions in the block and any conflicting hashes
		// if we are not moving forward any blocks, we need to make sure we create properly sized subtrees
		// so we pass in len(moveForwardBlocks) == 0 as the second parameter
		subtreesNodes, conflictingHashes, err := stp.moveBackBlock(ctx, block, len(moveForwardBlocks) == 0)
		if err != nil {
			return err
		}

		if len(conflictingHashes) > 0 {
			for _, hash := range conflictingHashes {
				processedConflictingHashesMap[hash] = true
			}
		}

		// add all the transactions in the block to the movedBackBlockTxMap
		for _, subtreeNodes := range subtreesNodes {
			for _, node := range subtreeNodes {
				if !node.Hash.Equal(subtreepkg.CoinbasePlaceholderHashValue) {
					movedBackBlockTxMap[node.Hash] = true
				}
			}
		}
	}

	if len(moveBackBlocks) > 0 {
		// we've moved back x blocks, we need to set the current block header to the parent of the last block we moved back
		lastMoveBackBlock := moveBackBlocks[len(moveBackBlocks)-1]
		parentHeader, _, err := stp.blockchainClient.GetBlockHeader(ctx, lastMoveBackBlock.Header.HashPrevBlock)
		if err != nil {
			return errors.NewProcessingError("[reorgBlocks] error getting parent block header during reorg", err)
		}

		stp.currentBlockHeader = parentHeader
	}

	var (
		transactionMap     txmap.TxMap
		markOnLongestChain = make([]chainhash.Hash, 0, 1024)
	)

	for blockIdx, block := range moveForwardBlocks {
		lastMoveForwardBlock := blockIdx == len(moveForwardBlocks)-1
		// we skip the notifications for now and do them all at the end
		// transactionMap is returned so we can check which transactions need to be marked as on the longest chain
		if transactionMap, err = stp.moveForwardBlock(ctx, block, true, processedConflictingHashesMap, true, lastMoveForwardBlock); err != nil {
			return err
		}

		if transactionMap != nil {
			transactionMap.Iter(func(hash chainhash.Hash, n uint64) bool {
				// if the transaction is not in the movedBackBlockTxMap, it means it was not part of the blocks we moved back
				// and therefore needs to be marked in the utxo store as on the longest chain now
				// since it was on the block moving forward
				if !hash.Equal(subtreepkg.CoinbasePlaceholderHashValue) && !movedBackBlockTxMap[hash] {
					markOnLongestChain = append(markOnLongestChain, hash)
				}

				return true
			})
		}

		stp.currentBlockHeader = block.Header
	}

	movedBackBlockTxMap = nil // free up memory

	// all the transactions in markOnLongestChain need to be marked as on the longest chain in the utxo store
	if len(markOnLongestChain) > 0 {
		if err = stp.utxoStore.MarkTransactionsOnLongestChain(ctx, markOnLongestChain, true); err != nil {
			return errors.NewProcessingError("[reorgBlocks] error marking transactions as on longest chain in utxo store", err)
		}
	}

	// everything now in block assembly is not mined on the longest chain
	// so we need to set the unminedSince for all transactions in block assembly
	for _, subtree := range stp.chainedSubtrees {
		notOnLongestChain := make([]chainhash.Hash, 0, len(subtree.Nodes))

		for _, node := range subtree.Nodes {
			if node.Hash.Equal(subtreepkg.CoinbasePlaceholderHashValue) {
				// skip coinbase placeholder
				continue
			}

			notOnLongestChain = append(notOnLongestChain, node.Hash)
		}

		if len(notOnLongestChain) > 0 {
			if err = stp.markNotOnLongestChain(ctx, moveBackBlocks, moveForwardBlocks, notOnLongestChain); err != nil {
				return err
			}
		}
	}

	// also for the current subtree
	notOnLongestChain := make([]chainhash.Hash, 0, len(stp.currentSubtree.Nodes))

	for _, node := range stp.currentSubtree.Nodes {
		if node.Hash.Equal(subtreepkg.CoinbasePlaceholderHashValue) {
			// skip coinbase placeholder
			continue
		}

		notOnLongestChain = append(notOnLongestChain, node.Hash)
	}

	if len(notOnLongestChain) > 0 {
		if err = stp.markNotOnLongestChain(ctx, moveBackBlocks, moveForwardBlocks, notOnLongestChain); err != nil {
			return err
		}
	}

	// announce all the subtrees to the network
	// this will also store it by the Server in the subtree store
	for _, subtree := range stp.chainedSubtrees {
		errCh := make(chan error)
		stp.newSubtreeChan <- NewSubtreeRequest{Subtree: subtree, ParentTxMap: stp.currentTxMap, ErrChan: errCh}

		if err = <-errCh; err != nil {
			return errors.NewProcessingError("[reorgBlocks] error sending subtree to newSubtreeChan", err)
		}
	}

	// Mark all the moveForwardBlocks as processed
	for _, block := range moveForwardBlocks {
		if err = stp.blockchainClient.SetBlockProcessedAt(ctx, block.Header.Hash()); err != nil {
			return errors.NewProcessingError("[reorgBlocks][%s] error setting block processed_at timestamp: %v", block.String(), err)
		}
	}

	// persist the current state
	if len(moveForwardBlocks) > 0 {
		stp.finalizeBlockProcessing(ctx, moveForwardBlocks[len(moveForwardBlocks)-1])
	} else if len(moveBackBlocks) > 0 {
		// we only moved back, finalize with the parent of the last block we moved back
		block, err := stp.blockchainClient.GetBlock(ctx, moveBackBlocks[len(moveBackBlocks)-1].Header.HashPrevBlock)
		if err != nil {
			return errors.NewProcessingError("[reorgBlocks] error getting parent block of last block we moved back", err)
		}
		stp.finalizeBlockProcessing(ctx, block)
	} else {
		return errors.NewProcessingError("[reorgBlocks] no blocks to finalize after reorg")
	}

	return nil
}

func (stp *SubtreeProcessor) markNotOnLongestChain(ctx context.Context, moveBackBlocks []*model.Block, moveForwardBlocks []*model.Block, markNotOnLongestChain []chainhash.Hash) error {
	if len(moveBackBlocks) == 1 && len(moveForwardBlocks) == 0 {
		// special case: likely an invalidation; validate whether to update the transactions or not
		_, blockHeaderMeta, err := stp.blockchainClient.GetBlockHeader(ctx, moveBackBlocks[0].Header.Hash())
		if err != nil {
			return errors.NewProcessingError("[reorgBlocks] error getting block header meta for block we moved back", err)
		}

		if blockHeaderMeta.Invalid {
			// the block we moved back is invalid, so we cannot just mark all transactions as not on the longest chain
			markNotOnLongestChain, err = stp.checkMarkNotOnLongestChain(ctx, moveBackBlocks[0], markNotOnLongestChain)
			if err != nil {
				return errors.NewProcessingError("[reorgBlocks] error checking which transactions to mark as not on longest chain", err)
			}
		}
	}

	// check again if we have any transactions to mark, after the checkMarkNotOnLongestChain
	if len(markNotOnLongestChain) > 0 {
		if err := stp.utxoStore.MarkTransactionsOnLongestChain(ctx, markNotOnLongestChain, false); err != nil {
			return errors.NewProcessingError("[reorgBlocks] error marking transactions as not on longest chain in utxo store", err)
		}
	}

	return nil
}

func (stp *SubtreeProcessor) checkMarkNotOnLongestChain(ctx context.Context, invalidBlock *model.Block, markNotOnLongestChain []chainhash.Hash) ([]chainhash.Hash, error) {
	stp.logger.Infof("[reorgBlocks] block %s we moved back is invalid, checking whether to mark transactions as not on longest chain", invalidBlock.String())

	checkMarkNotOnLongestChain := make([]chainhash.Hash, 0, len(markNotOnLongestChain))

	_, lastBlockHeaderMetas, err := stp.blockchainClient.GetBlockHeaders(ctx, invalidBlock.Header.HashPrevBlock, 1000)
	if err != nil {
		return nil, errors.NewProcessingError("[reorgBlocks] error getting last block headers", err)
	}

	lastBlockHeaderIDs := make(map[uint32]bool)
	for _, header := range lastBlockHeaderMetas {
		lastBlockHeaderIDs[header.ID] = true
	}

	txMetas := make([]*meta.Data, len(markNotOnLongestChain))
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(stp.settings.UtxoStore.GetBatcherSize * 2)

	// we need to check each transaction in the block we moved back and see if it is still on the longest chain or not
	for idx, hash := range markNotOnLongestChain {
		idx := idx
		hashRef := &hash

		g.Go(func() error {
			txMeta, err := stp.utxoStore.Get(gCtx, hashRef, fields.BlockIDs)
			if err != nil {
				return errors.NewProcessingError("[reorgBlocks] error getting transaction from utxo store", err)
			}

			txMetas[idx] = txMeta

			return nil
		})
	}

	if err = g.Wait(); err != nil {
		return nil, err
	}

	for idx, hash := range markNotOnLongestChain {
		txMeta := txMetas[idx]
		if txMeta == nil {
			// transaction not found, not OK
			return nil, errors.NewProcessingError("[reorgBlocks] error getting transaction %s from longest chain", hash.String(), err)
		}

		if len(txMeta.BlockIDs) == 1 && txMeta.BlockIDs[0] == invalidBlock.ID {
			// the transaction is only in the invalid block, so it is definitely not on the longest chain
			checkMarkNotOnLongestChain = append(checkMarkNotOnLongestChain, hash)
		} else {
			for _, blockID := range txMeta.BlockIDs {
				if lastBlockHeaderIDs[blockID] {
					// the transaction is still in one of the last 1000 blocks, so it is still on the longest chain
					// do not mark it as not on the longest chain
					continue
				}
			}

			// check BlockIDs to see if the transaction is still on the longest chain
			onLongestChain, err := stp.blockchainClient.CheckBlockIsInCurrentChain(ctx, txMeta.BlockIDs)
			if err != nil {
				return nil, errors.NewProcessingError("[reorgBlocks] error checking if transaction is on longest chain", err)
			}

			if !onLongestChain {
				checkMarkNotOnLongestChain = append(checkMarkNotOnLongestChain, hash)
			}
		}
	}

	return checkMarkNotOnLongestChain, nil
}

// setTxCountFromSubtrees recalculates the total transaction count
// by summing transactions across all subtrees, the current subtree,
// and the queue. This ensures the transaction count remains accurate
// after chain modifications.
func (stp *SubtreeProcessor) setTxCountFromSubtrees() {
	stp.txCount.Store(0)

	var (
		subtreeLen uint64
		err        error
	)

	for _, subtree := range stp.chainedSubtrees {
		subtreeLen, err = safeconversion.IntToUint64(subtree.Length())
		if err != nil {
			stp.logger.Errorf("error converting subtree length: %s", err)
			continue
		}

		stp.txCount.Add(subtreeLen)
	}

	currSubtreeLenUint64, err := safeconversion.IntToUint64(stp.currentSubtree.Length())
	if err != nil {
		stp.logger.Errorf("error converting current subtree length: %s", err)
		return
	}

	queueLenUint64, err := safeconversion.Int64ToUint64(stp.queue.length())
	if err != nil {
		stp.logger.Errorf("error converting queue length: %s", err)
		return
	}

	stp.txCount.Add(currSubtreeLenUint64)
	stp.txCount.Add(queueLenUint64)
}

// moveBackBlock processes a block during downward chain movement.
//
// Parameters:
//   - ctx: Context for cancellation
//   - block: Block to process
//
// Returns:
//   - error: Any error encountered during processing
func (stp *SubtreeProcessor) moveBackBlock(ctx context.Context, block *model.Block, createProperlySizedSubtrees bool) (subtreesNodes [][]subtreepkg.SubtreeNode, conflictingHashes []chainhash.Hash, err error) {
	if block == nil {
		return nil, nil, errors.NewProcessingError("[moveBackBlock] you must pass in a block to moveBackBlock")
	}

	_, _, deferFn := tracing.Tracer("subtreeprocessor").Start(ctx, "moveBackBlock",
		tracing.WithCounter(prometheusSubtreeProcessorMoveBackBlock),
		tracing.WithHistogram(prometheusSubtreeProcessorMoveBackBlockDuration),
		tracing.WithLogMessage(stp.logger, "[moveBackBlock][%s] with %d subtrees", block.String(), len(block.Subtrees)),
	)
	defer func() {
		deferFn()
	}()

	lastIncompleteSubtree := stp.currentSubtree
	chainedSubtrees := stp.chainedSubtrees

	// process coinbase utxos
	if err = stp.removeCoinbaseUtxos(ctx, block); err != nil {
		// no need to error out if the key doesn't exist anyway
		if !errors.Is(err, errors.ErrTxNotFound) {
			return nil, nil, errors.NewProcessingError("[moveBackBlock][%s] error removing coinbase utxo", block.String(), err)
		}
	}

	// create new subtrees and add all the transactions from the block to it
	if subtreesNodes, conflictingHashes, err = stp.moveBackBlockCreateNewSubtrees(ctx, block, createProperlySizedSubtrees); err != nil {
		return nil, nil, err
	}

	// add all the transactions from the previous state
	if err = stp.moveBackBlockAddPreviousNodes(ctx, block, chainedSubtrees, lastIncompleteSubtree); err != nil {
		return nil, nil, err
	}

	// set the tx count from the subtrees
	stp.setTxCountFromSubtrees()

	// Clear the block's processed timestamp
	if err = stp.blockchainClient.SetBlockProcessedAt(ctx, block.Header.Hash(), true); err != nil {
		// Don't return error here, as this is not critical for the operation
		stp.logger.Errorf("[moveBackBlock][%s] error clearing block processed_at timestamp: %v", block.String(), err)
	}

	return subtreesNodes, conflictingHashes, nil
}

func (stp *SubtreeProcessor) moveBackBlockAddPreviousNodes(ctx context.Context, block *model.Block, chainedSubtrees []*subtreepkg.Subtree, lastIncompleteSubtree *subtreepkg.Subtree) error {
	_, _, deferFn := tracing.Tracer("subtreeprocessor").Start(ctx, "moveBackBlock",
		tracing.WithLogMessage(stp.logger, "[moveBackBlock:AddPreviousNodes][%s] with %d subtrees: add previous nodes to subtrees", block.String(), len(block.Subtrees)),
	)
	defer deferFn()

	// add all the transactions from the previous state
	for _, subtree := range chainedSubtrees {
		for _, node := range subtree.Nodes {
			if node.Hash.Equal(subtreepkg.CoinbasePlaceholderHashValue) {
				// skip coinbase placeholder
				continue
			}

			if err := stp.addNode(node, nil, true); err != nil {
				return errors.NewProcessingError("[moveBackBlock:AddPreviousNodes][%s] error adding node to subtree", block.String(), err)
			}
		}
	}

	// add all the transactions from the last incomplete subtree
	for _, node := range lastIncompleteSubtree.Nodes {
		if node.Hash.Equal(subtreepkg.CoinbasePlaceholderHashValue) {
			// skip coinbase placeholder
			continue
		}

		if err := stp.addNode(node, nil, true); err != nil {
			return errors.NewProcessingError("[moveBackBlock:AddPreviousNodes][%s] error adding node to subtree", block.String(), err)
		}
	}

	return nil
}

func (stp *SubtreeProcessor) moveBackBlockCreateNewSubtrees(ctx context.Context, block *model.Block, createProperlySizedSubtrees bool) ([][]subtreepkg.SubtreeNode, []chainhash.Hash, error) {
	_, _, deferFn := tracing.Tracer("subtreeprocessor").Start(ctx, "moveBackBlockCreateNewSubtrees",
		tracing.WithLogMessage(stp.logger, "[moveBackBlock:CreateNewSubtrees][%s] with %d subtrees: create new subtrees", block.String(), len(block.Subtrees)),
	)
	defer deferFn()

	// get all the subtrees in the block
	subtreesNodes, subtreeMetaTxInpoints, conflictingHashes, err := stp.moveBackBlockGetSubtrees(ctx, block)
	if err != nil {
		return nil, nil, errors.NewProcessingError("[moveBackBlock:CreateNewSubtrees][%s] error getting subtrees", block.String(), err)
	}

	// reset the subtree processor
	subtreeSize := stp.currentItemsPerFile
	if !createProperlySizedSubtrees {
		// if we are moving forward blocks, we do not care about the subtree size
		// as we will create new subtrees anyway when moving forward so for simplicity and speed,
		// we create as few subtrees as possible when moving back, to avoid fragmentation and lots of small writes to disk
		subtreeSize = 1024 * 1024
	}
	stp.currentSubtree, err = subtreepkg.NewTreeByLeafCount(subtreeSize)
	if err != nil {
		return nil, nil, errors.NewProcessingError("[moveBackBlock:CreateNewSubtrees][%s] error creating new subtree", block.String(), err)
	}

	stp.chainedSubtrees = make([]*subtreepkg.Subtree, 0, ExpectedNumberOfSubtrees)
	stp.chainedSubtreeCount.Store(0)

	// add first coinbase placeholder transaction
	_ = stp.currentSubtree.AddCoinbaseNode()

	// run through the nodes of the subtrees in order and add to the new subtrees
	if len(subtreesNodes) > 0 {
		for idx, subtreeNodes := range subtreesNodes {
			subtreeHash := block.Subtrees[idx]

			if idx == 0 {
				// skip the first transaction of the first subtree (coinbase)
				for i := 1; i < len(subtreeNodes); i++ {
					if err = stp.addNode(subtreeNodes[i], &subtreeMetaTxInpoints[idx][i], true); err != nil {
						return nil, nil, errors.NewProcessingError("[moveBackBlock:CreateNewSubtrees][%s][%s] error adding node to subtree", block.String(), subtreeHash.String(), err)
					}
				}
			} else {
				for i, node := range subtreeNodes {
					if err = stp.addNode(node, &subtreeMetaTxInpoints[idx][i], true); err != nil {
						return nil, nil, errors.NewProcessingError("[moveBackBlock:CreateNewSubtrees][%s][%s] error adding node to subtree", block.String(), subtreeHash.String(), err)
					}
				}
			}
		}
	}

	return subtreesNodes, conflictingHashes, nil
}

// removeCoinbaseUtxos removes the coinbase UTXO and its child spends from the UTXO store.
//
// Parameters:
//   - ctx: Context for cancellation
//   - block: Block containing the coinbase transaction
//   - subtreeHash: Hash of the subtree containing the coinbase
//
// Returns:
//   - error: Any error encountered during removal
func (stp *SubtreeProcessor) removeCoinbaseUtxos(ctx context.Context, block *model.Block) error {
	// get all child spends of the coinbase, this will lock them in the utxo store
	// so they cannot be spent while we are processing the reorg
	childSpendHashes, err := utxostore.GetAndLockChildren(ctx, stp.utxoStore, *block.CoinbaseTx.TxIDChainHash())
	if err != nil {
		if errors.Is(err, errors.ErrTxNotFound) {
			// coinbase tx not found, nothing to do
			return nil
		}

		return errors.NewProcessingError("[removeCoinbaseUtxos][%s] error getting child spends for coinbase tx %s", block.String(), block.CoinbaseTx.String(), err)
	}

	if len(childSpendHashes) > 0 {
		stp.logger.Warnf("[removeCoinbaseUtxos][%s] removing %d child spends of coinbase tx %s", block.String(), len(childSpendHashes), block.CoinbaseTx.String())

		// remove all the child spends from the utxo store
		for _, childSpendHash := range childSpendHashes {
			if err = stp.utxoStore.Delete(ctx, &childSpendHash); err != nil {
				return errors.NewProcessingError("[removeCoinbaseUtxos][%s] error deleting child spend utxo for tx %s", block.String(), childSpendHash.String(), err)
			}

			// add to txRemoveMap to make sure queued transactions are not processed
			if !stp.removeMap.Exists(childSpendHash) {
				if err = stp.removeMap.Put(childSpendHash); err != nil {
					return errors.NewProcessingError("[removeCoinbaseUtxos][%s] error adding child spend to remove map for tx %s", block.String(), childSpendHash.String(), err)
				}
			}
		}

		// remove from the subtree processor as well
		if err = stp.removeTxsFromSubtrees(ctx, childSpendHashes); err != nil {
			return errors.NewProcessingError("[removeCoinbaseUtxos][%s] error removing child spends from subtrees", block.String(), err)
		}
	}

	if err = stp.utxoStore.Delete(ctx, block.CoinbaseTx.TxIDChainHash()); err != nil {
		return errors.NewProcessingError("[removeCoinbaseUtxos][%s] error deleting utxos for tx %s", block.String(), block.CoinbaseTx.String(), err)
	}

	return nil
}

func (stp *SubtreeProcessor) moveBackBlockGetSubtrees(ctx context.Context, block *model.Block) ([][]subtreepkg.SubtreeNode, [][]subtreepkg.TxInpoints, []chainhash.Hash, error) {
	_, _, deferFn := tracing.Tracer("subtreeprocessor").Start(ctx, "moveBackBlockGetSubtrees",
		tracing.WithLogMessage(stp.logger, "[moveBackBlock:GetSubtrees][%s] with %d subtrees: get subtrees", block.String(), len(block.Subtrees)),
	)
	defer deferFn()

	g, gCtx := errgroup.WithContext(ctx)
	util.SafeSetLimit(g, stp.settings.BlockAssembly.MoveBackBlockConcurrency)

	// get all the subtrees in parallel
	subtreesNodes := make([][]subtreepkg.SubtreeNode, len(block.Subtrees))
	subtreeMetaTxInpoints := make([][]subtreepkg.TxInpoints, len(block.Subtrees))
	conflictingHashes := make([]chainhash.Hash, 0, 1024) // preallocate some space
	conflictingHashesMu := sync.Mutex{}

	for idx, subtreeHash := range block.Subtrees {
		idx := idx
		subtreeHash := subtreeHash

		g.Go(func() error {
			subtreeReader, err := stp.subtreeStore.GetIoReader(gCtx, subtreeHash[:], fileformat.FileTypeSubtree)
			if err != nil {
				subtreeReader, err = stp.subtreeStore.GetIoReader(gCtx, subtreeHash[:], fileformat.FileTypeSubtreeToCheck)
				if err != nil {
					return errors.NewServiceError("[moveBackBlock:GetSubtrees][%s] error getting subtree %s", block.String(), subtreeHash.String(), err)
				}
			}

			defer func() {
				_ = subtreeReader.Close()
			}()

			subtree := &subtreepkg.Subtree{}

			if err = subtree.DeserializeFromReader(subtreeReader); err != nil {
				return errors.NewProcessingError("[moveBackBlock:GetSubtrees][%s] error deserializing subtree", block.String(), err)
			}

			subtreesNodes[idx] = subtree.Nodes

			if subtreeHash.IsEqual(subtreepkg.CoinbasePlaceholderHash) {
				// Skip coinbase placeholder subtree
				stp.logger.Debugf("[moveBackBlock:GetSubtrees][%s] skipping coinbase placeholder subtree %s", block.String(), subtreeHash.String())
				subtreeMetaTxInpoints[idx] = make([]subtreepkg.TxInpoints, len(subtree.Nodes))
				return nil
			}

			subtreeMetaReader, err := stp.subtreeStore.GetIoReader(gCtx, subtreeHash[:], fileformat.FileTypeSubtreeMeta)
			if err != nil {
				return errors.NewServiceError("[moveBackBlock:GetSubtrees][%s] error getting subtree meta %s", block.String(), subtreeHash.String(), err)
			}

			subtreeMeta, err := subtreepkg.NewSubtreeMetaFromReader(subtree, subtreeMetaReader)
			if err != nil {
				return errors.NewProcessingError("[moveBackBlock:GetSubtrees][%s] error deserializing subtree meta", block.String(), err)
			}

			subtreeMetaTxInpoints[idx] = subtreeMeta.TxInpoints

			// process conflicting hashes
			if len(subtree.ConflictingNodes) > 0 {
				conflictingHashesMu.Lock()
				conflictingHashes = append(conflictingHashes, subtree.ConflictingNodes...)
				conflictingHashesMu.Unlock()
			}

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, nil, errors.NewProcessingError("[moveBackBlock:GetSubtrees][%s] error getting subtrees", block.String(), err)
	}

	return subtreesNodes, subtreeMetaTxInpoints, conflictingHashes, nil
}

// processBlockSubtrees creates a reverse lookup map of block subtrees and filters out chained subtrees
func (stp *SubtreeProcessor) processBlockSubtrees(block *model.Block) (map[chainhash.Hash]int, []*subtreepkg.Subtree) {
	// create a reverse lookup map of all the subtrees in the block
	blockSubtreesMap := make(map[chainhash.Hash]int, len(block.Subtrees))
	for idx, subtree := range block.Subtrees {
		blockSubtreesMap[*subtree] = idx
	}

	// get all the subtrees that were not in the block
	// this should clear out all subtrees from our own blocks, giving an empty blockSubtreesMap as a result
	// and preventing processing of the map
	chainedSubtrees := make([]*subtreepkg.Subtree, 0, ExpectedNumberOfSubtrees)
	for _, subtree := range stp.chainedSubtrees {
		id := *subtree.RootHash()
		if _, ok := blockSubtreesMap[id]; !ok {
			// only add the subtrees that were not in the block
			chainedSubtrees = append(chainedSubtrees, subtree)
		} else {
			// remove the subtree from the block subtrees map, we had it in our list
			delete(blockSubtreesMap, id)
		}
	}

	return blockSubtreesMap, chainedSubtrees
}

// createTransactionMapIfNeeded creates a transaction map from block subtrees if needed
func (stp *SubtreeProcessor) createTransactionMapIfNeeded(ctx context.Context, block *model.Block, blockSubtreesMap map[chainhash.Hash]int) (txmap.TxMap, []chainhash.Hash, error) {
	var (
		transactionMap   txmap.TxMap
		conflictingNodes []chainhash.Hash
	)

	if len(blockSubtreesMap) > 0 {
		mapStartTime := time.Now()

		stp.logger.Debugf("[moveForwardBlock][%s] processing subtrees into transaction map", block.String())

		var err error
		if transactionMap, conflictingNodes, err = stp.CreateTransactionMap(ctx, blockSubtreesMap, len(block.Subtrees), block.TransactionCount); err != nil {
			// TODO revert the created utxos
			return nil, nil, errors.NewProcessingError("[moveForwardBlock][%s] error creating transaction map", block.String(), err)
		}

		stp.logger.Debugf("[moveForwardBlock][%s] processing subtrees into transaction map DONE in %s: %d", block.String(), time.Since(mapStartTime).String(), transactionMap.Length())
	}

	return transactionMap, conflictingNodes, nil
}

// processConflictingTransactions handles conflicting transactions and returns losing transaction hashes
func (stp *SubtreeProcessor) processConflictingTransactions(ctx context.Context, block *model.Block,
	conflictingNodes []chainhash.Hash, processedConflictingHashesMap map[chainhash.Hash]bool) (txmap.TxMap, error) {
	var losingTxHashesMap txmap.TxMap

	// process conflicting txs
	if len(conflictingNodes) > 0 {
		// before we process the conflicting transactions, we need to make sure this block has been marked as mined
		// that would mean any previous block is also marked as mined and the data should be in a correct state
		// we can then process the conflicting transactions
		_, err := stp.waitForBlockBeingMined(ctx, block.Header.Hash())
		if err != nil {
			return nil, errors.NewProcessingError("[moveForwardBlock][%s] error waiting for block to be mined", block.String(), err)
		}

		if losingTxHashesMap, err = utxostore.ProcessConflicting(ctx, stp.utxoStore, conflictingNodes, processedConflictingHashesMap); err != nil {
			return nil, errors.NewProcessingError("[moveForwardBlock][%s] error processing conflicting transactions", block.String(), err)
		}

		if losingTxHashesMap.Length() > 0 {
			// mark all the losing txs in the subtrees in the blocks they were mined into as conflicting
			if err = stp.markConflictingTxsInSubtrees(ctx, losingTxHashesMap); err != nil {
				return nil, errors.NewProcessingError("[moveForwardBlock][%s] error marking conflicting transactions", block.String(), err)
			}
		}
	}

	return losingTxHashesMap, nil
}

// resetSubtreeState resets the current subtree state and returns the old state
func (stp *SubtreeProcessor) resetSubtreeState(createProperlySizedSubtrees bool) (err error) {
	// Save current state
	stp.currentTxMap = txmap.NewSyncedMap[chainhash.Hash, subtreepkg.TxInpoints]()

	subtreeSize := stp.currentItemsPerFile
	if !createProperlySizedSubtrees {
		// if we are moving forward blocks, we do not care about the subtree size
		// as we will create new subtrees anyway when moving forward so for simplicity and speed,
		// we create as few subtrees as possible when moving back, to avoid fragmentation and lots of small writes to disk
		subtreeSize = 1024 * 1024
	}

	stp.currentSubtree, err = subtreepkg.NewTreeByLeafCount(subtreeSize)
	if err != nil {
		return err
	}

	stp.chainedSubtrees = make([]*subtreepkg.Subtree, 0, ExpectedNumberOfSubtrees)
	stp.chainedSubtreeCount.Store(0)

	// Add first coinbase placeholder transaction
	_ = stp.currentSubtree.AddCoinbaseNode()

	return nil
}

// processRemainderTransactionsAndDequeue processes remaining transactions from the block
func (stp *SubtreeProcessor) processRemainderTransactionsAndDequeue(ctx context.Context, params *RemainderTransactionParams) error {
	if params.TransactionMap != nil && params.TransactionMap.Length() > 0 {
		_, _, deferFn := tracing.Tracer("subtreeprocessor").Start(ctx, "processRemainderTransactionsAndDequeue",
			tracing.WithParentStat(stp.stats),
			tracing.WithLogMessage(stp.logger, "[moveForwardBlock][%s] processing %d remainder tx hashes into subtrees", params.Block.String(), params.TransactionMap.Length()),
		)

		defer deferFn()

		// Process external block transactions
		remainderSubtrees := make([]*subtreepkg.Subtree, 0, len(params.ChainedSubtrees)+1)
		remainderSubtrees = append(remainderSubtrees, params.ChainedSubtrees...)
		remainderSubtrees = append(remainderSubtrees, params.CurrentSubtree)

		remainderTxHashesStartTime := time.Now()

		stp.logger.Debugf("[moveForwardBlock][%s] processRemainderTxHashes with %d subtrees", params.Block.String(), len(params.ChainedSubtrees))

		if err := stp.processRemainderTxHashes(ctx, remainderSubtrees, params.TransactionMap, params.LosingTxHashesMap, params.CurrentTxMap, params.SkipNotification); err != nil {
			return errors.NewProcessingError("[moveForwardBlock][%s] error getting remainder tx hashes", params.Block.String(), err)
		}

		stp.logger.Debugf("[moveForwardBlock][%s] processRemainderTxHashes with %d subtrees DONE in %s", params.Block.String(), len(params.ChainedSubtrees), time.Since(remainderTxHashesStartTime).String())

		// Process queue
		dequeueStartTime := time.Now()

		stp.logger.Debugf("[moveForwardBlock][%s] processing queue while moveForwardBlock: %d", params.Block.String(), stp.queue.length())

		if !params.SkipDequeue {
			if err := stp.dequeueDuringBlockMovement(params.TransactionMap, params.LosingTxHashesMap, params.SkipNotification); err != nil {
				return errors.NewProcessingError("[moveForwardBlock][%s] error moving up block deQueue", params.Block.String(), err)
			}
		}

		stp.logger.Debugf("[moveForwardBlock][%s] processing queue while moveForwardBlock DONE in %s", params.Block.String(), time.Since(dequeueStartTime).String())
	} else {
		// Process our own block
		if err := stp.processOwnBlockNodes(ctx, params.Block, params.ChainedSubtrees, params.CurrentSubtree, params.CurrentTxMap, params.SkipNotification); err != nil {
			return err
		}
	}

	return nil
}

// processOwnBlockNodes processes nodes when this was most likely our own block
func (stp *SubtreeProcessor) processOwnBlockNodes(_ context.Context, block *model.Block, chainedSubtrees []*subtreepkg.Subtree, currentSubtree *subtreepkg.Subtree, currentTxMap *txmap.SyncedMap[chainhash.Hash, subtreepkg.TxInpoints], skipNotification bool) error {
	removeMapLength := stp.removeMap.Length()
	coinbaseID := block.CoinbaseTx.TxIDChainHash()

	// Process nodes from chained subtrees
	for _, subtree := range chainedSubtrees {
		if err := stp.processOwnBlockSubtreeNodes(block, subtree.Nodes, currentTxMap, removeMapLength, nil, skipNotification); err != nil {
			return err
		}
	}

	// Process nodes from current subtree
	if err := stp.processOwnBlockSubtreeNodes(block, currentSubtree.Nodes, currentTxMap, removeMapLength, coinbaseID, skipNotification); err != nil {
		return err
	}

	return nil
}

// processOwnBlockSubtreeNodes processes nodes from a subtree for our own block
func (stp *SubtreeProcessor) processOwnBlockSubtreeNodes(block *model.Block, nodes []subtreepkg.SubtreeNode, currentTxMap *txmap.SyncedMap[chainhash.Hash, subtreepkg.TxInpoints], removeMapLength int, coinbaseID *chainhash.Hash, skipNotification bool) error {
	for _, node := range nodes {
		if node.Hash.Equal(*subtreepkg.CoinbasePlaceholderHash) {
			continue
		}

		// Skip coinbase if provided
		if coinbaseID != nil && coinbaseID.Equal(node.Hash) {
			continue
		}

		if removeMapLength > 0 && stp.removeMap.Exists(node.Hash) {
			if err := stp.removeMap.Delete(node.Hash); err != nil {
				stp.logger.Errorf("[moveForwardBlock][%s] error removing tx from remove map: %s", block.String(), err.Error())
			}
		} else {
			nodeParents, found := currentTxMap.Get(node.Hash)
			if !found {
				return errors.NewProcessingError("[moveForwardBlock][%s] error getting node txInpoints from currentTxMap for %s", block.String(), node.Hash.String())
			}

			if err := stp.addNode(node, &nodeParents, skipNotification); err != nil {
				return errors.NewProcessingError("[moveForwardBlock][%s] error adding node %s to subtree", block.String(), node.Hash.String(), err)
			}
		}
	}

	return nil
}

// finalizeBlockProcessing performs final steps after processing block transactions
func (stp *SubtreeProcessor) finalizeBlockProcessing(ctx context.Context, block *model.Block) {
	// set the correct count of the current subtrees
	stp.setTxCountFromSubtrees()

	// set the current block header
	stp.currentBlockHeader = block.Header

	// When starting a new block, calculate the average interval per subtree
	// from the previous block and adjust the subtree size
	if stp.blockStartTime != (time.Time{}) {
		blockDuration := time.Since(stp.blockStartTime)

		if stp.subtreesInBlock > 0 {
			avgIntervalPerSubtree := blockDuration / time.Duration(stp.subtreesInBlock)
			stp.blockIntervals = append(stp.blockIntervals, avgIntervalPerSubtree)

			if len(stp.blockIntervals) > stp.maxBlockSamples {
				stp.blockIntervals = stp.blockIntervals[1:]
			}
		}
	}

	stp.adjustSubtreeSize()

	// Mark the block as processed
	if err := stp.blockchainClient.SetBlockProcessedAt(ctx, block.Header.Hash()); err != nil {
		// Don't return error here, as this is not critical for the operation
		stp.logger.Warnf("[moveForwardBlock][%s] error setting block processed_at timestamp: %v", block.String(), err)
	}
}

// moveForwardBlock cleans out all transactions that are in the current subtrees and also in the block
// given. It is akin to moving up the blockchain to the next block.
func (stp *SubtreeProcessor) moveForwardBlock(ctx context.Context, block *model.Block, skipNotification bool,
	processedConflictingHashesMap map[chainhash.Hash]bool, skipDequeue bool, createProperlySizedSubtrees bool) (transactionMap txmap.TxMap, err error) {
	if block == nil {
		return nil, errors.NewProcessingError("[moveForwardBlock] you must pass in a block to moveForwardBlock")
	}

	_, _, deferFn := tracing.Tracer("subtreeprocessor").Start(ctx, "moveForwardBlock",
		tracing.WithParentStat(stp.stats),
		tracing.WithCounter(prometheusSubtreeProcessorMoveForwardBlock),
		tracing.WithHistogram(prometheusSubtreeProcessorMoveForwardBlockDuration),
		tracing.WithLogMessage(stp.logger, "[moveForwardBlock][%s] with block", block.String()),
	)

	defer func() {
		deferFn()
	}()

	if !block.Header.HashPrevBlock.IsEqual(stp.currentBlockHeader.Hash()) {
		return nil, errors.NewProcessingError("the block passed in does not match the current block header: [%s] - [%s]", block.Header.StringDump(), stp.currentBlockHeader.StringDump())
	}

	stp.logger.Debugf("[moveForwardBlock][%s] resetting subtrees: %v", block.String(), block.Subtrees)

	// Process block subtrees and separate chained subtrees
	blockSubtreesMap, chainedSubtrees := stp.processBlockSubtrees(block)

	var conflictingNodes []chainhash.Hash

	// Create transaction map from remaining block subtrees
	transactionMap, conflictingNodes, err = stp.createTransactionMapIfNeeded(ctx, block, blockSubtreesMap)
	if err != nil {
		return nil, err
	}

	// Process conflicting transactions
	losingTxHashesMap, err := stp.processConflictingTransactions(ctx, block, conflictingNodes, processedConflictingHashesMap)
	if err != nil {
		return nil, err
	}

	originalCurrentSubtree := stp.currentSubtree
	originalCurrentTxMap := stp.currentTxMap

	// Reset subtree state
	if err = stp.resetSubtreeState(createProperlySizedSubtrees); err != nil {
		return nil, errors.NewProcessingError("[moveForwardBlock][%s] error resetting subtree state", block.String(), err)
	}

	// Process remainder transactions and dequeueDuringBlockMovement
	err = stp.processRemainderTransactionsAndDequeue(ctx, &RemainderTransactionParams{
		Block:             block,
		ChainedSubtrees:   chainedSubtrees,
		CurrentSubtree:    originalCurrentSubtree,
		TransactionMap:    transactionMap,
		LosingTxHashesMap: losingTxHashesMap,
		CurrentTxMap:      originalCurrentTxMap,
		SkipDequeue:       skipDequeue,
		SkipNotification:  skipNotification,
	})
	if err != nil {
		return nil, err
	}

	// create the coinbase after processing all other transaction operations
	if err = stp.processCoinbaseUtxos(ctx, block); err != nil {
		return nil, errors.NewProcessingError("[moveForwardBlock][%s] error processing coinbase utxos", block.String(), err)
	}

	// Log memory stats after block processing if debug logging is enabled
	if stp.logger.LogLevel() <= 0 { // 0 is DEBUG level
		var memStats runtime.MemStats
		runtime.ReadMemStats(&memStats)
		stp.logger.Debugf("Memory after moveForwardBlock complete: Alloc=%d MB, TotalAlloc=%d MB, Sys=%d MB, NumGC=%d",
			memStats.Alloc/(1024*1024), memStats.TotalAlloc/(1024*1024),
			memStats.Sys/(1024*1024), memStats.NumGC)

		// Force garbage collection for large blocks if memory usage is high
		// This helps ensure transaction maps are cleaned up promptly
		if block.TransactionCount > 100000 && memStats.Alloc > 1024*1024*1024 { // Over 1GB allocated
			stp.logger.Debugf("Forcing GC after processing large block with %d transactions", block.TransactionCount)
			runtime.GC()
			runtime.ReadMemStats(&memStats)
			stp.logger.Debugf("Memory after GC: Alloc=%d MB", memStats.Alloc/(1024*1024))
		}
	}

	return transactionMap, nil
}

func (stp *SubtreeProcessor) waitForBlockBeingMined(ctx context.Context, blockHash *chainhash.Hash) (bool, error) {
	// try to wait for the block to be mined for maximum 30 sec
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return false, errors.NewProcessingError("[waitForBlockBeingMined] block not mined within 30 seconds", nil)
		default:
			blockMined, err := stp.blockchainClient.GetBlockIsMined(ctx, blockHash)
			if err != nil {
				return false, errors.NewProcessingError("[waitForBlockBeingMined] error getting block mined status", err)
			}

			if blockMined {
				return true, nil
			}

			stp.logger.Infof("[waitForBlockBeingMined] waiting for block %s to be mined", blockHash.String())
			time.Sleep(1 * time.Second)
		}
	}
}

// WaitForPendingBlocks waits for any pending blocks to be processed before loading unmined transactions.
// This method continuously polls the blockchain client to check if there are any blocks that have not been
// marked as mined yet. It will wait until the list of blocks returned by GetBlocksMinedNotSet is empty,
// indicating that all blocks have been processed and marked as mined.
//
// The method implements a polling loop with a 1-second interval and includes logging to provide visibility
// into the waiting process. This ensures that the BlockAssembly service doesn't start loading unmined
// transactions until all pending blocks have been fully processed.
//
// Parameters:
//   - ctx: Context for cancellation and timeout support
//
// Returns:
//   - error: Any error encountered during the waiting process or blockchain client calls
func (stp *SubtreeProcessor) WaitForPendingBlocks(ctx context.Context) error {
	_, _, deferFn := tracing.Tracer("SubtreeProcessor").Start(ctx, "WaitForPendingBlocks",
		tracing.WithParentStat(stp.stats),
		tracing.WithLogMessage(stp.logger, "[WaitForPendingBlocks] checking for pending blocks"),
	)
	defer deferFn()

	// Use retry utility with infinite retries until no pending blocks remain
	_, err := retry.Retry(ctx, stp.logger, func() (interface{}, error) {
		blockNotMined, err := stp.blockchainClient.GetBlocksMinedNotSet(ctx)
		if err != nil {
			return nil, errors.NewProcessingError("error getting blocks with mined not set", err)
		}

		if len(blockNotMined) == 0 {
			stp.logger.Infof("[WaitForPendingBlocks] no pending blocks found, ready to load unmined transactions")
			return nil, nil
		}

		for _, block := range blockNotMined {
			stp.logger.Debugf("[WaitForPendingBlocks] waiting for block %s to be processed, height %d, ID %d", block.Hash(), block.Height, block.ID)
		}

		// Return an error to trigger retry when blocks are still pending
		return nil, errors.NewProcessingError("waiting for %d blocks to be processed", len(blockNotMined))
	},
		retry.WithMessage("[WaitForPendingBlocks] blockchain service check"),
		retry.WithInfiniteRetry(),
		retry.WithExponentialBackoff(),
		retry.WithBackoffDurationType(1*time.Second),
		retry.WithBackoffFactor(2.0),
		retry.WithMaxBackoff(30*time.Second),
	)

	return err
}

// dequeueDuringBlockMovement processes the transaction queue during block movement.
//
// Parameters:
//   - transactionMap: Map of transactions that were in the block and need to be removed
//   - losingTxHashesMap: Map of transactions that were conflicting and need to be removed
//   - skipNotification: Whether to skip notification of new subtrees
//
// Returns:
//   - error: Any error encountered during processing
func (stp *SubtreeProcessor) dequeueDuringBlockMovement(transactionMap, losingTxHashesMap txmap.TxMap, skipNotification bool) (err error) {
	queueLength := stp.queue.length()
	if queueLength > 0 {
		nrProcessed := int64(0)
		validFromMillis := time.Now().Add(-1 * stp.settings.BlockAssembly.DoubleSpendWindow).UnixMilli()

		for {
			node, txInpoints, _, found := stp.queue.dequeue(validFromMillis)
			if !found {
				break
			}

			if (transactionMap == nil || !transactionMap.Exists(node.Hash)) && (losingTxHashesMap == nil || !losingTxHashesMap.Exists(node.Hash)) {
				_ = stp.addNode(node, &txInpoints, skipNotification)
			}

			nrProcessed++
			if nrProcessed > queueLength {
				break
			}
		}
	}

	return nil
}

// processCoinbaseUtxos processes UTXOs from coinbase transactions.
//
// Parameters:
//   - ctx: Context for cancellation
//   - block: Block containing the coinbase transaction
//
// Returns:
//   - error: Any error encountered during processing
func (stp *SubtreeProcessor) processCoinbaseUtxos(ctx context.Context, block *model.Block) error {
	startTime := time.Now()

	prometheusSubtreeProcessorProcessCoinbaseTx.Inc()

	if block == nil || block.CoinbaseTx == nil {
		log.Printf("********************************************* block or coinbase is nil")
		return nil
	}

	utxos, err := utxostore.GetUtxoHashes(block.CoinbaseTx)
	if err != nil {
		return errors.NewProcessingError("[SubtreeProcessor][coinbase:%s] error extracting coinbase utxos", block.CoinbaseTx.TxIDChainHash(), err)
	}

	for _, u := range utxos {
		stp.logger.Debugf("[SubtreeProcessor][coinbase:%s] store utxo: %s", block.CoinbaseTx.TxIDChainHash(), u.String())
	}

	blockHeight := block.Height
	if blockHeight <= 0 {
		// lookup the block height for the current block from the blockchain service, we cannot rely on the block height
		// that is set in the utxo store, since that is the height of the current block in block assembly, which might
		// not be the same
		blockHeight = stp.utxoStore.GetBlockHeight()
		if blockHeight == 0 {
			return errors.NewServiceError("[SubtreeProcessor][coinbase:%s] error extracting coinbase height via utxo store", block.CoinbaseTx.TxIDChainHash())
		}
	}

	stp.logger.Debugf("[SubtreeProcessor][%s] height %d storeCoinbaseTx %s blockID %d", block.Header.Hash().String(), blockHeight, block.CoinbaseTx.TxIDChainHash().String(), block.ID)
	// we pass in the block height we are working on here, since the utxo store will recognize the tx as
	// a coinbase and add the correct spending height, which should be + 99
	if _, err = stp.utxoStore.Create(
		ctx,
		block.CoinbaseTx,
		blockHeight,
		utxostore.WithMinedBlockInfo(
			utxostore.MinedBlockInfo{
				BlockID:     block.ID,
				BlockHeight: blockHeight,
				SubtreeIdx:  0, // Coinbase is always the first transaction in the first subtree
			}),
	); err != nil {
		if errors.Is(err, errors.ErrTxExists) {
			// This will also be called for the 2 coinbase transactions that are duplicated on the network
			// These transactions were created twice:
			//   e3bf3d07d4b0375638d5f1db5255fe07ba2c4cb067cd81b84ee974b6585fb468
			//   d5d27987d2a3dfc724e359870c6644b40e497bdc0589a033220fe15429d88599
			stp.logger.Warnf("[SubtreeProcessor] coinbase utxos for %s already exist. Skipping", block.CoinbaseTx.TxIDChainHash())
		} else {
			stp.logger.Errorf("[SubtreeProcessor] error storing utxos: %v", err)
			return err
		}
	}

	prometheusSubtreeProcessorProcessCoinbaseTxDuration.Observe(float64(time.Since(startTime).Microseconds()) / 1_000_000)

	return nil
}

// processRemainderTxHashes processes remaining transaction hashes after reorganization.
//
// Parameters:
//   - ctx: Context for cancellation
//   - chainedSubtrees: List of subtrees to process
//   - transactionMap: Map of transactions that were in the block and need to be removed
//   - losingTxHashesMap: Map of transactions that were conflicting and need to be removed
//   - skipNotification: Whether to skip notification of new subtrees
//
// Returns:
//   - error: Any error encountered during processing
func (stp *SubtreeProcessor) processRemainderTxHashes(ctx context.Context, chainedSubtrees []*subtreepkg.Subtree,
	transactionMap, losingTxHashesMap txmap.TxMap, currentTxMap *txmap.SyncedMap[chainhash.Hash, subtreepkg.TxInpoints], skipNotification bool) error {
	var hashCount atomic.Int64

	// clean out the transactions from the old current subtree that were in the block
	// and add the remainderSubtreeNodes to the new current subtree
	g, _ := errgroup.WithContext(ctx)
	util.SafeSetLimit(g, stp.settings.BlockAssembly.ProcessRemainderTxHashesConcurrency)

	// we need to process this in order, so we first process all subtrees in parallel, but keeping the order
	remainderSubtrees := make([][]subtreepkg.SubtreeNode, len(chainedSubtrees))
	removeMapLength := stp.removeMap.Length()

	for idx, subtree := range chainedSubtrees {
		idx := idx
		st := subtree

		g.Go(func() error {
			remainderSubtrees[idx] = make([]subtreepkg.SubtreeNode, 0, len(st.Nodes)/10) // expect max 10% of the nodes to be different
			// don't use the util function, keep the memory local in this function, no jumping between heap and stack
			// err = st.Difference(transactionMap, &remainderSubtrees[idx])
			for _, node := range st.Nodes {
				if !node.Hash.Equal(*subtreepkg.CoinbasePlaceholderHash) {
					if removeMapLength > 0 && stp.removeMap.Exists(node.Hash) {
						if err := stp.removeMap.Delete(node.Hash); err != nil {
							stp.logger.Errorf("[SubtreeProcessor] error removing tx from remove map: %s", err.Error())
						}
					} else {
						exists := transactionMap.Exists(node.Hash)

						if exists {
							// we found a transaction that was in the block, mark it as processed in the transaction map
							if err := transactionMap.Set(node.Hash, 1); err != nil {
								return errors.NewProcessingError("error marking tx hash as processed in transaction map: %s", node.Hash.String(), err)
							}
						}

						if !exists && (losingTxHashesMap == nil || !losingTxHashesMap.Exists(node.Hash)) {
							remainderSubtrees[idx] = append(remainderSubtrees[idx], node)
						}
					}
				}
			}

			hashCount.Add(int64(len(remainderSubtrees[idx])))

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return errors.NewProcessingError("error getting remainder tx difference", err)
	}

	// add all found tx hashes to the final list, in order
	for _, subtreeNodes := range remainderSubtrees {
		for _, node := range subtreeNodes {
			parents, ok := currentTxMap.Get(node.Hash)
			if !ok {
				return errors.NewProcessingError("[processRemainderTxHashes] error getting node txInpoints from currentTxMap for %s", node.Hash.String())
			}

			_ = stp.addNode(node, &parents, skipNotification)
		}
	}

	return nil
}

// CreateTransactionMap creates a map of transactions from the provided subtrees.
//
// Parameters:
//   - ctx: Context for cancellation
//   - blockSubtreesMap: Map of subtree hashes to their indices
//   - totalSubtreesInBlock: Total number of subtrees in the block
//   - estimatedTxCount: Estimated transaction count from block (0 if unknown)
//
// Returns:
//   - util.TxMap: Created transaction map
//   - error: Any error encountered during map creation
func (stp *SubtreeProcessor) CreateTransactionMap(ctx context.Context, blockSubtreesMap map[chainhash.Hash]int,
	totalSubtreesInBlock int, estimatedTxCount uint64) (txmap.TxMap, []chainhash.Hash, error) {
	startTime := time.Now()

	prometheusSubtreeProcessorCreateTransactionMap.Inc()

	concurrentSubtreeReads := stp.settings.BlockAssembly.SubtreeProcessorConcurrentReads

	// Log memory stats before allocation if debug logging is enabled
	var memStatsBefore runtime.MemStats
	if stp.logger.LogLevel() <= 0 { // 0 is DEBUG level
		runtime.ReadMemStats(&memStatsBefore)
		stp.logger.Debugf("Memory before CreateTransactionMap: Alloc=%d MB, TotalAlloc=%d MB, Sys=%d MB",
			memStatsBefore.Alloc/(1024*1024), memStatsBefore.TotalAlloc/(1024*1024), memStatsBefore.Sys/(1024*1024))
	}

	stp.logger.Infof("CreateTransactionMap with %d subtrees, concurrency %d, estimated tx count %d",
		len(blockSubtreesMap), concurrentSubtreeReads, estimatedTxCount)

	// Calculate map size based on actual transaction count if available, otherwise use estimation
	var mapSize int
	if estimatedTxCount > 0 {
		// Add 10% buffer for hash collisions and growth
		mapSize = int(float64(estimatedTxCount) * 1.1)
	} else {
		// Fallback to old calculation but with more reasonable estimate
		// Average transactions per subtree is typically lower than max capacity
		avgTxPerSubtree := stp.currentItemsPerFile * 3 / 4 // Assume 75% fill rate
		mapSize = len(blockSubtreesMap) * avgTxPerSubtree
	}

	stp.logger.Debugf("Allocating transaction map with size: %d", mapSize)
	transactionMap := txmap.NewSplitSwissMap(mapSize)

	conflictingNodesPerSubtree := make([][]chainhash.Hash, totalSubtreesInBlock)

	g, ctx := errgroup.WithContext(ctx)
	util.SafeSetLimit(g, concurrentSubtreeReads)

	// get all the subtrees from the block that we have not yet cleaned out
	for subtreeHash, subtreeIdx := range blockSubtreesMap {
		st := subtreeHash

		g.Go(func() error {
			stp.logger.Debugf("getting subtree: %s", st.String())

			subtreeReader, err := stp.subtreeStore.GetIoReader(ctx, st[:], fileformat.FileTypeSubtree)
			if err != nil {
				subtreeReader, err = stp.subtreeStore.GetIoReader(ctx, st[:], fileformat.FileTypeSubtreeToCheck)
				if err != nil {
					return errors.NewServiceError("error getting subtree: %s", st.String(), err)
				}
			}

			defer func() {
				_ = subtreeReader.Close()
			}()

			// TODO add metrics about how many txs we are reading per second
			txHashBuckets, conflictingNodes, err := DeserializeHashesFromReaderIntoBuckets(subtreeReader, transactionMap.Buckets())
			if err != nil {
				return errors.NewProcessingError("error deserializing subtree: %s", st.String(), err)
			}

			bucketG := errgroup.Group{}

			for bucket, hashes := range txHashBuckets {
				bucket := bucket
				hashes := hashes
				// put the hashes into the transaction map in parallel, it has already been split into the correct buckets
				bucketG.Go(func() error {
					return transactionMap.PutMultiBucket(bucket, hashes, 0)
				})
			}

			if err = bucketG.Wait(); err != nil {
				return errors.NewProcessingError("error putting hashes into transaction map", err)
			}

			conflictingNodesPerSubtree[subtreeIdx] = conflictingNodes

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, errors.NewProcessingError("error getting subtrees", err)
	}

	conflictingNodesPerSubtreeCount := 0
	for _, subtreeConflictingNodes := range conflictingNodesPerSubtree {
		conflictingNodesPerSubtreeCount += len(subtreeConflictingNodes)
	}

	conflictingNodes := make([]chainhash.Hash, 0, conflictingNodesPerSubtreeCount)

	for _, subtreeConflictingNodes := range conflictingNodesPerSubtree {
		if subtreeConflictingNodes != nil {
			conflictingNodes = append(conflictingNodes, subtreeConflictingNodes...)
		}
	}

	stp.logger.Infof("CreateTransactionMap with %d subtrees DONE, map has %d entries", len(blockSubtreesMap), transactionMap.Length())

	// Log memory stats after allocation if debug logging is enabled
	if stp.logger.LogLevel() <= 0 { // 0 is DEBUG level
		var memStatsAfter runtime.MemStats
		runtime.ReadMemStats(&memStatsAfter)
		memDelta := memStatsAfter.Alloc - memStatsBefore.Alloc
		stp.logger.Debugf("Memory after CreateTransactionMap: Alloc=%d MB (delta=%d MB), TotalAlloc=%d MB, Sys=%d MB",
			memStatsAfter.Alloc/(1024*1024), memDelta/(1024*1024),
			memStatsAfter.TotalAlloc/(1024*1024), memStatsAfter.Sys/(1024*1024))
	}

	prometheusSubtreeProcessorCreateTransactionMapDuration.Observe(float64(time.Since(startTime).Microseconds()) / 1_000_000)

	return transactionMap, conflictingNodes, nil
}

func (stp *SubtreeProcessor) markConflictingTxsInSubtrees(ctx context.Context, losingTxHashesMap txmap.TxMap) error {
	if losingTxHashesMap == nil || losingTxHashesMap.Length() == 0 {
		return nil
	}

	blockIdsMap, err := stp.getBLockIDsMap(ctx, losingTxHashesMap)
	if err != nil {
		return err
	}

	g, gCtx := errgroup.WithContext(ctx)
	util.SafeSetLimit(g, stp.settings.BlockAssembly.MoveBackBlockConcurrency)

	// mark all the losing txs in the subtrees in the blocks they were mined into as conflicting
	for blockID, txHashes := range blockIdsMap {
		// get the block
		block, err := stp.blockchainClient.GetBlockByID(ctx, uint64(blockID))
		if err != nil {
			return errors.NewServiceError("error getting block %d", blockID)
		}

		// get the subtrees
		for _, subtreeHash := range block.Subtrees {
			subtreeHash := subtreeHash

			g.Go(func() error {
				subtree, conflictingTransactionsMap, err := stp.getSubtreeAndConflictingTransactionsMap(ctx, subtreeHash, txHashes)
				if err != nil {
					return err
				}

				// if we marked at least 1 node as conflicting, we should save the subtree again
				if len(conflictingTransactionsMap) > 0 {
					// create a slice of the conflicting nodes
					conflictingNodes := make([]chainhash.Hash, 0, len(conflictingTransactionsMap))
					for txHash := range conflictingTransactionsMap {
						conflictingNodes = append(conflictingNodes, txHash)
					}

					// sort the conflicting nodes by their index in the subtree
					slices.SortFunc(conflictingNodes, func(i, j chainhash.Hash) int {
						return conflictingTransactionsMap[i] - conflictingTransactionsMap[j]
					})

					// mark the transaction as conflicting in order of idx
					for _, txHash := range conflictingNodes {
						if err = subtree.AddConflictingNode(txHash); err != nil {
							return errors.NewProcessingError("error adding conflicting node %s to subtree %s", txHash.String(), subtreeHash.String(), err)
						}
					}

					subtreeBytes, err := subtree.Serialize()
					if err != nil {
						return errors.NewProcessingError("error serializing subtree %s", subtreeHash.String(), err)
					}

					if err = stp.subtreeStore.Set(gCtx,
						subtreeHash[:],
						fileformat.FileTypeSubtree,
						subtreeBytes,
						options.WithAllowOverwrite(true),
					); err != nil {
						return errors.NewServiceError("error saving subtree %s", subtreeHash.String(), err)
					}
				}

				return nil
			})
		}
	}

	if err = g.Wait(); err != nil {
		return errors.NewProcessingError("error marking conflicting txs in subtrees", err)
	}

	return nil
}

func (stp *SubtreeProcessor) getSubtreeAndConflictingTransactionsMap(ctx context.Context, subtreeHash *chainhash.Hash, txHashes []chainhash.Hash) (*subtreepkg.Subtree, map[chainhash.Hash]int, error) {
	subtreeReader, err := stp.subtreeStore.GetIoReader(ctx, subtreeHash[:], fileformat.FileTypeSubtree)
	if err != nil {
		subtreeReader, err = stp.subtreeStore.GetIoReader(ctx, subtreeHash[:], fileformat.FileTypeSubtreeToCheck)
		if err != nil {
			return nil, nil, errors.NewServiceError("error getting subtree %s", subtreeHash.String())
		}
	}

	subtree := &subtreepkg.Subtree{}
	if err = subtree.DeserializeFromReader(subtreeReader); err != nil {
		return nil, nil, errors.NewProcessingError("error deserializing subtree %s", subtreeHash.String(), err)
	}

	conflictingTransactionsMap := make(map[chainhash.Hash]int, len(txHashes))

	for _, txHash := range txHashes {
		idx := subtree.NodeIndex(txHash)
		if idx >= 0 {
			conflictingTransactionsMap[txHash] = idx
		}
	}

	return subtree, conflictingTransactionsMap, nil
}

func (stp *SubtreeProcessor) getBLockIDsMap(ctx context.Context, losingTxHashesMap txmap.TxMap) (map[uint32][]chainhash.Hash, error) {
	// get all the blocks these transactions were mined into
	blockIdsMap := make(map[uint32][]chainhash.Hash)
	blockIdsMapMu := sync.Mutex{}

	g, gCtx := errgroup.WithContext(ctx)

	for _, txHash := range losingTxHashesMap.Keys() {
		txHash := txHash

		g.Go(func() error {
			txMeta, err := stp.utxoStore.Get(gCtx, &txHash, fields.BlockIDs)
			if err != nil {
				return errors.NewServiceError("error getting utxos for tx %s", txHash.String())
			}

			blockIdsMapMu.Lock()

			for _, blockID := range txMeta.BlockIDs {
				if _, ok := blockIdsMap[blockID]; !ok {
					blockIdsMap[blockID] = make([]chainhash.Hash, 0, 1024)
				}

				blockIdsMap[blockID] = append(blockIdsMap[blockID], txHash)
			}

			blockIdsMapMu.Unlock()

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, errors.NewProcessingError("error getting mined blocks for conflicting txs", err)
	}

	return blockIdsMap, nil
}

// DeserializeHashesFromReaderIntoBuckets deserializes transaction hashes from a reader into buckets.
//
// Parameters:
//   - reader: Source reader containing hash data
//   - nBuckets: Number of buckets to distribute hashes into
//
// Returns:
//   - map[uint16][][32]byte: Map of bucketed hash arrays
//   - error: Any error encountered during deserialization
func DeserializeHashesFromReaderIntoBuckets(reader io.Reader, nBuckets uint16) (hashes map[uint16][]chainhash.Hash, conflictingNodes []chainhash.Hash, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.NewProcessingError("recovered in DeserializeHashesFromReaderIntoBuckets: %v", r)
		}
	}()

	buf := bufio.NewReaderSize(reader, 1024*64)

	if _, err = buf.Discard(48); err != nil { // skip headers
		return nil, nil, errors.NewProcessingError("unable to read header", err)
	}

	// read number of leaves
	bytes8 := make([]byte, 8)
	if _, err = io.ReadFull(buf, bytes8); err != nil {
		return nil, nil, errors.NewProcessingError("unable to read number of leaves", err)
	}

	numLeaves := binary.LittleEndian.Uint64(bytes8)

	// read leaves
	hashes = make(map[uint16][]chainhash.Hash, nBuckets)
	// Calculate expected bucket size with better distribution
	expectedPerBucket := int(numLeaves / uint64(nBuckets))
	// Add 20% buffer for uneven distribution, minimum 16 elements
	bucketCapacity := expectedPerBucket + expectedPerBucket/5
	if bucketCapacity < 16 {
		bucketCapacity = 16
	}

	for i := uint16(0); i < nBuckets; i++ {
		hashes[i] = make([]chainhash.Hash, 0, bucketCapacity)
	}

	var bucket uint16

	bytes48 := make([]byte, 48)

	for i := uint64(0); i < numLeaves; i++ {
		// read all the node data in 1 go
		if _, err = io.ReadFull(buf, bytes48); err != nil {
			return nil, nil, errors.NewProcessingError("unable to read node", err)
		}

		bucket = txmap.Bytes2Uint16Buckets(chainhash.Hash(bytes48[:32]), nBuckets)
		hashes[bucket] = append(hashes[bucket], chainhash.Hash(bytes48[:32]))
	}

	// read conflicting txs
	if _, err = io.ReadFull(buf, bytes8); err != nil {
		return nil, nil, errors.NewProcessingError("unable to read number of conflicting txs", err)
	}

	numConflicting := binary.LittleEndian.Uint64(bytes8)

	// Pre-allocate exact size for conflicting nodes to avoid reallocation
	if numConflicting > 0 {
		conflictingNodes = make([]chainhash.Hash, 0, numConflicting)
		bytes32 := make([]byte, 32)
		for i := uint64(0); i < numConflicting; i++ {
			if _, err = io.ReadFull(buf, bytes32); err != nil {
				return nil, nil, errors.NewProcessingError("unable to read node", err)
			}

			conflictingNodes = append(conflictingNodes, chainhash.Hash(bytes32))
		}
	} else {
		conflictingNodes = make([]chainhash.Hash, 0)
	}

	return hashes, conflictingNodes, nil
}

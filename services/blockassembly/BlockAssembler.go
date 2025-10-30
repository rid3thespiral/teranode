// Package blockassembly provides functionality for assembling Bitcoin blocks in Teranode.
package blockassembly

import (
	"context"
	"database/sql"
	"encoding/binary"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	safeconversion "github.com/bsv-blockchain/go-safe-conversion"
	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockassembly/subtreeprocessor"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob"
	"github.com/bsv-blockchain/teranode/stores/cleanup"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/tracing"
	"github.com/ordishs/go-utils"
	"github.com/ordishs/gocore"
)

// miningCandidateResponse encapsulates all data needed for a mining candidate response.
type miningCandidateResponse struct {
	// miningCandidate contains the block template for mining
	miningCandidate *model.MiningCandidate

	// subtrees contains all transaction subtrees included in the candidate
	subtrees []*subtree.Subtree

	// err contains any error encountered during candidate creation
	err error
}

type State uint32

const (
	// pendingBlocksPollInterval is the interval at which the block assembler
	// polls for pending blocks during startup
	pendingBlocksPollInterval = 1 * time.Second
)

// create state strings for the processor
var (
	// StateStarting indicates the processor is starting up
	StateStarting State = 0

	// StateRunning indicates the processor is actively processing
	StateRunning State = 1

	// StateResetting indicates the processor is resetting
	StateResetting State = 2

	// StateGetMiningCandidate indicates the processor is getting a mining candidate
	StateGetMiningCandidate State = 3

	// StateBlockchainSubscription indicates the processor is receiving blockchain notifications
	StateBlockchainSubscription State = 4

	// StateReorging indicates the processor is reorging the blockchain
	StateReorging State = 5

	// StateMovingUp indicates the processor is moving up the blockchain
	StateMovingUp State = 6
)

var StateStrings = map[State]string{
	StateStarting:               "starting",
	StateRunning:                "running",
	StateResetting:              "resetting",
	StateGetMiningCandidate:     "getMiningCandidate",
	StateBlockchainSubscription: "blockchainSubscription",
	StateReorging:               "reorging",
	StateMovingUp:               "movingUp",
}

// BlockAssembler manages the assembly of new blocks and coordinates mining operations.
// It is the central component responsible for transaction selection, block candidate
// generation, and interaction with the mining system.
//
// BlockAssembler maintains the current blockchain state, manages transaction queues,
// and coordinates with the subtree processor to organize transactions efficiently.
// It handles blockchain reorganizations and ensures that block templates remain valid
// as the chain state changes.
type BlockAssembler struct {
	// logger provides logging functionality for the assembler
	logger ulogger.Logger

	// stats tracks operational statistics for monitoring and debugging
	stats *gocore.Stat

	// settings contains configuration parameters for block assembly
	settings *settings.Settings

	// utxoStore manages the UTXO set storage and retrieval
	utxoStore utxo.Store

	// subtreeStore manages persistent storage of transaction subtrees
	subtreeStore blob.Store

	// blockchainClient interfaces with the blockchain for network operations
	blockchainClient blockchain.ClientI

	// subtreeProcessor handles the processing and organization of transaction subtrees
	subtreeProcessor subtreeprocessor.Interface

	// miningCandidateCh coordinates requests for mining candidates
	miningCandidateCh chan chan *miningCandidateResponse

	// bestBlock atomically stores the current best block header and height together
	bestBlock atomic.Pointer[BestBlockInfo]

	// stateChangeCh notifies listeners of state changes
	stateChangeCh chan BestBlockInfo

	// lastPersistedHeight tracks the last block height processed by block persister
	// This is updated via BlockPersisted notifications and used to coordinate with cleanup
	lastPersistedHeight atomic.Uint32

	// currentChainMap maps block hashes to their heights
	currentChainMap map[chainhash.Hash]uint32

	// currentChainMapIDs tracks block IDs in the current chain
	currentChainMapIDs map[uint32]struct{}

	// currentChainMapMu protects access to chain maps
	currentChainMapMu sync.RWMutex

	// blockchainSubscriptionCh receives blockchain notifications
	blockchainSubscriptionCh chan *blockchain.Notification

	// defaultMiningNBits stores the default mining difficulty
	defaultMiningNBits *model.NBit

	// resetCh handles reset requests for the assembler
	resetCh chan resetRequest

	// currentRunningState tracks the current operational state
	currentRunningState atomic.Value

	// cleanupService manages background cleanup tasks
	cleanupService cleanup.Service

	// cleanupServiceLoaded indicates if the cleanup service has been loaded
	cleanupServiceLoaded atomic.Bool

	// cleanupQueueCh queues cleanup operations (parent preserve + DAH cleanup) to prevent flooding during catchup
	cleanupQueueCh chan uint32

	// cleanupQueueWorkerStarted tracks if the cleanup queue worker is running
	cleanupQueueWorkerStarted atomic.Bool

	// unminedCleanupTicker manages periodic cleanup of old unmined transactions
	unminedCleanupTicker *time.Ticker
	// cachedCandidate stores the cached mining candidate
	cachedCandidate *CachedMiningCandidate

	// skipWaitForPendingBlocks allows tests to skip waiting for pending blocks during startup
	skipWaitForPendingBlocks bool

	// unminedTransactionsLoading indicates if unmined transactions are currently being loaded
	unminedTransactionsLoading atomic.Bool
}

// BestBlockInfo holds both the block header and height atomically
type BestBlockInfo struct {
	Header *model.BlockHeader
	Height uint32
}

type blockHeaderWithMeta struct {
	header *model.BlockHeader
	meta   *model.BlockHeaderMeta
}

type blockWithMeta struct {
	block *model.Block
	meta  *model.BlockHeaderMeta
}

// CachedMiningCandidate holds a cached mining candidate with expiration
type CachedMiningCandidate struct {
	mu             sync.RWMutex
	candidate      *model.MiningCandidate
	subtrees       []*subtree.Subtree
	lastHeight     uint32
	lastUpdate     time.Time
	generating     bool
	generationChan chan struct{}
}

// NewBlockAssembler creates and initializes a new BlockAssembler instance.
//
// Parameters:
//   - ctx: Context for cancellation
//   - logger: Logger for recording operations
//   - tSettings: Teranode settings configuration
//   - stats: Statistics tracking instance
//   - utxoStore: UTXO set storage
//   - subtreeStore: Subtree storage
//   - blockchainClient: Interface to blockchain operations
//   - newSubtreeChan: Channel for new subtree notifications
//
// Returns:
//   - *BlockAssembler: New block assembler instance
func NewBlockAssembler(ctx context.Context, logger ulogger.Logger, tSettings *settings.Settings, stats *gocore.Stat, utxoStore utxo.Store,
	subtreeStore blob.Store, blockchainClient blockchain.ClientI, newSubtreeChan chan subtreeprocessor.NewSubtreeRequest) (*BlockAssembler, error) {
	bytesLittleEndian := make([]byte, 4)

	if tSettings.ChainCfgParams == nil {
		return nil, errors.NewError("chain cfg params are nil")
	}

	binary.LittleEndian.PutUint32(bytesLittleEndian, tSettings.ChainCfgParams.PowLimitBits)

	defaultMiningBits, err := model.NewNBitFromSlice(bytesLittleEndian)
	if err != nil {
		return nil, err
	}

	subtreeProcessor, err := subtreeprocessor.NewSubtreeProcessor(ctx, logger, tSettings, subtreeStore, blockchainClient, utxoStore, newSubtreeChan)
	if err != nil {
		return nil, err
	}

	b := &BlockAssembler{
		logger:              logger,
		stats:               stats.NewStat("BlockAssembler"),
		settings:            tSettings,
		utxoStore:           utxoStore,
		subtreeStore:        subtreeStore,
		blockchainClient:    blockchainClient,
		subtreeProcessor:    subtreeProcessor,
		miningCandidateCh:   make(chan chan *miningCandidateResponse),
		currentChainMap:     make(map[chainhash.Hash]uint32, tSettings.BlockAssembly.MaxBlockReorgCatchup),
		currentChainMapIDs:  make(map[uint32]struct{}, tSettings.BlockAssembly.MaxBlockReorgCatchup),
		defaultMiningNBits:  defaultMiningBits,
		resetCh:             make(chan resetRequest, 2),
		currentRunningState: atomic.Value{},
		cachedCandidate:     &CachedMiningCandidate{},
	}

	b.setCurrentRunningState(StateStarting)

	return b, nil
}

// TxCount returns the total number of transactions in the assembler.
//
// Returns:
//   - uint64: Total transaction count
func (b *BlockAssembler) TxCount() uint64 {
	return b.subtreeProcessor.TxCount()
}

// QueueLength returns the current length of the transaction queue.
//
// Returns:
//   - int64: Current queue length
func (b *BlockAssembler) QueueLength() int64 {
	return b.subtreeProcessor.QueueLength()
}

// SubtreeCount returns the total number of subtrees.
//
// Returns:
//   - int: Total number of subtrees
func (b *BlockAssembler) SubtreeCount() int {
	return b.subtreeProcessor.SubtreeCount()
}

// startChannelListeners initializes and starts all channel listeners for block assembly operations.
// It handles blockchain notifications, mining candidate requests, and reset operations.
//
// Parameters:
//   - ctx: Context for cancellation
func (b *BlockAssembler) startChannelListeners(ctx context.Context) (err error) {
	// start a subscription for the best block header and the FSM state
	// this will be used to reset the subtree processor when a new block is mined
	b.blockchainSubscriptionCh, err = b.blockchainClient.Subscribe(ctx, "BlockAssembler")
	if err != nil {
		return errors.NewProcessingError("[BlockAssembler] error subscribing to blockchain notifications: %v", err)
	}

	go func() {
		// variables are defined here to prevent unnecessary allocations
		b.setCurrentRunningState(StateRunning)

		for {
			select {
			case <-ctx.Done():
				b.logger.Infof("Stopping blockassembler as ctx is done")
				close(b.miningCandidateCh)
				// Note: We don't close blockchainSubscriptionCh here because we don't own it -
				// it's created by the blockchain client's Subscribe method
				return

			case resetReq := <-b.resetCh:
				b.setCurrentRunningState(StateResetting)

				err := b.reset(ctx, resetReq.FullReset)

				// empty out the reset channel
				for len(b.resetCh) > 0 {
					bufferedCh := <-b.resetCh
					if bufferedCh.ErrCh != nil {
						bufferedCh.ErrCh <- nil
					}
				}

				if resetReq.ErrCh != nil {
					resetReq.ErrCh <- err
				}

				b.setCurrentRunningState(StateRunning)

			case responseCh := <-b.miningCandidateCh:
				b.setCurrentRunningState(StateGetMiningCandidate)
				// start, stat, _ := util.NewStatFromContext(context, "miningCandidateCh", channelStats)
				// wait for the reset to complete before getting a new mining candidate
				// 2 blocks && at least 20 minutes

				currentState, err := b.blockchainClient.GetFSMCurrentState(ctx)
				if err != nil {
					// TODO: how to handle it gracefully?
					b.logger.Errorf("[BlockAssembly] Failed to get current state: %s", err)
				}

				// if the current state is not running, we don't give a mining candidate
				if *currentState == blockchain.FSMStateRUNNING {
					miningCandidate, subtrees, err := b.getMiningCandidate()
					utils.SafeSend(responseCh, &miningCandidateResponse{
						miningCandidate: miningCandidate,
						subtrees:        subtrees,
						err:             err,
					})
				} else {
					utils.SafeSend(responseCh, &miningCandidateResponse{
						err: errors.NewProcessingError("blockchain is not in running state, current state: " + currentState.String()),
					})
				}

				// stat.AddTime(start)
				b.setCurrentRunningState(StateRunning)

			case notification := <-b.blockchainSubscriptionCh:
				b.setCurrentRunningState(StateBlockchainSubscription)

				if notification.Type == model.NotificationType_Block {
					b.processNewBlockAnnouncement(ctx)
				} else if notification.Type == model.NotificationType_BlockPersisted {
					// RUNTIME COORDINATION: Update persisted height from block persister
					//
					// Block persister sends this notification after successfully persisting a block
					// and creating its .subtree_data file. We track this height so cleanup can safely
					// delete transactions from earlier blocks without breaking catchup.
					//
					// This notification-based update keeps our persisted height current during normal
					// operation. Combined with the startup initialization from blockchain state,
					// we always know how far block persister has progressed.
					//
					// Cleanup uses this via GetLastPersistedHeight() to calculate safe deletion height.
					if notification.Metadata != nil && notification.Metadata.Metadata != nil {
						if heightStr, ok := notification.Metadata.Metadata["height"]; ok {
							if height, err := strconv.ParseUint(heightStr, 10, 32); err == nil {
								b.lastPersistedHeight.Store(uint32(height))
								b.logger.Debugf("[BlockAssembler] Block persister progress: height %d", height)
							} else {
								b.logger.Warnf("[BlockAssembler] Failed to parse persisted height from notification: %v", err)
							}
						}
					}
				}

				b.setCurrentRunningState(StateRunning)
			} // select
		} // for
	}()

	return nil
}

// reset performs a full reset of the block assembler state by clearing all subtrees and reloading from blockchain.
//
// This is the "nuclear option" for handling blockchain reorganizations and is used when:
// 1. Large reorgs (>= CoinbaseMaturity blocks AND height > 1000) where incremental reorg is too expensive
// 2. Failed reorgs where subtreeProcessor.Reorg() encountered errors
// 3. Reorgs involving invalid blocks that require clean state
//
// The reset process:
// 1. Waits for BlockValidation background jobs to complete (WaitForPendingBlocks)
//   - Ensures all blocks have mined_set=true
//   - Invalid blocks: Already have block_ids removed, unmined_since set
//   - moveForward blocks: Already have unmined_since cleared (processed with onLongestChain=true)
//
// 2. Marks transactions from moveBackBlocks as NOT on longest chain (sets unmined_since)
//   - These blocks were on main chain but are now on side chain
//   - They still have mined_set=true (won't be re-processed by BlockValidation)
//   - Must explicitly mark their transactions as unmined
//
// 3. Calls subtreeProcessor.Reset() to clear all subtrees
//
// 4. Calls loadUnminedTransactions() which:
//   - Loads all transactions with unmined_since set into block assembly
//   - Fixes any data inconsistencies (transactions with block_ids on main but unmined_since incorrectly set)
//
// Key insight: BlockValidation handles moveForward and invalid blocks via background jobs.
// reset() only needs to handle moveBack blocks that won't be re-processed.
//
// Parameters:
//   - ctx: Context for cancellation
//   - fullScan: If true, loadUnminedTransactions will scan all records and fix inconsistencies
//     If false, uses index-based query for faster reload
//
// Returns:
//   - error: Any error encountered during reset
func (b *BlockAssembler) reset(ctx context.Context, fullScan bool) error {
	bestBlockchainBlockHeader, meta, err := b.blockchainClient.GetBestBlockHeader(ctx)
	if err != nil {
		return errors.NewProcessingError("[Reset] error getting best block header", err)
	}

	// reset the block assembly
	hash, h := b.CurrentBlock()
	b.logger.Warnf("[BlockAssembler][Reset] resetting: %d: %s -> %d: %s", h, hash.Hash(), meta.Height, bestBlockchainBlockHeader.String())

	moveBackBlocksWithMeta, moveForwardBlocksWithMeta, err := b.getReorgBlocks(ctx, bestBlockchainBlockHeader, meta.Height)
	if err != nil {
		return errors.NewProcessingError("[Reset] error getting reorg blocks", err)
	}

	isLegacySync, err := b.blockchainClient.IsFSMCurrentState(ctx, blockchain.FSMStateLEGACYSYNCING)
	if err != nil {
		b.logger.Errorf("[BlockAssembler][Reset] error getting FSM state: %v", err)

		// if we can't get the FSM state, we assume we are not in legacy sync, which is the default, but less optimized
		isLegacySync = false
	}

	currentHeight := meta.Height

	moveBackBlocks := make([]*model.Block, len(moveBackBlocksWithMeta))
	for i, withMeta := range moveBackBlocksWithMeta {
		moveBackBlocks[i] = withMeta.block
	}

	moveForwardBlocks := make([]*model.Block, len(moveForwardBlocksWithMeta))
	for i, withMeta := range moveForwardBlocksWithMeta {
		moveForwardBlocks[i] = withMeta.block
	}

	b.logger.Warnf("[BlockAssembler][Reset] resetting to new best block header: %d", meta.Height)

	// make sure we have processed all pending blocks before resetting
	if err = b.subtreeProcessor.WaitForPendingBlocks(ctx); err != nil {
		return errors.NewProcessingError("[Reset] error waiting for pending blocks", err)
	}

	// Mark moveBack transactions as unmined (set unmined_since)
	//
	// Division of Responsibility During Reorg:
	// - Invalid blocks: BlockValidation handles via background job (unsetMined=true removes block_ids, sets unmined_since)
	// - moveForward blocks (side→main): BlockValidation handles via background job (mined_set=false → processes with onLongestChain=true → clears unmined_since)
	// - moveBack blocks (main→side): reset() handles HERE (sets unmined_since)
	//
	// Why moveBack needs explicit handling:
	// - These blocks were on main chain (unmined_since=NULL, mined_set=true)
	// - Reorg moved them to side chain
	// - BlockValidation won't re-process them (mined_set still true, not in GetBlocksMinedNotSet queue)
	// - No background job will update them
	// - Must explicitly mark their transactions as unmined here
	//
	// Why we DON'T handle moveForward:
	// - moveForward blocks have mined_set=false (newly processed or re-validated)
	// - BlockValidation background job processes them
	// - Calls setTxMinedStatus with onLongestChain=CheckBlockIsInCurrentChain() = true
	// - unmined_since is automatically cleared
	// - No action needed from reset()
	if len(moveBackBlocksWithMeta) > 0 {
		// First, build a map of transactions in moveForward blocks
		// These are transactions that are ALSO in the new main chain (don't need unmined_since set)
		// Even though BlockValidation handles moveForward, we need this map to avoid marking
		// transactions that appear in BOTH moveBack and moveForward as unmined
		moveForwardTxMap := make(map[chainhash.Hash]bool)
		for _, blockWithMeta := range moveForwardBlocksWithMeta {
			if blockWithMeta.meta.Invalid {
				continue
			}

			block := blockWithMeta.block
			blockSubtrees, err := block.GetSubtrees(ctx, b.logger, b.subtreeStore, b.settings.Block.GetAndValidateSubtreesConcurrency)
			if err != nil {
				continue
			}

			for _, st := range blockSubtrees {
				for _, node := range st.Nodes {
					if !node.Hash.IsEqual(subtree.CoinbasePlaceholderHash) {
						moveForwardTxMap[node.Hash] = true
					}
				}
			}
		}

		// Now collect moveBack transactions, excluding those in moveForward
		// Net unmined = transactions ONLY in moveBack (not also in moveForward)
		moveBackTxs := make([]chainhash.Hash, 0, len(moveBackBlocksWithMeta)*100)

		for _, blockWithMeta := range moveBackBlocksWithMeta {
			if blockWithMeta.meta.Invalid {
				// Skip invalid blocks - BlockValidation already handled them via unsetMined=true
				continue
			}

			block := blockWithMeta.block
			blockSubtrees, err := block.GetSubtrees(ctx, b.logger, b.subtreeStore, b.settings.Block.GetAndValidateSubtreesConcurrency)
			if err != nil {
				b.logger.Warnf("[BlockAssembler][Reset] error getting subtrees for moveBack block %s: %v (will skip)", block.Hash().String(), err)
				continue
			}

			for _, st := range blockSubtrees {
				for _, node := range st.Nodes {
					if !node.Hash.IsEqual(subtree.CoinbasePlaceholderHash) {
						// Only add if NOT in moveForward (these are net unmined)
						if !moveForwardTxMap[node.Hash] {
							moveBackTxs = append(moveBackTxs, node.Hash)
						}
					}
				}
			}
		}

		// Mark net unmined transactions as NOT on longest chain (set unmined_since)
		if len(moveBackTxs) > 0 {
			if err = b.utxoStore.MarkTransactionsOnLongestChain(ctx, moveBackTxs, false); err != nil {
				b.logger.Errorf("[BlockAssembler][Reset] error marking moveBack transactions as unmined: %v", err)
			} else {
				b.logger.Infof("[BlockAssembler][Reset] marked %d net unmined transactions (moveBack minus moveForward)", len(moveBackTxs))
			}
		}
	}

	// define a post process function to be called after the reset is complete, but before we release the lock
	// in the for/select in the subtreeprocessor
	postProcessFn := func() error {
		// reload the unmined transactions
		if err = b.loadUnminedTransactions(ctx, fullScan); err != nil {
			return errors.NewProcessingError("[Reset] error loading unmined transactions", err)
		}

		return nil
	}

	baBestBlockHeader, _ := b.CurrentBlock()

	// TODO: Is this logic right?
	if response := b.subtreeProcessor.Reset(baBestBlockHeader, moveBackBlocks, moveForwardBlocks, isLegacySync, postProcessFn); response.Err != nil {
		b.logger.Errorf("[BlockAssembler][Reset] resetting error resetting subtree processor: %v", response.Err)
		// something went wrong, we need to set the best block header in the block assembly to be the
		// same as the subtree processor's best block header
		bestBlockchainBlockHeader = b.subtreeProcessor.GetCurrentBlockHeader()

		_, bestBlockchainBlockHeaderMeta, err := b.blockchainClient.GetBlockHeader(ctx, bestBlockchainBlockHeader.Hash())
		if err != nil {
			return errors.NewProcessingError("[Reset] error getting best block header meta", err)
		}

		// set the new height based on the best block header from the subtree processor
		currentHeight = bestBlockchainBlockHeaderMeta.Height
	}

	b.setBestBlockHeader(bestBlockchainBlockHeader, currentHeight)

	if err = b.SetState(ctx); err != nil {
		return errors.NewProcessingError("[Reset] error setting state", err)
	}

	_, height := b.CurrentBlock()
	prometheusBlockAssemblyCurrentBlockHeight.Set(float64(height))

	b.logger.Warnf("[BlockAssembler][Reset] resetting block assembler DONE")

	return nil
}

// processNewBlockAnnouncement updates the best block information.
//
// Parameters:
//   - ctx: Context for cancellation
func (b *BlockAssembler) processNewBlockAnnouncement(ctx context.Context) {
	_, _, deferFn := tracing.Tracer("blockassembly").Start(ctx, "processNewBlockAnnouncement",
		tracing.WithParentStat(b.stats),
		tracing.WithHistogram(prometheusBlockAssemblerUpdateBestBlock),
		tracing.WithLogMessage(b.logger, "[processNewBlockAnnouncement] called"),
	)
	defer func() {
		b.setCurrentRunningState(StateRunning)

		deferFn()
	}()

	bestBlockchainBlockHeader, bestBlockchainBlockHeaderMeta, err := b.blockchainClient.GetBestBlockHeader(ctx)
	if err != nil {
		b.logger.Errorf("[BlockAssembler] error getting best block header: %v", err)
		return
	}

	b.logger.Infof("[BlockAssembler][%s] new best block header: %d", bestBlockchainBlockHeader.Hash(), bestBlockchainBlockHeaderMeta.Height)

	defer b.logger.Infof("[BlockAssembler][%s] new best block header: %d DONE", bestBlockchainBlockHeader.Hash(), bestBlockchainBlockHeaderMeta.Height)

	prometheusBlockAssemblyBestBlockHeight.Set(float64(bestBlockchainBlockHeaderMeta.Height))

	bestBlockAccordingToBlockAssembly, bestBlockAccordingToBlockAssemblyHeight := b.CurrentBlock()
	bestBlockAccordingToBlockchain := bestBlockchainBlockHeader

	b.logger.Debugf("[BlockAssembler] best block header according to blockchain: %d: %s", bestBlockchainBlockHeaderMeta.Height, bestBlockAccordingToBlockchain.Hash())
	b.logger.Debugf("[BlockAssembler] best block header according to block assembly : %d: %s", bestBlockAccordingToBlockAssemblyHeight, bestBlockAccordingToBlockAssembly.Hash())

	switch {
	case bestBlockAccordingToBlockchain.Hash().IsEqual(bestBlockAccordingToBlockAssembly.Hash()):
		b.logger.Infof("[BlockAssembler][%s] best block header is the same as the current best block header: %s", bestBlockchainBlockHeader.Hash(), bestBlockAccordingToBlockAssembly.Hash())
		return
	case !bestBlockchainBlockHeader.HashPrevBlock.IsEqual(bestBlockAccordingToBlockAssembly.Hash()):
		b.logger.Infof("[BlockAssembler][%s] best block header is not the same as the previous best block header, reorging: %s", bestBlockchainBlockHeader.Hash(), bestBlockAccordingToBlockAssembly.Hash())
		b.setCurrentRunningState(StateReorging)

		err = b.handleReorg(ctx, bestBlockchainBlockHeader, bestBlockchainBlockHeaderMeta.Height)
		if err != nil {
			if errors.Is(err, errors.ErrBlockAssemblyReset) {
				// only warn about the reset
				b.logger.Warnf("[BlockAssembler][%s] error handling reorg: %v", bestBlockchainBlockHeader.Hash(), err)
			} else {
				b.logger.Errorf("[BlockAssembler][%s] error handling reorg: %v", bestBlockchainBlockHeader.Hash(), err)
			}

			return
		}
	default:
		b.logger.Infof("[BlockAssembler][%s] best block header is the same as the previous best block header, moving up: %s", bestBlockchainBlockHeader.Hash(), bestBlockAccordingToBlockAssembly.Hash())

		var block *model.Block

		if block, err = b.blockchainClient.GetBlock(ctx, bestBlockchainBlockHeader.Hash()); err != nil {
			b.logger.Errorf("[BlockAssembler][%s] error getting block from blockchain: %v", bestBlockchainBlockHeader.Hash(), err)
			return
		}

		b.setCurrentRunningState(StateMovingUp)

		if err = b.subtreeProcessor.MoveForwardBlock(block); err != nil {
			b.logger.Errorf("[BlockAssembler][%s] error moveForwardBlock in subtree processor: %v", bestBlockchainBlockHeader.Hash(), err)
			return
		}
	}

	b.setBestBlockHeader(bestBlockchainBlockHeader, bestBlockchainBlockHeaderMeta.Height)

	_, height := b.CurrentBlock()
	prometheusBlockAssemblyCurrentBlockHeight.Set(float64(height))

	if err = b.SetState(ctx); err != nil && !errors.Is(err, context.Canceled) {
		b.logger.Errorf("[BlockAssembler][%s] error setting state: %v", bestBlockchainBlockHeader.Hash(), err)
	}
}

// setBestBlockHeader updates the internal best block header and height atomically.
// This method is used internally to maintain the current blockchain state within
// the block assembler. It updates both the best block header and height in a
// thread-safe manner using a single atomic operation.
//
// The function performs the following operations:
// - Logs the update operation with block hash and Height
// - Atomically stores the new best block info (Header + height together)
//
// This method is critical for maintaining consistency between the block assembler's
// view of the blockchain state and the actual blockchain tip.
//
// Parameters:
//   - bestBlockchainBlockHeader: The new best block header to set
//   - Height: The height of the new best block
func (b *BlockAssembler) setBestBlockHeader(bestBlockchainBlockHeader *model.BlockHeader, height uint32) {
	b.logger.Infof("[BlockAssembler][%s] setting best block header to height %d", bestBlockchainBlockHeader.Hash(), height)

	b.bestBlock.Store(&BestBlockInfo{
		Header: bestBlockchainBlockHeader,
		Height: height,
	})

	if b.stateChangeCh != nil {
		b.stateChangeCh <- BestBlockInfo{
			Header: bestBlockchainBlockHeader,
			Height: height,
		}
	}

	// Invalidate cache when block height changes
	b.invalidateMiningCandidateCache()

	// Queue cleanup operations to prevent flooding during catchup
	// The cleanup queue worker processes operations sequentially (parent preserve → DAH cleanup)
	// Capture channel reference to avoid race condition if worker restarts
	ch := b.cleanupQueueCh
	if b.utxoStore != nil && b.cleanupServiceLoaded.Load() && b.cleanupService != nil && height > 0 && ch != nil {
		// Non-blocking send - drop if queue is full (shouldn't happen with 100 buffer, but safety check)
		select {
		case ch <- height:
			// Successfully queued
		default:
			b.logger.Warnf("[BlockAssembler] cleanup queue full, dropping cleanup for height %d", height)
		}
	}
}

// setCurrentRunningState sets the current operational state.
//
// Parameters:
//   - state: New state to set
func (b *BlockAssembler) setCurrentRunningState(state State) {
	b.currentRunningState.Store(state)
	prometheusBlockAssemblerCurrentState.Set(float64(state))
}

// GetCurrentRunningState returns the current operational state.
//
// Returns:
//   - string: Current state description
func (b *BlockAssembler) GetCurrentRunningState() State {
	return b.currentRunningState.Load().(State)
}

// GetLastPersistedHeight returns the last block height processed by block persister.
// This is used by cleanup service to avoid deleting transactions before they're persisted.
//
// Returns:
//   - uint32: Last persisted block height
func (b *BlockAssembler) GetLastPersistedHeight() uint32 {
	return b.lastPersistedHeight.Load()
}

// startCleanupQueueWorker starts a background worker that processes cleanup operations sequentially.
// This prevents flooding the system with concurrent cleanup operations during block catchup.
//
// The worker processes one block height at a time, running parent preserve followed by DAH cleanup.
// If multiple heights are queued, only the latest is processed (deduplication).
//
// Parameters:
//   - ctx: Context for cancellation
func (b *BlockAssembler) startCleanupQueueWorker(ctx context.Context) {
	// Only start once
	if !b.cleanupQueueWorkerStarted.CompareAndSwap(false, true) {
		return
	}

	// Initialize the cleanup queue channel with a buffer to handle bursts during catchup
	b.cleanupQueueCh = make(chan uint32, 100)

	go func() {
		defer func() {
			// Close the channel to signal no more work will be sent
			if b.cleanupQueueCh != nil {
				close(b.cleanupQueueCh)
				b.cleanupQueueCh = nil
			}
			b.cleanupQueueWorkerStarted.Store(false)
		}()

		for {
			select {
			case <-ctx.Done():
				b.logger.Infof("[BlockAssembler] cleanup queue worker stopping")
				return

			case height := <-b.cleanupQueueCh:
				// Deduplicate: drain any additional heights and only process the latest
				latestHeight := height
				drained := false
				for {
					select {
					case nextHeight := <-b.cleanupQueueCh:
						latestHeight = nextHeight
						drained = true
					default:
						// No more heights in queue
						if drained {
							b.logger.Debugf("[BlockAssembler] deduplicating cleanup operations, skipping to height %d", latestHeight)
						}
						goto processHeight
					}
				}

			processHeight:
				// Step 1: Preserve parents of old unmined transactions FIRST
				// This sets preserve_until and clears delete_at_height on parent transactions
				if b.utxoStore != nil {
					_, err := utxo.PreserveParentsOfOldUnminedTransactions(ctx, b.utxoStore, latestHeight, b.settings, b.logger)
					if err != nil {
						b.logger.Errorf("[BlockAssembler] error preserving parents during block height %d update: %v", latestHeight, err)
						continue
					}
				}

				// Step 2: Then trigger DAH cleanup and WAIT for it to complete
				// This ensures true sequential execution: preserve → cleanup (complete) → next height
				// Without waiting, the cleanup is queued but runs async, which could cause races
				if b.cleanupServiceLoaded.Load() && b.cleanupService != nil {
					// Create a channel to wait for completion
					doneCh := make(chan string, 1)

					if err := b.cleanupService.UpdateBlockHeight(latestHeight, doneCh); err != nil {
						b.logger.Errorf("[BlockAssembler] cleanup service error updating block height %d: %v", latestHeight, err)
						continue
					}

					// Wait for cleanup to complete or context cancellation
					select {
					case status := <-doneCh:
						if status != "completed" {
							b.logger.Warnf("[BlockAssembler] cleanup for height %d finished with status: %s", latestHeight, status)
						}
					case <-ctx.Done():
						b.logger.Infof("[BlockAssembler] context cancelled while waiting for cleanup at height %d", latestHeight)
						return
					}
				}
			}
		}
	}()

	b.logger.Infof("[BlockAssembler] cleanup queue worker started")
}

// Start initializes and begins the block assembler operations.
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns:
//   - error: Any error encountered during startup
func (b *BlockAssembler) Start(ctx context.Context) (err error) {
	if err = b.initState(ctx); err != nil {
		return errors.NewProcessingError("[BlockAssembler] failed to initialize state: %v", err)
	}

	// Wait for any pending blocks to be processed before loading unmined transactions
	if !b.skipWaitForPendingBlocks {
		if err = b.subtreeProcessor.WaitForPendingBlocks(ctx); err != nil {
			// we cannot start block assembly if we have not processed all pending blocks
			return errors.NewProcessingError("[BlockAssembler] failed to wait for pending blocks: %v", err)
		}
	}

	// Load unmined transactions (this includes cleanup of old unmined transactions first)
	if err = b.loadUnminedTransactions(ctx, false); err != nil {
		// we cannot start block assembly if we have not loaded unmined transactions successfully
		return errors.NewStorageError("[BlockAssembler] failed to load un-mined transactions: %v", err)
	}

	if err = b.startChannelListeners(ctx); err != nil {
		return errors.NewProcessingError("[BlockAssembler] failed to start channel listeners: %v", err)
	}

	// CRITICAL STARTUP COORDINATION: Initialize persisted height from block persister's state
	//
	// PROBLEM: Block persister creates .subtree_data files after a delay (BlockPersisterPersistAge blocks),
	// but cleanup deletes transactions based only on delete_at_height. If cleanup runs before block persister
	// has created .subtree_data files, those files will reference deleted transactions, causing catchup failures
	// with "subtree length does not match tx data length" errors (actually missing transactions).
	//
	// SOLUTION: Cleanup coordinates with block persister by limiting deletion to:
	//   max_cleanup_height = min(requested_cleanup_height, persisted_height + retention)
	//
	// STARTUP RACE: Block persister notifications arrive asynchronously after BlockAssembler starts.
	// If cleanup runs before the first notification arrives, it doesn't know the persisted height and
	// could delete transactions that block persister still needs.
	//
	// PREVENTION: Read block persister's last persisted height from blockchain state on startup.
	// Block persister publishes this state on its own startup, so we have the current value immediately.
	//
	// SCENARIOS:
	//   1. Block persister running: State available, cleanup immediately coordinates correctly
	//   2. Block persister not deployed: State missing, cleanup proceeds normally (height=0 disables coordination)
	//   3. Block persister hasn't started yet: State missing, will get notification soon, cleanup waits
	//   4. Block persister disabled: State missing, cleanup works without coordination
	//
	// All scenarios are safe. This prevents premature cleanup during the startup window.
	if state, err := b.blockchainClient.GetState(ctx, "BlockPersisterHeight"); err == nil && len(state) >= 4 {
		height := binary.LittleEndian.Uint32(state)
		if height > 0 {
			b.lastPersistedHeight.Store(height)
			b.logger.Infof("[BlockAssembler] Initialized persisted height from block persister state: %d", height)
		}
	} else if err != nil {
		// State doesn't exist - block persister either not deployed, hasn't started, or first run.
		// All cases are safe (cleanup checks for height=0 and proceeds normally without coordination).
		b.logger.Debugf("[BlockAssembler] Block persister state not available: %v", err)
	}

	// Check if the UTXO store supports cleanup operations
	if !b.settings.UtxoStore.DisableDAHCleaner {
		if cleanupServiceProvider, ok := b.utxoStore.(cleanup.CleanupServiceProvider); ok {
			b.logger.Infof("[BlockAssembler] initialising cleanup service")

			b.cleanupService, err = cleanupServiceProvider.GetCleanupService()
			if err != nil {
				return err
			}

			if b.cleanupService != nil {
				// CLEANUP COORDINATION: Wire up block persister progress tracking
				//
				// Cleanup needs to know how far block persister has progressed so it doesn't
				// delete transactions that block persister still needs to create .subtree_data files.
				//
				// The cleanup service will call GetLastPersistedHeight() before each cleanup operation
				// and limit deletion to: min(requested_height, persisted_height + retention)
				//
				// This getter provides the persisted height that was:
				//   1. Initialized from blockchain state on startup (preventing startup race)
				//   2. Updated via BlockPersisted notifications during runtime (keeping current)
				//
				// See processCleanupJob in cleanup_service.go for the coordination logic.
				b.cleanupService.SetPersistedHeightGetter(b.GetLastPersistedHeight)
				b.logger.Infof("[BlockAssembler] Configured cleanup service to coordinate with block persister")

				b.logger.Infof("[BlockAssembler] starting cleanup service")
				b.cleanupService.Start(ctx)
			}

			b.cleanupServiceLoaded.Store(true)

			// Start the cleanup queue worker to process parent preserve and DAH cleanup operations
			// This prevents flooding the system with concurrent operations during block catchup
			b.startCleanupQueueWorker(ctx)
		}
	}

	_, height := b.CurrentBlock()
	prometheusBlockAssemblyCurrentBlockHeight.Set(float64(height))

	return nil
}

func (b *BlockAssembler) initState(ctx context.Context) error {
	bestBlockHeader, bestBlockHeight, err := b.GetState(ctx)
	if err != nil {
		if errors.Is(err, errors.ErrNotFound) || strings.Contains(err.Error(), sql.ErrNoRows.Error()) {
			b.logger.Warnf("[BlockAssembler] no state found in blockchain db")
		} else {
			b.logger.Errorf("[BlockAssembler] error getting state from blockchain db: %v", err)
		}
	} else {
		b.logger.Infof("[BlockAssembler] setting best block header from state: %d: %s", bestBlockHeight, bestBlockHeader.Hash())
		b.setBestBlockHeader(bestBlockHeader, bestBlockHeight)
		b.subtreeProcessor.InitCurrentBlockHeader(bestBlockHeader)
	}

	// we did not get any state back from the blockchain db, so we get the current best block header
	baBestBlockHeader, baBestBlockHeight := b.CurrentBlock()
	if baBestBlockHeader == nil || baBestBlockHeight == 0 {
		header, meta, err := b.blockchainClient.GetBestBlockHeader(ctx)
		if err != nil {
			// we must return an error here since we cannot continue without a best block header
			return errors.NewProcessingError("[BlockAssembler] error getting best block header: %v", err)
		} else {
			hash, _ := b.CurrentBlock()
			b.logger.Infof("[BlockAssembler] setting best block header from GetBestBlockHeader: %s", hash.Hash())
			b.setBestBlockHeader(header, meta.Height)
			b.subtreeProcessor.InitCurrentBlockHeader(header)
		}
	}

	if err = b.SetState(ctx); err != nil {
		b.logger.Errorf("[BlockAssembler] error setting state: %v", err)
	}

	return nil
}

// GetState retrieves the current state of the block assembler from the blockchain.
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns:
//   - *model.BlockHeader: Current best block header
//   - uint32: Current block height
//   - error: Any error encountered during state retrieval
func (b *BlockAssembler) GetState(ctx context.Context) (*model.BlockHeader, uint32, error) {
	state, err := b.blockchainClient.GetState(ctx, "BlockAssembler")
	if err != nil {
		return nil, 0, err
	}

	bestBlockHeight := binary.LittleEndian.Uint32(state[:4])

	bestBlockHeader, err := model.NewBlockHeaderFromBytes(state[4:])
	if err != nil {
		return nil, 0, err
	}

	return bestBlockHeader, bestBlockHeight, nil
}

// SetState persists the current state of the block assembler to the blockchain.
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns:
//   - error: Any error encountered during state persistence
func (b *BlockAssembler) SetState(ctx context.Context) error {
	blockHeader, blockHeight := b.CurrentBlock()
	if blockHeader == nil {
		return errors.NewError("bestBlockHeader is nil")
	}

	blockHeaderBytes := blockHeader.Bytes()

	state := make([]byte, 4+len(blockHeaderBytes))
	binary.LittleEndian.PutUint32(state[:4], blockHeight)
	state = append(state[:4], blockHeaderBytes...)

	b.logger.Debugf("[BlockAssembler] setting state: %d: %s", blockHeight, blockHeader.Hash())

	return b.blockchainClient.SetState(ctx, "BlockAssembler", state)
}

func (b *BlockAssembler) SetStateChangeCh(ch chan BestBlockInfo) {
	b.stateChangeCh = ch
}

// CurrentBlock returns the current best block header and height atomically.
// This is the preferred method to access the best block state as it ensures
// the header and height are always consistent with each other.
//
// Returns:
//   - *model.BlockHeader: Current best block header (nil if not set)
//   - uint32: Current block height (0 if not set)
func (b *BlockAssembler) CurrentBlock() (*model.BlockHeader, uint32) {
	info := b.bestBlock.Load()
	if info == nil {
		return nil, 0
	}
	return info.Header, info.Height
}

// AddTx adds a transaction to the block assembler.
//
// Parameters:
//   - node: Transaction node to add
func (b *BlockAssembler) AddTx(node subtree.SubtreeNode, txInpoints subtree.TxInpoints) {
	b.subtreeProcessor.Add(node, txInpoints)
}

// RemoveTx removes a transaction from the block assembler.
//
// Parameters:
//   - hash: Hash of the transaction to remove
//
// Returns:
//   - error: Any error encountered during removal
func (b *BlockAssembler) RemoveTx(hash chainhash.Hash) error {
	return b.subtreeProcessor.Remove(hash)
}

type resetRequest struct {
	FullReset bool
	ErrCh     chan error
}

// Reset triggers a reset of the block assembler state.
// This operation runs asynchronously to prevent blocking.
func (b *BlockAssembler) Reset(fullReset bool) {
	// run in a go routine to prevent blocking
	go func() {
		errCh := make(chan error, 1)

		b.resetCh <- resetRequest{
			FullReset: fullReset,
			ErrCh:     errCh,
		}

		if err := <-errCh; err != nil {
			b.logger.Errorf("[BlockAssembler] error resetting: %v", err)
		}
	}()
}

// GetMiningCandidate retrieves a candidate block for mining.
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns:
//   - *model.MiningCandidate: Mining candidate block
//   - []*util.Subtree: Associated subtrees
//   - error: Any error encountered during retrieval
func (b *BlockAssembler) GetMiningCandidate(ctx context.Context) (*model.MiningCandidate, []*subtree.Subtree, error) {
	// make sure we call this on the select, so we don't get a candidate when we found a new block
	ctx, _, deferFn := tracing.Tracer("blockassembly").Start(ctx, "GetMiningCandidate",
		tracing.WithParentStat(b.stats),
		tracing.WithHistogram(prometheusBlockAssemblyGetMiningCandidateDuration),
		tracing.WithDebugLogMessage(b.logger, "[GetMiningCandidate] called"),
	)
	defer deferFn()

	// Try to get from cache first
	b.cachedCandidate.mu.RLock()

	_, currentHeight := b.CurrentBlock()

	// Return cached if still valid (same height and within timeout)
	if !b.settings.ChainCfgParams.ReduceMinDifficulty && b.cachedCandidate.candidate != nil &&
		b.cachedCandidate.lastHeight == currentHeight &&
		time.Since(b.cachedCandidate.lastUpdate) < b.settings.BlockAssembly.MiningCandidateCacheTimeout {
		candidate := b.cachedCandidate.candidate
		subtrees := b.cachedCandidate.subtrees
		b.cachedCandidate.mu.RUnlock()

		// Record cache hit metrics
		prometheusBlockAssemblerCacheHits.Inc()

		b.logger.Debugf("[BlockAssembler] Returning cached mining candidate %s", candidate.Id)

		return candidate, subtrees, nil
	}

	// Check if already generating
	if b.cachedCandidate.generating {
		ch := b.cachedCandidate.generationChan
		b.cachedCandidate.mu.RUnlock()

		// Wait for ongoing generation
		<-ch

		return b.GetMiningCandidate(ctx)
	}

	// Mark as generating - upgrade to write lock
	b.cachedCandidate.mu.RUnlock()
	b.cachedCandidate.mu.Lock()

	// Double check generating flag in case another goroutine set it while we upgraded locks
	if b.cachedCandidate.generating {
		ch := b.cachedCandidate.generationChan
		b.cachedCandidate.mu.Unlock()

		// Wait for ongoing generation
		<-ch

		return b.GetMiningCandidate(ctx)
	}

	b.cachedCandidate.generating = true
	b.cachedCandidate.generationChan = make(chan struct{})
	b.cachedCandidate.mu.Unlock()

	// Ensure cache state is always cleaned up, even on early returns
	defer func() {
		b.cachedCandidate.mu.Lock()
		b.cachedCandidate.generating = false
		if b.cachedCandidate.generationChan != nil {
			close(b.cachedCandidate.generationChan)
			b.cachedCandidate.generationChan = nil
		}
		b.cachedCandidate.mu.Unlock()
	}()

	// Generate new candidate
	responseCh := make(chan *miningCandidateResponse)

	select {
	case <-ctx.Done():
		close(responseCh)
		// context cancelled, do not send
		return nil, nil, ctx.Err()
	case <-time.After(1 * time.Second):
		return nil, nil, errors.NewServiceError("timeout sending mining candidate request")
	case b.miningCandidateCh <- responseCh:
		// sent successfully
	}

	// wait for 10 seconds for the response
	var candidate *model.MiningCandidate

	var subtrees []*subtree.Subtree

	var err error

	select {
	case <-ctx.Done():
		// context cancelled, do not send
		close(responseCh)
		err = ctx.Err()
	case <-time.After(10 * time.Second):
		// make sure to close the channel, otherwise the for select will hang, because no one is reading from it
		close(responseCh)

		err = errors.NewServiceError("timeout getting mining candidate")
	case response := <-responseCh:
		candidate = response.miningCandidate
		subtrees = response.subtrees
		err = response.err
	}

	// Update cache on success
	if err == nil {
		b.cachedCandidate.mu.Lock()
		b.cachedCandidate.candidate = candidate
		b.cachedCandidate.subtrees = subtrees
		b.cachedCandidate.lastHeight = currentHeight
		b.cachedCandidate.lastUpdate = time.Now()
		b.cachedCandidate.mu.Unlock()

		// Record cache miss metrics
		prometheusBlockAssemblerCacheMisses.Inc()
	}

	return candidate, subtrees, err
}

// getMiningCandidate creates a new mining candidate from the current block state.
// This is an internal method called by GetMiningCandidate.
//
// Returns:
//   - *model.MiningCandidate: Created mining candidate
//   - []*util.Subtree: Associated subtrees
//   - error: Any error encountered during candidate creation
func (b *BlockAssembler) getMiningCandidate() (*model.MiningCandidate, []*subtree.Subtree, error) {
	prometheusBlockAssemblerGetMiningCandidate.Inc()

	baBestBlockHeader, baBestBlockHeight := b.CurrentBlock()

	if baBestBlockHeader == nil {
		return nil, nil, errors.NewError("best block header is not available")
	}

	b.logger.Debugf("[BlockAssembler] getting mining candidate for header: %s", baBestBlockHeader.Hash())

	// Get the list of completed containers for the current chaintip and height...
	subtrees := b.subtreeProcessor.GetCompletedSubtreesForMiningCandidate()

	blockMaxSizeUint64, err := safeconversion.IntToUint64(b.settings.Policy.BlockMaxSize)
	if err != nil {
		return nil, nil, errors.NewProcessingError("error converting block max size", err)
	}

	if b.settings.Policy.BlockMaxSize > 0 && len(subtrees) > 0 && blockMaxSizeUint64 < subtrees[0].SizeInBytes {
		b.logger.Warnf("[BlockAssembler] max block size is less than the size of the subtree: %d < %d", b.settings.Policy.BlockMaxSize, subtrees[0].SizeInBytes)

		return nil, nil, errors.NewProcessingError("max block size is less than the size of the subtree")
	}

	var coinbaseValue uint64

	currentHeight := baBestBlockHeight + 1

	// Log initial state for debugging
	b.logger.Debugf("Starting coinbase calculation for height %d", currentHeight)

	// Get the hash of the last subtree in the list...
	// We do this by using the same subtree processor logic to get the top tree hash.
	id := &chainhash.Hash{}

	var txCount uint32

	var sizeWithoutCoinbase uint64

	var subtreesToInclude []*subtree.Subtree

	var subtreeBytesToInclude [][]byte

	var coinbaseMerkleProofBytes [][]byte

	// set the size without the coinbase to the size of the block header
	// This should probably have the size of the varint for the tx count as well
	// but bitcoin-sv node doesn't do that.
	sizeWithoutCoinbase = 80

	if len(subtrees) == 0 {
		txCount = 1

		b.logger.Debugf("No subtrees to include, creating empty block with coinbase only")
	} else {
		currentBlockSize := uint64(0)
		totalFees := uint64(0)
		subtreeCount := 0

		topTree, err := subtree.NewIncompleteTreeByLeafCount(len(subtrees))
		if err != nil {
			return nil, nil, errors.NewProcessingError("error creating top tree", err)
		}

		b.logger.Debugf("Processing %d subtrees for inclusion", len(subtrees))

		for _, subtree := range subtrees {
			if b.settings.Policy.BlockMaxSize == 0 || currentBlockSize+subtree.SizeInBytes <= blockMaxSizeUint64 {
				subtreesToInclude = append(subtreesToInclude, subtree)
				subtreeBytesToInclude = append(subtreeBytesToInclude, subtree.RootHash().CloneBytes())
				coinbaseValue += subtree.Fees
				totalFees += subtree.Fees
				subtreeCount++
				currentBlockSize += subtree.SizeInBytes
				_ = topTree.AddNode(*subtree.RootHash(), subtree.Fees, subtree.SizeInBytes)

				b.logger.Debugf("Included subtree %d: fees=%d, size=%d, total_fees=%d", subtreeCount, subtree.Fees, subtree.SizeInBytes, totalFees)

				lenSubtreeNodesUint32, err := safeconversion.IntToUint32(len(subtree.Nodes))
				if err != nil {
					return nil, nil, errors.NewProcessingError("error converting subtree nodes length", err)
				}

				txCount += lenSubtreeNodesUint32

				sizeWithoutCoinbase += subtree.SizeInBytes
			} else {
				break
			}
		}

		b.logger.Debugf("Fee accumulation complete: included %d subtrees, total_fees=%d satoshis (%.8f BSV)", subtreeCount, totalFees, float64(totalFees)/1e8)

		if len(subtreesToInclude) > 0 {
			coinbaseMerkleProof, err := subtree.GetMerkleProofForCoinbase(subtreesToInclude)
			if err != nil {
				return nil, nil, errors.NewProcessingError("error getting merkle proof for coinbase", err)
			}

			for _, hash := range coinbaseMerkleProof {
				coinbaseMerkleProofBytes = append(coinbaseMerkleProofBytes, hash.CloneBytes())
			}
		}

		id = topTree.RootHash()
	}

	timeNow := time.Now().Unix()
	b.logger.Debugf("Current time: %d", timeNow)
	timeNowUint32, err := safeconversion.Int64ToUint32(timeNow)
	if err != nil {
		return nil, nil, errors.NewProcessingError("error converting time now", err)
	}

	timeBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(timeBytes, timeNowUint32)

	nBits, err := b.getNextNbits(timeNow)
	if err != nil {
		return nil, nil, err
	}

	// Log coinbase value before adding subsidy
	feesOnly := coinbaseValue
	b.logger.Debugf("Before adding subsidy: coinbase_value=%d satoshis (%.8f BSV) from transaction fees", feesOnly, float64(feesOnly)/1e8)

	// Critical subsidy calculation - add comprehensive logging
	subsidyHeight := baBestBlockHeight + 1
	b.logger.Debugf("Calculating block subsidy for height %d", subsidyHeight)

	// Validate ChainCfgParams before using
	if b.settings.ChainCfgParams == nil {
		b.logger.Errorf("CRITICAL: ChainCfgParams is nil! This will cause subsidy calculation to fail")
		return nil, nil, errors.NewProcessingError("ChainCfgParams is nil")
	}

	blockSubsidy := util.GetBlockSubsidyForHeight(subsidyHeight, b.settings.ChainCfgParams)
	b.logger.Debugf("Block subsidy calculated: %d satoshis (%.8f BSV) for height %d", blockSubsidy, float64(blockSubsidy)/1e8, subsidyHeight)

	coinbaseValue += blockSubsidy

	// Log final coinbase value
	b.logger.Debugf("Final coinbase value: fees=%d + subsidy=%d = total=%d satoshis (%.8f BSV)", feesOnly, blockSubsidy, coinbaseValue, float64(coinbaseValue)/1e8)

	// Additional validation - check for suspicious values
	if blockSubsidy == 0 && subsidyHeight < 6930000 {
		b.logger.Errorf("SUSPICIOUS: Block subsidy is 0 for height %d (should be non-zero until height 6930000)", subsidyHeight)
	}

	if coinbaseValue < blockSubsidy {
		b.logger.Errorf("CRITICAL BUG: Final coinbase value %d is less than subsidy %d - this indicates an overflow or logic error", coinbaseValue, blockSubsidy)
	}

	previousHash := baBestBlockHeader.Hash().CloneBytes()

	lenSubtreesToIncludeUint32, err := safeconversion.IntToUint32(len(subtreesToInclude))
	if err != nil {
		return nil, nil, errors.NewProcessingError("error converting subtree count", err)
	}

	// remove the coinbase tx from the tx count
	if txCount > 0 {
		txCount--
	}

	miningCandidate := &model.MiningCandidate{
		// create a job ID from the top tree hash and the previous block hash, to prevent empty block job id collisions
		Id:                  chainhash.HashB(append(append(id[:], previousHash...), timeBytes...)),
		PreviousHash:        previousHash,
		CoinbaseValue:       coinbaseValue,
		Version:             0x20000000,
		NBits:               nBits.CloneBytes(),
		Height:              baBestBlockHeight + 1,
		Time:                timeNowUint32,
		MerkleProof:         coinbaseMerkleProofBytes,
		NumTxs:              txCount,
		SizeWithoutCoinbase: sizeWithoutCoinbase,
		SubtreeCount:        lenSubtreesToIncludeUint32,
		SubtreeHashes:       subtreeBytesToInclude,
	}

	return miningCandidate, subtreesToInclude, nil
}

// handleReorg handles blockchain reorganization.
//
// Parameters:
//   - ctx: Context for cancellation
//   - header: New block header
//   - height: New block height
//
// Returns:
//   - error: Any error encountered during reorganization
func (b *BlockAssembler) handleReorg(ctx context.Context, header *model.BlockHeader, height uint32) error {
	startTime := time.Now()

	prometheusBlockAssemblerReorg.Inc()

	moveBackBlocksWithMeta, moveForwardBlocksWithMeta, err := b.getReorgBlocks(ctx, header, height)
	if err != nil {
		return errors.NewProcessingError("error getting reorg blocks", err)
	}

	b.logger.Infof("[BlockAssembler] handling reorg, moveBackBlocks: %d, moveForwardBlocks: %d", len(moveBackBlocksWithMeta), len(moveForwardBlocksWithMeta))

	_, currentHeight := b.CurrentBlock()

	if (len(moveBackBlocksWithMeta) >= int(b.settings.ChainCfgParams.CoinbaseMaturity) || len(moveForwardBlocksWithMeta) >= int(b.settings.ChainCfgParams.CoinbaseMaturity)) && currentHeight > 1000 {
		// large reorg, log it and Reset the block assembler
		b.logger.Warnf("[BlockAssembler] large reorg detected, resetting block assembly, moveBackBlocks: %d, moveForwardBlocks: %d", len(moveBackBlocksWithMeta), len(moveForwardBlocksWithMeta))

		// make sure we wait for the reset to complete
		if err = b.reset(ctx, false); err != nil {
			b.logger.Errorf("[BlockAssembler] error resetting after large reorg: %v", err)
		}

		// return an error to indicate we reset due to a large reorg
		return errors.NewBlockAssemblyResetError("large reorg, moveBackBlocks: %d, moveForwardBlocks: %d, resetting block assembly", len(moveBackBlocksWithMeta), len(moveForwardBlocksWithMeta))
	}

	hasInvalidBlock := false

	moveBackBlocks := make([]*model.Block, len(moveBackBlocksWithMeta))
	for i, moveBackBlockWithMeta := range moveBackBlocksWithMeta {
		if moveBackBlockWithMeta.meta.Invalid {
			hasInvalidBlock = true
		}

		moveBackBlocks[i] = moveBackBlockWithMeta.block
	}

	moveForwardBlocks := make([]*model.Block, len(moveForwardBlocksWithMeta))
	for i, moveForwardBlockWithMeta := range moveForwardBlocksWithMeta {
		moveForwardBlocks[i] = moveForwardBlockWithMeta.block
	}

	reset := hasInvalidBlock

	// now do the reorg in the subtree processor
	if err = b.subtreeProcessor.Reorg(moveBackBlocks, moveForwardBlocks); err != nil {
		b.logger.Warnf("[BlockAssembler] error doing reorg, will reset instead: %v", err)
		// fallback to full reset
		reset = true
	}

	if reset {
		// we have an invalid block in the reorg or reorg failed, we need to reset the block assembly and load the unmined transactions again
		b.logger.Warnf("[BlockAssembler] reorg contains invalid block, resetting block assembly, moveBackBlocks: %d, moveForwardBlocks: %d", len(moveBackBlocks), len(moveForwardBlocks))

		if err = b.reset(ctx, false); err != nil {
			return errors.NewProcessingError("error resetting block assembly after reorg with invalid block", err)
		}
	}

	prometheusBlockAssemblerReorgDuration.Observe(float64(time.Since(startTime).Microseconds()) / 1_000_000)

	return nil
}

// getReorgBlocks retrieves blocks involved in reorganization.
//
// Parameters:
//   - ctx: Context for cancellation
//   - header: Target block header
//   - height: Target block height
//
// Returns:
//   - []*model.Block: Blocks to move down
//   - []*model.Block: Blocks to move up
//   - error: Any error encountered
func (b *BlockAssembler) getReorgBlocks(ctx context.Context, header *model.BlockHeader, height uint32) ([]blockWithMeta, []blockWithMeta, error) {
	_, _, deferFn := tracing.Tracer("blockassembly").Start(ctx, "getReorgBlocks",
		tracing.WithParentStat(b.stats),
		tracing.WithHistogram(prometheusBlockAssemblerGetReorgBlocksDuration),
		tracing.WithLogMessage(b.logger, "[getReorgBlocks] called"),
	)
	defer deferFn()

	moveBackBlockHeadersWithMeta, moveForwardBlockHeadersWithMeta, err := b.getReorgBlockHeaders(ctx, header, height)
	if err != nil {
		return nil, nil, errors.NewServiceError("error getting reorg block headers", err)
	}

	// moveForwardBlocks will contain all blocks we need to move up to get to the new tip from the common ancestor
	moveForwardBlocks := make([]blockWithMeta, 0, len(moveForwardBlockHeadersWithMeta))

	// moveBackBlocks will contain all blocks we need to move down to get to the common ancestor
	moveBackBlocks := make([]blockWithMeta, 0, len(moveBackBlockHeadersWithMeta))

	var block *model.Block
	for _, headerWithMeta := range moveForwardBlockHeadersWithMeta {
		block, err = b.blockchainClient.GetBlock(ctx, headerWithMeta.header.Hash())
		if err != nil {
			return nil, nil, errors.NewServiceError("error getting block", err)
		}

		moveForwardBlocks = append(moveForwardBlocks, blockWithMeta{
			block: block,
			meta:  headerWithMeta.meta,
		})
	}

	for _, headerWithMeta := range moveBackBlockHeadersWithMeta {
		block, err = b.blockchainClient.GetBlock(ctx, headerWithMeta.header.Hash())
		if err != nil {
			return nil, nil, errors.NewServiceError("error getting block", err)
		}

		moveBackBlocks = append(moveBackBlocks, blockWithMeta{
			block: block,
			meta:  headerWithMeta.meta,
		})
	}

	return moveBackBlocks, moveForwardBlocks, nil
}

// getReorgBlockHeaders returns the block headers that need to be moved down and up to get to the new tip
// it is based on a common ancestor between the current chain and the new chain
// TODO optimize this function
func (b *BlockAssembler) getReorgBlockHeaders(ctx context.Context, header *model.BlockHeader, height uint32) ([]blockHeaderWithMeta, []blockHeaderWithMeta, error) {
	if header == nil {
		return nil, nil, errors.NewError("header is nil")
	}

	// We want to use block locators to find the common ancestor
	// This is because block locators are more efficient than fetching blocks
	// and we want to avoid fetching blocks that we don't need

	// We will get a block locator for requested header
	// and another block locator for on the best block header

	// Important: in order to compare 2 block locators, it is critical that
	// the starting height of both locators is the same
	// otherwise the common ancestor might be the genesis block
	// because none of the mentioned block hashes in the block locators
	// are necessarily going to be on the same height

	baBestBlockHeader, baBestBlockHeight := b.CurrentBlock()
	startingHeight := height

	if height > baBestBlockHeight {
		startingHeight = baBestBlockHeight
	}

	// Get block locator for current chain
	currentChainLocator, err := b.blockchainClient.GetBlockLocator(ctx, baBestBlockHeader.Hash(), startingHeight)
	if err != nil {
		return nil, nil, errors.NewServiceError("error getting block locator for current chain", err)
	}

	// Get block locator for the new best block
	newChainLocator, err := b.blockchainClient.GetBlockLocator(ctx, header.Hash(), startingHeight)
	if err != nil {
		return nil, nil, errors.NewServiceError("error getting block locator for new chain", err)
	}

	// Find common ancestor using locators
	var (
		commonAncestor     *model.BlockHeader
		commonAncestorMeta *model.BlockHeaderMeta
	)

	for _, currentHash := range currentChainLocator {
		for _, newHash := range newChainLocator {
			if currentHash.IsEqual(newHash) {
				commonAncestor, commonAncestorMeta, err = b.blockchainClient.GetBlockHeader(ctx, currentHash)
				if err != nil {
					return nil, nil, errors.NewServiceError("error getting common ancestor header", err)
				}

				goto FoundAncestor
			}
		}
	}

FoundAncestor:
	if commonAncestor == nil || commonAncestorMeta == nil {
		return nil, nil, errors.NewProcessingError("common ancestor not found, reorg not possible")
	}

	// Get headers from current tip down to common ancestor
	headerCount := baBestBlockHeight - commonAncestorMeta.Height + 1

	moveBackBlockHeaders, moveBackBlockHeaderMetas, err := b.blockchainClient.GetBlockHeaders(ctx, baBestBlockHeader.Hash(), uint64(headerCount))
	if err != nil {
		return nil, nil, errors.NewServiceError("error getting current chain headers", err)
	}

	moveBackBlockHeadersWithMeta := make([]blockHeaderWithMeta, 0, len(moveBackBlockHeaders))
	for i, moveBackBlockHeader := range moveBackBlockHeaders {
		moveBackBlockHeadersWithMeta = append(moveBackBlockHeadersWithMeta, blockHeaderWithMeta{
			header: moveBackBlockHeader,
			meta:   moveBackBlockHeaderMetas[i],
		})
	}

	// Handle empty moveBackBlockHeaders or when length is 1 (only common ancestor)
	var filteredMoveBack []blockHeaderWithMeta
	if len(moveBackBlockHeaders) > 1 {
		filteredMoveBack = moveBackBlockHeadersWithMeta[:len(moveBackBlockHeadersWithMeta)-1]
	}

	// Get headers from new tip down to common ancestor
	moveForwardBlockHeaders, moveForwardBlockHeaderMetas, err := b.blockchainClient.GetBlockHeaders(ctx, header.Hash(), uint64(height-commonAncestorMeta.Height))
	if err != nil {
		return nil, nil, errors.NewServiceError("error getting new chain headers", err)
	}

	moveForwardBlockHeadersWithMeta := make([]blockHeaderWithMeta, 0, len(moveForwardBlockHeaders))
	for i, moveForwardBlockHeader := range moveForwardBlockHeaders {
		moveForwardBlockHeadersWithMeta = append(moveForwardBlockHeadersWithMeta, blockHeaderWithMeta{
			header: moveForwardBlockHeader,
			meta:   moveForwardBlockHeaderMetas[i],
		})
	}

	// reverse moveForwardBlocks slice
	for i := len(moveForwardBlockHeadersWithMeta)/2 - 1; i >= 0; i-- {
		opp := len(moveForwardBlockHeadersWithMeta) - 1 - i
		moveForwardBlockHeadersWithMeta[i], moveForwardBlockHeadersWithMeta[opp] = moveForwardBlockHeadersWithMeta[opp], moveForwardBlockHeadersWithMeta[i]
	}

	maxGetReorgHashes := b.settings.BlockAssembly.MaxGetReorgHashes
	if len(filteredMoveBack) > maxGetReorgHashes {
		currentHeader, currentHeight := b.CurrentBlock()
		b.logger.Errorf("reorg is too big, max block reorg: current hash: %s, current height: %d, new hash: %s, new height: %d, common ancestor hash: %s, common ancestor height: %d, move down block count: %d, move up block count: %d, current locator: %v, new block locator: %v", currentHeader.Hash(), currentHeight, header.Hash(), height, commonAncestor.Hash(), commonAncestorMeta.Height, len(filteredMoveBack), len(moveForwardBlockHeaders), currentChainLocator, newChainLocator)
		return nil, nil, errors.NewProcessingError("reorg is too big, max block reorg: %d", maxGetReorgHashes)
	}

	return filteredMoveBack, moveForwardBlockHeadersWithMeta, nil
}

// getNextNbits retrieves the next required work difficulty target.
//
// Returns:
//   - *model.NBit: Next difficulty target
//   - error: Any error encountered during retrieval
func (b *BlockAssembler) getNextNbits(nextBlockTime int64) (*model.NBit, error) {
	baBestBlockHeader, _ := b.CurrentBlock()

	nbit, err := b.blockchainClient.GetNextWorkRequired(context.Background(), baBestBlockHeader.Hash(), nextBlockTime)
	if err != nil {
		return nil, errors.NewProcessingError("error getting next work required", err)
	}

	return nbit, nil
}

// loadUnminedTransactions loads transactions from the UTXO store into block assembly.
//
// Primary responsibility: Load unmined transactions
//   - Iterates through transactions with unmined_since set
//   - Filters out transactions already on main chain (skip those with block_ids on best chain)
//   - Loads remaining transactions into block assembly for potential inclusion in next block
//
// Secondary responsibility: Data integrity safety net (ALWAYS runs)
//   - Identifies transactions with block_ids on main chain BUT unmined_since still set
//   - Calls MarkTransactionsOnLongestChain to clear unmined_since for these transactions
//   - This catches edge cases from: previous bugs, crashes, timing issues
//   - Minimal performance impact since list is usually empty when system is healthy
//
// The fullScan parameter controls iterator behavior:
//   - fullScan=false: Uses index on unmined_since (fast, most common)
//   - fullScan=true: Scans ALL records (Aerospike only; SQL always uses index)
//
// Relationship with reorg handling:
//   - BlockValidation background jobs: Handle moveForward blocks and invalid blocks
//   - reset(): Handles moveBack blocks (sets unmined_since for transactions moved to side chain)
//   - loadUnminedTransactions(): Loads everything + catches any missed edge cases
//
// Note: For moveForward blocks, BlockValidation has already cleared unmined_since via
// background job (mined_set=false triggers setTxMinedStatus with onLongestChain=true).
// This function is just a safety net for any inconsistencies, not primary reorg handling.
//
// Called from:
//   - reset() as postProcessFn (after reorg processing)
//   - Startup initialization
//
// Parameters:
//   - ctx: Context for cancellation
//   - fullScan: true = scan all records, false = use index (faster)
//
// Returns:
//   - error: Any error encountered during loading
func (b *BlockAssembler) loadUnminedTransactions(ctx context.Context, fullScan bool) (err error) {
	_, _, deferFn := tracing.Tracer("blockassembly").Start(ctx, "loadUnminedTransactions",
		tracing.WithParentStat(b.stats),
		tracing.WithLogMessage(b.logger, "[loadUnminedTransactions] called with fullScan=%t", fullScan),
	)
	defer deferFn()

	// Set flag to indicate unmined transactions are being loaded
	b.unminedTransactionsLoading.Store(true)
	defer func() {
		// Clear flag when loading is complete
		b.unminedTransactionsLoading.Store(false)
		b.logger.Infof("[loadUnminedTransactions] unmined transaction loading completed")
	}()

	if b.utxoStore == nil {
		return errors.NewServiceError("[BlockAssembler] no utxostore")
	}

	scanHeaders := uint64(1000)

	if !fullScan {
		// Wait for the unmined_since index to be ready before attempting to get the iterator
		// This is similar to how the cleanup service waits for the delete_at_height index
		if indexWaiter, ok := b.utxoStore.(interface {
			WaitForIndexReady(ctx context.Context, indexName string) error
		}); ok {
			if err := indexWaiter.WaitForIndexReady(ctx, "unminedSinceIndex"); err != nil {
				b.logger.Warnf("[BlockAssembler] failed to wait for unmined_since index: %v", err)
				// Continue anyway as this may be a non-Aerospike store
			}
		}
	} else {
		// get the full header count so we can do a full scan of all unmined transactions
		_, bestBlockHeaderMeta, err := b.blockchainClient.GetBestBlockHeader(ctx)
		if err != nil {
			return errors.NewProcessingError("error getting best block header meta", err)
		}

		if bestBlockHeaderMeta.Height > 0 {
			scanHeaders = uint64(bestBlockHeaderMeta.Height)
		} else {
			scanHeaders = 1000
		}

		b.logger.Infof("[BlockAssembler] doing full scan of unmined transactions, scanning last %d headers", scanHeaders)
	}

	bestBlockHeader, _ := b.CurrentBlock()
	bestBlockHeaderIDs, err := b.blockchainClient.GetBlockHeaderIDs(ctx, bestBlockHeader.Hash(), scanHeaders)
	if err != nil {
		return errors.NewProcessingError("error getting best block headers", err)
	}

	bestBlockHeaderIDsMap := make(map[uint32]bool, len(bestBlockHeaderIDs))
	for _, id := range bestBlockHeaderIDs {
		bestBlockHeaderIDsMap[id] = true
	}

	b.logger.Infof("[loadUnminedTransactions] requesting unmined tx iterator from UTXO store (fullScan=%t)", fullScan)
	it, err := b.utxoStore.GetUnminedTxIterator(fullScan)
	if err != nil {
		return errors.NewProcessingError("error getting unmined tx iterator", err)
	}
	b.logger.Infof("[loadUnminedTransactions] successfully created unmined tx iterator, starting to process transactions")

	unminedTransactions := make([]*utxo.UnminedTransaction, 0, 1024*1024) // preallocate a large slice to avoid reallocations
	lockedTransactions := make([]chainhash.Hash, 0, 1024)

	// keep track of transactions that we need to mark as mined on the longest chain
	// this is for transactions that are included in a block that is in the best chain
	// but the transaction itself is still marked as unmined, this can happen if block assembly got dirty
	markAsMinedOnLongestChain := make([]chainhash.Hash, 0, 1024)

	for {
		unminedTransaction, err := it.Next(ctx)
		if err != nil {
			return errors.NewProcessingError("error getting unmined transaction", err)
		}

		if unminedTransaction == nil {
			break
		}

		if unminedTransaction.Skip {
			continue
		}

		if len(unminedTransaction.BlockIDs) > 0 {
			// If the transaction is already included in a block that is in the best chain, skip it
			skipAlreadyMined := false
			for _, blockID := range unminedTransaction.BlockIDs {
				if bestBlockHeaderIDsMap[blockID] {
					skipAlreadyMined = true
					break
				}
			}

			if skipAlreadyMined {
				// b.logger.Debugf("[BlockAssembler] skipping unmined transaction %s already included in best chain", unminedTransaction.Hash)

				if unminedTransaction.UnminedSince > 0 {
					markAsMinedOnLongestChain = append(markAsMinedOnLongestChain, *unminedTransaction.Hash)
				}

				continue
			}
		}

		unminedTransactions = append(unminedTransactions, unminedTransaction)

		if unminedTransaction.Locked {
			// if the transaction is locked, we need to add it to the locked transactions list, so we can unlock them
			lockedTransactions = append(lockedTransactions, *unminedTransaction.Hash)
		}
	}

	// Always fix data inconsistencies: transactions with block_ids on main chain but unmined_since set
	// This ensures data integrity on every load, catching issues from previous bugs, crashes, or edge cases
	// The performance impact is minimal since the list is usually empty when data is correct
	if len(markAsMinedOnLongestChain) > 0 {
		if err = b.utxoStore.MarkTransactionsOnLongestChain(ctx, markAsMinedOnLongestChain, true); err != nil {
			return errors.NewProcessingError("error marking transactions as mined on longest chain", err)
		}

		b.logger.Infof("[BlockAssembler] fixed %d transactions with inconsistent unmined_since (had block_ids on main but unmined_since set)", len(markAsMinedOnLongestChain))
	}

	// order the transactions by createdAt
	sort.Slice(unminedTransactions, func(i, j int) bool {
		// sort by createdAt, oldest first
		return unminedTransactions[i].CreatedAt < unminedTransactions[j].CreatedAt
	})

	for _, unminedTransaction := range unminedTransactions {
		subtreeNode := subtree.SubtreeNode{
			Hash:        *unminedTransaction.Hash,
			Fee:         unminedTransaction.Fee,
			SizeInBytes: unminedTransaction.Size,
		}

		if err = b.subtreeProcessor.AddDirectly(subtreeNode, unminedTransaction.TxInpoints, true); err != nil {
			return errors.NewProcessingError("error adding unmined transaction to subtree processor", err)
		}
	}

	// unlock any locked transactions
	if len(lockedTransactions) > 0 {
		if err = b.utxoStore.SetLocked(ctx, lockedTransactions, false); err != nil {
			return errors.NewProcessingError("[BlockAssembler] failed to unlock %d unmined transactions: %v", len(lockedTransactions), err)
		} else {
			b.logger.Infof("[BlockAssembler] unlocked %d previously locked unmined transactions", len(lockedTransactions))
		}
	}

	return nil
}

// invalidateMiningCandidateCache invalidates the cached mining candidate
func (b *BlockAssembler) invalidateMiningCandidateCache() {
	b.cachedCandidate.mu.Lock()
	b.cachedCandidate.candidate = nil
	b.cachedCandidate.subtrees = nil
	b.cachedCandidate.lastHeight = 0
	b.cachedCandidate.lastUpdate = time.Time{}
	b.cachedCandidate.generating = false
	b.cachedCandidate.mu.Unlock()
}

// SetSkipWaitForPendingBlocks sets the flag to skip waiting for pending blocks during startup.
// This is primarily used in test environments to prevent blocking on pending blocks.
func (b *BlockAssembler) SetSkipWaitForPendingBlocks(skip bool) {
	b.skipWaitForPendingBlocks = skip
}

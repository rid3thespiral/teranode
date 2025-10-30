# Block Persister Service Reference Documentation

## Overview

The Block Persister Service is responsible for taking blocks from the blockchain service and ensuring they are properly stored in persistent storage along with all related data (transactions, UTXOs, etc.). It plays a critical role in the overall blockchain data persistence strategy by:

- Processing and storing complete blocks in the blob store
- Managing subtree processing for efficient transaction handling
- Maintaining UTXO set differences for each block
- Ensuring data consistency and integrity during persistence operations
- Providing resilient error handling and recovery mechanisms

The service integrates with multiple stores (block store, subtree store, UTXO store) and coordinates between them to ensure consistent and reliable block data persistence. It employs concurrency and batching techniques to optimize performance for high transaction volumes.

## Types

### Server

```go
type Server struct {
    // ctx is the context for controlling server lifecycle and handling cancellation signals
    ctx context.Context

    // logger provides structured logging functionality for operational monitoring and debugging
    logger ulogger.Logger

    // settings contains configuration settings for the server, controlling behavior such as
    // concurrency levels, batch sizes, and persistence strategies
    settings *settings.Settings

    // blockStore provides persistent storage for complete blocks
    // This is typically implemented as a blob store capable of handling large block data
    blockStore blob.Store

    // subtreeStore provides storage for block subtrees, which are hierarchical structures
    // containing transaction references that make up parts of a block
    subtreeStore blob.Store

    // utxoStore provides storage for UTXO (Unspent Transaction Output) data
    // Used to track the current state of the UTXO set and process changes
    utxoStore utxo.Store

    // stats tracks operational statistics for monitoring and performance analysis
    stats *gocore.Stat

    // blockchainClient interfaces with the blockchain service to retrieve block data
    // and coordinate persistence operations with blockchain state
    blockchainClient blockchain.ClientI

    // state manages the persister's internal state, tracking which blocks have been
    // successfully persisted and allowing for recovery after interruptions
    state *state.State
}
```

The `Server` type is the main structure for the Block Persister Service. It contains components for managing stores, blockchain interactions, and state management.

## Functions

### Server Management

#### New

```go
func New(
    ctx context.Context,
    logger ulogger.Logger,
    tSettings *settings.Settings,
    blockStore blob.Store,
    subtreeStore blob.Store,
    utxoStore utxo.Store,
    blockchainClient blockchain.ClientI,
    opts ...func(*Server)
) *Server
```

Creates a new instance of the `Server` with the provided dependencies.

This constructor initializes all components required for block persistence operations, including stores, state management, and client connections. It accepts optional configuration functions to customize the server instance after construction.

Parameters:

- ctx: Context for controlling the server lifecycle
- logger: Logger for recording operational events and errors
- tSettings: Configuration settings that control server behavior
- blockStore: Storage interface for blocks
- subtreeStore: Storage interface for block subtrees
- utxoStore: Storage interface for UTXO data
- blockchainClient: Client for interacting with the blockchain service
- opts: Optional configuration functions to apply after construction

Returns a fully constructed and configured Server instance ready for initialization.

#### WithSetInitialState

```go
func WithSetInitialState(height uint32, hash *chainhash.Hash) func(*Server)
```

WithSetInitialState is an optional configuration function that sets the initial state of the block persister server. This can be used during initialization to establish a known starting point for block persistence operations.

Parameters:

- height: The blockchain height to set as the initial state
- hash: The block hash corresponding to the specified height

Returns a function that, when called with a Server instance, will set the initial state of that server. If the state cannot be set, an error is logged but not returned.

#### Health

```go
func (u *Server) Health(ctx context.Context, checkLiveness bool) (int, string, error)
```

Performs health checks on the server and its dependencies. This method implements the health.Check interface and is used by monitoring systems to determine the operational status of the service.

The health check distinguishes between liveness (is the service running?) and readiness (is the service able to handle requests?) checks:

- Liveness checks verify the service process is running and responsive
- Readiness checks verify all dependencies are available and functioning

Parameters:

- ctx: Context for coordinating cancellation or timeouts
- checkLiveness: When true, only liveness checks are performed; when false, both liveness and readiness checks are performed

Returns:

- int: HTTP status code (200 for healthy, 503 for unhealthy)
- string: Human-readable status message
- error: Any error encountered during health checking

Dependency checks include:

- Blockchain client and FSM status
- Block store availability
- Subtree store status
- UTXO store health

#### Init

```go
func (u *Server) Init(ctx context.Context) (err error)
```

Initializes the server, setting up any required resources.

This method is called after construction but before the server starts processing blocks. It performs one-time initialization tasks such as setting up Prometheus metrics.

Parameters:

- ctx: Context for coordinating initialization operations

Returns an error if initialization fails, nil otherwise.

#### Start

```go
func (u *Server) Start(ctx context.Context, readyCh chan<- struct{}) error
```

Initializes and begins the block persister service operations.

This method starts the main processing loop and sets up HTTP services if configured. It waits for the blockchain FSM to transition from IDLE state before beginning block persistence operations to ensure the blockchain is ready.

The method implements the following key operations:

- Waits for blockchain service readiness
- Sets up HTTP blob server if required by configuration
- Starts the main processing loop in a background goroutine
- Signals service readiness through the provided channel

Parameters:

- ctx: Context for controlling the service lifecycle and handling cancellation
- readyCh: Channel used to signal when the service is ready to accept requests

Returns an error if the service fails to start properly, nil otherwise.

#### Stop

```go
func (u *Server) Stop(_ context.Context) error
```

Gracefully shuts down the server.

This method is called when the service is being stopped and provides an opportunity to perform any necessary cleanup operations, such as closing connections, flushing buffers, or persisting state.

Currently, the Server doesn't need to perform any specific cleanup actions during shutdown as resource cleanup is handled by the context cancellation mechanism in the Start method.

Parameters:

- ctx: Context for controlling the shutdown operation (currently unused)

Returns an error if shutdown fails, or nil on successful shutdown.

### Internal Methods

#### persistBlock

```go
func (u *Server) persistBlock(ctx context.Context, hash *chainhash.Hash, blockBytes []byte) error
```

Stores a block and its associated data to persistent storage.

This is a core function of the blockpersister service that handles the complete persistence workflow for a single block. It ensures all components of a block (header, transactions, and UTXO changes) are properly stored in a consistent and recoverable manner.

!!! abstract "Processing Steps"
    The function implements a multi-stage persistence process:

    1. **Convert raw block bytes** into a structured block model
    2. **Create a new UTXO difference set** for tracking changes
    3. **Process the coinbase transaction** if no subtrees are present
    4. **For blocks with subtrees**, process each subtree concurrently according to configured limits
    5. **Close and finalize** the UTXO difference set once all transactions are processed
    6. **Write the complete block** to persistent storage

**Parameters:**

- `ctx`: Context for the operation, used for cancellation and tracing
- `hash`: Hash identifier of the block to persist
- `blockBytes`: Raw serialized bytes of the complete block

**Returns** an error if any part of the persistence process fails. The error will be wrapped with appropriate context to identify the specific failure point.

!!! note "Concurrency Management"
    Concurrency is managed through errgroup with configurable parallel processing limits to optimize performance while avoiding resource exhaustion.

!!! warning "Atomicity"
    Block persistence is atomic - if any part fails, the entire operation is considered failed and should be retried after resolving the underlying issue.

#### getNextBlockToProcess

```go
func (u *Server) getNextBlockToProcess(ctx context.Context) (*model.Block, error)
```

Retrieves the next block that needs to be processed based on the current state and configuration.

This method determines the next block to persist by comparing the last persisted block height with the current blockchain tip. It ensures blocks are persisted in sequence without gaps and respects the configured persistence age policy to control how far behind persistence can lag.

!!! info "Processing Logic"
    The method follows these steps:

    1. **Get the last persisted block height** from the state
    2. **Get the current best block** from the blockchain
    3. **If the difference exceeds BlockPersisterPersistAge**, return the next block
    4. **Otherwise, return nil** to indicate no blocks need processing yet

**Parameters:**

- `ctx`: Context for coordinating the block retrieval operation

**Returns:**

- `*model.Block`: The next block to process, or nil if no block needs processing yet
- `error`: Any error encountered during the operation

#### readSubtree

```go
func (u *Server) readSubtree(ctx context.Context, subtreeHash chainhash.Hash) (*subtreepkg.Subtree, error)
```

Retrieves a subtree from the subtree store and deserializes it.

This function is responsible for loading a subtree structure from persistent storage, which contains the hierarchical organization of transactions within a block. It retrieves the subtree file using the provided hash and deserializes it into a usable subtree object.

!!! abstract "Processing Steps"
    The process includes:

    1. **Attempting to read the subtree** from the store using the provided hash
    2. **If the primary read fails**, it attempts to read from a secondary location (FileTypeSubtreeToCheck)
    3. **Deserializing the retrieved subtree data** into a subtree object

**Parameters:**

- `ctx`: Context for the operation, enabling cancellation and timeout handling
- `subtreeHash`: Hash identifier of the subtree to retrieve and deserialize

**Returns:**

- `*subtreepkg.Subtree`: The deserialized subtree object ready for further processing
- `error`: Any error encountered during retrieval or deserialization

#### readSubtreeData

```go
func (u *Server) readSubtreeData(ctx context.Context, subtreeHash chainhash.Hash) (*subtreepkg.SubtreeData, error)
```

Retrieves and deserializes subtree data from the subtree store.

This internal method handles the two-stage process of loading subtree information: first retrieving the subtree structure itself, then loading the associated subtree data that contains the actual transaction references and metadata.

!!! abstract "Processing Steps"
    The function performs these operations:

    1. **Retrieves the subtree structure** from the subtree store using the provided hash
    2. **Deserializes the subtree** to understand its structure and transaction organization
    3. **Retrieves the corresponding subtree data file** containing transaction references
    4. **Deserializes the subtree data** into a usable format for transaction processing

**Parameters:**

- `ctx`: Context for the operation, enabling cancellation and timeout handling
- `subtreeHash`: Hash identifier of the subtree to retrieve and deserialize

**Returns:**

- `*subtreepkg.SubtreeData`: The deserialized subtree data ready for transaction processing
- `error`: Any error encountered during retrieval or deserialization

### Subtree Processing

#### ProcessSubtree

```go
func (u *Server) ProcessSubtree(pCtx context.Context, subtreeHash chainhash.Hash, coinbaseTx *bt.Tx, utxoDiff *utxopersister.UTXOSet) error
```

Processes a subtree of transactions, validating and storing them.

A subtree represents a hierarchical structure containing transaction references that make up part of a block. This method retrieves a subtree from the subtree store, processes all the transactions it contains, and creates subtree data for persistent storage.

!!! note "Processing Steps"
    The process follows these key steps:

    1. **Check if subtree data already exists** - if it does, just set DAH and skip processing
    2. **Retrieve the subtree** from the subtree store using its hash
    3. **Create subtree data** from the subtree structure
    4. **Add coinbase transaction** if the first node is a coinbase placeholder
    5. **Process transaction metadata** using the store
    6. **Serialize and store** the complete subtree data

!!! tip "Performance Optimization"
    The method includes an optimization to skip processing if subtree data already exists, only updating the Delete-At-Height (DAH) setting for persistence.

**Parameters:**

- `pCtx`: Parent context for the operation, used for cancellation and tracing
- `subtreeHash`: Hash identifier of the subtree to process
- `coinbaseTx`: The coinbase transaction for the block containing this subtree
- `utxoDiff`: UTXO set difference tracker for processing transaction changes

**Returns** an error if any part of the subtree processing fails. Errors are wrapped with appropriate context to identify the specific failure point (storage, processing, etc.).

!!! warning "Atomicity Note"
    Processing is not atomic across multiple subtrees - each subtree is processed individually, allowing partial block processing to succeed even if some subtrees fail.

#### WriteTxs

```go
func WriteTxs(_ context.Context, logger ulogger.Logger, writer *filestorer.FileStorer, txs []*bt.Tx, utxoDiff *utxopersister.UTXOSet) error
```

Writes a series of transactions to storage and processes their UTXO changes.

This function handles the final persistence of transaction data to storage and optionally processes UTXO set changes. It's a critical component in the block persistence pipeline that ensures transactions are properly serialized and stored.

!!! abstract "Processing Steps"
    The function performs the following steps:

    1. **For each transaction** in the provided slice:

        - Check for nil transactions and log errors if found
        - Write the raw transaction bytes to storage (using normal bytes, not extended)
        - If a UTXO diff is provided, process the transaction's UTXO changes
    2. **Report any errors** or validation issues encountered

The function includes safety checks to handle nil transactions, logging errors but continuing processing when possible to maximize resilience.

**Parameters:**

- `_`: Context parameter (currently unused in implementation)
- `logger`: Logger for recording operations, errors, and warnings
- `writer`: FileStorer destination for writing serialized transaction data
- `txs`: Slice of transaction objects to write
- `utxoDiff`: UTXO set difference tracker (optional, can be nil if UTXO tracking not needed)

**Returns** an error if writing fails at any point. Specific error conditions include:

- **Failure to write individual transaction data**
- **Errors during UTXO processing** for transactions

!!! warning "Atomicity Consideration"
    The operation is not fully atomic - some transactions may be written successfully even if others fail. The caller should handle partial success scenarios appropriately.

## Configuration

The service uses settings from the `settings.Settings` structure, primarily focused on the Block section. These settings control various aspects of block persistence behavior, from storage locations to processing strategies.

### Block Settings

#### Storage Configuration

- **`Block.StateFile`**: File path for state storage. This file maintains persistence state across service restarts.
- **`Block.BlockStore`**: Block store URL. Defines the location of the blob store used for block data.

#### Network Configuration

- **`Block.PersisterHTTPListenAddress`**: HTTP listener address for the blob server if enabled. Format should be "host:port".

#### Processing Configuration

- **`Block.BlockPersisterPersistAge`**: Age threshold (in blocks) for block persistence. Controls how far behind the current tip blocks will be persisted. Higher values allow more blocks to accumulate before persistence occurs.
- **`Block.BlockPersisterPersistSleep`**: Sleep duration between processing attempts when no blocks are available to process. Specified in milliseconds.
- **`Block.BatchMissingTransactions`**: When true, enables batched retrieval of transaction metadata, which improves performance for high transaction volumes by reducing individual store requests.

### Interaction with Other Components

!!! info "Component Dependencies"
    The BlockPersister service relies on interactions with several other components:

    - **Blockchain Service**: Provides information about the current blockchain state and blocks to be persisted
    - **Block Store**: Persistent storage for complete blocks
    - **Subtree Store**: Storage for block subtrees containing transaction references
    - **UTXO Store**: Storage for the current UTXO set and processing changes

## State Management

The service maintains persistence state through the `state.State` component:

!!! gear "State Management Features"
    - **Tracks last persisted block height** for sequential processing
    - **Manages block hash records** for integrity verification
    - **Provides atomic state updates** for consistency

## Error Handling

!!! warning "Error Handling Strategy"
    The service implements comprehensive error handling:

    - **Storage errors**: Trigger retries after delay
    - **Processing errors**: Logged with context for debugging
    - **Configuration errors**: Prevent service startup
    - **State management errors**: Trigger recovery procedures

## Metrics

The service provides Prometheus metrics for monitoring:

- Block persistence timing
- Subtree validation metrics
- Transaction processing stats
- Store health indicators

## Dependencies

Required components:

- Block Store (blob.Store)
- Subtree Store (blob.Store)
- UTXO Store (utxo.Store)
- Blockchain Client (blockchain.ClientI)
- Logger (ulogger.Logger)
- Settings (settings.Settings)

## Processing Flow

### Block Processing Loop

!!! abstract "Block Processing Steps"
    1. **Check for next block** to process based on persistence age
    2. **Retrieve block data** if available
    3. **Persist block data** to storage
    4. **Update state** with successful persistence
    5. **Sleep** if no blocks available or on error

### Subtree Processing Flow

!!! abstract "Subtree Processing Steps"
    1. **Retrieve subtree data** from store
    2. **Process transaction metadata** for all transactions
    3. **Write transactions** to storage
    4. **Update UTXO set** with changes
    5. **Handle errors** with appropriate recovery

## Health Checks

!!! success "Health Check Types"
    The service implements two types of health checks:

    ### Liveness Check
    - **Basic service health validation**
    - **No dependency checks**
    - **Quick response** for kubernetes probes

    ### Readiness Check
    - **Comprehensive dependency validation**
    - **Store connectivity verification**
    - **Service operational status**

## Other Resources

- [Block Persister](../../topics/services/blockPersister.md)
- [Prometheus Metrics](../prometheusMetrics.md)

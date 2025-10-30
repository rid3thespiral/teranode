package settings

import (
	"net/url"
	"time"

	"github.com/bsv-blockchain/go-chaincfg"
)

// listen mode constants
const (
	ListenModeFull       = "full"
	ListenModeListenOnly = "listen_only"
)

type Settings struct {
	Commit                       string
	Version                      string
	Context                      string
	IsAllInOneMode               bool // Runtime-computed: true if daemon is running multiple services in a single process
	ServiceName                  string
	TracingEnabled               bool
	TracingSampleRate            float64
	TracingCollectorURL          *url.URL
	ClientName                   string
	DataFolder                   string
	SecurityLevelHTTP            int
	ServerCertFile               string
	ServerKeyFile                string
	Logger                       string
	LogLevel                     string
	PrettyLogs                   bool
	JSONLogging                  bool
	ProfilerAddr                 string
	StatsPrefix                  string
	PrometheusEndpoint           string
	HealthCheckHTTPListenAddress string
	UseDatadogProfiler           bool
	LocalTestStartFromState      string
	PostgresCheckAddress         string
	UseCgoVerifier               bool
	GRPCResolver                 string
	GRPCMaxRetries               int
	GRPCRetryBackoff             time.Duration
	SecurityLevelGRPC            int
	UsePrometheusGRPCMetrics     bool
	GRPCAdminAPIKey              string
	ChainCfgParams               *chaincfg.Params
	Policy                       *PolicySettings
	Kafka                        KafkaSettings
	Aerospike                    AerospikeSettings
	Alert                        AlertSettings
	Asset                        AssetSettings
	Block                        BlockSettings
	BlockAssembly                BlockAssemblySettings
	BlockChain                   BlockChainSettings
	BlockValidation              BlockValidationSettings
	Validator                    ValidatorSettings
	Region                       RegionSettings
	Advertising                  AdvertisingSettings
	UtxoStore                    UtxoStoreSettings
	P2P                          P2PSettings
	Coinbase                     CoinbaseSettings
	SubtreeValidation            SubtreeValidationSettings
	Legacy                       LegacySettings
	Propagation                  PropagationSettings
	RPC                          RPCSettings
	Faucet                       FaucetSettings
	Dashboard                    DashboardSettings
	GlobalBlockHeightRetention   uint32
}

// GetUtxoStoreBlockHeightRetention calculates the effective block height retention for UTXO store
// by adding the adjustment to the global value. Returns 0 if the result would be negative.
func (s *Settings) GetUtxoStoreBlockHeightRetention() uint32 {
	result := int64(s.GlobalBlockHeightRetention) + int64(s.UtxoStore.BlockHeightRetentionAdjustment)
	if result < 0 {
		return 0
	}

	return uint32(result)
}

// GetSubtreeValidationBlockHeightRetention calculates the effective block height retention for subtree validation
// by adding the adjustment to the global value. Returns 0 if the result would be negative.
func (s *Settings) GetSubtreeValidationBlockHeightRetention() uint32 {
	result := int64(s.GlobalBlockHeightRetention) + int64(s.SubtreeValidation.BlockHeightRetentionAdjustment)
	if result < 0 {
		return 0
	}

	return uint32(result)
}

type DashboardSettings struct {
	Enabled        bool
	DevServerPorts []int  // Vite dev server ports (e.g., 517, 417)
	WebSocketPort  string // WebSocket port for development environment
	WebSocketPath  string // WebSocket path (e.g., "/connection/websocket")
}

type KafkaSettings struct {
	Blocks                string
	BlocksFinal           string
	BlocksValidate        string
	Hosts                 string
	InvalidBlocks         string
	InvalidSubtrees       string
	LegacyInv             string
	Partitions            int
	Port                  int
	RejectedTx            string
	ReplicationFactor     int
	Subtrees              string
	TxMeta                string
	UnitTest              string
	ValidatorTxsConfig    *url.URL
	TxMetaConfig          *url.URL
	LegacyInvConfig       *url.URL
	BlocksFinalConfig     *url.URL
	RejectedTxConfig      *url.URL
	InvalidBlocksConfig   *url.URL
	InvalidSubtreesConfig *url.URL
	SubtreesConfig        *url.URL
	BlocksConfig          *url.URL
	// TLS settings
	EnableTLS     bool
	TLSSkipVerify bool
	TLSCAFile     string
	TLSCertFile   string
	TLSKeyFile    string
	// Debug logging
	EnableDebugLogging bool
}

type AerospikeSettings struct {
	Debug                  bool
	Host                   string
	BatchPolicyURL         *url.URL
	ReadPolicyURL          *url.URL
	WritePolicyURL         *url.URL
	Port                   int
	UseDefaultBasePolicies bool
	UseDefaultPolicies     bool
	WarmUp                 bool
	StoreBatcherDuration   time.Duration
	StatsRefreshDuration   time.Duration
}

type AlertSettings struct {
	GenesisKeys   []string
	P2PPrivateKey string
	ProtocolID    string
	Store         string
	StoreURL      *url.URL
	TopicName     string
	P2PPort       int
}

type AssetSettings struct {
	APIPrefix               string
	CentrifugeListenAddress string
	CentrifugeDisable       bool
	HTTPAddress             string
	HTTPPublicAddress       string
	HTTPListenAddress       string
	HTTPPort                int
	SignHTTPResponses       bool
	EchoDebug               bool
}

type BlockSettings struct {
	MinedCacheMaxMB                       int
	PersisterStore                        *url.URL
	PersisterHTTPListenAddress            string
	StateFile                             string
	CheckDuplicateTransactionsConcurrency int
	GetAndValidateSubtreesConcurrency     int
	KafkaWorkers                          int
	ValidOrderAndBlessedConcurrency       int
	StoreCacheEnabled                     bool
	StoreCacheSize                        int
	MaxSize                               int
	BlockStore                            *url.URL
	FailFastValidation                    bool
	FinalizeBlockValidationConcurrency    int
	GetMissingTransactions                int
	QuorumTimeout                         time.Duration
	BlockPersisterConcurrency             int
	BatchMissingTransactions              bool
	ProcessTxMetaUsingStoreBatchSize      int
	SkipUTXODelete                        bool
	UTXOPersisterBufferSize               string
	TxStore                               *url.URL
	UTXOPersisterDirect                   bool
	BlockPersisterPersistAge              uint32
	BlockPersisterPersistSleep            time.Duration
	UtxoStore                             *url.URL
	FileStoreReadConcurrency              int
	FileStoreWriteConcurrency             int
}

type BlockChainSettings struct {
	GRPCAddress           string
	GRPCListenAddress     string
	HTTPListenAddress     string
	MaxRetries            int
	RetrySleep            int
	StoreURL              *url.URL
	FSMStateRestore       bool
	FSMStateChangeDelay   time.Duration // used by tests to delay the state change and have time to capture the state
	StoreDBTimeoutMillis  int
	InitializeNodeInState string
}

type BlockAssemblySettings struct {
	Disabled                            bool
	GRPCAddress                         string
	GRPCListenAddress                   string
	GRPCMaxRetries                      int
	GRPCRetryBackoff                    time.Duration
	LocalDAHCache                       string
	MaxBlockReorgCatchup                int
	MaxBlockReorgRollback               int
	MoveBackBlockConcurrency            int
	ProcessRemainderTxHashesConcurrency int
	SendBatchSize                       int
	SendBatchTimeout                    int
	SubtreeProcessorBatcherSize         int
	SubtreeProcessorConcurrentReads     int
	NewSubtreeChanBuffer                int
	SubtreeRetryChanBuffer              int
	SubmitMiningSolutionWaitForResponse bool
	InitialMerkleItemsPerSubtree        int
	MinimumMerkleItemsPerSubtree        int
	MaximumMerkleItemsPerSubtree        int
	DoubleSpendWindow                   time.Duration
	MaxGetReorgHashes                   int
	MinerWalletPrivateKeys              []string
	DifficultyCache                     bool
	UseDynamicSubtreeSize               bool
	MiningCandidateCacheTimeout         time.Duration
	BlockchainSubscriptionTimeout       time.Duration
}

type BlockValidationSettings struct {
	MaxRetries                                       int
	RetrySleep                                       time.Duration
	GRPCAddress                                      string
	GRPCListenAddress                                string
	KafkaWorkers                                     int
	LocalSetTxMinedConcurrency                       int
	MaxPreviousBlockHeadersToCheck                   uint64
	MissingTransactionsBatchSize                     int
	ProcessTxMetaUsingCacheBatchSize                 int
	ProcessTxMetaUsingCacheConcurrency               int
	ProcessTxMetaUsingCacheMissingTxThreshold        int
	ProcessTxMetaUsingStoreBatchSize                 int
	ProcessTxMetaUsingStoreConcurrency               int
	ProcessTxMetaUsingStoreMissingTxThreshold        int
	SkipCheckParentMined                             bool
	SubtreeFoundChConcurrency                        int
	SubtreeValidationAbandonThreshold                int
	ValidateBlockSubtreesConcurrency                 int
	ValidationMaxRetries                             int
	ValidationRetrySleep                             time.Duration
	OptimisticMining                                 bool
	IsParentMinedRetryMaxRetry                       int
	IsParentMinedRetryBackoffMultiplier              int
	SubtreeGroupConcurrency                          int
	BlockFoundChBufferSize                           int
	CatchupChBufferSize                              int
	UseCatchupWhenBehind                             bool
	CatchupConcurrency                               int
	ValidationWarmupCount                            int
	BatchMissingTransactions                         bool
	CheckSubtreeFromBlockTimeout                     time.Duration
	CheckSubtreeFromBlockRetries                     int
	CheckSubtreeFromBlockRetryBackoffDuration        time.Duration
	SecretMiningThreshold                            uint32
	ArePreviousBlocksProcessedMaxRetry               int
	ArePreviousBlocksProcessedRetryBackoffMultiplier int
	PreviousBlockHeaderCount                         uint64
	// Catchup configuration
	CatchupMaxRetries            int // Maximum number of retries for catchup operations
	CatchupIterationTimeout      int // Timeout in seconds for each catchup iteration
	CatchupOperationTimeout      int // Timeout in seconds for the entire catchup operation
	CatchupMaxAccumulatedHeaders int // Maximum headers to accumulate during catchup (default: 100000)
	// Circuit breaker configuration
	CircuitBreakerFailureThreshold int // Number of consecutive failures before opening circuit
	CircuitBreakerSuccessThreshold int // Number of consecutive successes before closing circuit
	CircuitBreakerTimeoutSeconds   int // Timeout in seconds before transitioning from open to half-open
	// Block fetching configuration
	FetchLargeBatchSize     int // Large batches for maximum HTTP efficiency (default: 100, peer limit)
	FetchNumWorkers         int // Number of worker goroutines for parallel processing (default: 16)
	FetchBufferSize         int // Buffer size for channels (default: 500)
	SubtreeFetchConcurrency int // Concurrent subtree fetches per block (default: 8)
	// Transaction extension timeout
	ExtendTransactionTimeout time.Duration // Timeout for extending transactions (default: 120s)
	// Concurrency limits
	GetBlockTransactionsConcurrency int // Concurrency limit for getBlockTransactions (default: 64)
	// Priority queue and fork processing settings
	NearForkThreshold int // Heights within this range are considered "near" forks (default: coinbase maturity / 2)
	MaxParallelForks  int // Maximum number of forks to process in parallel (default: 4)
	MaxTrackedForks   int // Maximum total number of forks to track (default: 1000)
}

type ValidatorSettings struct {
	GRPCAddress               string
	GRPCListenAddress         string
	KafkaWorkers              int
	SendBatchSize             int
	SendBatchTimeout          int
	SendBatchWorkers          int
	BlockValidationDelay      int
	BlockValidationMaxRetries int
	BlockValidationRetrySleep string
	VerboseDebug              bool
	HTTPListenAddress         string
	HTTPAddress               *url.URL
	HTTPRateLimit             int
	KafkaMaxMessageBytes      int // Maximum Kafka message size in bytes for transaction validation
	UseLocalValidator         bool
}

type RegionSettings struct {
	Name string
}

type AdvertisingSettings struct {
	Interval string
	URL      string
}

type UtxoStoreSettings struct {
	UtxoStore                         *url.URL
	BlockHeightRetention              uint32
	UnminedTxRetention                uint32
	ParentPreservationBlocks          uint32
	OutpointBatcherSize               int
	OutpointBatcherDurationMillis     int
	SpendBatcherDurationMillis        int
	SpendBatcherSize                  int
	SpendBatcherConcurrency           int
	StoreBatcherDurationMillis        int
	StoreBatcherSize                  int
	UtxoBatchSize                     int
	IncrementBatcherSize              int
	IncrementBatcherDurationMillis    int
	SetDAHBatcherSize                 int
	SetDAHBatcherDurationMillis       int
	LockedBatcherSize                 int
	LockedBatcherDurationMillis       int
	LongestChainBatcherSize           int
	LongestChainBatcherDurationMillis int
	GetBatcherSize                    int
	GetBatcherDurationMillis          int
	DBTimeout                         time.Duration
	UseExternalTxCache                bool
	ExternalizeAllTransactions        bool
	PostgresMaxIdleConns              int
	PostgresMaxOpenConns              int
	VerboseDebug                      bool
	UpdateTxMinedStatus               bool
	MaxMinedRoutines                  int
	MaxMinedBatchSize                 int
	BlockHeightRetentionAdjustment    int32 // Adjustment to GlobalBlockHeightRetention (can be positive or negative)
	DisableDAHCleaner                 bool  // Disable the DAH cleaner process completely
	// Cleanup-specific settings
	CleanupParentUpdateBatcherSize           int // Batch size for parent record updates during cleanup
	CleanupParentUpdateBatcherDurationMillis int // Batch duration for parent record updates during cleanup (ms)
	CleanupDeleteBatcherSize                 int // Batch size for record deletions during cleanup
	CleanupDeleteBatcherDurationMillis       int // Batch duration for record deletions during cleanup (ms)
	CleanupMaxConcurrentOperations           int // Maximum concurrent operations during cleanup processing
}

type P2PSettings struct {
	BootstrapAddresses []string

	GRPCAddress       string
	GRPCListenAddress string

	HTTPAddress       string
	HTTPListenAddress string

	ListenAddresses    []string
	AdvertiseAddresses []string
	ListenMode         string // "full" (default) or "listen_only"

	PeerID string
	Port   int

	PrivateKey string

	BlockTopic      string
	NodeStatusTopic string // pubsub topic for node status messages
	RejectedTxTopic string
	SubtreeTopic    string

	StaticPeers []string
	RelayPeers  []string // Relay peers for NAT traversal (multiaddr strings)

	// Peer persistence (from go-p2p improvements)
	PeerCacheDir string // Directory for peer cache file (empty = binary directory)

	BanThreshold int
	BanDuration  time.Duration

	// Sync manager configuration
	ForceSyncPeer string // Force sync from specific peer ID, overrides automatic selection

	// Address sharing configuration
	// SharePrivateAddresses controls whether to advertise private/local IP addresses to peers.
	// When true (default), allows local/test environments to work properly.
	// When false, only public addresses are advertised (better for production privacy).
	SharePrivateAddresses bool
	// Peer map cleanup configuration
	PeerMapMaxSize         int           // Maximum entries in peer maps (default: 100000)
	PeerMapTTL             time.Duration // Time-to-live for peer map entries (default: 30m)
	PeerMapCleanupInterval time.Duration // Cleanup interval (default: 5m)

	// Peer health checker configuration
	PeerHealthCheckInterval       time.Duration // Interval between health checks (default: 30s)
	PeerHealthHTTPTimeout         time.Duration // HTTP timeout for DataHub checks (default: 5s)
	PeerHealthRemoveAfterFailures int           // Consecutive failures before removing a peer (default: 3)

	// Node mode configuration (full vs pruned)
	AllowPrunedNodeFallback bool // If true, fall back to pruned nodes when no full nodes available (default: true). Selects youngest pruned node (smallest height) to minimize UTXO pruning risk.
}

type CoinbaseSettings struct {
	DB                    string
	UserPwd               string
	ArbitraryText         string
	GRPCAddress           string
	GRPCListenAddress     string
	NotificationThreshold int
	P2PPeerID             string
	P2PPrivateKey         string
	P2PStaticPeers        []string
	ShouldWait            bool
	Store                 *url.URL
	StoreDBTimeoutMillis  int
	WaitForPeers          bool
	WalletPrivateKey      string
	PeerStatusTimeout     time.Duration
	SlackChannel          string
	SlackToken            string
	TestMode              bool
	P2PPort               int
}

type SubtreeValidationSettings struct {
	QuorumPath                                string
	QuorumAbsoluteTimeout                     time.Duration
	SubtreeStore                              *url.URL
	GetMissingTransactions                    int
	GRPCAddress                               string
	GRPCListenAddress                         string
	ProcessTxMetaUsingCacheBatchSize          int
	ProcessTxMetaUsingCacheConcurrency        int
	ProcessTxMetaUsingCacheMissingTxThreshold int
	SubtreeBlockHeightRetention               uint32
	SubtreeDAHConcurrency                     int
	TxMetaCacheEnabled                        bool
	TxMetaCacheMaxMB                          int
	TxChanBufferSize                          int
	BatchMissingTransactions                  bool
	SpendBatcherSize                          int
	MissingTransactionsBatchSize              int
	PercentageMissingGetFullData              float64
	// BlacklistedBaseURLs is a set of base URLs that are not allowed for subtree validation
	BlacklistedBaseURLs            map[string]struct{}
	BlockHeightRetentionAdjustment int32 // Adjustment to GlobalBlockHeightRetention (can be positive or negative)
	OrphanageTimeout               time.Duration
	// Concurrency limits
	CheckBlockSubtreesConcurrency int           // Concurrency limit for CheckBlockSubtrees operations (default: 32)
	PauseTimeout                  time.Duration // Maximum duration for subtree processing pauses during block validation (default: 5 minutes)
}

type LegacySettings struct {
	WorkingDir                       string
	ListenAddresses                  []string
	ConnectPeers                     []string
	OrphanEvictionDuration           time.Duration
	StoreBatcherSize                 int
	StoreBatcherConcurrency          int
	SpendBatcherSize                 int
	SpendBatcherConcurrency          int
	OutpointBatcherSize              int
	OutpointBatcherConcurrency       int
	PrintInvMessages                 bool
	GRPCAddress                      string
	AllowBlockPriority               bool
	GRPCListenAddress                string
	SavePeers                        bool
	AllowSyncCandidateFromLocalPeers bool
	TempStore                        *url.URL
	PeerIdleTimeout                  time.Duration
	PeerProcessingTimeout            time.Duration
}

type PropagationSettings struct {
	IPv6Addresses        string
	IPv6Interface        string
	GRPCMaxConnectionAge time.Duration
	HTTPListenAddress    string
	HTTPAddresses        []string
	AlwaysUseHTTP        bool
	HTTPRateLimit        int
	SendBatchSize        int
	SendBatchTimeout     int
	GRPCAddresses        []string
	GRPCListenAddress    string
}

type RPCSettings struct {
	RPCUser           string
	RPCPass           string
	RPCLimitUser      string
	RPCLimitPass      string
	RPCMaxClients     int
	RPCQuirks         bool
	RPCListenerURL    *url.URL
	CacheEnabled      bool
	RPCTimeout        time.Duration
	ClientCallTimeout time.Duration
}

type FaucetSettings struct {
	HTTPListenAddress string
}

# Teranode Project Structure

The Teranode project is structured as follows:

```text
teranode/
│
├── main.go                       # Entry point to start the services
│
├── Makefile                      # Facilitates a variety of development and build tasks for the project
│
├── settings.conf                 # Global settings with sensible defaults for all environments
│
├── Dockerfile                    # Main Dockerfile for containerization
│
├── cmd/                          # Directory containing command-line tools and utilities
│   ├── aerospikereader/          # Command related to Aerospike reader functionality
│   ├── bitcointoutxoset/         # Bitcoin to UTXO set utility
│   ├── checkblock/               # Tool to check individual blocks
│   ├── checkblocktemplate/       # Tool to check block templates
│   ├── filereader/               # Utility for reading files
│   ├── getfsmstate/              # Tool to get FSM state
│   ├── keygen/                   # Key generation utility
│   ├── keypairgen/               # Key pair generation utility
│   ├── peercli/                  # Peer network command-line interface
│   ├── resetblockassembly/       # Tool to reset block assembly state
│   ├── seeder/                   # Seeder functionality
│   ├── setfsmstate/              # Tool to set FSM state
│   ├── settings/                 # Settings management tools
│   ├── teranode/                 # Teranode main executable
│   ├── teranodecli/              # Teranode command-line interface
│   ├── utxopersister/            # UTXO persistence utility
│   └── utxovalidator/            # UTXO validation utility
│
├── services/                     # Core service implementations
│   ├── alert/                    # Alert service
│   ├── asset/                    # Asset service
│   ├── blockassembly/            # Block assembly service
│   ├── blockchain/               # Blockchain service
│   ├── blockpersister/           # Block persister service
│   ├── blockvalidation/          # Block validation service
│   ├── legacy/                   # Legacy services
│   ├── p2p/                      # Peer-to-peer networking service
│   ├── propagation/              # Transaction propagation service
│   ├── rpc/                      # RPC service
│   ├── subtreevalidation/        # Subtree validation service
│   ├── utxopersister/            # UTXO persister service
│   └── validator/                # Transaction validator service
│
├── stores/                       # Data storage implementations
│   ├── blob/                     # Blob storage implementation
│   ├── blockchain/               # Blockchain storage implementation
│   ├── cleanup/                  # Cleanup storage utilities
│   ├── txmetacache/             # Transaction metadata cache implementation
│   └── utxo/                     # UTXO storage implementation
│
├── docs/                         # Documentation for the project
│   ├── howto/                    # How-to guides and tutorials
│   ├── misc/                     # Miscellaneous documentation
│   ├── references/               # Reference documentation
│   │   ├── protobuf_docs/        # Protobuf API documentation
│   │   ├── services/            # Service reference documentation
│   │   ├── stores/              # Store reference documentation
│   │   ├── settings/            # Settings reference documentation
│   │   └── kafkaMessageFormat.md # Kafka message format documentation
│   ├── topics/                   # Topic-based documentation
│   └── tutorials/                # Step-by-step tutorials
│
├── compose/                      # Docker compose configurations
│
├── daemon/                       # Daemon implementation
│
├── deploy/                       # Deployment configurations and scripts
│
├── errors/                       # Error handling and definitions
│
├── interfaces/                   # Interface definitions
│
├── model/                        # Data models
│
├── pkg/                          # Reusable packages
│
├── scripts/                      # Various utility scripts
│
├── settings/                     # Settings management implementation
│
├── test/                         # Test utilities and integration tests
│
├── ui/                           # User interface components
│   └── dashboard/                # Teranode Dashboard UI
│
├── ulogger/                      # Unified logging implementation
│
├── util/                         # Common utilities
│
└── venv/                         # Python virtual environment (local development)
```

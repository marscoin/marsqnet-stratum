# marsqnet-stratum

A RandomX mining pool stratum server for Marscoin's **marsqnet** quantum-resistant testnet.

Built in Go. Adapted from [sammy007/monero-stratum](https://github.com/sammy007/monero-stratum) with a full rewrite of the RPC layer, block construction, and hashing for Bitcoin-style nodes running RandomX.

**Live pool:** [mining-mars.com/testnet](https://mining-mars.com/testnet)

## Features

- **RandomX proof-of-work** via CGo bindings to librandomx
- **Bitcoin-style RPC** -- connects to Marscoin Core nodes (not CryptoNote)
- **xmrig-compatible stratum** protocol (login, submit, getjob, keepalived)
- **Block construction** -- coinbase transactions, merkle trees, 80-byte headers
- **Share validation** -- full RandomX hash verification per share
- **Automatic block submission** when a share meets the network target
- **Job broadcasting** -- pushes new work to all miners on new blocks
- **Cookie auth** -- reads RPC credentials from the node's cookie file
- **Multi-port** with configurable per-port difficulty

## Quick Start

### Prerequisites

- Go 1.18+
- A running marsqnet node ([Marscoin Core](https://github.com/marscoin/marscoin) `feat/pow-randomx` branch)
- GCC/G++ for building the RandomX library

### 1. Build the RandomX library

The stratum server links against librandomx. Build it from the marsqnet node source:

```bash
# Assuming marsqnet node source is at /opt/marscoin-marsqnet
RX_SRC=/opt/marscoin-marsqnet/src/crypto/randomx_vendor/src
mkdir -p randomx/lib randomx/include
cp $RX_SRC/randomx.h randomx/include/

# Compile C files
for f in argon2_core.c argon2_ref.c argon2_ssse3.c argon2_avx2.c reciprocal.c virtual_memory.c; do
    gcc -c -fPIC -O2 -maes -mssse3 -mavx2 -I$RX_SRC -I$RX_SRC/blake2 $RX_SRC/$f -o /tmp/rx_$f.o
done
gcc -c -fPIC -O2 -I$RX_SRC/blake2 $RX_SRC/blake2/blake2b.c -o /tmp/rx_blake2b.o
gcc -c -fPIC $RX_SRC/jit_compiler_x86_static.S -o /tmp/rx_jit_x86_static.o

# Compile C++ files
for f in aes_hash.cpp allocator.cpp assembly_generator_x86.cpp blake2_generator.cpp \
         bytecode_machine.cpp cpu.cpp dataset.cpp instruction.cpp instructions_portable.cpp \
         jit_compiler_x86.cpp randomx.cpp soft_aes.cpp superscalar.cpp virtual_machine.cpp \
         vm_compiled.cpp vm_compiled_light.cpp vm_interpreted.cpp vm_interpreted_light.cpp; do
    g++ -c -fPIC -O2 -std=c++11 -maes -mssse3 -I$RX_SRC -I$RX_SRC/blake2 $RX_SRC/$f -o /tmp/rx_$f.o
done

# Create static library
ar rcs randomx/lib/librandomx.a /tmp/rx_*.o
```

### 2. Build the stratum server

```bash
git clone https://github.com/marscoin/marsqnet-stratum.git
cd marsqnet-stratum
go build -o marsqnet-stratum .
```

### 3. Configure

```bash
cp config.example.json config.json
# Edit config.json with your node's RPC port and cookie file path
```

### 4. Run

```bash
./marsqnet-stratum config.json
```

### 5. Connect a miner

```bash
./xmrig -a rx/0 -o stratum+tcp://YOUR_HOST:3434 -u YOUR_ADDRESS.worker1 -p x
```

## Configuration

```json
{
    "address": "mqt1...",
    "chain": "marsqnet",
    "bypassAddressValidation": true,

    "upstream": [{
        "name": "marsqnet-local",
        "host": "127.0.0.1",
        "port": 49332,
        "timeout": "10s",
        "cookieFile": "/var/lib/marscoin-marsqnet/regtest/.cookie"
    }],

    "stratum": {
        "timeout": "120s",
        "listen": [{
            "host": "0.0.0.0",
            "port": 3434,
            "diff": 1000,
            "maxConn": 1024
        }]
    },

    "blockRefreshInterval": "500ms"
}
```

| Field | Description |
|---|---|
| `address` | Pool payout address (marsqnet bech32 `mqt1...`) |
| `upstream.port` | marsqnet node RPC port |
| `upstream.cookieFile` | Path to the node's `.cookie` auth file |
| `stratum.listen.port` | Port miners connect to |
| `stratum.listen.diff` | Share difficulty for miners |
| `blockRefreshInterval` | How often to poll for new block templates |

## Architecture

```
CPU Miners (xmrig)
    │
    │ stratum protocol (login, submit, job)
    ▼
┌──────────────────┐
│ marsqnet-stratum │
│   (Go + RandomX) │
│                  │
│ - Job manager    │
│ - Share validator │
│ - Block submitter │
└────────┬─────────┘
         │ Bitcoin-style JSON-RPC
         ▼
┌──────────────────┐
│  marsqnet node   │
│ (Marscoin Core)  │
│  RandomX PoW     │
└──────────────────┘
```

## How It Works

1. **Template fetch** -- polls the node's `getblocktemplate` every 500ms
2. **Job creation** -- builds a coinbase transaction, computes merkle root, serializes an 80-byte block header
3. **Job distribution** -- sends the header blob + RandomX seed hash to connected miners
4. **Share validation** -- when a miner submits a nonce, the stratum hashes the header with RandomX and checks against the share difficulty
5. **Block submission** -- if a share also meets the network target, the full block is assembled and submitted via `submitblock`

## Project Structure

```
marsqnet-stratum/
├── main.go                 # Entry point
├── config.json             # Runtime configuration
├── pool/
│   └── pool.go             # Config types
├── rpc/
│   └── rpc.go              # Bitcoin-style RPC client with cookie auth
├── blockutil/
│   └── blockutil.go        # Block construction (coinbase, merkle, headers)
├── randomx/
│   ├── randomx.go          # CGo bindings to librandomx
│   ├── include/randomx.h   # RandomX C header
│   └── lib/librandomx.a    # Compiled RandomX library (not committed)
├── stratum/
│   ├── stratum.go          # Stratum server (connections, jobs, shares, blocks)
│   └── proto.go            # Protocol message types
└── cmd/rpctest/
    └── main.go             # Standalone test miner (for development)
```

## Why RandomX?

Marscoin's marsqnet testnet replaces scrypt with RandomX to achieve:

- **CPU-friendly mining** -- anyone with a laptop can mine, no ASICs needed
- **ASIC resistance** -- RandomX is designed to be inefficient on specialized hardware
- **Quantum readiness** -- preparing the network for post-quantum cryptography
- **True decentralization** -- when mining is accessible to everyone, no single entity controls the hashrate

## Credits

- Adapted from [sammy007/monero-stratum](https://github.com/sammy007/monero-stratum) (Go stratum framework)
- [RandomX](https://github.com/tevador/randomx) by tevador (PoW algorithm)
- [Marscoin](https://marscoin.org) -- the cryptocurrency for Mars

## License

Released under the GNU General Public License v2.

http://www.gnu.org/licenses/gpl-2.0.html

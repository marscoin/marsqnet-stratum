# Adapting monero-stratum for marsqnet (RandomX)

## Overview

Fork sammy007/monero-stratum and adapt it for Marscoin's marsqnet testnet
which uses RandomX PoW with Bitcoin-style RPC.

## Key Differences: CryptoNote vs Bitcoin-style

| Aspect | monero-stratum (current) | marsqnet (needed) |
|---|---|---|
| RPC endpoint | `/json_rpc` | Standard HTTP JSON-RPC |
| getblocktemplate | Returns `blocktemplate_blob` + `reserved_offset` | Returns `previousblockhash`, `bits`, `coinbasevalue`, `transactions` |
| submitblock | Submits hex blob | Submits hex-encoded serialized block |
| Block structure | CryptoNote blob | Bitcoin-style header (80 bytes) + txs |
| Hashing | CryptoNight via C lib | RandomX via C lib |
| Address format | CryptoNote base58 (95 chars) | Bech32 `mqt1...` prefix |
| Nonce | 4 bytes at offset 39 in blob | 4 bytes in block header |

## Files to Modify

### 1. `rpc/rpc.go` - RPC Client (REWRITE)

**Current:** Posts to `/json_rpc` with CryptoNote method names.
**New:** Standard Bitcoin JSON-RPC.

```go
// Change URL from /json_rpc to /
rawUrl := fmt.Sprintf("http://%s:%v/", cfg.Host, cfg.Port)

// Add cookie auth or basic auth support
// marsqnet uses RPC cookie file at /var/lib/marscoin-marsqnet/regtest/.cookie

// GetBlockTemplate changes:
// Current: {method: "getblocktemplate", params: {reserve_size, wallet_address}}
// New: {method: "getblocktemplate", params: [{"rules": ["segwit"]}]}

// GetBlockTemplateReply changes:
type GetBlockTemplateReply struct {
    PreviousBlockHash string        `json:"previousblockhash"`
    Transactions      []TxTemplate  `json:"transactions"`
    CoinbaseValue     int64         `json:"coinbasevalue"`
    Bits              string        `json:"bits"`
    Height            int64         `json:"height"`
    CurTime           int64         `json:"curtime"`
    Version           int64         `json:"version"`
    Target            string        `json:"target"`
}

// GetInfo -> getmininginfo + getnetworkinfo
// submitblock stays similar but payload format differs
```

### 2. `hashing/` - Replace CryptoNight with RandomX (REWRITE)

**Current:** Links against CryptoNight C library.
**New:** Link against RandomX from `/opt/marscoin-marsqnet/src/crypto/randomx_vendor/`.

```go
// New hashing interface:
// - Initialize RandomX cache with seed hash (changes every 2048 blocks)
// - Create VM for hash computation
// - Hash block headers instead of CryptoNote blobs

// Key functions needed:
// randomx_alloc_cache() + randomx_init_cache(key)
// randomx_create_vm()
// randomx_calculate_hash(vm, input, input_len, output)

// Seed hash rotation: get from node via getblocktemplate or calculate
// from block height (seed = hash of block at height - (height % 2048))
```

### 3. `cnutil/` - Replace with block construction (REWRITE -> `blockutil/`)

**Current:** `ConvertBlob()` converts CryptoNote block to hashable blob.
**New:** Build Bitcoin-style block header + coinbase + merkle root.

```
Block header (80 bytes):
  - version (4 bytes LE)
  - prev_hash (32 bytes)
  - merkle_root (32 bytes)
  - timestamp (4 bytes LE)
  - bits (4 bytes LE)
  - nonce (4 bytes LE)
```

Need to:
- Build coinbase transaction (with pool payout address)
- Calculate merkle root from coinbase + template transactions
- Serialize block header for hashing

### 4. `util/util.go` - Address Validation (MODIFY)

**Current:** Checks CryptoNote base58 addresses with matching first char.
**New:** Validate marsqnet bech32 addresses (prefix `mqt`).

```go
func ValidateAddress(addr string) bool {
    return strings.HasPrefix(addr, "mqt1") || strings.HasPrefix(addr, "M")
}
```

### 5. `stratum/blocks.go` - Block Template Handling (REWRITE)

**Current:** Stores raw CryptoNote blob, inserts extraNonce at reserved_offset.
**New:** Store parsed template, build header + coinbase per job.

```go
type BlockTemplate struct {
    height        int64
    prevHash      [32]byte
    bits          uint32
    coinbaseValue int64
    version       int32
    curTime       uint32
    transactions  [][]byte
    target        *big.Int
    // Coinbase fields
    extraNonce1   uint32
    coinbaseTx    []byte
}
```

### 6. `stratum/miner.go` - Share Processing (MODIFY)

**Current:** Inserts nonce at offset 39 in CryptoNote blob, hashes with CryptoNight.
**New:** Build block header with submitted nonce, hash with RandomX.

### 7. `pool/pool.go` - Config (ADD FIELDS)

```go
type Config struct {
    // ... existing ...
    Chain     string `json:"chain"`     // "marsqnet"
    CookieFile string `json:"cookieFile"` // RPC cookie auth
    SeedHash   string `json:"seedHash"`   // RandomX seed (auto-updated)
}
```

## Build Dependencies

- Go 1.19+
- RandomX C library (already at `/opt/marscoin-marsqnet/src/crypto/randomx_vendor/`)
- CGo for RandomX bindings

## Runtime Config (config.json)

```json
{
    "chain": "marsqnet",
    "address": "mqt1...",
    "upstream": [{
        "name": "marsqnet",
        "host": "127.0.0.1",
        "port": 49332,
        "timeout": "10s"
    }],
    "cookieFile": "/var/lib/marscoin-marsqnet/regtest/.cookie",
    "stratum": {
        "timeout": "120s",
        "listen": [{
            "host": "0.0.0.0",
            "port": 3434,
            "diff": 10000,
            "maxConn": 1024
        }]
    },
    "blockRefreshInterval": "500ms",
    "frontend": {
        "enabled": false
    }
}
```

## Implementation Order

1. RPC layer (can test immediately against live marsqnet node)
2. Block construction (build headers, coinbase, merkle)
3. RandomX hashing (CGo bindings)
4. Share validation (tie it all together)
5. Integration test against marsqnet node

## Testing Strategy

Per the spec's rollout phases:
1. **Simulation mode**: Fetch templates, log what we'd do
2. **Shadow mode**: Validate shares, no submission
3. **Active mode**: Full mining

## MarsForge Integration

- Add `/api/marsqnet/pool` endpoint reading from marsqnet stratum stats
- Network switcher toggle in navbar
- Separate dashboard view for testnet metrics

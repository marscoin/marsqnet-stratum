package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/marscoin/marsqnet-stratum/blockutil"
	"github.com/marscoin/marsqnet-stratum/pool"
	"github.com/marscoin/marsqnet-stratum/randomx"
	"github.com/marscoin/marsqnet-stratum/rpc"
)

func main() {
	rand.Seed(time.Now().UnixNano())

	cfg := pool.Upstream{
		Name:       "marsqnet-local",
		Host:       "127.0.0.1",
		Port:       49332,
		Timeout:    "10s",
		CookieFile: "/var/lib/marscoin-marsqnet/regtest/.cookie",
	}

	client, err := rpc.NewRPCClient(&cfg)
	if err != nil {
		log.Fatal("Failed to create RPC client:", err)
	}

	// 1. Get chain info
	fmt.Println("=== Chain Info ===")
	mining, err := client.GetMiningInfo()
	if err != nil {
		log.Fatal("getmininginfo:", err)
	}
	fmt.Printf("Chain: %s, Height: %d, Difficulty: %f\n\n", mining.Chain, mining.Blocks, mining.Difficulty)

	// 2. Get block template
	fmt.Println("=== Block Template ===")
	tmpl, err := client.GetBlockTemplate()
	if err != nil {
		log.Fatal("getblocktemplate:", err)
	}
	data, _ := json.MarshalIndent(tmpl, "", "  ")
	fmt.Printf("%s\n\n", data)

	// 3. Build a block from the template
	fmt.Println("=== Building Block ===")

	prevHash, err := blockutil.ReverseHexToBytes(tmpl.PreviousBlockHash)
	if err != nil {
		log.Fatal("prevhash decode:", err)
	}

	bits, err := blockutil.DecodeBits(tmpl.Bits)
	if err != nil {
		log.Fatal("bits decode:", err)
	}

	// Get a payout scriptPubKey from the node
	// For testing, use OP_TRUE (0x51) as payout script
	payoutScript := []byte{0x51}

	// Build coinbase
	extraNonce1 := rand.Uint32()
	coinbaseTx := blockutil.BuildCoinbaseTx(
		tmpl.Height,
		tmpl.CoinbaseValue,
		payoutScript,
		extraNonce1,
		0, // extraNonce2
		nil, // no witness commitment for now
	)
	fmt.Printf("Coinbase TX: %d bytes\n", len(coinbaseTx))
	fmt.Printf("Coinbase hex: %s\n\n", hex.EncodeToString(coinbaseTx))

	// Compute coinbase hash
	coinbaseHash := blockutil.TxHash(coinbaseTx)

	// Collect transaction hashes for merkle tree
	txHashes := [][32]byte{coinbaseHash}
	var txDataList [][]byte
	for _, tx := range tmpl.Transactions {
		txBytes, _ := hex.DecodeString(tx.Data)
		txDataList = append(txDataList, txBytes)

		var txHash [32]byte
		// Use the txid from the template (already in internal order for merkle)
		hashBytes, _ := blockutil.ReverseHexToBytes(tx.TxID)
		txHash = hashBytes
		txHashes = append(txHashes, txHash)
	}

	// Compute merkle root
	merkleRoot := blockutil.MerkleRoot(txHashes)
	fmt.Printf("Merkle root: %s\n", hex.EncodeToString(merkleRoot[:]))

	// Build header
	header := &blockutil.BlockHeader{
		Version:    int32(tmpl.Version),
		PrevHash:   prevHash,
		MerkleRoot: merkleRoot,
		Timestamp:  uint32(tmpl.CurTime),
		Bits:       bits,
		Nonce:      0,
	}

	target, err := blockutil.TargetFromHex(tmpl.Target)
	if err != nil {
		log.Fatal("target parse:", err)
	}
	fmt.Printf("Target: %s\n", hex.EncodeToString(target[:]))
	fmt.Printf("Header (80 bytes): %s\n\n", hex.EncodeToString(header.Serialize()))

	// 4. Initialize RandomX VM
	fmt.Println("=== Initializing RandomX ===")
	// Use the previous block hash as seed key (simplified - real impl uses seed height)
	seedKey, _ := hex.DecodeString(tmpl.PreviousBlockHash)
	vm, err := randomx.NewVM(seedKey)
	if err != nil {
		log.Fatal("RandomX init failed:", err)
	}
	defer vm.Close()
	fmt.Println("RandomX VM ready")

	// 5. Mine with RandomX!
	fmt.Println("\n=== Mining with RandomX ===")
	startTime := time.Now()

	for nonce := uint32(0); nonce < 100000000; nonce++ {
		header.Nonce = nonce
		headerBytes := header.Serialize()
		hash := vm.Hash(headerBytes)

		if nonce%10000 == 0 && nonce > 0 {
			elapsed := time.Since(startTime).Seconds()
			rate := float64(nonce) / elapsed
			fmt.Printf("  Nonce %d, %.0f H/s\n", nonce, rate)
		}

		if blockutil.HashMeetsTarget(hash, target) {
			fmt.Printf("Found valid hash at nonce %d!\n", nonce)
			fmt.Printf("Hash: %s\n", blockutil.BytesToReverseHex(hash))

			// Serialize the full block
			fullBlock := blockutil.SerializeBlock(header, coinbaseTx, txDataList)
			blockHex := hex.EncodeToString(fullBlock)
			fmt.Printf("Block size: %d bytes\n\n", len(fullBlock))

			// Try submitting
			fmt.Println("=== Submitting block ===")
			err := client.SubmitBlock(blockHex)
			if err != nil {
				fmt.Printf("Submit result: REJECTED - %v\n", err)
				fmt.Println("(Expected: marsqnet uses RandomX, not SHA256d)")
			} else {
				fmt.Println("Submit result: ACCEPTED!")
				// Check new height
				newHeight, _ := client.GetBlockCount()
				fmt.Printf("New height: %d\n", newHeight)
			}
			break
		}
	}

	fmt.Println("\n=== Test complete ===")
}

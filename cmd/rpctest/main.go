package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marscoin/marsqnet-stratum/blockutil"
	"github.com/marscoin/marsqnet-stratum/pool"
	"github.com/marscoin/marsqnet-stratum/randomx"
	"github.com/marscoin/marsqnet-stratum/rpc"
)

func main() {
	rand.Seed(time.Now().UnixNano())
	numCPU := runtime.NumCPU()
	runtime.GOMAXPROCS(numCPU)

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

	// Get chain info
	mining, err := client.GetMiningInfo()
	if err != nil {
		log.Fatal("getmininginfo:", err)
	}
	fmt.Printf("Chain: %s, Height: %d, Difficulty: %f\n", mining.Chain, mining.Blocks, mining.Difficulty)

	// Get block template
	tmpl, err := client.GetBlockTemplate()
	if err != nil {
		log.Fatal("getblocktemplate:", err)
	}
	data, _ := json.MarshalIndent(tmpl, "", "  ")
	fmt.Printf("Template:\n%s\n\n", data)

	// Build block components
	prevHash, err := blockutil.ReverseHexToBytes(tmpl.PreviousBlockHash)
	if err != nil {
		log.Fatal("prevhash:", err)
	}
	bits, err := blockutil.DecodeBits(tmpl.Bits)
	if err != nil {
		log.Fatal("bits:", err)
	}
	target, err := blockutil.TargetFromHex(tmpl.Target)
	if err != nil {
		log.Fatal("target:", err)
	}

	payoutScript := []byte{0x51} // OP_TRUE for testnet
	extraNonce1 := rand.Uint32()

	coinbaseTx := blockutil.BuildCoinbaseTx(
		tmpl.Height, tmpl.CoinbaseValue, payoutScript,
		extraNonce1, 0, nil,
	)
	coinbaseHash := blockutil.TxHash(coinbaseTx)
	merkleRoot := blockutil.MerkleRoot([][32]byte{coinbaseHash})

	fmt.Printf("Mining block %d with %d CPU cores (RandomX)\n", tmpl.Height, numCPU)
	fmt.Printf("Target: %s\n\n", hex.EncodeToString(target[:]))

	// Initialize RandomX VMs - one per thread
	seedKey, _ := hex.DecodeString(tmpl.PreviousBlockHash)
	fmt.Printf("Initializing %d RandomX VMs...\n", numCPU)

	vms := make([]*randomx.VM, numCPU)
	for i := 0; i < numCPU; i++ {
		vm, err := randomx.NewVM(seedKey)
		if err != nil {
			log.Fatalf("RandomX VM %d init failed: %v", i, err)
		}
		defer vm.Close()
		vms[i] = vm
	}
	fmt.Println("All VMs ready. Mining...\n")

	// Multi-threaded mining
	var found int32
	var totalHashes int64
	startTime := time.Now()

	// Stats printer
	go func() {
		for {
			time.Sleep(5 * time.Second)
			if atomic.LoadInt32(&found) > 0 {
				return
			}
			h := atomic.LoadInt64(&totalHashes)
			elapsed := time.Since(startTime).Seconds()
			fmt.Printf("  [%s] %d hashes, %.1f H/s (%d threads)\n",
				time.Now().Format("15:04:05"), h, float64(h)/elapsed, numCPU)
		}
	}()

	var wg sync.WaitGroup
	nonceRange := uint32(0xFFFFFFFF) / uint32(numCPU)

	for t := 0; t < numCPU; t++ {
		wg.Add(1)
		go func(threadID int, vm *randomx.VM) {
			defer wg.Done()
			startNonce := uint32(threadID) * nonceRange
			endNonce := startNonce + nonceRange

			header := &blockutil.BlockHeader{
				Version:    int32(tmpl.Version),
				PrevHash:   prevHash,
				MerkleRoot: merkleRoot,
				Timestamp:  uint32(tmpl.CurTime),
				Bits:       bits,
				Nonce:      startNonce,
			}

			for nonce := startNonce; nonce < endNonce; nonce++ {
				if atomic.LoadInt32(&found) > 0 {
					return
				}

				header.Nonce = nonce
				headerBytes := header.Serialize()
				hash := vm.Hash(headerBytes)
				atomic.AddInt64(&totalHashes, 1)

				if blockutil.HashMeetsTarget(hash, target) {
					if !atomic.CompareAndSwapInt32(&found, 0, 1) {
						return // another thread found it
					}

					elapsed := time.Since(startTime).Seconds()
					h := atomic.LoadInt64(&totalHashes)
					fmt.Printf("\n*** BLOCK FOUND! ***\n")
					fmt.Printf("Thread: %d\n", threadID)
					fmt.Printf("Nonce: %d\n", nonce)
					fmt.Printf("Hash: %s\n", blockutil.BytesToReverseHex(hash))
					fmt.Printf("Hashes: %d in %.1fs (%.1f H/s)\n\n", h, elapsed, float64(h)/elapsed)

					// Build and submit
					fullBlock := blockutil.SerializeBlock(header, coinbaseTx, nil)
					blockHex := hex.EncodeToString(fullBlock)
					fmt.Printf("Block size: %d bytes\n", len(fullBlock))
					fmt.Println("Submitting to marsqnet node...")

					err := client.SubmitBlock(blockHex)
					if err != nil {
						fmt.Printf("REJECTED: %v\n", err)
					} else {
						fmt.Println("*** ACCEPTED! Block submitted successfully! ***")
						newHeight, _ := client.GetBlockCount()
						fmt.Printf("New chain height: %d\n", newHeight)
					}
					return
				}
			}
		}(t, vms[t])
	}

	wg.Wait()

	if atomic.LoadInt32(&found) == 0 {
		h := atomic.LoadInt64(&totalHashes)
		fmt.Printf("Exhausted nonce range (%d hashes) without finding block\n", h)
	}
}

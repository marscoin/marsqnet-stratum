package blockutil

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// BlockHeader is an 80-byte Bitcoin-style block header
type BlockHeader struct {
	Version    int32
	PrevHash   [32]byte
	MerkleRoot [32]byte
	Timestamp  uint32
	Bits       uint32
	Nonce      uint32
}

// Serialize produces the 80-byte header for hashing
func (h *BlockHeader) Serialize() []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, h.Version)
	buf.Write(h.PrevHash[:])
	buf.Write(h.MerkleRoot[:])
	binary.Write(buf, binary.LittleEndian, h.Timestamp)
	binary.Write(buf, binary.LittleEndian, h.Bits)
	binary.Write(buf, binary.LittleEndian, h.Nonce)
	return buf.Bytes()
}

// SerializeBlock produces the full serialized block (header + tx count + transactions)
func SerializeBlock(header *BlockHeader, coinbaseTx []byte, txData [][]byte) []byte {
	buf := new(bytes.Buffer)
	buf.Write(header.Serialize())
	WriteVarInt(buf, uint64(1+len(txData))) // coinbase + other txs
	buf.Write(coinbaseTx)
	for _, tx := range txData {
		buf.Write(tx)
	}
	return buf.Bytes()
}

// BuildCoinbaseTx constructs a coinbase transaction
// extraNonce is used by the pool to create unique work per miner
func BuildCoinbaseTx(height int64, coinbaseValue int64, payoutAddress []byte, extraNonce1 uint32, extraNonce2 uint64, witnessCommitment []byte) []byte {
	buf := new(bytes.Buffer)

	// Version
	binary.Write(buf, binary.LittleEndian, int32(2))

	// Segwit marker + flag
	hasWitness := len(witnessCommitment) > 0
	if hasWitness {
		buf.WriteByte(0x00) // marker
		buf.WriteByte(0x01) // flag
	}

	// Input count: 1 (coinbase)
	WriteVarInt(buf, 1)

	// Coinbase input
	// prev_hash: 32 bytes of zeros
	buf.Write(make([]byte, 32))
	// prev_index: 0xffffffff
	binary.Write(buf, binary.LittleEndian, uint32(0xffffffff))

	// scriptSig: height + extraNonce
	scriptSig := new(bytes.Buffer)
	// BIP34: encode height
	heightBytes := serializeScriptNum(height)
	scriptSig.WriteByte(byte(len(heightBytes)))
	scriptSig.Write(heightBytes)
	// Extra nonce (8 bytes: 4 from pool + 4 from miner)
	binary.Write(scriptSig, binary.LittleEndian, extraNonce1)
	binary.Write(scriptSig, binary.LittleEndian, uint32(extraNonce2))
	// Pool tag
	tag := []byte("/marsqnet-stratum/")
	scriptSig.Write(tag)

	WriteVarInt(buf, uint64(scriptSig.Len()))
	buf.Write(scriptSig.Bytes())
	// Sequence
	binary.Write(buf, binary.LittleEndian, uint32(0xffffffff))

	// Outputs
	if hasWitness {
		// 2 outputs: payout + witness commitment
		WriteVarInt(buf, 2)
	} else {
		WriteVarInt(buf, 1)
	}

	// Output 0: payout to pool address
	binary.Write(buf, binary.LittleEndian, coinbaseValue)
	WriteVarInt(buf, uint64(len(payoutAddress)))
	buf.Write(payoutAddress)

	// Output 1: witness commitment (if segwit)
	if hasWitness {
		binary.Write(buf, binary.LittleEndian, int64(0)) // 0 value
		// OP_RETURN + commitment
		commitScript := append([]byte{0x6a, 0x24, 0xaa, 0x21, 0xa9, 0xed}, witnessCommitment...)
		WriteVarInt(buf, uint64(len(commitScript)))
		buf.Write(commitScript)
	}

	// Witness data for coinbase (if segwit)
	if hasWitness {
		// 1 witness item: 32 bytes of zeros
		WriteVarInt(buf, 1)
		WriteVarInt(buf, 32)
		buf.Write(make([]byte, 32))
	}

	// Locktime
	binary.Write(buf, binary.LittleEndian, uint32(0))

	return buf.Bytes()
}

// MerkleRoot computes the merkle root from a list of transaction hashes
func MerkleRoot(txHashes [][32]byte) [32]byte {
	if len(txHashes) == 0 {
		return [32]byte{}
	}
	if len(txHashes) == 1 {
		return txHashes[0]
	}

	for len(txHashes) > 1 {
		if len(txHashes)%2 != 0 {
			txHashes = append(txHashes, txHashes[len(txHashes)-1])
		}
		var next [][32]byte
		for i := 0; i < len(txHashes); i += 2 {
			combined := append(txHashes[i][:], txHashes[i+1][:]...)
			next = append(next, DoubleSHA256(combined))
		}
		txHashes = next
	}
	return txHashes[0]
}

// DoubleSHA256 computes SHA256(SHA256(data))
func DoubleSHA256(data []byte) [32]byte {
	first := sha256.Sum256(data)
	return sha256.Sum256(first[:])
}

// TxHash computes the double-SHA256 hash of a raw transaction
func TxHash(rawTx []byte) [32]byte {
	return DoubleSHA256(rawTx)
}

// ReverseHexToBytes converts a hex string (like prevhash from RPC) to
// bytes in internal byte order (reversed)
func ReverseHexToBytes(hexStr string) ([32]byte, error) {
	var result [32]byte
	decoded, err := hex.DecodeString(hexStr)
	if err != nil {
		return result, fmt.Errorf("invalid hex: %w", err)
	}
	if len(decoded) != 32 {
		return result, fmt.Errorf("expected 32 bytes, got %d", len(decoded))
	}
	// Reverse byte order
	for i := 0; i < 32; i++ {
		result[i] = decoded[31-i]
	}
	return result, nil
}

// BytesToReverseHex converts internal byte order back to RPC hex format
func BytesToReverseHex(b [32]byte) string {
	reversed := make([]byte, 32)
	for i := 0; i < 32; i++ {
		reversed[i] = b[31-i]
	}
	return hex.EncodeToString(reversed)
}

// DecodeBits converts compact "bits" representation to a uint32
func DecodeBits(bitsHex string) (uint32, error) {
	b, err := hex.DecodeString(bitsHex)
	if err != nil {
		return 0, err
	}
	if len(b) != 4 {
		return 0, fmt.Errorf("bits must be 4 bytes, got %d", len(b))
	}
	return binary.BigEndian.Uint32(b), nil
}

// BitsToTarget converts compact bits to full 256-bit target
func BitsToTarget(bits uint32) [32]byte {
	var target [32]byte
	exponent := bits >> 24
	mantissa := bits & 0x007fffff
	if exponent <= 3 {
		mantissa >>= 8 * (3 - exponent)
		target[31] = byte(mantissa)
		target[30] = byte(mantissa >> 8)
		target[29] = byte(mantissa >> 16)
	} else {
		pos := 32 - int(exponent)
		if pos >= 0 && pos < 30 {
			target[pos+2] = byte(mantissa)
			target[pos+1] = byte(mantissa >> 8)
			target[pos] = byte(mantissa >> 16)
		}
	}
	return target
}

// HashMeetsTarget checks if a hash is below the target
// Both are in standard byte order (big-endian: index 0 = most significant)
func HashMeetsTarget(hash [32]byte, target [32]byte) bool {
	for i := 0; i < 32; i++ {
		if hash[i] < target[i] {
			return true
		}
		if hash[i] > target[i] {
			return false
		}
	}
	return true // equal
}

// TargetFromHex parses the target hex string from getblocktemplate
// Returns target in standard byte order (big-endian)
func TargetFromHex(targetHex string) ([32]byte, error) {
	var target [32]byte
	decoded, err := hex.DecodeString(targetHex)
	if err != nil {
		return target, err
	}
	if len(decoded) != 32 {
		return target, fmt.Errorf("target must be 32 bytes, got %d", len(decoded))
	}
	copy(target[:], decoded)
	return target, nil
}

// WriteVarInt writes a Bitcoin-style variable length integer
func WriteVarInt(buf *bytes.Buffer, n uint64) {
	if n < 0xfd {
		buf.WriteByte(byte(n))
	} else if n <= 0xffff {
		buf.WriteByte(0xfd)
		binary.Write(buf, binary.LittleEndian, uint16(n))
	} else if n <= 0xffffffff {
		buf.WriteByte(0xfe)
		binary.Write(buf, binary.LittleEndian, uint32(n))
	} else {
		buf.WriteByte(0xff)
		binary.Write(buf, binary.LittleEndian, n)
	}
}

// serializeScriptNum encodes an int64 as a script number for BIP34
func serializeScriptNum(n int64) []byte {
	if n == 0 {
		return []byte{}
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var result []byte
	for n > 0 {
		result = append(result, byte(n&0xff))
		n >>= 8
	}
	if result[len(result)-1]&0x80 != 0 {
		if negative {
			result = append(result, 0x80)
		} else {
			result = append(result, 0x00)
		}
	} else if negative {
		result[len(result)-1] |= 0x80
	}
	return result
}

// ScriptPubKeyFromAddress creates a P2PKH scriptPubKey from raw address bytes
// For now, accepts raw scriptPubKey bytes directly
func ScriptPubKeyFromHex(scriptHex string) ([]byte, error) {
	return hex.DecodeString(scriptHex)
}

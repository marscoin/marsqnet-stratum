package randomx

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -L${SRCDIR}/lib -lrandomx -lstdc++ -lm -lpthread
#include <stdlib.h>
#include "randomx.h"
*/
import "C"
import (
	"fmt"
	"sync"
	"unsafe"
)

// VM wraps a RandomX virtual machine for hashing
type VM struct {
	mu    sync.Mutex
	cache *C.randomx_cache
	vm    *C.randomx_vm
	flags C.randomx_flags
}

// NewVM creates a new RandomX VM initialized with a seed key
// The seed key determines the RandomX program (rotates every N blocks)
func NewVM(seedKey []byte) (*VM, error) {
	flags := C.randomx_get_flags()
	// Use light mode (cache only, no full dataset) - saves 2GB RAM
	// Don't set RANDOMX_FLAG_FULL_MEM

	cache := C.randomx_alloc_cache(flags)
	if cache == nil {
		return nil, fmt.Errorf("failed to allocate RandomX cache")
	}

	C.randomx_init_cache(cache, unsafe.Pointer(&seedKey[0]), C.size_t(len(seedKey)))

	vm := C.randomx_create_vm(flags, cache, nil)
	if vm == nil {
		C.randomx_release_cache(cache)
		return nil, fmt.Errorf("failed to create RandomX VM")
	}

	return &VM{
		cache: cache,
		vm:    vm,
		flags: flags,
	}, nil
}

// Hash computes the RandomX hash of the input data
// Returns a 32-byte hash
func (v *VM) Hash(input []byte) [32]byte {
	var result [32]byte
	v.mu.Lock()
	C.randomx_calculate_hash(v.vm, unsafe.Pointer(&input[0]), C.size_t(len(input)), unsafe.Pointer(&result[0]))
	v.mu.Unlock()
	return result
}

// UpdateSeed re-initializes the VM with a new seed key
// Call this when the RandomX seed changes (every N blocks)
func (v *VM) UpdateSeed(seedKey []byte) {
	v.mu.Lock()
	C.randomx_init_cache(v.cache, unsafe.Pointer(&seedKey[0]), C.size_t(len(seedKey)))
	v.mu.Unlock()
}

// Close releases all RandomX resources
func (v *VM) Close() {
	v.mu.Lock()
	if v.vm != nil {
		C.randomx_destroy_vm(v.vm)
		v.vm = nil
	}
	if v.cache != nil {
		C.randomx_release_cache(v.cache)
		v.cache = nil
	}
	v.mu.Unlock()
}

package stratum

import (
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
)

// Share rejection reason counters
type RejectCounters struct {
	InvalidJobId     int64
	StaleJob         int64
	DuplicateNonce   int64
	BadNonce         int64
	BadHash          int64
	LowDifficulty    int64
	SubmitBlockError int64
}

// Metrics endpoint for Prometheus-style scraping
func (s *StratumServer) StartMetricsServer(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/health", s.handleHealth)

	log.Printf("Metrics listening on %s", addr)
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("Metrics server error: %v", err)
		}
	}()
}

func (s *StratumServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.sessionsMu.RLock()
	miners := len(s.sessions)
	s.sessionsMu.RUnlock()

	t := s.currentTemplate()
	var height int64
	var difficulty string = "0"
	if t != nil {
		height = t.height
		if t.difficulty != nil {
			difficulty = t.difficulty.String()
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	fmt.Fprintf(w, "# HELP marsqnet_stratum_up Whether the stratum server is up\n")
	fmt.Fprintf(w, "# TYPE marsqnet_stratum_up gauge\n")
	fmt.Fprintf(w, "marsqnet_stratum_up 1\n\n")

	fmt.Fprintf(w, "# HELP marsqnet_stratum_miners_connected Current miners connected\n")
	fmt.Fprintf(w, "# TYPE marsqnet_stratum_miners_connected gauge\n")
	fmt.Fprintf(w, "marsqnet_stratum_miners_connected %d\n\n", miners)

	fmt.Fprintf(w, "# HELP marsqnet_stratum_height Current block template height\n")
	fmt.Fprintf(w, "# TYPE marsqnet_stratum_height gauge\n")
	fmt.Fprintf(w, "marsqnet_stratum_height %d\n\n", height)

	fmt.Fprintf(w, "# HELP marsqnet_stratum_difficulty Current network difficulty\n")
	fmt.Fprintf(w, "# TYPE marsqnet_stratum_difficulty gauge\n")
	fmt.Fprintf(w, "marsqnet_stratum_difficulty %s\n\n", difficulty)

	fmt.Fprintf(w, "# HELP marsqnet_stratum_shares_total Total shares received\n")
	fmt.Fprintf(w, "# TYPE marsqnet_stratum_shares_total counter\n")
	fmt.Fprintf(w, "marsqnet_stratum_shares_total %d\n\n", atomic.LoadInt64(&s.totalShares))

	fmt.Fprintf(w, "# HELP marsqnet_stratum_shares_invalid Invalid shares (with reason labels)\n")
	fmt.Fprintf(w, "# TYPE marsqnet_stratum_shares_invalid counter\n")
	fmt.Fprintf(w, "marsqnet_stratum_shares_invalid{reason=\"invalid_job\"} %d\n", atomic.LoadInt64(&s.rejects.InvalidJobId))
	fmt.Fprintf(w, "marsqnet_stratum_shares_invalid{reason=\"stale_job\"} %d\n", atomic.LoadInt64(&s.rejects.StaleJob))
	fmt.Fprintf(w, "marsqnet_stratum_shares_invalid{reason=\"duplicate_nonce\"} %d\n", atomic.LoadInt64(&s.rejects.DuplicateNonce))
	fmt.Fprintf(w, "marsqnet_stratum_shares_invalid{reason=\"bad_nonce\"} %d\n", atomic.LoadInt64(&s.rejects.BadNonce))
	fmt.Fprintf(w, "marsqnet_stratum_shares_invalid{reason=\"bad_hash\"} %d\n", atomic.LoadInt64(&s.rejects.BadHash))
	fmt.Fprintf(w, "marsqnet_stratum_shares_invalid{reason=\"low_difficulty\"} %d\n\n", atomic.LoadInt64(&s.rejects.LowDifficulty))

	fmt.Fprintf(w, "# HELP marsqnet_stratum_blocks_found Total blocks found\n")
	fmt.Fprintf(w, "# TYPE marsqnet_stratum_blocks_found counter\n")
	fmt.Fprintf(w, "marsqnet_stratum_blocks_found %d\n\n", atomic.LoadInt64(&s.blocksMined))

	fmt.Fprintf(w, "# HELP marsqnet_stratum_blocks_rejected Blocks rejected by node\n")
	fmt.Fprintf(w, "# TYPE marsqnet_stratum_blocks_rejected counter\n")
	fmt.Fprintf(w, "marsqnet_stratum_blocks_rejected %d\n", atomic.LoadInt64(&s.rejects.SubmitBlockError))
}

func (s *StratumServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	t := s.currentTemplate()
	if t == nil {
		http.Error(w, "no template", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "OK height=%d\n", t.height)
}

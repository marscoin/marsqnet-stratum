package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/marscoin/marsqnet-stratum/pool"
	"github.com/marscoin/marsqnet-stratum/stratum"
)

func main() {
	var cfg pool.Config
	readConfig(&cfg)

	if cfg.Threads > 0 {
		runtime.GOMAXPROCS(cfg.Threads)
	}

	log.Println("==============================================")
	log.Println("  marsqnet-stratum: RandomX Pool for Marscoin")
	log.Println("  Quantum-resistant testnet mining")
	log.Println("==============================================")

	s := stratum.NewStratumServer(&cfg)
	s.Listen()
}

func readConfig(cfg *pool.Config) {
	configFile := "config.json"
	if len(os.Args) > 1 {
		configFile = os.Args[1]
	}
	configFile, _ = filepath.Abs(configFile)
	log.Printf("Loading config: %s", configFile)

	f, err := os.Open(configFile)
	if err != nil {
		log.Fatal("Config file error:", err)
	}
	defer f.Close()

	if err := json.NewDecoder(f).Decode(cfg); err != nil {
		log.Fatal("Config parse error:", err)
	}
}

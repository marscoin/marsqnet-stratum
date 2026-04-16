package stratum

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marscoin/marsqnet-stratum/blockutil"
	"github.com/marscoin/marsqnet-stratum/pool"
	"github.com/marscoin/marsqnet-stratum/randomx"
	"github.com/marscoin/marsqnet-stratum/rpc"
)

const MaxReqSize = 10 * 1024

// BlockTemplate holds the current work
type BlockTemplate struct {
	height         int64
	prevHash       [32]byte
	prevHashHex    string
	bits           uint32
	target         [32]byte
	version        int32
	curTime        uint32
	coinbaseValue  int64
	transactions   [][]byte
	txHashes       [][32]byte
	difficulty     *big.Int
	seedHash       string // RandomX seed hash (= previous block hash for now)
}

type Job struct {
	id         string
	height     int64
	headerBlob []byte // 80-byte header the miner hashes
	coinbaseTx []byte
	txData     [][]byte
	nonces     map[string]struct{}
	sync.Mutex
}

type Session struct {
	sync.Mutex
	conn     *net.TCPConn
	enc      *json.Encoder
	ip       string
	login    string
	agent    string
	lastBeat int64
	jobs     []*Job
}

type StratumServer struct {
	config       *pool.Config
	rpcClient    *rpc.RPCClient
	rxVM         *randomx.VM
	template     atomic.Value // *BlockTemplate
	sessions     map[*Session]struct{}
	sessionsMu   sync.RWMutex
	jobCounter   uint64
	extraNonce   uint32
	roundShares  int64
	blocksMined  int64

	// Stats
	totalShares   int64
	invalidShares int64
	staleShares   int64
	rejects       RejectCounters
}

func NewStratumServer(cfg *pool.Config) *StratumServer {
	// Connect to marsqnet node
	upstream := cfg.Upstream[0]
	client, err := rpc.NewRPCClient(&upstream)
	if err != nil {
		log.Fatal("RPC client failed:", err)
	}
	log.Printf("Connected to upstream: %s:%d", upstream.Host, upstream.Port)

	s := &StratumServer{
		config:    cfg,
		rpcClient: client,
		sessions:  make(map[*Session]struct{}),
	}

	// Initialize RandomX VM
	log.Println("Initializing RandomX VM...")
	hash, err := client.GetBestBlockHash()
	if err != nil {
		log.Fatal("Cannot get best block hash:", err)
	}
	seedKey, _ := hex.DecodeString(hash)
	vm, err := randomx.NewVM(seedKey)
	if err != nil {
		log.Fatal("RandomX VM init failed:", err)
	}
	s.rxVM = vm
	log.Println("RandomX VM ready")

	// Fetch initial template
	s.refreshBlockTemplate()

	// Background: refresh template
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		for range ticker.C {
			if s.refreshBlockTemplate() {
				s.broadcastJobs()
			}
		}
	}()

	// Background: log stats
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		for range ticker.C {
			t := s.currentTemplate()
			if t == nil {
				continue
			}
			s.sessionsMu.RLock()
			miners := len(s.sessions)
			s.sessionsMu.RUnlock()
			shares := atomic.LoadInt64(&s.totalShares)
			blocks := atomic.LoadInt64(&s.blocksMined)
			log.Printf("Height: %d | Miners: %d | Shares: %d | Blocks: %d",
				t.height, miners, shares, blocks)
		}
	}()

	return s
}

func (s *StratumServer) Listen() {
	for _, port := range s.config.Stratum.Ports {
		go s.listenOnPort(port)
	}
	select {} // block forever
}

func (s *StratumServer) listenOnPort(cfg pool.Port) {
	bindAddr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	addr, err := net.ResolveTCPAddr("tcp", bindAddr)
	if err != nil {
		log.Fatal("Resolve:", err)
	}
	ln, err := net.ListenTCP("tcp", addr)
	if err != nil {
		log.Fatal("Listen:", err)
	}
	defer ln.Close()
	log.Printf("Stratum listening on %s (diff %d)", bindAddr, cfg.Difficulty)

	for {
		conn, err := ln.AcceptTCP()
		if err != nil {
			continue
		}
		conn.SetKeepAlive(true)
		ip, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		cs := &Session{
			conn: conn,
			enc:  json.NewEncoder(conn),
			ip:   ip,
		}
		go s.handleClient(cs)
	}
}

func (s *StratumServer) handleClient(cs *Session) {
	log.Printf("New connection from %s", cs.ip)
	connbuff := bufio.NewReaderSize(cs.conn, MaxReqSize)
	cs.conn.SetDeadline(time.Now().Add(2 * time.Minute))

	for {
		data, isPrefix, err := connbuff.ReadLine()
		if isPrefix {
			log.Printf("Flood from %s", cs.ip)
			break
		} else if err == io.EOF {
			break
		} else if err != nil {
			break
		}

		if len(data) > 1 {
			var req JSONRpcReq
			if err := json.Unmarshal(data, &req); err != nil {
				log.Printf("Bad JSON from %s: %v", cs.ip, err)
				break
			}
			cs.conn.SetDeadline(time.Now().Add(2 * time.Minute))
			if err := s.handleMessage(cs, &req); err != nil {
				break
			}
		}
	}

	s.removeSession(cs)
	cs.conn.Close()
}

func (s *StratumServer) handleMessage(cs *Session, req *JSONRpcReq) error {
	if req.Id == nil || req.Params == nil {
		return fmt.Errorf("missing id or params")
	}

	switch req.Method {
	case "login":
		var params LoginParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return err
		}
		return s.handleLogin(cs, req.Id, &params)

	case "submit":
		var params SubmitParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return err
		}
		return s.handleSubmit(cs, req.Id, &params)

	case "keepalived":
		return cs.sendResult(req.Id, &StatusReply{Status: "KEEPALIVED"})

	case "getjob":
		var params GetJobParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return err
		}
		t := s.currentTemplate()
		if t == nil {
			return cs.sendError(req.Id, &ErrorReply{Code: -1, Message: "No job"})
		}
		job := s.buildJob(t)
		cs.pushJob(job)
		return cs.sendResult(req.Id, s.jobToData(job, t))

	default:
		return cs.sendError(req.Id, &ErrorReply{Code: -1, Message: "Unknown method"})
	}
}

func (s *StratumServer) handleLogin(cs *Session, id *json.RawMessage, params *LoginParams) error {
	cs.login = params.Login
	cs.agent = params.Agent
	cs.lastBeat = time.Now().Unix()

	log.Printf("Miner login: %s (%s) from %s", params.Login, params.Agent, cs.ip)

	s.addSession(cs)

	t := s.currentTemplate()
	if t == nil {
		return cs.sendError(id, &ErrorReply{Code: -1, Message: "Job not ready"})
	}

	job := s.buildJob(t)
	cs.pushJob(job)

	reply := &JobReply{
		Id:     cs.login,
		Job:    s.jobToData(job, t),
		Status: "OK",
	}
	return cs.sendResult(id, reply)
}

func (s *StratumServer) handleSubmit(cs *Session, id *json.RawMessage, params *SubmitParams) error {
	cs.lastBeat = time.Now().Unix()

	// Find the job
	job := cs.findJob(params.JobId)
	if job == nil {
		atomic.AddInt64(&s.rejects.InvalidJobId, 1)
		atomic.AddInt64(&s.staleShares, 1)
		return cs.sendError(id, &ErrorReply{Code: -1, Message: "Invalid job id"})
	}

	// Check stale
	t := s.currentTemplate()
	if t == nil || job.height != t.height {
		atomic.AddInt64(&s.rejects.StaleJob, 1)
		atomic.AddInt64(&s.staleShares, 1)
		log.Printf("Stale share from %s: job height %d != template height %d", cs.login, job.height, t.height)
		return cs.sendError(id, &ErrorReply{Code: -1, Message: "Stale job"})
	}

	// Check duplicate nonce
	nonce := strings.ToLower(params.Nonce)
	job.Lock()
	if _, exists := job.nonces[nonce]; exists {
		job.Unlock()
		atomic.AddInt64(&s.rejects.DuplicateNonce, 1)
		atomic.AddInt64(&s.invalidShares, 1)
		return cs.sendError(id, &ErrorReply{Code: -1, Message: "Duplicate nonce"})
	}
	job.nonces[nonce] = struct{}{}
	job.Unlock()

	// Reconstruct header with submitted nonce
	header := make([]byte, len(job.headerBlob))
	copy(header, job.headerBlob)
	nonceBytes, err := hex.DecodeString(nonce)
	if err != nil || len(nonceBytes) != 4 {
		atomic.AddInt64(&s.rejects.BadNonce, 1)
		atomic.AddInt64(&s.invalidShares, 1)
		return cs.sendError(id, &ErrorReply{Code: -1, Message: "Bad nonce"})
	}
	copy(header[76:80], nonceBytes) // nonce is last 4 bytes of 80-byte header

	// Verify hash with RandomX
	hash := s.rxVM.Hash(header)

	// Check against share target (pool difficulty)
	shareDiff := hashToDifficulty(hash)
	if shareDiff == nil {
		atomic.AddInt64(&s.rejects.BadHash, 1)
		atomic.AddInt64(&s.invalidShares, 1)
		return cs.sendError(id, &ErrorReply{Code: -1, Message: "Bad hash"})
	}

	// Check if meets pool difficulty
	poolDiff := big.NewInt(s.config.Stratum.Ports[0].Difficulty)
	if shareDiff.Cmp(poolDiff) < 0 {
		atomic.AddInt64(&s.rejects.LowDifficulty, 1)
		atomic.AddInt64(&s.invalidShares, 1)
		log.Printf("Low diff share from %s: %v < %v", cs.login, shareDiff, poolDiff)
		return cs.sendError(id, &ErrorReply{Code: -1, Message: "Low difficulty"})
	}

	atomic.AddInt64(&s.totalShares, 1)
	atomic.AddInt64(&s.roundShares, 1)

	// Check if meets network target (block found!)
	if blockutil.HashMeetsTarget(hash, t.target) {
		log.Printf("*** BLOCK FOUND at height %d by %s! ***", t.height, cs.login)
		log.Printf("Hash: %s", blockutil.BytesToReverseHex(hash))

		// Build full block
		fullBlock := blockutil.SerializeBlock(
			&blockutil.BlockHeader{
				Version:    t.version,
				PrevHash:   t.prevHash,
				MerkleRoot: computeMerkle(job),
				Timestamp:  t.curTime,
				Bits:       t.bits,
				Nonce:      binary.LittleEndian.Uint32(nonceBytes),
			},
			job.coinbaseTx,
			job.txData,
		)

		blockHex := hex.EncodeToString(fullBlock)
		err := s.rpcClient.SubmitBlock(blockHex)
		if err != nil {
			atomic.AddInt64(&s.rejects.SubmitBlockError, 1)
			log.Printf("Block REJECTED: %v", err)
		} else {
			log.Printf("*** Block ACCEPTED! Height %d ***", t.height)
			atomic.AddInt64(&s.blocksMined, 1)
			s.refreshBlockTemplate()
			s.broadcastJobs()
		}
	}

	return cs.sendResult(id, &StatusReply{Status: "OK"})
}

// Block template management

func (s *StratumServer) currentTemplate() *BlockTemplate {
	if t := s.template.Load(); t != nil {
		return t.(*BlockTemplate)
	}
	return nil
}

func (s *StratumServer) refreshBlockTemplate() bool {
	tmpl, err := s.rpcClient.GetBlockTemplate()
	if err != nil {
		log.Printf("Template fetch error: %v", err)
		return false
	}

	current := s.currentTemplate()
	if current != nil && current.prevHashHex == tmpl.PreviousBlockHash && current.height == tmpl.Height {
		return false // no change
	}

	prevHash, _ := blockutil.ReverseHexToBytes(tmpl.PreviousBlockHash)
	bits, _ := blockutil.DecodeBits(tmpl.Bits)
	target, _ := blockutil.TargetFromHex(tmpl.Target)

	// Parse transactions
	var txData [][]byte
	var txHashes [][32]byte
	for _, tx := range tmpl.Transactions {
		txBytes, _ := hex.DecodeString(tx.Data)
		txData = append(txData, txBytes)
		txHash, _ := blockutil.ReverseHexToBytes(tx.TxID)
		txHashes = append(txHashes, txHash)
	}

	// Update RandomX seed if prev hash changed
	if current == nil || current.prevHashHex != tmpl.PreviousBlockHash {
		seedKey, _ := hex.DecodeString(tmpl.PreviousBlockHash)
		s.rxVM.UpdateSeed(seedKey)
	}

	bt := &BlockTemplate{
		height:        tmpl.Height,
		prevHash:      prevHash,
		prevHashHex:   tmpl.PreviousBlockHash,
		bits:          bits,
		target:        target,
		version:       int32(tmpl.Version),
		curTime:       uint32(tmpl.CurTime),
		coinbaseValue: tmpl.CoinbaseValue,
		transactions:  txData,
		txHashes:      txHashes,
		seedHash:      tmpl.PreviousBlockHash,
	}

	// Compute difficulty from target
	diff := targetToDifficulty(target)
	bt.difficulty = diff

	s.template.Store(bt)
	log.Printf("New block template: height=%d diff=%v txs=%d", tmpl.Height, diff, len(txData))
	return true
}

// Job building

func (s *StratumServer) buildJob(t *BlockTemplate) *Job {
	jobId := strconv.FormatUint(atomic.AddUint64(&s.jobCounter, 1), 10)
	extraNonce := atomic.AddUint32(&s.extraNonce, 1)

	// Build coinbase
	payoutScript := []byte{0x51} // OP_TRUE for testnet
	coinbaseTx := blockutil.BuildCoinbaseTx(
		t.height, t.coinbaseValue, payoutScript,
		extraNonce, 0, nil,
	)

	// Build header
	coinbaseHash := blockutil.TxHash(coinbaseTx)
	allHashes := append([][32]byte{coinbaseHash}, t.txHashes...)
	merkleRoot := blockutil.MerkleRoot(allHashes)

	header := &blockutil.BlockHeader{
		Version:    t.version,
		PrevHash:   t.prevHash,
		MerkleRoot: merkleRoot,
		Timestamp:  t.curTime,
		Bits:       t.bits,
		Nonce:      0,
	}
	headerBlob := header.Serialize()

	return &Job{
		id:         jobId,
		height:     t.height,
		headerBlob: headerBlob,
		coinbaseTx: coinbaseTx,
		txData:     t.transactions,
		nonces:     make(map[string]struct{}),
	}
}

func (s *StratumServer) jobToData(job *Job, t *BlockTemplate) *JobData {
	return &JobData{
		Blob:     hex.EncodeToString(job.headerBlob),
		JobId:    job.id,
		Target:   targetToCompactHex(s.config.Stratum.Ports[0].Difficulty),
		Height:   t.height,
		SeedHash: t.seedHash,
	}
}

func computeMerkle(job *Job) [32]byte {
	coinbaseHash := blockutil.TxHash(job.coinbaseTx)
	hashes := [][32]byte{coinbaseHash}
	for _, tx := range job.txData {
		hashes = append(hashes, blockutil.TxHash(tx))
	}
	return blockutil.MerkleRoot(hashes)
}

// Session management

func (s *StratumServer) addSession(cs *Session) {
	s.sessionsMu.Lock()
	s.sessions[cs] = struct{}{}
	s.sessionsMu.Unlock()
}

func (s *StratumServer) removeSession(cs *Session) {
	s.sessionsMu.Lock()
	delete(s.sessions, cs)
	s.sessionsMu.Unlock()
	if cs.login != "" {
		log.Printf("Miner disconnected: %s from %s", cs.login, cs.ip)
	}
}

func (s *StratumServer) broadcastJobs() {
	t := s.currentTemplate()
	if t == nil {
		return
	}
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()

	for cs := range s.sessions {
		job := s.buildJob(t)
		cs.pushJob(job)
		data := s.jobToData(job, t)
		if err := cs.pushMessage("job", data); err != nil {
			log.Printf("Broadcast error to %s: %v", cs.ip, err)
		}
	}
	log.Printf("Broadcast new job to %d miners", len(s.sessions))
}

func (cs *Session) pushJob(job *Job) {
	cs.Lock()
	defer cs.Unlock()
	cs.jobs = append(cs.jobs, job)
	if len(cs.jobs) > 4 {
		cs.jobs = cs.jobs[1:]
	}
}

func (cs *Session) findJob(id string) *Job {
	cs.Lock()
	defer cs.Unlock()
	for _, job := range cs.jobs {
		if job.id == id {
			return job
		}
	}
	return nil
}

func (cs *Session) sendResult(id *json.RawMessage, result interface{}) error {
	cs.Lock()
	defer cs.Unlock()
	return cs.enc.Encode(&JSONRpcResp{Id: id, Version: "2.0", Result: result})
}

func (cs *Session) sendError(id *json.RawMessage, err *ErrorReply) error {
	cs.Lock()
	defer cs.Unlock()
	return cs.enc.Encode(&JSONRpcResp{Id: id, Version: "2.0", Error: err})
}

func (cs *Session) pushMessage(method string, params interface{}) error {
	cs.Lock()
	defer cs.Unlock()
	return cs.enc.Encode(&JSONPushMessage{Version: "2.0", Method: method, Params: params})
}

// Utility

var diff1 = new(big.Int).SetBytes([]byte{
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
})

func hashToDifficulty(hash [32]byte) *big.Int {
	// Hash is in standard byte order (big-endian)
	hashInt := new(big.Int).SetBytes(hash[:])
	if hashInt.Sign() == 0 {
		return nil
	}
	return new(big.Int).Div(diff1, hashInt)
}

func targetToDifficulty(target [32]byte) *big.Int {
	targetInt := new(big.Int).SetBytes(target[:])
	if targetInt.Sign() == 0 {
		return big.NewInt(1)
	}
	return new(big.Int).Div(diff1, targetInt)
}

func targetToCompactHex(diff int64) string {
	// Convert difficulty to 4-byte target for stratum
	padded := make([]byte, 32)
	d := new(big.Int).Div(diff1, big.NewInt(diff))
	dBytes := d.Bytes()
	copy(padded[32-len(dBytes):], dBytes)
	// Return first 4 bytes reversed (little-endian for stratum)
	return hex.EncodeToString([]byte{padded[31], padded[30], padded[29], padded[28]})
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

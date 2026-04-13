package rpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marscoin/marsqnet-stratum/pool"
)

type RPCClient struct {
	sync.RWMutex
	sickRate         int64
	successRate      int64
	Accepts          int64
	Rejects          int64
	LastSubmissionAt int64
	FailsCount       int64
	Url              *url.URL
	login            string
	password         string
	cookieFile       string
	Name             string
	sick             bool
	client           *http.Client
	info             atomic.Value
}

// Bitcoin-style getblocktemplate response
type GetBlockTemplateReply struct {
	Version           int64         `json:"version"`
	PreviousBlockHash string        `json:"previousblockhash"`
	Transactions      []TxTemplate  `json:"transactions"`
	CoinbaseAux       *CoinbaseAux  `json:"coinbaseaux"`
	CoinbaseValue     int64         `json:"coinbasevalue"`
	Target            string        `json:"target"`
	MinTime           int64         `json:"mintime"`
	Mutable           []string      `json:"mutable"`
	NonceRange        string        `json:"noncerange"`
	SigOpLimit        int64         `json:"sigoplimit"`
	SizeLimit         int64         `json:"sizelimit"`
	WeightLimit       int64         `json:"weightlimit"`
	CurTime           int64         `json:"curtime"`
	Bits              string        `json:"bits"`
	Height            int64         `json:"height"`
	DefaultWitnessCommitment string `json:"default_witness_commitment"`
}

type TxTemplate struct {
	Data    string `json:"data"`
	TxID    string `json:"txid"`
	Hash    string `json:"hash"`
	Fee     int64  `json:"fee"`
	SigOps  int64  `json:"sigops"`
	Weight  int64  `json:"weight"`
}

type CoinbaseAux struct {
	Flags string `json:"flags"`
}

// getmininginfo response
type GetMiningInfoReply struct {
	Blocks      int64   `json:"blocks"`
	Difficulty  float64 `json:"difficulty"`
	NetworkHash float64 `json:"networkhashps"`
	PooledTx    int64   `json:"pooledtx"`
	Chain       string  `json:"chain"`
}

// getnetworkinfo response
type GetNetworkInfoReply struct {
	Version     int64  `json:"version"`
	Subversion  string `json:"subversion"`
	Connections int64  `json:"connections"`
}

// Bitcoin-style JSON-RPC request/response
type JSONRpcReq struct {
	JSONRPC string      `json:"jsonrpc"`
	Id      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type JSONRpcResp struct {
	Id     int              `json:"id"`
	Result *json.RawMessage `json:"result"`
	Error  *RPCError        `json:"error"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func NewRPCClient(cfg *pool.Upstream) (*RPCClient, error) {
	rawUrl := fmt.Sprintf("http://%s:%v/", cfg.Host, cfg.Port)
	u, err := url.Parse(rawUrl)
	if err != nil {
		return nil, err
	}
	rpcClient := &RPCClient{
		Name:       cfg.Name,
		Url:        u,
		cookieFile: cfg.CookieFile,
	}
	timeout, _ := time.ParseDuration(cfg.Timeout)
	rpcClient.client = &http.Client{
		Timeout: timeout,
	}
	// Load initial auth
	rpcClient.loadAuth()
	return rpcClient, nil
}

// loadAuth reads credentials from cookie file or uses configured login/password
func (r *RPCClient) loadAuth() {
	if r.cookieFile != "" {
		data, err := ioutil.ReadFile(r.cookieFile)
		if err != nil {
			log.Printf("Warning: cannot read cookie file %s: %v", r.cookieFile, err)
			return
		}
		parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
		if len(parts) == 2 {
			r.login = parts[0]
			r.password = parts[1]
		}
	}
}

func (r *RPCClient) GetBlockTemplate() (*GetBlockTemplateReply, error) {
	// Re-read cookie in case it rotated
	r.loadAuth()

	params := []interface{}{
		map[string]interface{}{
			"rules": []string{"segwit"},
		},
	}
	rpcResp, err := r.doPost("getblocktemplate", params)
	if err != nil {
		return nil, err
	}
	var reply GetBlockTemplateReply
	if rpcResp.Result != nil {
		err = json.Unmarshal(*rpcResp.Result, &reply)
	}
	return &reply, err
}

func (r *RPCClient) GetMiningInfo() (*GetMiningInfoReply, error) {
	rpcResp, err := r.doPost("getmininginfo", []interface{}{})
	if err != nil {
		return nil, err
	}
	var reply GetMiningInfoReply
	if rpcResp.Result != nil {
		err = json.Unmarshal(*rpcResp.Result, &reply)
	}
	return &reply, err
}

func (r *RPCClient) GetNetworkInfo() (*GetNetworkInfoReply, error) {
	rpcResp, err := r.doPost("getnetworkinfo", []interface{}{})
	if err != nil {
		return nil, err
	}
	var reply GetNetworkInfoReply
	if rpcResp.Result != nil {
		err = json.Unmarshal(*rpcResp.Result, &reply)
	}
	return &reply, err
}

func (r *RPCClient) GetBlockCount() (int64, error) {
	rpcResp, err := r.doPost("getblockcount", []interface{}{})
	if err != nil {
		return 0, err
	}
	var count int64
	if rpcResp.Result != nil {
		err = json.Unmarshal(*rpcResp.Result, &count)
	}
	return count, err
}

func (r *RPCClient) GetBestBlockHash() (string, error) {
	rpcResp, err := r.doPost("getbestblockhash", []interface{}{})
	if err != nil {
		return "", err
	}
	var hash string
	if rpcResp.Result != nil {
		err = json.Unmarshal(*rpcResp.Result, &hash)
	}
	return hash, err
}

func (r *RPCClient) SubmitBlock(blockHex string) error {
	rpcResp, err := r.doPost("submitblock", []interface{}{blockHex})
	if err != nil {
		return err
	}
	// submitblock returns null on success, string on error
	if rpcResp.Result != nil {
		var result interface{}
		json.Unmarshal(*rpcResp.Result, &result)
		if result != nil {
			return fmt.Errorf("submitblock rejected: %v", result)
		}
	}
	return nil
}

func (r *RPCClient) ValidateAddress(address string) (bool, error) {
	rpcResp, err := r.doPost("validateaddress", []interface{}{address})
	if err != nil {
		return false, err
	}
	var reply struct {
		IsValid bool `json:"isvalid"`
	}
	if rpcResp.Result != nil {
		err = json.Unmarshal(*rpcResp.Result, &reply)
	}
	return reply.IsValid, err
}

func (r *RPCClient) doPost(method string, params interface{}) (*JSONRpcResp, error) {
	jsonReq := JSONRpcReq{
		JSONRPC: "1.0",
		Id:      1,
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(jsonReq)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", r.Url.String(), bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.login != "" {
		req.SetBasicAuth(r.login, r.password)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		r.markSick()
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		r.markSick()
		return nil, fmt.Errorf("RPC auth failed (HTTP %d) - check cookie file", resp.StatusCode)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		body, _ := ioutil.ReadAll(resp.Body)
		r.markSick()
		return nil, fmt.Errorf("RPC error HTTP %d: %s", resp.StatusCode, string(body))
	}

	var rpcResp JSONRpcResp
	err = json.NewDecoder(resp.Body).Decode(&rpcResp)
	if err != nil {
		r.markSick()
		return nil, err
	}
	if rpcResp.Error != nil {
		r.markSick()
		return nil, fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	r.markAlive()
	return &rpcResp, nil
}

func (r *RPCClient) Check() (bool, error) {
	_, err := r.GetBlockCount()
	if err != nil {
		return false, err
	}
	return !r.Sick(), nil
}

func (r *RPCClient) Sick() bool {
	r.RLock()
	defer r.RUnlock()
	return r.sick
}

func (r *RPCClient) markSick() {
	r.Lock()
	if !r.sick {
		atomic.AddInt64(&r.FailsCount, 1)
	}
	r.sickRate++
	r.successRate = 0
	if r.sickRate >= 5 {
		r.sick = true
	}
	r.Unlock()
}

func (r *RPCClient) markAlive() {
	r.Lock()
	r.successRate++
	if r.successRate >= 5 {
		r.sick = false
		r.sickRate = 0
		r.successRate = 0
	}
	r.Unlock()
}

// UpdateInfo fetches and caches mining info
func (r *RPCClient) UpdateInfo() (*GetMiningInfoReply, error) {
	info, err := r.GetMiningInfo()
	if err == nil {
		r.info.Store(info)
	}
	return info, err
}

func (r *RPCClient) Info() *GetMiningInfoReply {
	reply, _ := r.info.Load().(*GetMiningInfoReply)
	return reply
}


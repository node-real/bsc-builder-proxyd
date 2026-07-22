package forward

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/sha3"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

// TailBundleSource is the canonical source marker for tail (blind-backrun-protected) bundles.
// Must match types.TailBundleSource in bsc-builder.
const TailBundleSource = "tail-bundle"

// Manager handles outbound QUIC forwarding for proxyd.
// There is no inbound server — proxyd only sends, never receives.
// Callers invoke TryForwardRawTx / TryForwardBundle directly from the HTTP handler.
type Manager struct {
	client                 *Client
	config                 *Config
	tailBundleWhitelists   map[string]struct{} // domain/X-Tx-Source set for O(1) lookup
	tailBundleIPWhitelists []string            // IP whitelist entries (may include port suffix)

	quit chan struct{}

	bucketCache  *TimeBucketCache
	cacheCleanup *time.Ticker
}

func NewManager(config *Config) *Manager {
	if config == nil {
		config = DefaultConfig()
	}
	if !config.Enabled {
		log.Info("QUIC forward disabled")
		return nil
	}
	if err := config.SanitizeAndValidate(); err != nil {
		log.Error("Invalid forward configuration", "err", err)
		return nil
	}

	whitelist := make(map[string]struct{}, len(config.TailBundleWhitelists))
	for _, entry := range config.TailBundleWhitelists {
		whitelist[entry] = struct{}{}
	}

	m := &Manager{
		config:                 config,
		client:                 NewClient(config),
		tailBundleWhitelists:   whitelist,
		tailBundleIPWhitelists: config.TailBundleIPWhitelists,
		quit:                   make(chan struct{}),
		bucketCache:            NewTimeBucketCache(3*time.Second, 5*time.Minute),
		cacheCleanup:           time.NewTicker(15 * time.Second),
	}
	go m.cacheCleanupRoutine()
	return m
}

func (m *Manager) isTailBundle(submittedFromDomain, clientIP string) bool {
	if submittedFromDomain != "" {
		if _, ok := m.tailBundleWhitelists[submittedFromDomain]; ok {
			return true
		}
	}
	return ipInWhitelist(clientIP, m.tailBundleIPWhitelists)
}

// ipInWhitelist reports whether clientIP matches any entry in the whitelist.
// Whitelist entries may be "ip:port" or bare "ip"; only the host portion is compared.
func ipInWhitelist(clientIP string, whitelist []string) bool {
	if clientIP == "" || len(whitelist) == 0 {
		return false
	}
	for _, entry := range whitelist {
		if entry == "" {
			continue
		}
		cand := entry
		if host, _, err := net.SplitHostPort(entry); err == nil {
			cand = host
		}
		if strings.EqualFold(clientIP, cand) {
			return true
		}
	}
	return false
}

func (m *Manager) Start() error {
	if !m.config.Enabled {
		return nil
	}
	log.Info("QUIC forward client started",
		"name", m.config.Name,
		"remotes", m.config.Remotes)
	return nil
}

func (m *Manager) Stop() {
	if !m.config.Enabled {
		return
	}
	close(m.quit)
	if m.client != nil {
		m.client.Shutdown()
	}
	if m.bucketCache != nil {
		m.bucketCache.Cleanup()
	}
	log.Info("QUIC forward client stopped")
}

// TryForwardRawTx is called from the HTTP handler when an eth_sendRawTransaction
// request is received. Forwards to all configured remotes via QUIC unless the tx
// was recently received from a remote (anti-loop).
func (m *Manager) TryForwardRawTx(params json.RawMessage, txSource string) {
	if m == nil || m.client == nil {
		return
	}

	var rawTxParams []hexutil.Bytes
	if err := json.Unmarshal(params, &rawTxParams); err != nil || len(rawTxParams) == 0 {
		return
	}
	rawTx := rawTxParams[0]

	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(rawTx); err != nil {
		log.Debug("QUIC forward: failed to decode tx", "err", err)
		return
	}
	txHash := tx.Hash()

	if m.wasRecentlyReceived(txHash) {
		return
	}
	m.markRecentlyReceived(txHash)

	go func() {
		if err := m.client.ForwardRawTransaction(rawTx, txHash, false, 1, txSource); err != nil {
			log.Debug("QUIC forward: tx send failed", "hash", txHash, "err", err)
		}
	}()
}

// TryForwardBundle is called from the HTTP handler when an eth_sendBundle request
// is received. Forwards to all configured remotes unless recently forwarded.
// clientIP is the first X-Forwarded-For hop (bare IP, no port).
func (m *Manager) TryForwardBundle(params json.RawMessage, submittedFromDomain, clientIP string) {
	if m == nil || m.client == nil {
		return
	}

	var args []bundleSendArgs
	if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
		return
	}
	arg := args[0]

	bundleHash := computeBundleHash(arg.Txs, uint64(arg.BlockNumber), arg.MinTimestamp, arg.MaxTimestamp, arg.RevertingTxHashes, arg.DroppingTxHashes)

	if m.wasRecentlyReceived(bundleHash) {
		return
	}
	m.markRecentlyReceived(bundleHash)

	source := m.config.Name
	if m.isTailBundle(submittedFromDomain, clientIP) {
		source = TailBundleSource
	}

	req := &BundleRequest{
		BundleHash:          bundleHash,
		Txs:                 arg.Txs,
		MaxBlockNumber:      uint64(arg.BlockNumber),
		MinTimestamp:        arg.MinTimestamp,
		MaxTimestamp:        arg.MaxTimestamp,
		RevertingTxHashes:   arg.RevertingTxHashes,
		DroppingTxHashes:    arg.DroppingTxHashes,
		Priority:            1,
		Timestamp:           time.Now().UnixMilli(),
		Source:              source,
		SubmittedFromDomain: submittedFromDomain,
	}

	go func() {
		if err := m.client.ForwardBundle(req); err != nil {
			log.Debug("QUIC forward: bundle send failed", "hash", bundleHash, "err", err)
		}
	}()
}

func (m *Manager) cacheCleanupRoutine() {
	for {
		select {
		case <-m.cacheCleanup.C:
			m.bucketCache.Cleanup()
		case <-m.quit:
			m.cacheCleanup.Stop()
			return
		}
	}
}

func (m *Manager) markRecentlyReceived(hash common.Hash) { m.bucketCache.Add(hash) }
func (m *Manager) wasRecentlyReceived(hash common.Hash) bool {
	return m.bucketCache.Contains(hash)
}

// bundleSendArgs matches the eth_sendBundle JSON-RPC parameter object.
type bundleSendArgs struct {
	Txs               []hexutil.Bytes `json:"txs"`
	BlockNumber       hexutil.Uint64  `json:"blockNumber"`
	MinTimestamp      uint64          `json:"minTimestamp,omitempty"`
	MaxTimestamp      uint64          `json:"maxTimestamp,omitempty"`
	RevertingTxHashes []common.Hash   `json:"revertingTxHashes,omitempty"`
	DroppingTxHashes  []common.Hash   `json:"droppingTxHashes,omitempty"`
}

func computeBundleHash(txs []hexutil.Bytes, maxBlockNumber, minTimestamp, maxTimestamp uint64, revertingTxHashes, droppingTxHashes []common.Hash) common.Hash {
	h := sha3.NewLegacyKeccak256()
	for _, rawTx := range txs {
		h.Write(rawTx)
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], maxBlockNumber)
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], minTimestamp)
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], maxTimestamp)
	h.Write(buf[:])
	for _, rh := range revertingTxHashes {
		h.Write(rh[:])
	}
	for _, dh := range droppingTxHashes {
		h.Write(dh[:])
	}
	return common.BytesToHash(h.Sum(nil))
}

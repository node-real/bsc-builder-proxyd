package proxyd

import (
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
)

func (bg *BackendGroup) isImmediateBackend(name string) bool {
	for _, immediate := range bg.stagedForwarding.ImmediateBackends {
		if name == immediate {
			return true
		}
	}
	return false
}

func (bg *BackendGroup) multicallDelay(reqs []*RPCReq) time.Duration {
	cfg := bg.stagedForwarding
	if !cfg.Enabled || cfg.Threshold == 0 || !isValidMulticallTx(reqs) {
		return 0
	}
	hash, err := submissionHash(reqs[0])
	if err != nil {
		// Preserve upstream validation and normal forwarding for malformed params.
		log.Warn("cannot calculate staged forwarding hash", "backend_group", bg.Name, "method", reqs[0].Method, "err", err)
		return 0
	}
	remainder := new(big.Int).Mod(hash.Big(), big.NewInt(10000)).Int64()
	if remainder >= int64(cfg.Threshold) {
		return 0
	}
	log.Info("staging builder submission", "backend_group", bg.Name, "method", reqs[0].Method,
		"hash", hash, "remainder", remainder, "delay", time.Duration(cfg.Delay), "immediate_backends", cfg.ImmediateBackends)
	return time.Duration(cfg.Delay)
}

// submissionHash uses the canonical transaction hash, excluding blob sidecars.
// A bundle's routing hash is Keccak256 of its ordered transaction hashes. This
// stays deterministic even when builders fill in different default validity times.
func submissionHash(req *RPCReq) (common.Hash, error) {
	var raws []hexutil.Bytes
	switch req.Method {
	case "eth_sendRawTransaction":
		if err := json.Unmarshal(req.Params, &raws); err != nil {
			return common.Hash{}, err
		}
		if len(raws) != 1 {
			return common.Hash{}, fmt.Errorf("expected one raw transaction")
		}
	case "eth_sendBundle":
		var params []struct {
			Txs []hexutil.Bytes `json:"txs"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return common.Hash{}, err
		}
		if len(params) != 1 || len(params[0].Txs) == 0 {
			return common.Hash{}, fmt.Errorf("expected one non-empty bundle")
		}
		raws = params[0].Txs
	default:
		return common.Hash{}, fmt.Errorf("unsupported submission method %s", req.Method)
	}

	hashes := make([]byte, 0, common.HashLength*len(raws))
	for _, raw := range raws {
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(raw); err != nil {
			return common.Hash{}, err
		}
		hash := tx.Hash()
		if req.Method == "eth_sendRawTransaction" {
			return hash, nil
		}
		hashes = append(hashes, hash[:]...)
	}
	return crypto.Keccak256Hash(hashes), nil
}

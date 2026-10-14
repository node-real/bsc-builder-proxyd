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

const defaultStagedPolicyGroup = "default"

type stagedForwardingPlan struct {
	policyGroup       string
	delay             time.Duration
	immediateBackends []string
}

func (p stagedForwardingPlan) isImmediateBackend(name string) bool {
	for _, immediate := range p.immediateBackends {
		if name == immediate {
			return true
		}
	}
	return false
}

func (bg *BackendGroup) multicallDelay(reqs []*RPCReq) time.Duration {
	return bg.multicallPlan(reqs).delay
}

func (bg *BackendGroup) multicallPlan(reqs []*RPCReq) stagedForwardingPlan {
	cfg := bg.stagedForwarding
	plan := stagedForwardingPlan{policyGroup: defaultStagedPolicyGroup}
	if !cfg.Enabled || !isValidMulticallTx(reqs) || reqs[0].Method != "eth_sendRawTransaction" {
		return plan
	}
	if len(cfg.Groups) == 0 && cfg.defaultPolicy().Threshold == 0 {
		return plan
	}
	txs, err := submissionTransactions(reqs[0])
	if err != nil {
		// Preserve upstream validation and normal forwarding for malformed params.
		log.Warn("cannot calculate staged forwarding hash", "backend_group", bg.Name, "method", reqs[0].Method, "err", err)
		return plan
	}
	group, policy := cfg.policyForTransaction(txs[0])
	plan.policyGroup = group
	plan.immediateBackends = policy.ImmediateBackends
	if policy.Threshold == 0 {
		return plan
	}
	hash := submissionTransactionsHash(reqs[0].Method, txs)
	remainder := new(big.Int).Mod(hash.Big(), big.NewInt(10000)).Int64()
	if remainder >= int64(policy.Threshold) {
		return plan
	}
	plan.delay = time.Duration(policy.Delay)
	log.Info("staging builder submission", "backend_group", bg.Name, "policy_group", group, "method", reqs[0].Method,
		"hash", hash, "remainder", remainder, "delay", plan.delay, "immediate_backends", policy.ImmediateBackends)
	return plan
}

func (cfg StagedForwardingConfig) policyForTransaction(tx *types.Transaction) (string, StagedForwardingPolicy) {
	if len(cfg.Groups) > 0 {
		from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
		if err != nil {
			return defaultStagedPolicyGroup, cfg.defaultPolicy()
		}
		for _, group := range cfg.Groups {
			for _, address := range group.FromAddresses {
				if from == common.HexToAddress(address) {
					return group.Name, group.StagedForwardingPolicy
				}
			}
		}
	}
	return defaultStagedPolicyGroup, cfg.defaultPolicy()
}

// submissionHash uses the canonical transaction hash, excluding blob sidecars.
// A bundle's routing hash is Keccak256 of its ordered transaction hashes. This
// stays deterministic even when builders fill in different default validity times.
func submissionHash(req *RPCReq) (common.Hash, error) {
	txs, err := submissionTransactions(req)
	if err != nil {
		return common.Hash{}, err
	}
	return submissionTransactionsHash(req.Method, txs), nil
}

func submissionTransactions(req *RPCReq) (types.Transactions, error) {
	var raws []hexutil.Bytes
	switch req.Method {
	case "eth_sendRawTransaction":
		if err := json.Unmarshal(req.Params, &raws); err != nil {
			return nil, err
		}
		if len(raws) != 1 {
			return nil, fmt.Errorf("expected one raw transaction")
		}
	case "eth_sendBundle":
		var params []struct {
			Txs []hexutil.Bytes `json:"txs"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, err
		}
		if len(params) != 1 || len(params[0].Txs) == 0 {
			return nil, fmt.Errorf("expected one non-empty bundle")
		}
		raws = params[0].Txs
	default:
		return nil, fmt.Errorf("unsupported submission method %s", req.Method)
	}

	txs := make(types.Transactions, 0, len(raws))
	for _, raw := range raws {
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(raw); err != nil {
			return nil, err
		}
		txs = append(txs, tx)
	}
	return txs, nil
}

func submissionTransactionsHash(method string, txs types.Transactions) common.Hash {
	if method == "eth_sendRawTransaction" {
		return txs[0].Hash()
	}
	hashes := make([]byte, 0, common.HashLength*len(txs))
	for _, tx := range txs {
		hash := tx.Hash()
		hashes = append(hashes, hash[:]...)
	}
	return crypto.Keccak256Hash(hashes)
}

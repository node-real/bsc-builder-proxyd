package forward

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type PacketType uint8

const (
	PacketTypeTransaction PacketType = 1
	PacketTypeBundle      PacketType = 2
)

// Packet wraps the actual request with type information
type Packet struct {
	Type PacketType `json:"type"`
	Data []byte     `json:"data"`
}

// TransactionRequest represents a transaction forwarding request for BSC
type TransactionRequest struct {
	// Transaction data
	TxHash    common.Hash   `json:"txHash"`
	RawTxData hexutil.Bytes `json:"rawTxData"` // RLP encoded transaction

	// Forwarding metadata
	Private   bool   `json:"private"`   // Whether the transaction is private
	Priority  uint8  `json:"priority"`  // 0=low, 1=normal, 2=high
	Timestamp int64  `json:"timestamp"` // Unix mill timestamp when forwarded
	Source    string `json:"source,omitempty"`
	TxSource  string `json:"txSource,omitempty"` // From which partner does the transaction first come (e.g., pcs, w3w)
}

// BundleRequest represents a bundle forwarding request for BSC
type BundleRequest struct {
	// Bundle identification
	BundleHash common.Hash `json:"bundleHash"`

	// Bundle data
	Txs               []hexutil.Bytes `json:"txs"` // RLP encoded transactions
	MaxBlockNumber    uint64          `json:"maxBlockNumber"`
	MinTimestamp      uint64          `json:"minTimestamp"`
	MaxTimestamp      uint64          `json:"maxTimestamp"`
	RevertingTxHashes []common.Hash   `json:"revertingTxHashes"`
	DroppingTxHashes  []common.Hash   `json:"droppingTxHashes"`

	// Bundle metadata
	Price               *big.Int `json:"price,omitempty"` // Bundle price from simulation
	TotalProfit         *big.Int `json:"totalProfit,omitempty"`
	Priority            uint8    `json:"priority"`  // 0=low, 1=normal, 2=high
	Timestamp           int64    `json:"timestamp"` // Unix mill timestamp when forwarded
	Source              string   `json:"source,omitempty"`
	SubmittedFromDomain string   `json:"submittedFromDomain,omitempty"` // HTTP Host of the original bundle submitter
}

// encode serializes the TransactionRequest using gob encoding with type wrapper
func (req *TransactionRequest) encode() ([]byte, error) {
	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)
	err := encoder.Encode(req)
	if err != nil {
		return nil, err
	}

	packet := Packet{
		Type: PacketTypeTransaction,
		Data: buf.Bytes(),
	}

	var packetBuf bytes.Buffer
	packetEncoder := gob.NewEncoder(&packetBuf)
	err = packetEncoder.Encode(packet)
	if err != nil {
		return nil, err
	}
	return packetBuf.Bytes(), nil
}

// encode serializes the BundleRequest using gob encoding with type wrapper
func (req *BundleRequest) encode() ([]byte, error) {
	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)
	err := encoder.Encode(req)
	if err != nil {
		return nil, err
	}

	packet := Packet{
		Type: PacketTypeBundle,
		Data: buf.Bytes(),
	}

	var packetBuf bytes.Buffer
	packetEncoder := gob.NewEncoder(&packetBuf)
	err = packetEncoder.Encode(packet)
	if err != nil {
		return nil, err
	}
	return packetBuf.Bytes(), nil
}

// decodeForwardTransactionRequest deserializes TransactionRequest using gob encoding with type validation
func decodeForwardTransactionRequest(data []byte) (*TransactionRequest, error) {
	// First decode the packet wrapper
	var packet Packet
	decoder := gob.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&packet); err != nil {
		return nil, fmt.Errorf("failed to decode packet wrapper: %v", err)
	}

	// Validate packet type
	if packet.Type != PacketTypeTransaction {
		return nil, fmt.Errorf("expected transaction packet, got type %d", packet.Type)
	}

	// Decode the actual transaction request
	var decoded TransactionRequest
	dataDecoder := gob.NewDecoder(bytes.NewReader(packet.Data))
	if err := dataDecoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("failed to decode transaction request: %v", err)
	}
	return &decoded, nil
}

// decodeForwardBundleRequest deserializes BundleRequest using gob encoding with type validation
func decodeForwardBundleRequest(data []byte) (*BundleRequest, error) {
	// First decode the packet wrapper
	var packet Packet
	decoder := gob.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&packet); err != nil {
		return nil, fmt.Errorf("failed to decode packet wrapper: %v", err)
	}

	// Validate packet type
	if packet.Type != PacketTypeBundle {
		return nil, fmt.Errorf("expected bundle packet, got type %d", packet.Type)
	}

	// Decode the actual bundle request
	var decoded BundleRequest
	dataDecoder := gob.NewDecoder(bytes.NewReader(packet.Data))
	if err := dataDecoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("failed to decode bundle request: %v", err)
	}
	return &decoded, nil
}

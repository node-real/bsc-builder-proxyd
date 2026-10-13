package proxyd

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/redis/go-redis/v9"
)

const (
	pendingOwnerLease       = 30 * time.Second
	pendingRedisTimeout     = 5 * time.Second
	pendingRecoveryInterval = time.Second
)

var savePendingScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 0 then return 0 end
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
return 1
`)

var claimPendingScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], ARGV[1]) ~= ARGV[2] then return 0 end
if redis.call('EXISTS', KEYS[2]) == 1 or redis.call('EXISTS', KEYS[3]) == 0 then return 0 end
redis.call('HSET', KEYS[1], ARGV[1], ARGV[3])
return 1
`)

var finishPendingScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], ARGV[1]) ~= ARGV[2] then return 0 end
if ARGV[3] == '' then
  redis.call('HDEL', KEYS[1], ARGV[1])
else
  redis.call('HSET', KEYS[1], ARGV[1], ARGV[3])
end
return 1
`)

type pendingSubmission struct {
	Owner        string  `json:"owner"`
	Request      *RPCReq `json:"request"`
	Auth         string  `json:"auth"`
	Origin       string  `json:"origin"`
	RequestID    string  `json:"request_id"`
	ForwardedFor string  `json:"forwarded_for"`
	ProxyAuth    string  `json:"proxy_auth"`
	TxSource     string  `json:"tx_source"`
}

type pendingRecord struct {
	id         string
	value      string
	submission pendingSubmission
}

// Pending records have no TTL: process death must not expire the payload along
// with its owner lease. All keys for a group share a Redis Cluster hash slot.
type pendingForwardingStore struct {
	client   redis.UniversalClient
	group    *BackendGroup
	prefix   string
	owner    string
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopOnce sync.Once
}

func pendingToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", token), nil
}

func newPendingForwardingStore(client redis.UniversalClient, namespace string, group *BackendGroup) (*pendingForwardingStore, error) {
	owner, err := pendingToken()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &pendingForwardingStore{
		client: client, group: group, owner: owner, ctx: ctx, cancel: cancel,
		prefix: fmt.Sprintf("%s:staged_forwarding:{%x}", namespace, group.Name),
	}
	if err := s.renewLease(); err != nil {
		cancel()
		return nil, err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(pendingOwnerLease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				if err := s.renewLease(); err != nil {
					log.Error("cannot renew pending forwarding lease", "backend_group", s.group.Name, "err", err)
				}
			}
		}
	}()
	return s, nil
}

func (s *pendingForwardingStore) pendingKey() string           { return s.prefix + ":pending" }
func (s *pendingForwardingStore) ownerKey(owner string) string { return s.prefix + ":owner:" + owner }

func (s *pendingForwardingStore) renewLease() error {
	ctx, cancel := context.WithTimeout(s.ctx, pendingRedisTimeout)
	defer cancel()
	return s.client.Set(ctx, s.ownerKey(s.owner), "1", pendingOwnerLease).Err()
}

func (s *pendingForwardingStore) save(ctx context.Context, req *RPCReq) (*pendingRecord, error) {
	id, err := pendingToken()
	if err != nil {
		return nil, err
	}
	record := &pendingRecord{id: id, submission: pendingSubmission{
		Owner: s.owner, Request: req, Auth: GetAuthCtx(ctx), Origin: GetOriginCtx(ctx),
		RequestID: GetReqID(ctx), ForwardedFor: GetXForwardedFor(ctx),
		ProxyAuth: GetOpTxProxyAuthHeader(ctx), TxSource: GetTxSource(ctx),
	}}
	encoded, err := json.Marshal(record.submission)
	if err != nil {
		return nil, err
	}
	record.value = string(encoded)
	ctx, cancel := context.WithTimeout(ctx, pendingRedisTimeout)
	defer cancel()
	ok, err := savePendingScript.Run(ctx, s.client, []string{s.pendingKey(), s.ownerKey(s.owner)}, id, record.value).Int()
	if err != nil {
		return nil, err
	}
	if ok != 1 {
		return nil, fmt.Errorf("pending forwarding owner lease is unavailable")
	}
	return record, nil
}

// Compare the whole value so a late sender cannot delete or release a record
// that another process has already taken over after a lease expired.
func (s *pendingForwardingStore) finish(record *pendingRecord, delivered bool) {
	value := ""
	if !delivered {
		submission := record.submission
		submission.Owner = ""
		encoded, err := json.Marshal(submission)
		if err != nil {
			log.Error("cannot encode pending forwarding retry", "err", err)
			return
		}
		value = string(encoded)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pendingRedisTimeout)
	defer cancel()
	if err := finishPendingScript.Run(ctx, s.client, []string{s.pendingKey()}, record.id, record.value, value).Err(); err != nil {
		RecordRedisError("PendingForwardingFinish")
		log.Error("cannot finish pending forwarding", "backend_group", s.group.Name, "err", err)
	}
}

func (s *pendingForwardingStore) recoverPending() error {
	var cursor uint64
	for {
		ctx, cancel := context.WithTimeout(s.ctx, pendingRedisTimeout)
		entries, next, err := s.client.HScan(ctx, s.pendingKey(), cursor, "*", 100).Result()
		cancel()
		if err != nil {
			return err
		}
		for i := 0; i < len(entries); i += 2 {
			record := &pendingRecord{id: entries[i], value: entries[i+1]}
			if err := json.Unmarshal([]byte(record.value), &record.submission); err != nil || record.submission.Request == nil {
				log.Error("invalid pending forwarding record", "backend_group", s.group.Name, "record_id", record.id)
				continue
			}
			if !isValidMulticallTx([]*RPCReq{record.submission.Request}) {
				log.Error("invalid pending forwarding method", "backend_group", s.group.Name, "record_id", record.id)
				continue
			}
			previousOwner := record.submission.Owner
			record.submission.Owner = s.owner
			encoded, err := json.Marshal(record.submission)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(s.ctx, pendingRedisTimeout)
			claimed, err := claimPendingScript.Run(ctx, s.client,
				[]string{s.pendingKey(), s.ownerKey(previousOwner), s.ownerKey(s.owner)},
				record.id, record.value, string(encoded)).Int()
			cancel()
			if err != nil {
				return err
			}
			if claimed != 1 {
				continue
			}
			record.value = string(encoded)
			log.Info("recovering pending builder submission", "backend_group", s.group.Name, "record_id", record.id, "method", record.submission.Request.Method)
			RecordPendingForwardingRecoveryRequest(s.group, record.submission.Request.Method)
			s.finish(record, s.broadcast(record.submission))
		}
		cursor = next
		if cursor == 0 {
			s.recordPendingQueueSize()
			return nil
		}
	}
}

func (s *pendingForwardingStore) recordPendingQueueSize() {
	ctx, cancel := context.WithTimeout(s.ctx, pendingRedisTimeout)
	defer cancel()
	count, err := s.client.HLen(ctx, s.pendingKey()).Result()
	if err != nil {
		if s.ctx.Err() == nil {
			RecordRedisError("PendingForwardingMetrics")
			log.Error("cannot sample pending forwarding queue size", "backend_group", s.group.Name, "err", err)
		}
		return
	}
	RecordPendingForwardingRecords(s.group, count)
}

func (s *pendingForwardingStore) broadcast(submission pendingSubmission) bool {
	ctx := s.ctx
	for key, value := range map[string]string{
		ContextKeyAuth: submission.Auth, ContextKeyOrigin: submission.Origin, ContextKeyReqID: submission.RequestID,
		ContextKeyXForwardedFor: submission.ForwardedFor, ContextKeyOpTxProxyAuth: submission.ProxyAuth, XTxSource: submission.TxSource,
	} {
		ctx = context.WithValue(ctx, key, value)
	}
	var failed atomic.Bool
	var wg sync.WaitGroup
	ch := make(chan *multicallTuple, len(s.group.Backends))
	// Recovery bypasses sampling, delay and health filtering to attempt every
	// configured builder, including builders that were skipped on the first pass.
	for _, backend := range s.group.Backends {
		wg.Add(1)
		go func(backend *Backend) {
			defer wg.Done()
			if !s.group.multicallRequest(backend, []*RPCReq{submission.Request}, ctx, ch) {
				failed.Store(true)
			}
		}(backend)
	}
	wg.Wait()
	return len(s.group.Backends) > 0 && !failed.Load()
}

func (s *pendingForwardingStore) startRecovery() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(pendingRecoveryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				if err := s.recoverPending(); err != nil && s.ctx.Err() == nil {
					RecordRedisError("PendingForwardingRecovery")
					log.Error("cannot recover pending forwarding", "backend_group", s.group.Name, "err", err)
				}
			}
		}
	}()
}

func (s *pendingForwardingStore) stop() {
	s.stopOnce.Do(func() {
		s.cancel()
		s.wg.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), pendingRedisTimeout)
		defer cancel()
		if err := s.client.Del(ctx, s.ownerKey(s.owner)).Err(); err != nil {
			log.Error("cannot release pending forwarding lease", "backend_group", s.group.Name, "err", err)
		}
	})
}

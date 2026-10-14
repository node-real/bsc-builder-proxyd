package proxyd

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/redis/go-redis/v9"
)

const (
	pendingRedisTimeout          = redisOperationTimeout
	pendingSubmissionTTL         = 30 * time.Minute
	startupRecoveryRetryInterval = time.Second
)

var savePendingScript = redis.NewScript(`
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
local now = redis.call('TIME')
local nowms = now[1] * 1000 + math.floor(now[2] / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', nowms)
redis.call('ZADD', KEYS[2], nowms + ARGV[3], ARGV[1])
redis.call('PEXPIRE', KEYS[2], ARGV[3])
return redis.call('ZCARD', KEYS[2])
`)

var finishPendingScript = redis.NewScript(`
redis.call('DEL', KEYS[1])
redis.call('ZREM', KEYS[2], ARGV[1])
local now = redis.call('TIME')
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now[1] * 1000 + math.floor(now[2] / 1000))
return redis.call('ZCARD', KEYS[2])
`)

var expireLegacyPendingScript = redis.NewScript(`
if redis.call('PTTL', KEYS[1]) == -1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
return 1
`)

var migratePendingScript = redis.NewScript(`
local value = redis.call('HGET', KEYS[1], ARGV[1])
if not value then return 0 end
if redis.call('EXISTS', KEYS[2]) == 0 then
  redis.call('SET', KEYS[2], value, 'PX', ARGV[2])
  local now = redis.call('TIME')
  redis.call('ZADD', KEYS[3], now[1] * 1000 + math.floor(now[2] / 1000) + ARGV[2], ARGV[1])
  redis.call('PEXPIRE', KEYS[3], ARGV[2])
end
redis.call('HDEL', KEYS[1], ARGV[1])
return 1
`)

var prunePendingScript = redis.NewScript(`
local now = redis.call('TIME')
return redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now[1] * 1000 + math.floor(now[2] / 1000))
`)

type pendingSubmission struct {
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

// Full requests have individual TTLs. The sorted-set index and all record keys
// share a Redis Cluster hash slot. Payloads are read only during startup recovery.
type pendingForwardingStore struct {
	client   redis.UniversalClient
	group    *BackendGroup
	prefix   string
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopOnce sync.Once
	// Only IDs of in-flight sends are kept locally; payloads remain in Redis.
	active          sync.Map
	startupComplete atomic.Bool
	startupCutoff   atomic.Int64
}

func pendingToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", token), nil
}

func newPendingForwardingStore(client redis.UniversalClient, namespace string, group *BackendGroup) *pendingForwardingStore {
	ctx, cancel := context.WithCancel(context.Background())
	return &pendingForwardingStore{
		client: client, group: group, ctx: ctx, cancel: cancel,
		prefix: fmt.Sprintf("%s:staged_forwarding:{%x}", namespace, group.Name),
	}
}

func (s *pendingForwardingStore) pendingKey() string         { return s.prefix + ":pending_index" }
func (s *pendingForwardingStore) recordKey(id string) string { return s.prefix + ":record:" + id }
func (s *pendingForwardingStore) legacyPendingKey() string   { return s.prefix + ":pending" }

func (s *pendingForwardingStore) save(ctx context.Context, req *RPCReq) (*pendingRecord, error) {
	id, err := pendingToken()
	if err != nil {
		return nil, err
	}
	record := &pendingRecord{id: id, submission: pendingSubmission{
		Request: req, Auth: GetAuthCtx(ctx), Origin: GetOriginCtx(ctx),
		RequestID: GetReqID(ctx), ForwardedFor: GetXForwardedFor(ctx),
		ProxyAuth: GetOpTxProxyAuthHeader(ctx), TxSource: GetTxSource(ctx),
	}}
	encoded, err := json.Marshal(record.submission)
	if err != nil {
		return nil, err
	}
	record.value = string(encoded)
	s.active.Store(id, struct{}{})
	ctx, cancel := context.WithTimeout(ctx, pendingRedisTimeout)
	defer cancel()
	ok, err := savePendingScript.Run(ctx, s.client,
		[]string{s.recordKey(id), s.pendingKey()}, id, record.value, pendingSubmissionTTL.Milliseconds()).Int()
	if err != nil {
		// The write may have succeeded despite a lost response. Keep the record
		// active until the immediate fallback finishes, then try to clean it up.
		return record, err
	}
	if ok < 1 {
		return record, fmt.Errorf("pending forwarding save was not confirmed")
	}
	RecordPendingForwardingRecords(s.group, int64(ok))
	return record, nil
}

// Record IDs are unique and payloads immutable. Any successful complete
// broadcast may remove the record; failed sends retain it for the next startup.
func (s *pendingForwardingStore) finish(record *pendingRecord, delivered bool) {
	defer s.active.Delete(record.id)
	s.finishRecord(record, delivered)
}

func (s *pendingForwardingStore) finishRecord(record *pendingRecord, delivered bool) {
	if !delivered {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pendingRedisTimeout)
	defer cancel()
	count, err := finishPendingScript.Run(ctx, s.client, []string{s.recordKey(record.id), s.pendingKey()}, record.id).Int64()
	if err != nil {
		RecordRedisError("PendingForwardingFinish")
		log.Error("cannot finish pending forwarding", "backend_group", s.group.Name, "err", err)
	} else {
		RecordPendingForwardingRecords(s.group, count)
	}
}

func (s *pendingForwardingStore) recoverPending() error {
	if s.startupComplete.Load() {
		return nil
	}
	if err := s.migrateLegacyPending(); err != nil {
		return err
	}
	if s.startupCutoff.Load() == 0 {
		ctx, cancel := context.WithTimeout(s.ctx, pendingRedisTimeout)
		now, err := s.client.Time(ctx).Result()
		cancel()
		if err != nil {
			return err
		}
		// The index stores first-write time + TTL. Limit forced recovery to
		// records already queued at startup; concurrent new sends keep their policy.
		s.startupCutoff.CompareAndSwap(0, now.Add(pendingSubmissionTTL).UnixMilli())
	}
	ctx, cancel := context.WithTimeout(s.ctx, pendingRedisTimeout)
	err := prunePendingScript.Run(ctx, s.client, []string{s.pendingKey()}).Err()
	cancel()
	if err != nil {
		return err
	}
	var cursor uint64
	for {
		ctx, cancel := context.WithTimeout(s.ctx, pendingRedisTimeout)
		entries, next, err := s.client.ZScan(ctx, s.pendingKey(), cursor, "*", 100).Result()
		cancel()
		if err != nil {
			return err
		}
		for i := 0; i < len(entries); i += 2 {
			expiresAt, err := strconv.ParseFloat(entries[i+1], 64)
			if err != nil {
				return err
			}
			if expiresAt > float64(s.startupCutoff.Load()) {
				continue
			}
			ctx, cancel := context.WithTimeout(s.ctx, pendingRedisTimeout)
			value, err := s.client.Get(ctx, s.recordKey(entries[i])).Result()
			if err == redis.Nil {
				err = s.client.ZRem(ctx, s.pendingKey(), entries[i]).Err()
				cancel()
				if err != nil {
					return err
				}
				continue
			}
			cancel()
			if err != nil {
				return err
			}
			if err := s.recoverRecord(&pendingRecord{id: entries[i], value: value}); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			s.recordPendingQueueSize()
			s.startupComplete.Store(true)
			return nil
		}
	}
}

// Upgrade old hash records atomically so a deployment never discards requests
// queued by the previous format. Legacy requests get 30 minutes from migration.
func (s *pendingForwardingStore) migrateLegacyPending() error {
	ctx, cancel := context.WithTimeout(s.ctx, pendingRedisTimeout)
	err := expireLegacyPendingScript.Run(ctx, s.client, []string{s.legacyPendingKey()}, pendingSubmissionTTL.Milliseconds()).Err()
	cancel()
	if err != nil {
		return err
	}
	var cursor uint64
	for {
		ctx, cancel := context.WithTimeout(s.ctx, pendingRedisTimeout)
		entries, next, err := s.client.HScan(ctx, s.legacyPendingKey(), cursor, "*", 100).Result()
		cancel()
		if err != nil {
			return err
		}
		for i := 0; i < len(entries); i += 2 {
			ctx, cancel := context.WithTimeout(s.ctx, pendingRedisTimeout)
			err := migratePendingScript.Run(ctx, s.client,
				[]string{s.legacyPendingKey(), s.recordKey(entries[i]), s.pendingKey()}, entries[i], pendingSubmissionTTL.Milliseconds()).Err()
			cancel()
			if err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

func (s *pendingForwardingStore) recoverRecord(record *pendingRecord) error {
	if _, active := s.active.LoadOrStore(record.id, struct{}{}); active {
		return nil
	}
	defer s.active.Delete(record.id)
	if err := json.Unmarshal([]byte(record.value), &record.submission); err != nil || record.submission.Request == nil {
		log.Error("invalid pending forwarding record", "backend_group", s.group.Name, "record_id", record.id)
		return nil
	}
	if !isValidMulticallTx([]*RPCReq{record.submission.Request}) {
		log.Error("invalid pending forwarding method", "backend_group", s.group.Name, "record_id", record.id)
		return nil
	}
	log.Info("recovering pending builder submission", "backend_group", s.group.Name, "record_id", record.id, "method", record.submission.Request.Method)
	RecordPendingForwardingRecoveryRequest(s.group, record.submission.Request.Method)
	s.finishRecord(record, s.broadcast(record.submission))
	return nil
}

func (s *pendingForwardingStore) recordPendingQueueSize() {
	ctx, cancel := context.WithTimeout(s.ctx, pendingRedisTimeout)
	defer cancel()
	count, err := s.client.ZCard(ctx, s.pendingKey()).Result()
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

func (s *pendingForwardingStore) retryStartupRecovery() {
	if s.startupComplete.Load() {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(startupRecoveryRetryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				if err := s.recoverPending(); err != nil {
					if s.ctx.Err() == nil {
						RecordRedisError("PendingForwardingRecovery")
						log.Error("cannot complete startup forwarding recovery", "backend_group", s.group.Name, "err", err)
					}
				} else {
					return
				}
			}
		}
	}()
}

func (s *pendingForwardingStore) stop() {
	s.stopOnce.Do(func() {
		s.cancel()
		s.wg.Wait()
	})
}

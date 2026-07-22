package forward

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/log"
)

const protocol = "bsc-builder-forward"

type taskType int

const (
	taskTypeTransaction taskType = iota
	taskTypeBundle
)

type sendTask struct {
	taskType  taskType
	remote    string
	conn      *quic.Conn
	packet    []byte
	orderHash *common.Hash
	timestamp int64
	private   bool
}

type workerPool struct {
	client    *Client
	workers   int
	queueSize int

	done chan struct{}
	wg   sync.WaitGroup

	mu     sync.Mutex
	queues map[string]chan *sendTask
}

type Client struct {
	name    string
	enabled bool
	remotes []string
	done    chan struct{}

	connectingMu sync.Map
	connections  sync.Map
	connCount    int64

	tlsConfig  *tls.Config
	quicConfig *quic.Config

	pool *workerPool
}

func newWorkerPool(workers, queueSize int, client *Client) *workerPool {
	pool := &workerPool{
		client:    client,
		workers:   workers,
		queueSize: queueSize,
		done:      make(chan struct{}),
		queues:    make(map[string]chan *sendTask),
	}
	return pool
}

func (p *workerPool) workerRemote(remote string, q <-chan *sendTask) {
	defer p.wg.Done()
	for {
		select {
		case <-p.done:
			return
		case task := <-q:
			if task != nil {
				p.processTask(task)
				UpdateWorkerPoolQueueLength(remote, len(q))
			}
		}
	}
}

func (p *workerPool) processTask(task *sendTask) {
	switch task.taskType {
	case taskTypeTransaction:
		p.sendTransaction(task)
	case taskTypeBundle:
		p.sendBundle(task)
	}
	UpdateForwardLatency(time.Now().UnixMilli() - task.timestamp)
}

func (p *workerPool) sendTransaction(task *sendTask) {
	if err := p.attemptSend(task.remote, task.conn, task.packet); err == nil {
		RecordSendTx(task.private, "success")
		return
	} else if !task.private {
		log.Error("Failed to forward transaction", "txHash", task.orderHash, "remote", task.remote, "err", err)
		RecordSendTx(task.private, "failed")
		return
	}

	select {
	case <-p.done:
		return
	case <-time.After(1 * time.Millisecond):
	}

	if err := p.attemptSend(task.remote, task.conn, task.packet); err != nil {
		log.Error("Failed to forward transaction", "txHash", task.orderHash, "remote", task.remote, "attempts", 2, "err", err)
		RecordSendTx(task.private, "failed")
		return
	}
	RecordSendTx(task.private, "success")
}

func (p *workerPool) sendBundle(task *sendTask) {
	if err := p.attemptSend(task.remote, task.conn, task.packet); err == nil {
		RecordSendBundle("success")
		return
	}

	select {
	case <-p.done:
		return
	case <-time.After(1 * time.Millisecond):
	}

	if err := p.attemptSend(task.remote, task.conn, task.packet); err != nil {
		log.Error("Failed to forward bundle", "bundleHash", task.orderHash, "remote", task.remote, "attempts", 2, "err", err)
		RecordSendBundle("failed")
		return
	}
	RecordSendBundle("success")
}

func (p *workerPool) attemptSend(remote string, conn *quic.Conn, packet []byte) error {
	if conn == nil {
		return fmt.Errorf("connection is nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3000*time.Millisecond)
	done := p.done
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer cancel()

	stream, err := conn.OpenUniStreamSync(ctx)
	if err != nil {
		p.client.logConnectionError("Open stream failed", remote, err)
		return err
	}

	_ = stream.SetWriteDeadline(time.Now().Add(1500 * time.Millisecond))
	if _, err := stream.Write(packet); err != nil {
		stream.CancelWrite(0)
		p.client.logConnectionError("Stream write failed", remote, err)
		return err
	}
	stream.Close()
	return nil
}

func (p *workerPool) submit(task *sendTask) bool {
	if task == nil {
		return false
	}

	q := p.getOrCreateQueue(task.remote)
	select {
	case q <- task:
		UpdateWorkerPoolQueueLength(task.remote, len(q))
		return true
	case <-p.done:
		return false
	default:
		log.Warn("Worker pool queue full, dropping task",
			"taskType", task.taskType,
			"remote", task.remote,
			"queueLength", len(q))
		RecordTaskDropped(task.remote)
		return false
	}
}

func (p *workerPool) shutdown() {
	close(p.done)
	p.wg.Wait()
}

func (p *workerPool) getOrCreateQueue(remote string) chan *sendTask {
	p.mu.Lock()
	defer p.mu.Unlock()

	if q, ok := p.queues[remote]; ok {
		return q
	}

	q := make(chan *sendTask, p.queueSize)
	p.queues[remote] = q
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.workerRemote(remote, q)
	}
	return q
}

func NewClient(config *Config) *Client {
	if config == nil {
		config = DefaultConfig()
	}

	if err := config.SanitizeAndValidate(); err != nil {
		log.Error("Invalid forwarding client configuration", "err", err)
		config.Enabled = false
	}

	client := &Client{
		name:    config.Name,
		enabled: config.Enabled,
		remotes: config.Remotes,
		done:    make(chan struct{}),
	}

	client.tlsConfig = &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{protocol},
	}

	client.quicConfig = &quic.Config{
		KeepAlivePeriod:      30 * time.Second,
		MaxIdleTimeout:       5 * time.Minute,
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIncomingUniStreams: int64(config.Workers),
	}

	if client.enabled {
		client.pool = newWorkerPool(config.Workers, config.QueueSize, client)
		client.init()
	}

	return client
}

func (c *Client) init() {
	if !c.enabled {
		return
	}
	if len(c.remotes) == 0 {
		log.Warn("No remote endpoints configured for transaction forwarding")
		return
	}
	go c.keepConnections()
}

func (c *Client) keepConnections() {
	c.connectToRemotes()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.connectToRemotes()
		}
	}
}

func (c *Client) connectToRemotes() {
	for _, remote := range c.remotes {
		go c.connectToRemote(remote)
	}
}

func (c *Client) connectToRemote(remote string) {
	muInterface, _ := c.connectingMu.LoadOrStore(remote, &sync.Mutex{})
	mu := muInterface.(*sync.Mutex)

	if !mu.TryLock() {
		return
	}
	defer mu.Unlock()

	connInterface, hasConnection := c.connections.Load(remote)
	if hasConnection {
		conn := connInterface.(*quic.Conn)
		if !c.isConnectionHealthy(conn) {
			log.Warn("Connection is unhealthy, attempting to reconnect", "remote", remote)
			_ = conn.CloseWithError(0, "connection unhealthy")
			c.connections.Delete(remote)
			atomic.AddInt64(&c.connCount, -1)
			UpdateClientConnectionCount(atomic.LoadInt64(&c.connCount))
		} else {
			return
		}
	}
	c.establishConnection(remote)
}

func (c *Client) isConnectionHealthy(conn *quic.Conn) bool {
	return conn != nil && conn.Context().Err() == nil
}

func (c *Client) logConnectionError(operation, remote string, err error) {
	if err == nil {
		return
	}
	var appErr *quic.ApplicationError
	if errors.As(err, &appErr) {
		log.Warn("QUIC application error",
			"operation", operation,
			"remote", remote,
			"errorCode", uint64(appErr.ErrorCode),
			"errorMessage", appErr.ErrorMessage,
			"description", GetErrorDescription(uint64(appErr.ErrorCode)))
	} else {
		log.Debug("Connection error", "operation", operation, "remote", remote, "error", err)
	}
}

func (c *Client) establishConnection(remote string) {
	maxRetries := 3
	baseDelay := 30 * time.Second

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			delay := baseDelay * (1 << uint(attempt-1))
			select {
			case <-c.done:
				return
			case <-time.After(delay):
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := quic.DialAddr(ctx, remote, c.tlsConfig, c.quicConfig)
		cancel()

		if err != nil || conn == nil {
			c.logConnectionError("Dial failed", remote, err)
			log.Debug("Forward client failed to dial remote", "remote", remote, "attempt", attempt+1, "err", err)
			continue
		}

		log.Info("Forward client connected", "remote", remote, "attempt", attempt+1)
		oldConnInterface, hasOld := c.connections.Swap(remote, conn)
		if hasOld {
			oldConn := oldConnInterface.(*quic.Conn)
			go func() {
				time.Sleep(5 * time.Second)
				_ = oldConn.CloseWithError(0, "replaced")
			}()
		} else {
			atomic.AddInt64(&c.connCount, 1)
			UpdateClientConnectionCount(atomic.LoadInt64(&c.connCount))
		}
		return
	}
	log.Warn("Failed to connect to remote after all retries", "remote", remote, "retries", maxRetries)
}

func (c *Client) Shutdown() {
	if !c.enabled {
		return
	}
	close(c.done)
	if c.pool != nil {
		c.pool.shutdown()
	}
	c.connections.Range(func(key, value interface{}) bool {
		conn := value.(*quic.Conn)
		if conn != nil {
			_ = conn.CloseWithError(0, "")
		}
		c.connections.Delete(key)
		return true
	})
	atomic.StoreInt64(&c.connCount, 0)
	UpdateClientConnectionCount(0)
}

// ForwardRawTransaction forwards a raw transaction (already RLP-encoded) to all remotes.
func (c *Client) ForwardRawTransaction(rawTx hexutil.Bytes, txHash common.Hash, private bool, priority uint8, txSource string) error {
	if !c.enabled {
		return nil
	}

	request := &TransactionRequest{
		TxHash:    txHash,
		RawTxData: rawTx,
		Private:   private,
		Priority:  priority,
		Timestamp: time.Now().UnixMilli(),
		Source:    c.name,
		TxSource:  txSource,
	}

	packet, err := request.encode()
	if err != nil {
		RecordSendTx(private, "failed")
		return fmt.Errorf("failed to encode request: %v", err)
	}

	c.connections.Range(func(key, value interface{}) bool {
		conn := value.(*quic.Conn)
		if conn == nil {
			return true
		}
		task := &sendTask{
			taskType:  taskTypeTransaction,
			remote:    key.(string),
			conn:      conn,
			packet:    packet,
			orderHash: &request.TxHash,
			timestamp: request.Timestamp,
			private:   private,
		}
		if !c.pool.submit(task) {
			RecordSendTx(private, "failed")
		}
		return true
	})
	return nil
}

// ForwardBundle forwards a fully-populated BundleRequest to all remotes.
func (c *Client) ForwardBundle(req *BundleRequest) error {
	if !c.enabled {
		return nil
	}
	if req == nil {
		RecordSendBundle("failed")
		return fmt.Errorf("bundle request is nil")
	}

	packet, err := req.encode()
	if err != nil {
		RecordSendBundle("failed")
		return fmt.Errorf("failed to encode bundle request: %v", err)
	}

	c.connections.Range(func(key, value interface{}) bool {
		conn := value.(*quic.Conn)
		if conn == nil {
			return true
		}
		task := &sendTask{
			taskType:  taskTypeBundle,
			remote:    key.(string),
			conn:      conn,
			packet:    packet,
			orderHash: &req.BundleHash,
			timestamp: req.Timestamp,
		}
		if !c.pool.submit(task) {
			RecordSendBundle("failed")
		}
		return true
	})
	return nil
}

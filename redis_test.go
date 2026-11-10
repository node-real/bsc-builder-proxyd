package proxyd

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRedisContextDeadline(t *testing.T) {
	for _, clusterMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("cluster=%t", clusterMode), func(t *testing.T) {
			testRedisContextDeadline(t, clusterMode)
		})
	}
}

func testRedisContextDeadline(t *testing.T, clusterMode bool) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan struct{})
	defer close(done)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Accept the connection but never acknowledge any Redis command.
		<-done
	}()
	client, err := NewRedisClient("redis://"+listener.Addr().String()+"?read_timeout=30s&write_timeout=30s&max_retries=-1", clusterMode)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	start := time.Now()
	require.Error(t, CheckRedisConnection(client))
	require.Less(t, time.Since(start), time.Second, "context deadlines must override long socket timeouts")
}

func TestRedisClusterNodeContextDeadlines(t *testing.T) {
	client, err := NewRedisClient("redis://127.0.0.1:6379", true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	cluster := client.(*redis.ClusterClient)
	require.True(t, cluster.Options().ContextTimeoutEnabled)
	node := cluster.Options().NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	defer node.Close()
	require.True(t, node.Options().ContextTimeoutEnabled, "cluster node socket I/O must honor the command deadline too")
}

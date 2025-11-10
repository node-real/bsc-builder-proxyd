package integration_tests

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum-optimism/infra/proxyd"
)

func TestDomainRPCMethodMappings(t *testing.T) {
	goodBackend1 := NewMockBackend(BatchedResponseHandler(200, goodResponse))
	defer goodBackend1.Close()

	goodBackend2 := NewMockBackend(BatchedResponseHandler(200, goodResponse))
	defer goodBackend2.Close()

	require.NoError(t, os.Setenv("GOOD_BACKEND_RPC_URL_1", goodBackend1.URL()))
	require.NoError(t, os.Setenv("GOOD_BACKEND_RPC_URL_2", goodBackend2.URL()))

	config := ReadConfig("domain_routing")
	_, shutdown, err := proxyd.Start(config)
	require.NoError(t, err)
	defer shutdown()

	t.Run("default domain uses default mappings", func(t *testing.T) {
		// Reset counters
		goodBackend1.Reset()
		goodBackend2.Reset()

		client := NewProxydClient("http://127.0.0.1:8545")
		res, statusCode, err := client.SendRPC("eth_blockNumber", nil)
		require.NoError(t, err)
		require.Equal(t, 200, statusCode)
		require.NotNil(t, res)

		// eth_blockNumber should route to backend1 based on default rpc_method_mappings
		require.Equal(t, 1, len(goodBackend1.Requests()))
		require.Equal(t, 0, len(goodBackend2.Requests()))
	})

	t.Run("domain1 uses custom mappings", func(t *testing.T) {
		// Reset counters
		goodBackend1.Reset()
		goodBackend2.Reset()

		client := NewProxydClient("http://127.0.0.1:8545")
		// Set X-Forwarded-Host header to match domain1.example.com
		req := NewRPCReq("1", "eth_blockNumber", nil)
		res, statusCode, err := client.SendRequestWithHeaders(req, map[string]string{
			"X-Forwarded-Host": "domain1.example.com",
		})
		require.NoError(t, err)
		require.Equal(t, 200, statusCode)
		require.NotNil(t, res)

		// For domain1.example.com, eth_blockNumber should route to backend2
		require.Equal(t, 0, len(goodBackend1.Requests()))
		require.Equal(t, 1, len(goodBackend2.Requests()))
	})

	t.Run("domain2 uses custom mappings", func(t *testing.T) {
		// Reset counters
		goodBackend1.Reset()
		goodBackend2.Reset()

		client := NewProxydClient("http://127.0.0.1:8545")
		req := NewRPCReq("1", "eth_call", []interface{}{
			map[string]interface{}{"to": "0x1234"},
			"latest",
		})
		res, statusCode, err := client.SendRequestWithHeaders(req, map[string]string{
			"X-Forwarded-Host": "domain2.example.com",
		})
		require.NoError(t, err)
		require.Equal(t, 200, statusCode)
		require.NotNil(t, res)

		// For domain2.example.com, eth_call should route to backend1
		require.Equal(t, 1, len(goodBackend1.Requests()))
		require.Equal(t, 0, len(goodBackend2.Requests()))
	})

	t.Run("unknown domain falls back to default mappings", func(t *testing.T) {
		// Reset counters
		goodBackend1.Reset()
		goodBackend2.Reset()

		client := NewProxydClient("http://127.0.0.1:8545")
		req := NewRPCReq("1", "eth_blockNumber", nil)
		res, statusCode, err := client.SendRequestWithHeaders(req, map[string]string{
			"X-Forwarded-Host": "unknown.example.com",
		})
		require.NoError(t, err)
		require.Equal(t, 200, statusCode)
		require.NotNil(t, res)

		// Unknown domain should use default mappings (backend1)
		require.Equal(t, 1, len(goodBackend1.Requests()))
		require.Equal(t, 0, len(goodBackend2.Requests()))
	})

	t.Run("batch requests use domain-specific mappings", func(t *testing.T) {
		// Reset counters
		goodBackend1.Reset()
		goodBackend2.Reset()

		client := NewProxydClient("http://127.0.0.1:8545")
		batch := []*proxyd.RPCReq{
			NewRPCReq("1", "eth_blockNumber", nil),
			NewRPCReq("2", "eth_chainId", nil),
		}

		res, statusCode, err := client.SendBatchRequestWithHeaders(batch, map[string]string{
			"X-Forwarded-Host": "domain1.example.com",
		})
		require.NoError(t, err)
		require.Equal(t, 200, statusCode)
		require.NotNil(t, res)

		// For domain1.example.com, both methods should route to backend2
		require.Equal(t, 0, len(goodBackend1.Requests()))
		require.Equal(t, 1, len(goodBackend2.Requests())) // batched into one request
	})
}

func TestDomainRPCMethodMappingsWithMultipleBackendGroups(t *testing.T) {
	backend1 := NewMockBackend(BatchedResponseHandler(200, goodResponse))
	defer backend1.Close()

	backend2 := NewMockBackend(BatchedResponseHandler(200, goodResponse))
	defer backend2.Close()

	backend3 := NewMockBackend(BatchedResponseHandler(200, goodResponse))
	defer backend3.Close()

	require.NoError(t, os.Setenv("BACKEND_1_URL", backend1.URL()))
	require.NoError(t, os.Setenv("BACKEND_2_URL", backend2.URL()))
	require.NoError(t, os.Setenv("BACKEND_3_URL", backend3.URL()))

	config := ReadConfig("domain_routing_multigroup")
	_, shutdown, err := proxyd.Start(config)
	require.NoError(t, err)
	defer shutdown()

	t.Run("different domains route to different backend groups", func(t *testing.T) {
		// Reset counters
		backend1.Reset()
		backend2.Reset()
		backend3.Reset()

		client := NewProxydClient("http://127.0.0.1:8545")

		// Domain A: eth_call -> group_a (backend1)
		req1 := NewRPCReq("1", "eth_call", []interface{}{
			map[string]interface{}{"to": "0x1234"},
			"latest",
		})
		res1, statusCode1, err := client.SendRequestWithHeaders(req1, map[string]string{
			"X-Forwarded-Host": "domainA.example.com",
		})
		require.NoError(t, err)
		require.Equal(t, 200, statusCode1)
		require.NotNil(t, res1)
		require.Equal(t, 1, len(backend1.Requests()))

		// Domain B: eth_call -> group_b (backend2)
		backend1.Reset()
		req2 := NewRPCReq("2", "eth_call", []interface{}{
			map[string]interface{}{"to": "0x1234"},
			"latest",
		})
		res2, statusCode2, err := client.SendRequestWithHeaders(req2, map[string]string{
			"X-Forwarded-Host": "domainB.example.com",
		})
		require.NoError(t, err)
		require.Equal(t, 200, statusCode2)
		require.NotNil(t, res2)
		require.Equal(t, 1, len(backend2.Requests()))

		// Default: eth_call -> group_c (backend3)
		backend2.Reset()
		res3, statusCode3, err := client.SendRPC("eth_call", []interface{}{
			map[string]interface{}{"to": "0x1234"},
			"latest",
		})
		require.NoError(t, err)
		require.Equal(t, 200, statusCode3)
		require.NotNil(t, res3)
		require.Equal(t, 1, len(backend3.Requests()))
	})
}

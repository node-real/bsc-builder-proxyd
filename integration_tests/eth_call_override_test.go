package integration_tests

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/ethereum-optimism/infra/proxyd"
	"github.com/stretchr/testify/require"
)

func TestEthCallOverride(t *testing.T) {
	config := ReadConfig("eth_call_override")

	// Create a mock backend that handles non-override calls
	hdlr := NewBatchRPCResponseRouter()
	hdlr.SetRoute("eth_call", "999", "mock_backend_response")

	backend := NewMockBackend(hdlr)
	defer backend.Close()

	require.NoError(t, os.Setenv("MAIN_BACKEND_RPC_URL", backend.URL()))

	client := NewProxydClient("http://127.0.0.1:8545")
	_, shutdown, err := proxyd.Start(config)
	require.NoError(t, err)
	defer shutdown()

	tests := []struct {
		name             string
		toAddress        string
		value            interface{}
		expectedRes      string
		shouldHitBackend bool
	}{
		{
			name:             "match",
			toAddress:        "0xaBcD123456789012345678901234567890123456",
			value:            "0xaBcD1234",
			expectedRes:      "0x1000",
			shouldHitBackend: false,
		},
		{
			name:             "match - case insensitive",
			toAddress:        "0xAbCd123456789012345678901234567890123456",
			value:            "0xAbCd1234",
			expectedRes:      "0x1000",
			shouldHitBackend: false,
		},
		{
			name:             "no match - different address",
			toAddress:        "0x1111111111111111111111111111111111111111",
			value:            "0x0",
			expectedRes:      "mock_backend_response",
			shouldHitBackend: true,
		},
		{
			name:             "no match - same address but different value",
			toAddress:        "0x1234567890123456789012345678901234567890",
			value:            "0x1",
			expectedRes:      "mock_backend_response",
			shouldHitBackend: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend.Reset()

			// Prepare eth_call parameters
			var params []interface{}
			callData := map[string]interface{}{
				"to": tt.toAddress,
			}
			if tt.value != nil {
				callData["value"] = tt.value
			}
			params = append(params, callData, "latest")

			res, statusCode, err := client.SendRPC("eth_call", params)
			require.NoError(t, err)
			require.Equal(t, 200, statusCode)

			// Parse response to check result
			var jsonRes map[string]interface{}
			require.NoError(t, json.Unmarshal(res, &jsonRes))

			if tt.shouldHitBackend {
				// Should forward to backend
				require.Equal(t, 1, len(backend.Requests()))
				require.Equal(t, tt.expectedRes, jsonRes["result"])
			} else {
				// Should be handled by override
				require.Equal(t, 0, len(backend.Requests()))
				require.Equal(t, tt.expectedRes, jsonRes["result"])
			}
		})
	}
}

func TestEthCallOverride48Club(t *testing.T) {
	config := ReadConfig("eth_call_override")

	// Create a mock backend
	hdlr := NewBatchRPCResponseRouter()
	hdlr.SetRoute("eth_call", "999", "should_not_be_called")

	backend := NewMockBackend(hdlr)
	defer backend.Close()

	require.NoError(t, os.Setenv("MAIN_BACKEND_RPC_URL", backend.URL()))

	client := NewProxydClient("http://127.0.0.1:8545")
	_, shutdown, err := proxyd.Start(config)
	require.NoError(t, err)
	defer shutdown()

	tests := []struct {
		name        string
		toAddress   string
		value       string
		rpcURL      string
		description string
	}{
		{
			name:        "48Club override rule",
			toAddress:   "0x0000000000000000000000000000000000000048",
			value:       "0x30",
			rpcURL:      "https://bscrpc.pancakeswap.finance",
			description: "Compare proxyd override result with direct 48Club RPC call",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := []interface{}{
				map[string]interface{}{
					"to":    tt.toAddress,
					"value": tt.value,
				},
				"latest",
			}

			// 1. Get result from proxyd (should use override)
			backend.Reset()
			proxydRes, statusCode, err := client.SendRPC("eth_call", params)
			require.NoError(t, err)
			require.Equal(t, 200, statusCode)

			var proxydJSON map[string]interface{}
			require.NoError(t, json.Unmarshal(proxydRes, &proxydJSON))

			// Should be handled by override (no backend calls)
			require.Equal(t, 0, len(backend.Requests()))

			// 2. Get result from direct RPC call to the external service
			directClient := NewProxydClient(tt.rpcURL)
			directRes, directStatusCode, directErr := directClient.SendRPC("eth_call", params)

			if directErr != nil {
				t.Fatalf("Direct RPC call failed: %v", directErr)
				return
			}

			if directStatusCode != 200 {
				t.Fatalf("Direct RPC call returned non-200 status %d", directStatusCode)
				return
			}

			var directJSON map[string]interface{}
			require.NoError(t, json.Unmarshal(directRes, &directJSON))

			if directJSON["result"] != nil {
				t.Logf("Direct RPC result: %v", directJSON["result"])
				t.Logf("Proxyd override result: %v", proxydJSON["result"])

				// Check if results match
				if directJSON["result"] == proxydJSON["result"] {
					t.Logf("✅ Results match: %s", tt.description)
				} else {
					t.Fatalf("⚠️ Results differ: %s", tt.description)
				}
			} else {
				t.Fatalf("Direct RPC call returned error: %v", directJSON["error"])
			}
		})
	}
}

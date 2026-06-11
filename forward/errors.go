package forward

// QUIC error codes for different disconnection reasons
const (
	ErrorCodeNormalShutdown    = 0x0 // Normal shutdown
	ErrorCodeStreamReadTimeout = 0x1 // Stream read timeout
	ErrorCodeStreamReadError   = 0x2 // Stream read error
	ErrorCodeConnectionTimeout = 0x3 // Connection accept timeout
)

// GetErrorDescription returns a human-readable description for the error code
func GetErrorDescription(code uint64) string {
	switch code {
	case ErrorCodeNormalShutdown:
		return "Normal shutdown"
	case ErrorCodeStreamReadTimeout:
		return "Stream read timeout"
	case ErrorCodeStreamReadError:
		return "Stream read error"
	case ErrorCodeConnectionTimeout:
		return "Connection accept timeout"
	default:
		return "Unknown error"
	}
}

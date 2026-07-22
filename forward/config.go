package forward

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/log"
)

// Config contains the configuration for transaction forwarding
type Config struct {
	Enabled              bool     // Whether to enable transaction forwarding
	Name                 string   // Name of the forwarding client
	Port                 int      // Port for forwarding server to listen on
	Remotes              []string // List of remote forwarding endpoints
	Workers              int      // Number of worker goroutines for sending tasks
	QueueSize            int      // Size of the task queue buffer
	TailBundleWhitelists   []string // X-Tx-Source / HTTP Host values treated as TailBundle sources
	TailBundleIPWhitelists []string // Client IP addresses treated as TailBundle sources (first X-Forwarded-For hop)
}

// DefaultConfig returns a default forwarding configuration
func DefaultConfig() *Config {
	return &Config{
		Enabled:   false,
		Name:      "builder",
		Port:      35501,
		Remotes:   nil,
		Workers:   1000,
		QueueSize: 4000,
	}
}

// Validate checks the forwarding configuration for validity
func (fc *Config) Validate() error {
	if !fc.Enabled {
		return nil // No validation needed if disabled
	}

	// Validate port range
	if fc.Port < 1024 || fc.Port > 65535 {
		return fmt.Errorf("forwarding port %d is out of valid range (1024-65535)", fc.Port)
	}

	// Validate remote endpoints
	for i, remote := range fc.Remotes {
		if remote == "" {
			return fmt.Errorf("remote endpoint %d is empty", i)
		}

		// Basic format validation (should contain host:port)
		if !strings.Contains(remote, ":") {
			return fmt.Errorf("remote endpoint %d (%s) should be in 'host:port' format", i, remote)
		}

		// Split and validate port
		parts := strings.Split(remote, ":")
		if len(parts) != 2 {
			return fmt.Errorf("remote endpoint %d (%s) has invalid format, should be 'host:port'", i, remote)
		}

		port, err := strconv.Atoi(parts[1])
		if err != nil {
			return fmt.Errorf("remote endpoint %d (%s) has invalid port: %v", i, remote, err)
		}

		if port < 1 || port > 65535 {
			return fmt.Errorf("remote endpoint %d (%s) has port %d out of valid range (1-65535)", i, remote, port)
		}
	}

	return nil
}

// SanitizeAndValidate sanitizes the configuration and validates it
func (fc *Config) SanitizeAndValidate() error {
	// Sanitize: remove empty remotes
	validRemotes := make([]string, 0, len(fc.Remotes))
	for _, remote := range fc.Remotes {
		if strings.TrimSpace(remote) != "" {
			validRemotes = append(validRemotes, strings.TrimSpace(remote))
		}
	}
	fc.Remotes = validRemotes

	// Set default port if not set
	if fc.Enabled && fc.Port == 0 {
		fc.Port = 35501
		log.Info("Forwarding port not set, using default", "port", fc.Port)
	}

	if fc.Name == "" {
		fc.Name = "bsc-builder"
		log.Info("Forwarding name not set, using default", "name", fc.Name)
	}

	// Set default worker count if not set
	if fc.Enabled && fc.Workers <= 0 {
		fc.Workers = 50
		log.Info("Forwarding workers not set, using default", "workers", fc.Workers)
	}

	// Set default queue size if not set
	if fc.Enabled && fc.QueueSize <= 0 {
		fc.QueueSize = 2000
		log.Info("Forwarding queue size not set, using default", "queueSize", fc.QueueSize)
	}

	return fc.Validate()
}

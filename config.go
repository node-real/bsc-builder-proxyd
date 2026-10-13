package proxyd

import (
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

type ServerConfig struct {
	RPCHost                    string `toml:"rpc_host"`
	RPCPort                    int    `toml:"rpc_port"`
	WSHost                     string `toml:"ws_host"`
	WSPort                     int    `toml:"ws_port"`
	MaxBodySizeBytes           int64  `toml:"max_body_size_bytes"`
	MaxConcurrentRPCs          int64  `toml:"max_concurrent_rpcs"`
	MaxConcurrentConsensusRPCs int64  `toml:"max_concurrent_consensus_rpcs"`
	LogLevel                   string `toml:"log_level"`

	// TimeoutSeconds specifies the maximum time spent serving an HTTP request. Note that isn't used for websocket connections
	TimeoutSeconds int `toml:"timeout_seconds"`

	MaxUpstreamBatchSize int `toml:"max_upstream_batch_size"`

	EnableRequestLog      bool `toml:"enable_request_log"`
	MaxRequestBodyLogLen  int  `toml:"max_request_body_log_len"`
	EnablePprof           bool `toml:"enable_pprof"`
	EnableXServedByHeader bool `toml:"enable_served_by_header"`
	AllowAllOrigins       bool `toml:"allow_all_origins"`
}

type CacheConfig struct {
	Enabled bool         `toml:"enabled"`
	TTL     TOMLDuration `toml:"ttl"`
}

type RedisConfig struct {
	// If `redis_cluster = true`, you can specify url string for multi-node cluster:
	//    "redis://<user>:<password>@<host>:<port>?addr=<host2>:<port2>&addr=<host3>:<port3>"
	// OR "rediss://<user>:<password>@<host>:<port>?addr=<host2>:<port2>&addr=<host3>:<port3>"
	//
	// Otherwise, it is also possible to specify single url for Redis cluster with proxy support.
	URL              string `toml:"url"`
	Namespace        string `toml:"namespace"`
	ReadURL          string `toml:"read_url"`
	FallbackToMemory bool   `toml:"fallback_to_memory"`
	RedisCluster     bool   `toml:"redis_cluster"`
}

type MetricsConfig struct {
	Enabled bool   `toml:"enabled"`
	Host    string `toml:"host"`
	Port    int    `toml:"port"`
}

type RateLimitConfig struct {
	UseRedis           bool                                `toml:"use_redis"`
	BaseRate           int                                 `toml:"base_rate"`
	BaseInterval       TOMLDuration                        `toml:"base_interval"`
	ExemptOrigins      []string                            `toml:"exempt_origins"`
	ExemptUserAgents   []string                            `toml:"exempt_user_agents"`
	ExemptBaseRate     int                                 `toml:"exempt_base_rate"`
	ExemptBaseInterval TOMLDuration                        `toml:"exempt_base_interval"`
	ExemptOverrides    map[string]*RateLimitExemptOrigin   `toml:"exempt_overrides"`
	ErrorMessage       string                              `toml:"error_message"`
	MethodOverrides    map[string]*RateLimitMethodOverride `toml:"method_overrides"`
	IPHeaderOverride   string                              `toml:"ip_header_override"`
}

type RateLimitMethodOverride struct {
	Limit    int          `toml:"limit"`
	Interval TOMLDuration `toml:"interval"`
	Global   bool         `toml:"global"`
}

type RateLimitExemptOrigin struct {
	Limit    int          `toml:"limit"`
	Interval TOMLDuration `toml:"interval"`
}

type TOMLDuration time.Duration

func (t *TOMLDuration) UnmarshalText(b []byte) error {
	d, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}

	*t = TOMLDuration(d)
	return nil
}

type BackendOptions struct {
	ResponseTimeoutSeconds      int          `toml:"response_timeout_seconds"`
	MaxResponseSizeBytes        int64        `toml:"max_response_size_bytes"`
	MaxRetries                  int          `toml:"max_retries"`
	OutOfServiceSeconds         int          `toml:"out_of_service_seconds"`
	MaxDegradedLatencyThreshold TOMLDuration `toml:"max_degraded_latency_threshold"`
	MaxLatencyThreshold         TOMLDuration `toml:"max_latency_threshold"`
	MaxErrorRateThreshold       float64      `toml:"max_error_rate_threshold"`
}

type BackendConfig struct {
	Username         string            `toml:"username"`
	Password         string            `toml:"password"`
	RPCURL           string            `toml:"rpc_url"`
	WSURL            string            `toml:"ws_url"`
	WSPort           int               `toml:"ws_port"`
	MaxRPS           int               `toml:"max_rps"`
	MaxWSConns       int               `toml:"max_ws_conns"`
	CAFile           string            `toml:"ca_file"`
	ClientCertFile   string            `toml:"client_cert_file"`
	ClientKeyFile    string            `toml:"client_key_file"`
	StripTrailingXFF bool              `toml:"strip_trailing_xff"`
	Headers          map[string]string `toml:"headers"`

	Weight int `toml:"weight"`

	ConsensusSkipPeerCountCheck bool   `toml:"consensus_skip_peer_count"`
	ConsensusForcedCandidate    bool   `toml:"consensus_forced_candidate"`
	ConsensusReceiptsTarget     string `toml:"consensus_receipts_target"`
}

type BackendsConfig map[string]*BackendConfig

type RoutingStrategy string

func (b *BackendGroupConfig) ValidateRoutingStrategy(bgName string) bool {
	// If Consensus Aware is Set and Routing RoutingStrategy is populated fail
	if b.ConsensusAware && b.RoutingStrategy != "" {
		log.Warn("consensus_aware is now deprecated, please use routing_strategy = consensus_aware")
		log.Crit("Exiting consensus_aware and routing strategy are mutually exclusive, they cannot both be defined")
	}

	// If Consensus Aware is Set set RoutingStrategy to consensus_aware
	if b.ConsensusAware {
		b.RoutingStrategy = ConsensusAwareRoutingStrategy
		log.Info("consensus_aware is now deprecated, please use routing_strategy = consenus_aware in the future")
	}

	switch b.RoutingStrategy {
	case ConsensusAwareRoutingStrategy:
		return true
	case MulticallRoutingStrategy:
		return true
	case FallbackRoutingStrategy:
		return true
	case "":
		log.Info("Empty routing strategy provided for backend_group, using fallback strategy ", "name", bgName)
		b.RoutingStrategy = FallbackRoutingStrategy
		return true
	default:
		return false
	}
}

const (
	ConsensusAwareRoutingStrategy RoutingStrategy = "consensus_aware"
	MulticallRoutingStrategy      RoutingStrategy = "multicall"
	FallbackRoutingStrategy       RoutingStrategy = "fallback"
)

type BackendGroupConfig struct {
	Backends []string `toml:"backends"`

	WeightedRouting bool `toml:"weighted_routing"`

	RoutingStrategy RoutingStrategy `toml:"routing_strategy"`

	MulticallRPCErrorCheck bool                   `toml:"multicall_rpc_error_check"`
	StagedForwarding       StagedForwardingConfig `toml:"staged_forwarding"`

	/*
		Deprecated: Use routing_strategy config to create a consensus_aware proxyd instance
	*/
	ConsensusAware          bool         `toml:"consensus_aware"`
	ConsensusAsyncHandler   string       `toml:"consensus_handler"`
	ConsensusPollerInterval TOMLDuration `toml:"consensus_poller_interval"`

	ConsensusBanPeriod          TOMLDuration `toml:"consensus_ban_period"`
	ConsensusMaxUpdateThreshold TOMLDuration `toml:"consensus_max_update_threshold"`
	ConsensusMaxBlockLag        uint64       `toml:"consensus_max_block_lag"`
	ConsensusMaxBlockRange      uint64       `toml:"consensus_max_block_range"`
	ConsensusMinPeerCount       int          `toml:"consensus_min_peer_count"`

	ConsensusHA                  bool         `toml:"consensus_ha"`
	ConsensusHAHeartbeatInterval TOMLDuration `toml:"consensus_ha_heartbeat_interval"`
	ConsensusHALockPeriod        TOMLDuration `toml:"consensus_ha_lock_period"`
	ConsensusHARedis             RedisConfig  `toml:"consensus_ha_redis"`

	Fallbacks []string `toml:"fallbacks"`
}

type BackendGroupsConfig map[string]*BackendGroupConfig

// StagedForwardingConfig gives selected builders a head start for a deterministic
// fraction of raw transaction submissions to a multicall group.
type StagedForwardingConfig struct {
	Enabled bool `toml:"enabled"`
	// Legacy flat fields are treated as the default policy when Default and
	// Groups are absent, so existing raw transaction policies remain compatible.
	Threshold         int                     `toml:"threshold"`
	Delay             TOMLDuration            `toml:"delay"`
	ImmediateBackends []string                `toml:"immediate_backends"`
	Default           *StagedForwardingPolicy `toml:"default"`
	Groups            []StagedForwardingGroup `toml:"groups"`
}

type StagedForwardingPolicy struct {
	Threshold         int          `toml:"threshold"`
	Delay             TOMLDuration `toml:"delay"`
	ImmediateBackends []string     `toml:"immediate_backends"`
}

type StagedForwardingGroup struct {
	Name          string   `toml:"name"`
	FromAddresses []string `toml:"from_addresses"`
	StagedForwardingPolicy
}

func (c StagedForwardingConfig) defaultPolicy() StagedForwardingPolicy {
	if c.Default != nil {
		return *c.Default
	}
	return StagedForwardingPolicy{Threshold: c.Threshold, Delay: c.Delay, ImmediateBackends: c.ImmediateBackends}
}

func (b *BackendGroupConfig) validateStagedForwarding() error {
	cfg := b.StagedForwarding
	if !cfg.Enabled {
		return nil
	}
	if b.RoutingStrategy != MulticallRoutingStrategy || b.ConsensusAware {
		return fmt.Errorf("staged_forwarding requires routing_strategy = multicall")
	}
	if len(cfg.Groups) > 0 && cfg.Default == nil {
		return fmt.Errorf("staged_forwarding default policy is required when groups are configured")
	}
	if cfg.Default != nil && (cfg.Threshold != 0 || cfg.Delay != 0 || len(cfg.ImmediateBackends) > 0) {
		return fmt.Errorf("staged_forwarding cannot combine legacy flat policy fields with default")
	}
	if err := cfg.defaultPolicy().validate(b.Backends); err != nil {
		return fmt.Errorf("staged_forwarding default: %w", err)
	}
	names := make(map[string]bool, len(cfg.Groups))
	for _, group := range cfg.Groups {
		if strings.TrimSpace(group.Name) == "" || strings.EqualFold(group.Name, "default") {
			return fmt.Errorf("staged_forwarding group name must be non-empty and cannot be default")
		}
		if names[group.Name] {
			return fmt.Errorf("staged_forwarding group %s is duplicated", group.Name)
		}
		names[group.Name] = true
		if len(group.FromAddresses) == 0 {
			return fmt.Errorf("staged_forwarding group %s must define from_addresses", group.Name)
		}
		addresses := make(map[common.Address]bool, len(group.FromAddresses))
		for _, address := range group.FromAddresses {
			if !common.IsHexAddress(address) {
				return fmt.Errorf("staged_forwarding group %s has invalid from address %s", group.Name, address)
			}
			from := common.HexToAddress(address)
			if addresses[from] {
				return fmt.Errorf("staged_forwarding group %s has duplicate from address %s", group.Name, address)
			}
			addresses[from] = true
		}
		if err := group.StagedForwardingPolicy.validate(b.Backends); err != nil {
			return fmt.Errorf("staged_forwarding group %s: %w", group.Name, err)
		}
	}
	return nil
}

func (cfg StagedForwardingPolicy) validate(backendNames []string) error {
	if cfg.Threshold < 0 || cfg.Threshold > 10000 {
		return fmt.Errorf("staged_forwarding threshold must be between 0 and 10000")
	}
	if cfg.Delay < 0 || (cfg.Threshold > 0 && cfg.Delay == 0) {
		return fmt.Errorf("staged_forwarding delay must be positive")
	}
	backends := make(map[string]bool, len(backendNames))
	for _, name := range backendNames {
		backends[name] = true
	}
	immediate := make(map[string]bool, len(cfg.ImmediateBackends))
	for _, name := range cfg.ImmediateBackends {
		if !backends[name] {
			return fmt.Errorf("staged_forwarding immediate backend %s is not in this group", name)
		}
		if immediate[name] {
			return fmt.Errorf("staged_forwarding immediate backend %s is duplicated", name)
		}
		immediate[name] = true
	}
	if cfg.Threshold > 0 && (len(immediate) == 0 || len(immediate) >= len(backends)) {
		return fmt.Errorf("staged_forwarding immediate_backends must be a non-empty proper subset of backends")
	}
	return nil
}

type MethodMappingsConfig map[string]string

type BatchConfig struct {
	MaxSize      int    `toml:"max_size"`
	ErrorMessage string `toml:"error_message"`
}

// SenderRateLimitConfig configures the sender-based rate limiter
// for eth_sendRawTransaction requests.
// To enable pre-eip155 transactions, add '0' to allowed_chain_ids.
type SenderRateLimitConfig struct {
	Enabled         bool
	Interval        TOMLDuration
	Limit           int
	AllowedChainIds []*big.Int `toml:"allowed_chain_ids"`
}

type EthCallRule struct {
	Address string `toml:"address"`
	Value   string `toml:"value"`
	Result  string `toml:"result"`
}

type EthCallOverrideConfig struct {
	Rules []EthCallRule `toml:"rules"`
}

type Config struct {
	WSBackendGroup          string                       `toml:"ws_backend_group"`
	Server                  ServerConfig                 `toml:"server"`
	Cache                   CacheConfig                  `toml:"cache"`
	Redis                   RedisConfig                  `toml:"redis"`
	Metrics                 MetricsConfig                `toml:"metrics"`
	RateLimit               RateLimitConfig              `toml:"rate_limit"`
	BackendOptions          BackendOptions               `toml:"backend"`
	Backends                BackendsConfig               `toml:"backends"`
	BatchConfig             BatchConfig                  `toml:"batch"`
	Authentication          map[string]string            `toml:"authentication"`
	BackendGroups           BackendGroupsConfig          `toml:"backend_groups"`
	RPCMethodMappings       map[string]string            `toml:"rpc_method_mappings"`
	DomainRPCMethodMappings map[string]map[string]string `toml:"domain_rpc_method_mappings"`
	WSMethodWhitelist       []string                     `toml:"ws_method_whitelist"`
	WhitelistErrorMessage   string                       `toml:"whitelist_error_message"`
	SenderRateLimit         SenderRateLimitConfig        `toml:"sender_rate_limit"`
	EthCallOverride         EthCallOverrideConfig        `toml:"eth_call_override"`
}

func ReadFromEnvOrConfig(value string) (string, error) {
	if strings.HasPrefix(value, "$") {
		envValue := os.Getenv(strings.TrimPrefix(value, "$"))
		if envValue == "" {
			return "", fmt.Errorf("config env var %s not found", value)
		}
		return envValue, nil
	}

	if strings.HasPrefix(value, "\\") {
		return strings.TrimPrefix(value, "\\"), nil
	}

	return value, nil
}

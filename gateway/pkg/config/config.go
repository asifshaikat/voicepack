// pkg/config/config.go
package config

import (
	"fmt"
	"io/ioutil"
	"time"

	"gopkg.in/yaml.v3"

	"gateway/pkg/ami"
	"gateway/pkg/common"
	"gateway/pkg/rtpengine"
)

// Config represents the main configuration for the gateway
type Config struct {
	LogLevel            string          `yaml:"log_level"`
	ShutdownWaitSeconds int             `yaml:"shutdown_wait_seconds"`
	Redis               RedisConfig     `yaml:"redis"`
	MemoryStorage       MemoryConfig    `yaml:"memory_storage"`
	RTPEngine           RTPEngineConfig `yaml:"rtpengine"`
	Asterisk            AsteriskConfig  `yaml:"asterisk"`
	SIP                 SIPConfig       `yaml:"sip"`
	WebSocket           WebSocketConfig `yaml:"websocket"`
	Metrics             MetricsConfig   `yaml:"metrics"`
	HighAvailability    HAConfig        `yaml:"high_availability"`
}

// RedisConfig represents Redis configuration
type RedisConfig struct {
	Enabled     bool     `yaml:"enabled"`
	Addresses   []string `yaml:"addresses"`
	Password    string   `yaml:"password"`
	DB          int      `yaml:"db"`
	PoolSize    int      `yaml:"pool_size"`
	MaxRetries  int      `yaml:"max_retries"`
	DialTimeout int      `yaml:"dial_timeout_ms"`
}

// MemoryConfig represents in-memory storage configuration
type MemoryConfig struct {
	MaxKeys                int    `yaml:"max_keys"`
	CleanupIntervalSeconds int    `yaml:"cleanup_interval_seconds"`
	PersistPath            string `yaml:"persist_path"`
	PersistOnShutdown      bool   `yaml:"persist_on_shutdown"`
	ShardCount             int    `yaml:"shard_count"`
}

// RTPEngineConfig represents RTPEngine configuration
type RTPEngineConfig struct {
	Engines []RTPEngineInstanceConfig `yaml:"engines"`
}

// RTPEngineInstanceConfig represents a single RTPEngine instance configuration
type RTPEngineInstanceConfig struct {
	Address        string               `yaml:"address"`
	Port           int                  `yaml:"port"`
	TimeoutMS      int                  `yaml:"timeout_ms"`
	Weight         int                  `yaml:"weight"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
}

// AsteriskConfig represents Asterisk configuration
type AsteriskConfig struct {
	Clients                    []AsteriskClientConfig `yaml:"clients"`
	DefaultClient              int                    `yaml:"default_client"`
	HealthCheckIntervalSeconds int                    `yaml:"health_check_interval_seconds"`
	ConnectionTimeoutSeconds   int                    `yaml:"connection_timeout_seconds"`
	EnableReconnect            bool                   `yaml:"enable_reconnect"`
	ReconnectIntervalSeconds   int                    `yaml:"reconnect_interval_seconds"`
	MaxReconnectAttempts       int                    `yaml:"max_reconnect_attempts"`
	MaxRetries                 int                    `yaml:"max_retries"`
	RetryDelay                 string                 `yaml:"retry_delay"`
	// High availability proxy settings
	EnableHAProxy     bool     `yaml:"enable_ha_proxy"`
	OriginalAddresses []string `yaml:"original_addresses"`
}

// AsteriskClientConfig represents a single Asterisk AMI client configuration
type AsteriskClientConfig struct {
	Address        string               `yaml:"address"`
	Username       string               `yaml:"username"`
	Secret         string               `yaml:"secret"`
	TimeoutMS      int                  `yaml:"timeout_ms"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
}

// SIPConfig represents SIP proxy configuration
type SIPConfig struct {
	UDPBindAddr          string `yaml:"udp_bind_addr"`
	ProxyURI             string `yaml:"proxy_uri"`
	DefaultNextHop       string `yaml:"default_next_hop"`
	MaxForwards          int    `yaml:"max_forwards"`
	UserAgent            string `yaml:"user_agent"`
	DisableSIPProcessing bool   `yaml:"disable_sip_processing"`
}

// WebSocketConfig represents WebSocket server configuration
type WebSocketConfig struct {
	BindAddr             string `yaml:"bind_addr"`
	CertFile             string `yaml:"cert_file"`
	KeyFile              string `yaml:"key_file"`
	MaxConnections       int    `yaml:"max_connections"`
	ReadTimeoutMS        int    `yaml:"read_timeout_ms"`
	WriteTimeoutMS       int    `yaml:"write_timeout_ms"`
	IdleTimeoutMS        int    `yaml:"idle_timeout_ms"`
	EnableIPv4Only       bool   `yaml:"enable_ipv4_only"`
	ServerName           string `yaml:"server_name"`
	DisableSIPProcessing bool   `yaml:"disable_sip_processing"`
}

// MetricsConfig represents Prometheus metrics configuration
type MetricsConfig struct {
	Enabled  bool   `yaml:"enabled"`
	BindAddr string `yaml:"bind_addr"`
}

// HAConfig represents high availability configuration
type HAConfig struct {
	Enabled           bool   `yaml:"enabled"`
	HeartbeatInterval int    `yaml:"heartbeat_interval_ms"`
	LeaseTimeout      int    `yaml:"lease_timeout_ms"`
	NodeID            string `yaml:"node_id"`
}

// CircuitBreakerConfig represents circuit breaker configuration
type CircuitBreakerConfig struct {
	FailureThreshold int `yaml:"failure_threshold"`
	ResetSeconds     int `yaml:"reset_seconds"`
	HalfOpenMax      int `yaml:"half_open_max"`
}

// LoadConfig loads configuration from a YAML file
func LoadConfig(path string) (*Config, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Set defaults
	if config.LogLevel == "" {
		config.LogLevel = "info"
	}

	if config.ShutdownWaitSeconds <= 0 {
		config.ShutdownWaitSeconds = 30
	}

	// Memory storage defaults
	if config.MemoryStorage.MaxKeys <= 0 {
		config.MemoryStorage.MaxKeys = 10000
	}

	if config.MemoryStorage.CleanupIntervalSeconds <= 0 {
		config.MemoryStorage.CleanupIntervalSeconds = 300 // 5 minutes
	}

	if config.MemoryStorage.ShardCount <= 0 {
		config.MemoryStorage.ShardCount = 32
	}

	// SIP defaults
	if config.SIP.MaxForwards <= 0 {
		config.SIP.MaxForwards = 70
	}

	if config.SIP.UserAgent == "" {
		config.SIP.UserAgent = "WebRTC-SIP Gateway"
	}

	// WebSocket defaults
	if config.WebSocket.MaxConnections <= 0 {
		config.WebSocket.MaxConnections = 1000
	}

	if config.WebSocket.ReadTimeoutMS <= 0 {
		config.WebSocket.ReadTimeoutMS = 30000 // 30 seconds
	}

	if config.WebSocket.WriteTimeoutMS <= 0 {
		config.WebSocket.WriteTimeoutMS = 30000 // 30 seconds
	}

	if config.WebSocket.IdleTimeoutMS <= 0 {
		config.WebSocket.IdleTimeoutMS = 120000 // 2 minutes
	}

	if config.WebSocket.ServerName == "" {
		config.WebSocket.ServerName = "WebRTC-SIP Gateway"
	}

	// HA defaults
	if config.HighAvailability.HeartbeatInterval <= 0 {
		config.HighAvailability.HeartbeatInterval = 5000 // 5 seconds
	}

	if config.HighAvailability.LeaseTimeout <= 0 {
		config.HighAvailability.LeaseTimeout = 15000 // 15 seconds
	}

	return &config, nil
}

// ToRTPEngineManagerConfig converts the configuration to RTPEngine manager config
func (c *Config) ToRTPEngineManagerConfig() rtpengine.ManagerConfig {
	// Create and return a proper rtpengine.ManagerConfig
	engines := make([]rtpengine.Config, 0, len(c.RTPEngine.Engines))
	for _, eng := range c.RTPEngine.Engines {
		engines = append(engines, rtpengine.Config{
			Address: eng.Address,
			Port:    eng.Port,
			Timeout: time.Duration(eng.TimeoutMS) * time.Millisecond,
			Weight:  eng.Weight,
			CircuitBreaker: common.CircuitBreakerConfig{
				FailureThreshold: eng.CircuitBreaker.FailureThreshold,
				ResetTimeout:     time.Duration(eng.CircuitBreaker.ResetSeconds) * time.Second,
				HalfOpenMaxReqs:  eng.CircuitBreaker.HalfOpenMax,
			},
		})
	}

	return rtpengine.ManagerConfig{
		Engines: engines,
	}
}

// ToAsteriskManagerConfig converts the configuration to Asterisk manager config
func (c *Config) ToAsteriskManagerConfig() ami.ManagerConfig {
	// Convert client configs to ami.Config format
	clients := make([]ami.Config, 0, len(c.Asterisk.Clients))
	for _, client := range c.Asterisk.Clients {
		// Convert timeout to duration
		timeout := time.Duration(client.TimeoutMS) * time.Millisecond
		if timeout <= 0 {
			timeout = 5 * time.Second // Default timeout
		}

		clients = append(clients, ami.Config{
			Address:  client.Address,
			Username: client.Username,
			Secret:   client.Secret,
			Timeout:  timeout,
			CircuitBreaker: common.CircuitBreakerConfig{
				FailureThreshold: client.CircuitBreaker.FailureThreshold,
				ResetTimeout:     time.Duration(client.CircuitBreaker.ResetSeconds) * time.Second,
				HalfOpenMaxReqs:  client.CircuitBreaker.HalfOpenMax,
			},
		})
	}

	return ami.ManagerConfig{
		Clients:           clients,
		MaxRetries:        c.Asterisk.MaxRetries,
		RetryDelay:        c.Asterisk.RetryDelay,
		EnableHAProxy:     c.Asterisk.EnableHAProxy,
		OriginalAddresses: c.Asterisk.OriginalAddresses,
	}
}

// GetWebSocketReadTimeout returns the WebSocket read timeout as a duration
func (c *Config) GetWebSocketReadTimeout() time.Duration {
	return time.Duration(c.WebSocket.ReadTimeoutMS) * time.Millisecond
}

// GetWebSocketWriteTimeout returns the WebSocket write timeout as a duration
func (c *Config) GetWebSocketWriteTimeout() time.Duration {
	return time.Duration(c.WebSocket.WriteTimeoutMS) * time.Millisecond
}

// GetWebSocketIdleTimeout returns the WebSocket idle timeout as a duration
func (c *Config) GetWebSocketIdleTimeout() time.Duration {
	return time.Duration(c.WebSocket.IdleTimeoutMS) * time.Millisecond
}

// GetHeartbeatInterval returns the HA heartbeat interval as a duration
func (c *Config) GetHeartbeatInterval() time.Duration {
	return time.Duration(c.HighAvailability.HeartbeatInterval) * time.Millisecond
}

// GetLeaseTimeout returns the HA lease timeout as a duration
func (c *Config) GetLeaseTimeout() time.Duration {
	return time.Duration(c.HighAvailability.LeaseTimeout) * time.Millisecond
}

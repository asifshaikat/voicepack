// pkg/common/coordination.go
package common

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"gateway/pkg/storage"
)

// Coordinator handles coordination between service instances
type Coordinator struct {
	instanceID      string
	storage         storage.StateStorage
	logger          *zap.Logger
	registry        *GoroutineRegistry
	leaderLock      sync.RWMutex
	isLeader        bool
	leaseExpiration time.Time
	leadership      map[string]bool // Component name -> leadership status
	componentMu     sync.RWMutex

	// Notification channels
	leadershipCh chan string // Channel to notify when leadership changes

	// Config
	heartbeatInterval time.Duration
	leaseTimeout      time.Duration
}

// CoordinatorConfig holds configuration for the coordinator
type CoordinatorConfig struct {
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`
	LeaseTimeout      time.Duration `json:"lease_timeout"`
}

// NewCoordinator creates a new coordinator
func NewCoordinator(storage storage.StateStorage, config CoordinatorConfig, logger *zap.Logger) (*Coordinator, error) {
	if logger == nil {
		var err error
		logger, err = zap.NewProduction()
		if err != nil {
			return nil, err
		}
	}

	if storage == nil {
		return nil, fmt.Errorf("storage is required")
	}

	// Set defaults
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = 5 * time.Second
	}

	if config.LeaseTimeout <= 0 {
		config.LeaseTimeout = 15 * time.Second
	}

	// Generate a unique instance ID
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	instanceID := fmt.Sprintf("%s-%d", hostname, time.Now().UnixNano())

	return &Coordinator{
		instanceID:        instanceID,
		storage:           storage,
		logger:            logger,
		registry:          NewGoroutineRegistry(logger),
		leadership:        make(map[string]bool),
		leadershipCh:      make(chan string, 10),
		heartbeatInterval: config.HeartbeatInterval,
		leaseTimeout:      config.LeaseTimeout,
	}, nil
}

// Start begins the coordination process
func (c *Coordinator) Start(ctx context.Context) error {
	// Register this instance
	if err := c.registerInstance(ctx); err != nil {
		return err
	}

	// Start leadership election and heartbeat
	c.registry.Go("leadership-monitor", func(ctx context.Context) {
		ticker := time.NewTicker(c.heartbeatInterval)
		defer ticker.Stop()

		// Try to become leader on startup
		c.tryAcquireLeadership(ctx)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Check if we're still the leader
				if c.IsLeader() {
					c.renewLeadership(ctx)
				} else {
					c.tryAcquireLeadership(ctx)
				}
			}
		}
	})

	// Start component leadership monitoring
	c.registry.Go("component-leadership", func(ctx context.Context) {
		ticker := time.NewTicker(c.heartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.monitorComponentLeadership(ctx)
			}
		}
	})

	return nil
}

// Stop stops the coordination process
func (c *Coordinator) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Release leadership if we have it
	if c.IsLeader() {
		c.releaseLeadership(ctx)
	}

	// Unregister this instance
	c.unregisterInstance(ctx)

	// Stop all goroutines
	return c.registry.Shutdown(10 * time.Second)
}

// IsLeader returns whether this instance is the leader
func (c *Coordinator) IsLeader() bool {
	c.leaderLock.RLock()
	defer c.leaderLock.RUnlock()

	// Check if leader and lease is still valid
	return c.isLeader && time.Now().Before(c.leaseExpiration)
}

// RegisterComponentLeadership registers interest in leadership for a component
func (c *Coordinator) RegisterComponentLeadership(component string) {
	c.componentMu.Lock()
	defer c.componentMu.Unlock()

	c.leadership[component] = false
}

// IsComponentLeader returns whether this instance is the leader for a component
func (c *Coordinator) IsComponentLeader(component string) bool {
	c.componentMu.RLock()
	defer c.componentMu.RUnlock()

	leader, exists := c.leadership[component]
	return exists && leader && c.IsLeader()
}

// WaitForLeadership blocks until this instance becomes leader or context is done
func (c *Coordinator) WaitForLeadership(ctx context.Context) bool {
	if c.IsLeader() {
		return true
	}

	for {
		select {
		case <-ctx.Done():
			return false
		case component := <-c.leadershipCh:
			if component == "global" && c.IsLeader() {
				return true
			}
		case <-time.After(c.heartbeatInterval):
			if c.IsLeader() {
				return true
			}
		}
	}
}

// GetLeaderInfo returns information about the current leader
func (c *Coordinator) GetLeaderInfo(ctx context.Context) (map[string]interface{}, error) {
	leaderKey := "coordinator:leader"

	data, err := c.storage.Get(ctx, leaderKey)
	if err != nil {
		if err == storage.ErrNotFound {
			return nil, fmt.Errorf("no leader elected")
		}
		return nil, err
	}

	var leaderInfo map[string]interface{}
	if err := json.Unmarshal(data, &leaderInfo); err != nil {
		return nil, fmt.Errorf("failed to parse leader info: %w", err)
	}

	return leaderInfo, nil
}

// Internal methods

// registerInstance registers this instance
func (c *Coordinator) registerInstance(ctx context.Context) error {
	instanceKey := fmt.Sprintf("coordinator:instance:%s", c.instanceID)

	// Get IP addresses
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return fmt.Errorf("failed to get interface addresses: %w", err)
	}

	// Find first non-loopback IPv4 address
	ipAddr := "unknown"
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
			ipAddr = ipNet.IP.String()
			break
		}
	}

	// Create instance info
	instanceInfo := map[string]interface{}{
		"id":        c.instanceID,
		"host":      c.instanceID,
		"ip":        ipAddr,
		"startTime": time.Now().Unix(),
		"lastSeen":  time.Now().Unix(),
	}

	// Serialize and store
	data, err := json.Marshal(instanceInfo)
	if err != nil {
		return fmt.Errorf("failed to serialize instance info: %w", err)
	}

	if err := c.storage.Set(ctx, instanceKey, data, c.leaseTimeout*2); err != nil {
		return fmt.Errorf("failed to register instance: %w", err)
	}

	c.logger.Info("Instance registered",
		zap.String("instanceID", c.instanceID),
		zap.String("ip", ipAddr))

	return nil
}

// unregisterInstance removes this instance from the registry
func (c *Coordinator) unregisterInstance(ctx context.Context) error {
	instanceKey := fmt.Sprintf("coordinator:instance:%s", c.instanceID)

	if err := c.storage.Delete(ctx, instanceKey); err != nil {
		if err != storage.ErrNotFound {
			return fmt.Errorf("failed to unregister instance: %w", err)
		}
	}

	c.logger.Info("Instance unregistered", zap.String("instanceID", c.instanceID))

	return nil
}

// tryAcquireLeadership attempts to become the leader
func (c *Coordinator) tryAcquireLeadership(ctx context.Context) {
	leaderKey := "coordinator:leader"

	// Check if there's already a leader
	data, err := c.storage.Get(ctx, leaderKey)
	if err == nil {
		// Leader exists, check if it's valid
		var leaderInfo map[string]interface{}
		if err := json.Unmarshal(data, &leaderInfo); err != nil {
			c.logger.Error("Failed to parse leader info", zap.Error(err))
			return
		}

		// Check expiration
		if expireTime, ok := leaderInfo["expireTime"].(float64); ok {
			expiration := time.Unix(int64(expireTime), 0)
			if time.Now().Before(expiration) {
				// Leader is still valid
				return
			}
		}
	}

	// Try to become leader
	leaderInfo := map[string]interface{}{
		"instanceID":  c.instanceID,
		"acquireTime": time.Now().Unix(),
		"expireTime":  time.Now().Add(c.leaseTimeout).Unix(),
	}

	// Serialize and store
	data, err = json.Marshal(leaderInfo)
	if err != nil {
		c.logger.Error("Failed to serialize leader info", zap.Error(err))
		return
	}

	if err := c.storage.Set(ctx, leaderKey, data, c.leaseTimeout); err != nil {
		c.logger.Error("Failed to acquire leadership", zap.Error(err))
		return
	}

	// Verify we're the leader (could be a race)
	data, err = c.storage.Get(ctx, leaderKey)
	if err != nil {
		c.logger.Error("Failed to verify leadership", zap.Error(err))
		return
	}

	var verifyInfo map[string]interface{}
	if err := json.Unmarshal(data, &verifyInfo); err != nil {
		c.logger.Error("Failed to parse verify info", zap.Error(err))
		return
	}

	if instanceID, ok := verifyInfo["instanceID"].(string); ok && instanceID == c.instanceID {
		// We are the leader!
		c.leaderLock.Lock()
		wasLeader := c.isLeader
		c.isLeader = true
		c.leaseExpiration = time.Now().Add(c.leaseTimeout)
		c.leaderLock.Unlock()

		if !wasLeader {
			c.logger.Info("Acquired leadership", zap.String("instanceID", c.instanceID))
			// Notify listeners
			select {
			case c.leadershipCh <- "global":
			default:
				// Channel full, skip notification
			}
		}
	}
}

// renewLeadership renews the leadership lease
func (c *Coordinator) renewLeadership(ctx context.Context) {
	leaderKey := "coordinator:leader"

	// Get current leader info
	data, err := c.storage.Get(ctx, leaderKey)
	if err != nil {
		c.logger.Error("Failed to get leader info", zap.Error(err))

		// We thought we were leader, but the key is gone
		c.leaderLock.Lock()
		c.isLeader = false
		c.leaderLock.Unlock()
		return
	}

	var leaderInfo map[string]interface{}
	if err := json.Unmarshal(data, &leaderInfo); err != nil {
		c.logger.Error("Failed to parse leader info", zap.Error(err))
		return
	}

	// Check if we're still the leader
	if instanceID, ok := leaderInfo["instanceID"].(string); !ok || instanceID != c.instanceID {
		// Someone else became leader
		c.leaderLock.Lock()
		c.isLeader = false
		c.leaderLock.Unlock()
		return
	}

	// Update expiration
	leaderInfo["expireTime"] = time.Now().Add(c.leaseTimeout).Unix()

	// Serialize and store
	data, err = json.Marshal(leaderInfo)
	if err != nil {
		c.logger.Error("Failed to serialize leader info", zap.Error(err))
		return
	}

	if err := c.storage.Set(ctx, leaderKey, data, c.leaseTimeout); err != nil {
		c.logger.Error("Failed to renew leadership", zap.Error(err))
		return
	}

	// Update local expiration
	c.leaderLock.Lock()
	c.leaseExpiration = time.Now().Add(c.leaseTimeout)
	c.leaderLock.Unlock()
}

// releaseLeadership explicitly releases leadership
func (c *Coordinator) releaseLeadership(ctx context.Context) {
	leaderKey := "coordinator:leader"

	// Get current leader info
	data, err := c.storage.Get(ctx, leaderKey)
	if err != nil {
		return
	}

	var leaderInfo map[string]interface{}
	if err := json.Unmarshal(data, &leaderInfo); err != nil {
		return
	}

	// Check if we're the leader
	if instanceID, ok := leaderInfo["instanceID"].(string); !ok || instanceID != c.instanceID {
		// We're not the leader
		return
	}

	// Release by deleting the key
	if err := c.storage.Delete(ctx, leaderKey); err != nil {
		c.logger.Error("Failed to release leadership", zap.Error(err))
	}

	c.logger.Info("Released leadership", zap.String("instanceID", c.instanceID))

	// Update local state
	c.leaderLock.Lock()
	c.isLeader = false
	c.leaderLock.Unlock()
}

// monitorComponentLeadership assigns leadership for components
func (c *Coordinator) monitorComponentLeadership(ctx context.Context) {
	// If we're not the global leader, clear component leadership
	if !c.IsLeader() {
		c.componentMu.Lock()
		for component := range c.leadership {
			if c.leadership[component] {
				c.leadership[component] = false

				// Notify of leadership change
				select {
				case c.leadershipCh <- component:
				default:
					// Channel full, skip notification
				}
			}
		}
		c.componentMu.Unlock()
		return
	}

	// We're the leader, assign leadership for all components
	c.componentMu.Lock()
	for component := range c.leadership {
		if !c.leadership[component] {
			c.leadership[component] = true

			// Notify of leadership change
			select {
			case c.leadershipCh <- component:
			default:
				// Channel full, skip notification
			}

			c.logger.Info("Acquired component leadership",
				zap.String("component", component),
				zap.String("instanceID", c.instanceID))
		}
	}
	c.componentMu.Unlock()
}

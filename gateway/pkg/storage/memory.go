// pkg/storage/memory.go
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

// MemoryStorage implements StateStorage with in-memory maps
type MemoryStorage struct {
	// General key-value store with sharding
	data       []*sync.Map
	shardCount int

	// Specialized stores for each type with their own mutex
	dialogs        sync.Map
	rtpSessions    sync.Map
	amiActions     sync.Map
	clientSessions sync.Map
	wsConnections  sync.Map
	registrations  sync.Map

	// Configuration
	maxKeys           int
	cleanupInterval   time.Duration
	persistPath       string
	persistOnShutdown bool
	logger            *zap.Logger

	// Statistics
	stats   StorageStats
	statsMu sync.RWMutex

	// Stopping channel
	stopCleanup chan struct{}
	cleanupDone chan struct{}
}

// MemoryConfig defines configuration for MemoryStorage
type MemoryConfig struct {
	MaxKeys           int           `json:"max_keys"`            // Maximum number of keys (0 = unlimited)
	CleanupInterval   time.Duration `json:"cleanup_interval"`    // Interval for cleanup (0 = disable)
	PersistPath       string        `json:"persist_path"`        // Path to save on shutdown (empty = disable)
	PersistOnShutdown bool          `json:"persist_on_shutdown"` // Whether to persist data on shutdown
	ShardCount        int           `json:"shard_count"`         // Number of shards for key-value store
}

// NewMemoryStorage creates a new in-memory storage
func NewMemoryStorage(config MemoryConfig, logger *zap.Logger) (*MemoryStorage, error) {
	if logger == nil {
		var err error
		logger, err = zap.NewProduction()
		if err != nil {
			return nil, err
		}
	}

	// Set defaults
	if config.MaxKeys <= 0 {
		config.MaxKeys = 10000 // Default limit
	}

	if config.CleanupInterval <= 0 {
		config.CleanupInterval = 5 * time.Minute // Default cleanup interval
	}

	if config.ShardCount <= 0 {
		config.ShardCount = 32 // Default shard count (power of 2)
	}

	// Create sharded maps
	shards := make([]*sync.Map, config.ShardCount)
	for i := 0; i < config.ShardCount; i++ {
		shards[i] = &sync.Map{}
	}

	storage := &MemoryStorage{
		data:              shards,
		shardCount:        config.ShardCount,
		maxKeys:           config.MaxKeys,
		cleanupInterval:   config.CleanupInterval,
		persistPath:       config.PersistPath,
		persistOnShutdown: config.PersistOnShutdown,
		logger:            logger,
		stopCleanup:       make(chan struct{}),
		cleanupDone:       make(chan struct{}),
	}

	// Load persisted data if available
	if config.PersistPath != "" {
		if err := storage.loadFromFile(); err != nil {
			logger.Warn("Failed to load persisted data", zap.Error(err))
		}
	}

	// Start background cleanup
	go storage.cleanupLoop()

	logger.Info("In-memory storage initialized",
		zap.Int("maxKeys", config.MaxKeys),
		zap.String("cleanupInterval", config.CleanupInterval.String()),
		zap.String("persistPath", config.PersistPath),
		zap.Int("shardCount", config.ShardCount))

	return storage, nil
}

// getShard returns the appropriate shard for a key
func (m *MemoryStorage) getShard(key string) *sync.Map {
	// Simple hash function
	hash := 0
	for i := 0; i < len(key); i++ {
		hash = 31*hash + int(key[i])
	}
	if hash < 0 {
		hash = -hash
	}
	return m.data[hash%m.shardCount]
}

// Get retrieves a value from the key-value store
func (m *MemoryStorage) Get(ctx context.Context, key string) ([]byte, error) {
	if key == "" {
		return nil, ErrInvalidKey
	}

	shard := m.getShard(key)
	if value, ok := shard.Load(key); ok {
		if kv, ok := value.(KeyValue); ok {
			// Check expiration
			if !kv.Expiration.IsZero() && kv.Expiration.Before(time.Now()) {
				// Expired, remove it
				shard.Delete(key)
				return nil, ErrKeyExpired
			}
			return kv.Value, nil
		}
	}

	return nil, ErrNotFound
}

// Set stores a value in the key-value store
func (m *MemoryStorage) Set(ctx context.Context, key string, value []byte, expiration time.Duration) error {
	if key == "" {
		return ErrInvalidKey
	}

	if value == nil {
		return ErrInvalidValue
	}

	// Calculate expiration time
	var expireTime time.Time
	if expiration > 0 {
		expireTime = time.Now().Add(expiration)
	}

	// Store the value
	shard := m.getShard(key)
	shard.Store(key, KeyValue{
		Value:      value,
		Expiration: expireTime,
	})

	// Update stats
	m.statsMu.Lock()
	m.stats.TotalItems++
	m.statsMu.Unlock()

	return nil
}

// Delete removes a key from the key-value store
func (m *MemoryStorage) Delete(ctx context.Context, key string) error {
	if key == "" {
		return ErrInvalidKey
	}

	shard := m.getShard(key)
	if _, ok := shard.Load(key); ok {
		shard.Delete(key)

		// Update stats
		m.statsMu.Lock()
		m.stats.TotalItems--
		m.statsMu.Unlock()

		return nil
	}

	return ErrNotFound
}

// GetDialog retrieves a dialog
func (m *MemoryStorage) GetDialog(ctx context.Context, callID string) (*Dialog, error) {
	if callID == "" {
		return nil, ErrInvalidKey
	}

	if value, ok := m.dialogs.Load(callID); ok {
		dialog := value.(*Dialog)

		// Check expiration
		if !dialog.ExpireTime.IsZero() && dialog.ExpireTime.Before(time.Now()) {
			m.dialogs.Delete(callID)

			// Update stats
			m.statsMu.Lock()
			m.stats.DialogCount--
			m.stats.ExpiredItems++
			m.statsMu.Unlock()

			return nil, ErrKeyExpired
		}

		return dialog, nil
	}

	return nil, ErrNotFound
}

// StoreDialog saves a dialog
func (m *MemoryStorage) StoreDialog(ctx context.Context, dialog *Dialog) error {
	if dialog == nil || dialog.CallID == "" {
		return ErrInvalidValue
	}

	// Update timestamps
	dialog.UpdateTime = time.Now()
	if dialog.CreateTime.IsZero() {
		dialog.CreateTime = dialog.UpdateTime
	}

	// Set expiration if not already set
	if dialog.ExpireTime.IsZero() {
		dialog.ExpireTime = time.Now().Add(24 * time.Hour)
	}

	// Check if this is a new dialog
	_, exists := m.dialogs.Load(dialog.CallID)

	// Store the dialog
	m.dialogs.Store(dialog.CallID, dialog)

	// Update stats
	if !exists {
		m.statsMu.Lock()
		m.stats.DialogCount++
		m.statsMu.Unlock()
	}

	return nil
}

// DeleteDialog removes a dialog
func (m *MemoryStorage) DeleteDialog(ctx context.Context, callID string) error {
	if callID == "" {
		return ErrInvalidKey
	}

	if _, ok := m.dialogs.Load(callID); ok {
		m.dialogs.Delete(callID)

		// Update stats
		m.statsMu.Lock()
		m.stats.DialogCount--
		m.statsMu.Unlock()

		return nil
	}

	return ErrNotFound
}

// GetRTPSession retrieves an RTPEngine session
func (m *MemoryStorage) GetRTPSession(ctx context.Context, callID string) (*RTPSession, error) {
	if callID == "" {
		return nil, ErrInvalidKey
	}

	if value, ok := m.rtpSessions.Load(callID); ok {
		session := value.(*RTPSession)

		// Check expiration
		if !session.ExpireTime.IsZero() && session.ExpireTime.Before(time.Now()) {
			m.rtpSessions.Delete(callID)

			// Update stats
			m.statsMu.Lock()
			m.stats.RTPSessionCount--
			m.stats.ExpiredItems++
			m.statsMu.Unlock()

			return nil, ErrKeyExpired
		}

		return session, nil
	}

	return nil, ErrNotFound
}

// StoreRTPSession saves an RTPEngine session
func (m *MemoryStorage) StoreRTPSession(ctx context.Context, session *RTPSession) error {
	if session == nil || session.CallID == "" {
		return ErrInvalidValue
	}

	// Update timestamps
	session.Updated = time.Now()
	if session.Created.IsZero() {
		session.Created = session.Updated
	}

	// Set expiration if not already set
	if session.ExpireTime.IsZero() {
		session.ExpireTime = time.Now().Add(24 * time.Hour)
	}

	// Check if this is a new session
	_, exists := m.rtpSessions.Load(session.CallID)

	// Store the session
	m.rtpSessions.Store(session.CallID, session)

	// Update stats
	if !exists {
		m.statsMu.Lock()
		m.stats.RTPSessionCount++
		m.statsMu.Unlock()
	}

	return nil
}

// DeleteRTPSession removes an RTPEngine session
func (m *MemoryStorage) DeleteRTPSession(ctx context.Context, callID string) error {
	if callID == "" {
		return ErrInvalidKey
	}

	if _, ok := m.rtpSessions.Load(callID); ok {
		m.rtpSessions.Delete(callID)

		// Update stats
		m.statsMu.Lock()
		m.stats.RTPSessionCount--
		m.statsMu.Unlock()

		return nil
	}

	return ErrNotFound
}

// GetAMIAction retrieves an AMI action
func (m *MemoryStorage) GetAMIAction(ctx context.Context, actionID string) (*AMIAction, error) {
	if actionID == "" {
		return nil, ErrInvalidKey
	}

	if value, ok := m.amiActions.Load(actionID); ok {
		action := value.(*AMIAction)

		// Check expiration
		if !action.ExpireTime.IsZero() && action.ExpireTime.Before(time.Now()) {
			m.amiActions.Delete(actionID)

			// Update stats
			m.statsMu.Lock()
			m.stats.AMIActionCount--
			m.stats.ExpiredItems++
			m.statsMu.Unlock()

			return nil, ErrKeyExpired
		}

		return action, nil
	}

	return nil, ErrNotFound
}

// StoreAMIAction saves an AMI action
func (m *MemoryStorage) StoreAMIAction(ctx context.Context, action *AMIAction) error {
	if action == nil || action.ActionID == "" {
		return ErrInvalidValue
	}

	// Update timestamp
	if action.Timestamp.IsZero() {
		action.Timestamp = time.Now()
	}

	// Set expiration if not already set
	if action.ExpireTime.IsZero() {
		action.ExpireTime = time.Now().Add(1 * time.Hour)
	}

	// Check if this is a new action
	_, exists := m.amiActions.Load(action.ActionID)

	// Store the action
	m.amiActions.Store(action.ActionID, action)

	// Update stats
	if !exists {
		m.statsMu.Lock()
		m.stats.AMIActionCount++
		m.statsMu.Unlock()
	}

	return nil
}

// DeleteAMIAction removes an AMI action
func (m *MemoryStorage) DeleteAMIAction(ctx context.Context, actionID string) error {
	if actionID == "" {
		return ErrInvalidKey
	}

	if _, ok := m.amiActions.Load(actionID); ok {
		m.amiActions.Delete(actionID)

		// Update stats
		m.statsMu.Lock()
		m.stats.AMIActionCount--
		m.statsMu.Unlock()

		return nil
	}

	return ErrNotFound
}

// GetClientSession retrieves a client session
func (m *MemoryStorage) GetClientSession(ctx context.Context, clientID string) (*ClientSession, error) {
	if clientID == "" {
		return nil, ErrInvalidKey
	}

	if value, ok := m.clientSessions.Load(clientID); ok {
		session := value.(*ClientSession)

		// Check expiration
		if !session.ExpireTime.IsZero() && session.ExpireTime.Before(time.Now()) {
			m.clientSessions.Delete(clientID)

			// Update stats
			m.statsMu.Lock()
			m.stats.ClientCount--
			m.stats.ExpiredItems++
			m.statsMu.Unlock()

			return nil, ErrKeyExpired
		}

		return session, nil
	}

	return nil, ErrNotFound
}

// StoreClientSession saves a client session
func (m *MemoryStorage) StoreClientSession(ctx context.Context, session *ClientSession) error {
	if session == nil || session.ClientID == "" {
		return ErrInvalidValue
	}

	// Update timestamps
	session.LastSeen = time.Now()
	if session.CreateTime.IsZero() {
		session.CreateTime = session.LastSeen
	}

	// Set expiration if not already set
	if session.ExpireTime.IsZero() {
		session.ExpireTime = time.Now().Add(24 * time.Hour)
	}

	// Check if this is a new session
	_, exists := m.clientSessions.Load(session.ClientID)

	// Store the session
	m.clientSessions.Store(session.ClientID, session)

	// Update stats
	if !exists {
		m.statsMu.Lock()
		m.stats.ClientCount++
		m.statsMu.Unlock()
	}

	return nil
}

// DeleteClientSession removes a client session
func (m *MemoryStorage) DeleteClientSession(ctx context.Context, clientID string) error {
	if clientID == "" {
		return ErrInvalidKey
	}

	if _, ok := m.clientSessions.Load(clientID); ok {
		m.clientSessions.Delete(clientID)

		// Update stats
		m.statsMu.Lock()
		m.stats.ClientCount--
		m.statsMu.Unlock()

		return nil
	}

	return ErrNotFound
}

// GetWSConnection retrieves WebSocket connection info
func (m *MemoryStorage) GetWSConnection(ctx context.Context, clientID string) (*WSConnectionInfo, error) {
	if clientID == "" {
		return nil, ErrInvalidKey
	}

	if value, ok := m.wsConnections.Load(clientID); ok {
		info := value.(*WSConnectionInfo)

		// Check expiration
		if !info.ExpireTime.IsZero() && info.ExpireTime.Before(time.Now()) {
			m.wsConnections.Delete(clientID)

			// Update stats
			m.statsMu.Lock()
			m.stats.WSConnCount--
			m.stats.ExpiredItems++
			m.statsMu.Unlock()

			return nil, ErrKeyExpired
		}

		return info, nil
	}

	return nil, ErrNotFound
}

// StoreWSConnection saves WebSocket connection info
func (m *MemoryStorage) StoreWSConnection(ctx context.Context, clientID string, info *WSConnectionInfo) error {
	if clientID == "" || info == nil {
		return ErrInvalidValue
	}

	// Update timestamps
	info.LastSeen = time.Now()
	if info.Created.IsZero() {
		info.Created = info.LastSeen
	}

	// Set expiration if not already set
	if info.ExpireTime.IsZero() {
		info.ExpireTime = time.Now().Add(1 * time.Hour)
	}

	// Check if this is a new connection
	_, exists := m.wsConnections.Load(clientID)

	// Store the connection info
	m.wsConnections.Store(clientID, info)

	// Update stats
	if !exists {
		m.statsMu.Lock()
		m.stats.WSConnCount++
		m.statsMu.Unlock()
	}

	return nil
}

// DeleteWSConnection removes WebSocket connection info
func (m *MemoryStorage) DeleteWSConnection(ctx context.Context, clientID string) error {
	if clientID == "" {
		return ErrInvalidKey
	}

	if _, ok := m.wsConnections.Load(clientID); ok {
		m.wsConnections.Delete(clientID)

		// Update stats
		m.statsMu.Lock()
		m.stats.WSConnCount--
		m.statsMu.Unlock()

		return nil
	}

	return ErrNotFound
}

// GetRegistration retrieves a SIP registration
func (m *MemoryStorage) GetRegistration(ctx context.Context, aor string) (*Registration, error) {
	if aor == "" {
		return nil, ErrInvalidKey
	}

	if value, ok := m.registrations.Load(aor); ok {
		reg := value.(*Registration)

		// Check expiration
		if reg.ExpireTime.Before(time.Now()) {
			m.registrations.Delete(aor)

			// Update stats
			m.statsMu.Lock()
			m.stats.RegCount--
			m.stats.ExpiredItems++
			m.statsMu.Unlock()

			return nil, ErrKeyExpired
		}

		return reg, nil
	}

	return nil, ErrNotFound
}

// StoreRegistration saves a SIP registration
func (m *MemoryStorage) StoreRegistration(ctx context.Context, reg *Registration) error {
	if reg == nil || reg.AOR == "" {
		return ErrInvalidValue
	}

	// Update timestamps
	if reg.RegisterTime.IsZero() {
		reg.RegisterTime = time.Now()
	}

	// Calculate expiration time
	if reg.ExpireTime.IsZero() {
		reg.ExpireTime = time.Now().Add(time.Duration(reg.Expires) * time.Second)
	}

	// Check if this is a new registration
	_, exists := m.registrations.Load(reg.AOR)

	// Store the registration
	m.registrations.Store(reg.AOR, reg)

	// Update stats
	if !exists {
		m.statsMu.Lock()
		m.stats.RegCount++
		m.statsMu.Unlock()
	}

	return nil
}

// DeleteRegistration removes a SIP registration
func (m *MemoryStorage) DeleteRegistration(ctx context.Context, aor string) error {
	if aor == "" {
		return ErrInvalidKey
	}

	if _, ok := m.registrations.Load(aor); ok {
		m.registrations.Delete(aor)

		// Update stats
		m.statsMu.Lock()
		m.stats.RegCount--
		m.statsMu.Unlock()

		return nil
	}

	return ErrNotFound
}

// ListDialogs returns a list of all dialogs
func (m *MemoryStorage) ListDialogs(ctx context.Context) ([]*Dialog, error) {
	var dialogs []*Dialog

	m.dialogs.Range(func(key, value interface{}) bool {
		dialog := value.(*Dialog)

		// Skip expired dialogs
		if !dialog.ExpireTime.IsZero() && dialog.ExpireTime.Before(time.Now()) {
			m.dialogs.Delete(key)

			// Update stats
			m.statsMu.Lock()
			m.stats.DialogCount--
			m.stats.ExpiredItems++
			m.statsMu.Unlock()

			return true
		}

		dialogs = append(dialogs, dialog)
		return true
	})

	return dialogs, nil
}

// ListRTPSessions returns a list of all RTP sessions
func (m *MemoryStorage) ListRTPSessions(ctx context.Context) ([]*RTPSession, error) {
	var sessions []*RTPSession

	m.rtpSessions.Range(func(key, value interface{}) bool {
		session := value.(*RTPSession)

		// Skip expired sessions
		if !session.ExpireTime.IsZero() && session.ExpireTime.Before(time.Now()) {
			m.rtpSessions.Delete(key)

			// Update stats
			m.statsMu.Lock()
			m.stats.RTPSessionCount--
			m.stats.ExpiredItems++
			m.statsMu.Unlock()

			return true
		}

		sessions = append(sessions, session)
		return true
	})

	return sessions, nil
}

// ListClientSessions returns a list of all client sessions
func (m *MemoryStorage) ListClientSessions(ctx context.Context) ([]*ClientSession, error) {
	var sessions []*ClientSession

	m.clientSessions.Range(func(key, value interface{}) bool {
		session := value.(*ClientSession)

		// Skip expired sessions
		if !session.ExpireTime.IsZero() && session.ExpireTime.Before(time.Now()) {
			m.clientSessions.Delete(key)

			// Update stats
			m.statsMu.Lock()
			m.stats.ClientCount--
			m.stats.ExpiredItems++
			m.statsMu.Unlock()

			return true
		}

		sessions = append(sessions, session)
		return true
	})

	return sessions, nil
}

// ListRegistrations returns a list of all registrations
func (m *MemoryStorage) ListRegistrations(ctx context.Context) ([]*Registration, error) {
	var registrations []*Registration

	m.registrations.Range(func(key, value interface{}) bool {
		reg := value.(*Registration)

		// Skip expired registrations
		if reg.ExpireTime.Before(time.Now()) {
			m.registrations.Delete(key)

			// Update stats
			m.statsMu.Lock()
			m.stats.RegCount--
			m.stats.ExpiredItems++
			m.statsMu.Unlock()

			return true
		}

		registrations = append(registrations, reg)
		return true
	})

	return registrations, nil
}

// Ping performs a health check - always healthy for in-memory
func (m *MemoryStorage) Ping(ctx context.Context) error {
	return nil
}

// Stats returns storage statistics
func (m *MemoryStorage) Stats(ctx context.Context) (*StorageStats, error) {
	m.statsMu.RLock()
	defer m.statsMu.RUnlock()

	// Create a copy of the stats
	stats := StorageStats{
		TotalItems:      m.stats.TotalItems,
		DialogCount:     m.stats.DialogCount,
		RTPSessionCount: m.stats.RTPSessionCount,
		AMIActionCount:  m.stats.AMIActionCount,
		ClientCount:     m.stats.ClientCount,
		WSConnCount:     m.stats.WSConnCount,
		RegCount:        m.stats.RegCount,
		ExpiredItems:    m.stats.ExpiredItems,
		EvictedItems:    m.stats.EvictedItems,
	}

	return &stats, nil
}

// Cleanup removes expired items
func (m *MemoryStorage) Cleanup(ctx context.Context) error {
	// Clean up general key-value store
	for i := 0; i < m.shardCount; i++ {
		shard := m.data[i]

		// Create a list of keys to delete to avoid modifying while iterating
		var keysToDelete []interface{}

		shard.Range(func(key, value interface{}) bool {
			if kv, ok := value.(KeyValue); ok {
				if !kv.Expiration.IsZero() && kv.Expiration.Before(time.Now()) {
					keysToDelete = append(keysToDelete, key)
				}
			}
			return true
		})

		// Delete expired keys
		for _, key := range keysToDelete {
			shard.Delete(key)

			// Update stats
			m.statsMu.Lock()
			m.stats.TotalItems--
			m.stats.ExpiredItems++
			m.statsMu.Unlock()
		}
	}

	// Clean up specialized stores - each store handles its own expiration in its List methods
	_, _ = m.ListDialogs(ctx)
	_, _ = m.ListRTPSessions(ctx)
	_, _ = m.ListClientSessions(ctx)
	_, _ = m.ListRegistrations(ctx)

	m.logger.Debug("Cleanup completed",
		zap.Int("expiredItems", m.stats.ExpiredItems),
		zap.Int("totalItems", m.stats.TotalItems))

	return nil
}

// Close stops the cleanup goroutine and persists data if configured
func (m *MemoryStorage) Close() error {
	// Signal cleanup to stop
	close(m.stopCleanup)

	// Wait for cleanup to finish
	<-m.cleanupDone

	// Persist data if enabled
	if m.persistOnShutdown && m.persistPath != "" {
		if err := m.persistToFile(); err != nil {
			m.logger.Error("Failed to persist data", zap.Error(err))
			return err
		}
	}

	return nil
}

// cleanupLoop periodically removes expired entries
func (m *MemoryStorage) cleanupLoop() {
	defer close(m.cleanupDone)

	ticker := time.NewTicker(m.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCleanup:
			m.logger.Debug("Cleanup loop stopped")
			return
		case <-ticker.C:
			m.Cleanup(context.Background())
		}
	}
}

// persistToFile saves all data to disk
func (m *MemoryStorage) persistToFile() error {
	// Create directory if it doesn't exist
	dir := filepath.Dir(m.persistPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Create a temporary file
	tempFile := m.persistPath + ".tmp"
	file, err := os.Create(tempFile)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer file.Close()

	// Create encoder
	encoder := json.NewEncoder(file)

	// Write version and stats
	if err := encoder.Encode(map[string]interface{}{
		"version":   1,
		"stats":     m.stats,
		"timestamp": time.Now(),
	}); err != nil {
		return fmt.Errorf("failed to encode header: %w", err)
	}

	// Write dialogs
	dialogs, _ := m.ListDialogs(context.Background())
	if err := encoder.Encode(dialogs); err != nil {
		return fmt.Errorf("failed to encode dialogs: %w", err)
	}

	// Write RTP sessions
	rtpSessions, _ := m.ListRTPSessions(context.Background())
	if err := encoder.Encode(rtpSessions); err != nil {
		return fmt.Errorf("failed to encode RTP sessions: %w", err)
	}

	// Write client sessions
	clientSessions, _ := m.ListClientSessions(context.Background())
	if err := encoder.Encode(clientSessions); err != nil {
		return fmt.Errorf("failed to encode client sessions: %w", err)
	}

	// Write registrations
	registrations, _ := m.ListRegistrations(context.Background())
	if err := encoder.Encode(registrations); err != nil {
		return fmt.Errorf("failed to encode registrations: %w", err)
	}

	// Write AMI actions
	var amiActions []*AMIAction
	m.amiActions.Range(func(key, value interface{}) bool {
		action := value.(*AMIAction)
		if !action.ExpireTime.IsZero() && !action.ExpireTime.Before(time.Now()) {
			amiActions = append(amiActions, action)
		}
		return true
	})
	if err := encoder.Encode(amiActions); err != nil {
		return fmt.Errorf("failed to encode AMI actions: %w", err)
	}

	// Close the file before rename to ensure all data is written
	file.Close()

	// Rename temporary file to target file
	if err := os.Rename(tempFile, m.persistPath); err != nil {
		return fmt.Errorf("failed to rename file: %w", err)
	}

	m.logger.Info("Data persisted to file",
		zap.String("path", m.persistPath),
		zap.Int("dialogs", len(dialogs)),
		zap.Int("rtpSessions", len(rtpSessions)),
		zap.Int("clientSessions", len(clientSessions)),
		zap.Int("registrations", len(registrations)),
		zap.Int("amiActions", len(amiActions)))

	return nil
}

// loadFromFile loads data from disk
func (m *MemoryStorage) loadFromFile() error {
	// Check if the file exists
	if _, err := os.Stat(m.persistPath); os.IsNotExist(err) {
		return nil // File doesn't exist, nothing to load
	}

	// Open the file
	file, err := os.Open(m.persistPath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Create decoder
	decoder := json.NewDecoder(file)

	// Read version and stats
	var header map[string]interface{}
	if err := decoder.Decode(&header); err != nil {
		return fmt.Errorf("failed to decode header: %w", err)
	}

	// Read dialogs
	var dialogs []*Dialog
	if err := decoder.Decode(&dialogs); err != nil {
		return fmt.Errorf("failed to decode dialogs: %w", err)
	}

	// Read RTP sessions
	var rtpSessions []*RTPSession
	if err := decoder.Decode(&rtpSessions); err != nil {
		return fmt.Errorf("failed to decode RTP sessions: %w", err)
	}

	// Read client sessions
	var clientSessions []*ClientSession
	if err := decoder.Decode(&clientSessions); err != nil {
		return fmt.Errorf("failed to decode client sessions: %w", err)
	}

	// Read registrations
	var registrations []*Registration
	if err := decoder.Decode(&registrations); err != nil {
		return fmt.Errorf("failed to decode registrations: %w", err)
	}

	// Read AMI actions
	var amiActions []*AMIAction
	if err := decoder.Decode(&amiActions); err != nil {
		return fmt.Errorf("failed to decode AMI actions: %w", err)
	}

	// Store dialogs
	for _, dialog := range dialogs {
		if dialog.ExpireTime.IsZero() || !dialog.ExpireTime.Before(time.Now()) {
			m.dialogs.Store(dialog.CallID, dialog)
			m.statsMu.Lock()
			m.stats.DialogCount++
			m.statsMu.Unlock()
		}
	}

	// Store RTP sessions
	for _, session := range rtpSessions {
		if session.ExpireTime.IsZero() || !session.ExpireTime.Before(time.Now()) {
			m.rtpSessions.Store(session.CallID, session)
			m.statsMu.Lock()
			m.stats.RTPSessionCount++
			m.statsMu.Unlock()
		}
	}

	// Store client sessions
	for _, session := range clientSessions {
		if session.ExpireTime.IsZero() || !session.ExpireTime.Before(time.Now()) {
			m.clientSessions.Store(session.ClientID, session)
			m.statsMu.Lock()
			m.stats.ClientCount++
			m.statsMu.Unlock()
		}
	}

	// Store registrations
	for _, reg := range registrations {
		if !reg.ExpireTime.Before(time.Now()) {
			m.registrations.Store(reg.AOR, reg)
			m.statsMu.Lock()
			m.stats.RegCount++
			m.statsMu.Unlock()
		}
	}

	// Store AMI actions
	for _, action := range amiActions {
		if action.ExpireTime.IsZero() || !action.ExpireTime.Before(time.Now()) {
			m.amiActions.Store(action.ActionID, action)
			m.statsMu.Lock()
			m.stats.AMIActionCount++
			m.statsMu.Unlock()
		}
	}

	m.logger.Info("Data loaded from file",
		zap.String("path", m.persistPath),
		zap.Int("dialogs", m.stats.DialogCount),
		zap.Int("rtpSessions", m.stats.RTPSessionCount),
		zap.Int("clientSessions", m.stats.ClientCount),
		zap.Int("registrations", m.stats.RegCount),
		zap.Int("amiActions", m.stats.AMIActionCount))

	return nil
}

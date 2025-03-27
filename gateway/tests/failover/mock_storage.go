package failover

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gateway/pkg/storage"
)

// MockStorage provides a simple in-memory implementation of storage.StateStorage
type MockStorage struct {
	data           map[string][]byte
	dialogs        map[string]*storage.Dialog
	rtpSessions    map[string]*storage.RTPSession
	amiActions     map[string]*storage.AMIAction
	clientSessions map[string]*storage.ClientSession
	wsConnections  map[string]*storage.WSConnectionInfo
	registrations  map[string]*storage.Registration
	mutex          sync.RWMutex
	healthy        bool
}

// NewMockStorage creates a new mock storage instance
func NewMockStorage() *MockStorage {
	return &MockStorage{
		data:           make(map[string][]byte),
		dialogs:        make(map[string]*storage.Dialog),
		rtpSessions:    make(map[string]*storage.RTPSession),
		amiActions:     make(map[string]*storage.AMIAction),
		clientSessions: make(map[string]*storage.ClientSession),
		wsConnections:  make(map[string]*storage.WSConnectionInfo),
		registrations:  make(map[string]*storage.Registration),
		healthy:        true, // Start in healthy state
	}
}

// SetHealthy sets the health state of the storage
func (m *MockStorage) SetHealthy(healthy bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.healthy = healthy
}

// Get retrieves a value from storage
func (m *MockStorage) Get(ctx context.Context, key string) ([]byte, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return nil, fmt.Errorf("storage is unhealthy")
	}

	if val, ok := m.data[key]; ok {
		return val, nil
	}
	return nil, storage.ErrNotFound
}

// Set stores a value in storage
func (m *MockStorage) Set(ctx context.Context, key string, value []byte, expiration time.Duration) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	m.data[key] = value
	return nil
}

// Delete removes a value from storage
func (m *MockStorage) Delete(ctx context.Context, key string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	if _, ok := m.data[key]; ok {
		delete(m.data, key)
		return nil
	}
	return storage.ErrNotFound
}

// GetDialog retrieves a dialog
func (m *MockStorage) GetDialog(ctx context.Context, callID string) (*storage.Dialog, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return nil, fmt.Errorf("storage is unhealthy")
	}

	if dialog, ok := m.dialogs[callID]; ok {
		return dialog, nil
	}
	return nil, storage.ErrNotFound
}

// StoreDialog saves a dialog
func (m *MockStorage) StoreDialog(ctx context.Context, dialog *storage.Dialog) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	m.dialogs[dialog.CallID] = dialog
	return nil
}

// DeleteDialog removes a dialog
func (m *MockStorage) DeleteDialog(ctx context.Context, callID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	if _, ok := m.dialogs[callID]; ok {
		delete(m.dialogs, callID)
		return nil
	}
	return storage.ErrNotFound
}

// GetRTPSession retrieves an RTP session
func (m *MockStorage) GetRTPSession(ctx context.Context, callID string) (*storage.RTPSession, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return nil, fmt.Errorf("storage is unhealthy")
	}

	if session, ok := m.rtpSessions[callID]; ok {
		return session, nil
	}
	return nil, storage.ErrNotFound
}

// StoreRTPSession saves an RTP session
func (m *MockStorage) StoreRTPSession(ctx context.Context, session *storage.RTPSession) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	m.rtpSessions[session.CallID] = session
	return nil
}

// DeleteRTPSession removes an RTP session
func (m *MockStorage) DeleteRTPSession(ctx context.Context, callID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	if _, ok := m.rtpSessions[callID]; ok {
		delete(m.rtpSessions, callID)
		return nil
	}
	return storage.ErrNotFound
}

// GetAMIAction retrieves an AMI action
func (m *MockStorage) GetAMIAction(ctx context.Context, actionID string) (*storage.AMIAction, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return nil, fmt.Errorf("storage is unhealthy")
	}

	if action, ok := m.amiActions[actionID]; ok {
		return action, nil
	}
	return nil, storage.ErrNotFound
}

// StoreAMIAction saves an AMI action
func (m *MockStorage) StoreAMIAction(ctx context.Context, action *storage.AMIAction) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	m.amiActions[action.ActionID] = action
	return nil
}

// DeleteAMIAction removes an AMI action
func (m *MockStorage) DeleteAMIAction(ctx context.Context, actionID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	if _, ok := m.amiActions[actionID]; ok {
		delete(m.amiActions, actionID)
		return nil
	}
	return storage.ErrNotFound
}

// GetClientSession retrieves a client session
func (m *MockStorage) GetClientSession(ctx context.Context, clientID string) (*storage.ClientSession, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return nil, fmt.Errorf("storage is unhealthy")
	}

	if session, ok := m.clientSessions[clientID]; ok {
		return session, nil
	}
	return nil, storage.ErrNotFound
}

// StoreClientSession saves a client session
func (m *MockStorage) StoreClientSession(ctx context.Context, session *storage.ClientSession) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	m.clientSessions[session.ClientID] = session
	return nil
}

// DeleteClientSession removes a client session
func (m *MockStorage) DeleteClientSession(ctx context.Context, clientID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	if _, ok := m.clientSessions[clientID]; ok {
		delete(m.clientSessions, clientID)
		return nil
	}
	return storage.ErrNotFound
}

// GetWSConnection retrieves WebSocket connection info
func (m *MockStorage) GetWSConnection(ctx context.Context, clientID string) (*storage.WSConnectionInfo, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return nil, fmt.Errorf("storage is unhealthy")
	}

	if conn, ok := m.wsConnections[clientID]; ok {
		return conn, nil
	}
	return nil, storage.ErrNotFound
}

// StoreWSConnection saves WebSocket connection info
func (m *MockStorage) StoreWSConnection(ctx context.Context, clientID string, info *storage.WSConnectionInfo) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	m.wsConnections[clientID] = info
	return nil
}

// DeleteWSConnection removes WebSocket connection info
func (m *MockStorage) DeleteWSConnection(ctx context.Context, clientID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	if _, ok := m.wsConnections[clientID]; ok {
		delete(m.wsConnections, clientID)
		return nil
	}
	return storage.ErrNotFound
}

// GetRegistration retrieves a registration
func (m *MockStorage) GetRegistration(ctx context.Context, aor string) (*storage.Registration, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return nil, fmt.Errorf("storage is unhealthy")
	}

	if reg, ok := m.registrations[aor]; ok {
		return reg, nil
	}
	return nil, storage.ErrNotFound
}

// StoreRegistration saves a registration
func (m *MockStorage) StoreRegistration(ctx context.Context, reg *storage.Registration) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	m.registrations[reg.AOR] = reg
	return nil
}

// DeleteRegistration removes a registration
func (m *MockStorage) DeleteRegistration(ctx context.Context, aor string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}

	if _, ok := m.registrations[aor]; ok {
		delete(m.registrations, aor)
		return nil
	}
	return storage.ErrNotFound
}

// ListDialogs returns a list of all dialogs
func (m *MockStorage) ListDialogs(ctx context.Context) ([]*storage.Dialog, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return nil, fmt.Errorf("storage is unhealthy")
	}

	dialogs := make([]*storage.Dialog, 0, len(m.dialogs))
	for _, dialog := range m.dialogs {
		dialogs = append(dialogs, dialog)
	}
	return dialogs, nil
}

// ListRTPSessions returns a list of all RTP sessions
func (m *MockStorage) ListRTPSessions(ctx context.Context) ([]*storage.RTPSession, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return nil, fmt.Errorf("storage is unhealthy")
	}

	sessions := make([]*storage.RTPSession, 0, len(m.rtpSessions))
	for _, session := range m.rtpSessions {
		sessions = append(sessions, session)
	}
	return sessions, nil
}

// ListAMIActionIDs returns a list of all AMI action IDs
func (m *MockStorage) ListAMIActionIDs(ctx context.Context) ([]string, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return nil, fmt.Errorf("storage is unhealthy")
	}

	actionIDs := make([]string, 0, len(m.amiActions))
	for actionID := range m.amiActions {
		actionIDs = append(actionIDs, actionID)
	}
	return actionIDs, nil
}

// ListClientSessions returns a list of all client sessions
func (m *MockStorage) ListClientSessions(ctx context.Context) ([]*storage.ClientSession, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return nil, fmt.Errorf("storage is unhealthy")
	}

	sessions := make([]*storage.ClientSession, 0, len(m.clientSessions))
	for _, session := range m.clientSessions {
		sessions = append(sessions, session)
	}
	return sessions, nil
}

// ListRegistrations returns a list of all registrations
func (m *MockStorage) ListRegistrations(ctx context.Context) ([]*storage.Registration, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return nil, fmt.Errorf("storage is unhealthy")
	}

	registrations := make([]*storage.Registration, 0, len(m.registrations))
	for _, reg := range m.registrations {
		registrations = append(registrations, reg)
	}
	return registrations, nil
}

// Ping performs a health check
func (m *MockStorage) Ping(ctx context.Context) error {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return fmt.Errorf("storage is unhealthy")
	}
	return nil
}

// Stats returns storage statistics
func (m *MockStorage) Stats(ctx context.Context) (*storage.StorageStats, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.healthy {
		return nil, fmt.Errorf("storage is unhealthy")
	}

	return &storage.StorageStats{
		TotalItems:      len(m.data),
		DialogCount:     len(m.dialogs),
		RTPSessionCount: len(m.rtpSessions),
		AMIActionCount:  len(m.amiActions),
		ClientCount:     len(m.clientSessions),
		WSConnCount:     len(m.wsConnections),
		RegCount:        len(m.registrations),
	}, nil
}

// Cleanup removes expired items (no-op in mock)
func (m *MockStorage) Cleanup(ctx context.Context) error {
	return nil
}

// Close cleans up resources (no-op in mock)
func (m *MockStorage) Close() error {
	return nil
}

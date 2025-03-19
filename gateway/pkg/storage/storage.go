// pkg/storage/storage.go
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Common errors
var (
	ErrNotFound      = errors.New("item not found")
	ErrAlreadyExists = errors.New("item already exists")
	ErrInvalidKey    = errors.New("invalid key")
	ErrInvalidValue  = errors.New("invalid value")
	ErrStorageFull   = errors.New("storage is full")
	ErrKeyExpired    = errors.New("key expired")
)

// KeyValue represents a value with expiration
type KeyValue struct {
	Value      []byte
	Expiration time.Time
}

// Dialog represents a SIP dialog
type Dialog struct {
	CallID       string            `json:"call_id"`
	FromTag      string            `json:"from_tag"`
	ToTag        string            `json:"to_tag,omitempty"`
	State        string            `json:"state"`                   // early, confirmed, terminated
	Route        []string          `json:"route,omitempty"`         // Route set for dialog
	RemoteSeq    int               `json:"remote_seq,omitempty"`    // Remote sequence number
	LocalSeq     int               `json:"local_seq,omitempty"`     // Local sequence number
	RemoteURI    string            `json:"remote_uri,omitempty"`    // URI of remote party
	LocalURI     string            `json:"local_uri,omitempty"`     // URI of local party
	RemoteTarget string            `json:"remote_target,omitempty"` // Remote target URI
	CreateTime   time.Time         `json:"create_time"`
	UpdateTime   time.Time         `json:"update_time"`
	ExpireTime   time.Time         `json:"expire_time,omitempty"`
	Data         map[string]string `json:"data,omitempty"` // Additional data
}

// RTPSession represents an RTPEngine session
type RTPSession struct {
	CallID     string    `json:"call_id"`
	EngineIdx  int       `json:"engine_idx"`
	EngineAddr string    `json:"engine_addr,omitempty"`
	FromTag    string    `json:"from_tag"`
	ToTag      string    `json:"to_tag,omitempty"`
	MediaInfo  string    `json:"media_info,omitempty"` // Media information (codecs, etc.)
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
	ExpireTime time.Time `json:"expire_time,omitempty"`
}

// AMIAction represents an Asterisk Manager Interface action
type AMIAction struct {
	ActionID   string            `json:"action_id"`
	Command    string            `json:"command"`
	Params     map[string]string `json:"params,omitempty"`
	ClientID   string            `json:"client_id"`
	Timestamp  time.Time         `json:"timestamp"`
	Retries    int               `json:"retries"`
	ExpireTime time.Time         `json:"expire_time,omitempty"`
}

// ClientSession represents a client connection session
type ClientSession struct {
	ClientID   string            `json:"client_id"`
	UserAgent  string            `json:"user_agent,omitempty"`
	SIPAddress string            `json:"sip_address,omitempty"`
	CallIDs    []string          `json:"call_ids,omitempty"`
	CreateTime time.Time         `json:"create_time"`
	LastSeen   time.Time         `json:"last_seen"`
	ExpireTime time.Time         `json:"expire_time,omitempty"`
	Data       map[string]string `json:"data,omitempty"`
}

// WSConnectionInfo tracks WebSocket connection info
type WSConnectionInfo struct {
	ClientID   string    `json:"client_id"`
	ServerAddr string    `json:"server_addr"`
	RemoteAddr string    `json:"remote_addr"`
	Protocol   string    `json:"protocol,omitempty"`
	Created    time.Time `json:"created"`
	LastSeen   time.Time `json:"last_seen"`
	ExpireTime time.Time `json:"expire_time,omitempty"`
}

// Registration tracks SIP registrations
type Registration struct {
	AOR          string    `json:"aor"` // Address of Record
	Contact      string    `json:"contact"`
	Expires      int       `json:"expires"`
	UserAgent    string    `json:"user_agent,omitempty"`
	CallID       string    `json:"call_id,omitempty"`
	CSeq         int       `json:"cseq,omitempty"`
	RegisterTime time.Time `json:"register_time"`
	ExpireTime   time.Time `json:"expire_time"`
}

// StorageStats provides statistics about storage usage
type StorageStats struct {
	TotalItems      int `json:"total_items"`
	DialogCount     int `json:"dialog_count"`
	RTPSessionCount int `json:"rtp_session_count"`
	AMIActionCount  int `json:"ami_action_count"`
	ClientCount     int `json:"client_count"`
	WSConnCount     int `json:"ws_conn_count"`
	RegCount        int `json:"reg_count"`
	ExpiredItems    int `json:"expired_items"`
	EvictedItems    int `json:"evicted_items"`
}

// StateStorage defines the interface for state persistence
type StateStorage interface {
	// Basic key-value operations
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, expiration time.Duration) error
	Delete(ctx context.Context, key string) error

	// Dialog operations
	GetDialog(ctx context.Context, callID string) (*Dialog, error)
	StoreDialog(ctx context.Context, dialog *Dialog) error
	DeleteDialog(ctx context.Context, callID string) error

	// RTPEngine session operations
	GetRTPSession(ctx context.Context, callID string) (*RTPSession, error)
	StoreRTPSession(ctx context.Context, session *RTPSession) error
	DeleteRTPSession(ctx context.Context, callID string) error

	// AMI action operations
	GetAMIAction(ctx context.Context, actionID string) (*AMIAction, error)
	StoreAMIAction(ctx context.Context, action *AMIAction) error
	DeleteAMIAction(ctx context.Context, actionID string) error

	// Client session operations
	GetClientSession(ctx context.Context, clientID string) (*ClientSession, error)
	StoreClientSession(ctx context.Context, session *ClientSession) error
	DeleteClientSession(ctx context.Context, clientID string) error

	// WebSocket connection operations
	GetWSConnection(ctx context.Context, clientID string) (*WSConnectionInfo, error)
	StoreWSConnection(ctx context.Context, clientID string, info *WSConnectionInfo) error
	DeleteWSConnection(ctx context.Context, clientID string) error

	// Registration operations
	GetRegistration(ctx context.Context, aor string) (*Registration, error)
	StoreRegistration(ctx context.Context, reg *Registration) error
	DeleteRegistration(ctx context.Context, aor string) error

	// List operations
	ListDialogs(ctx context.Context) ([]*Dialog, error)
	ListRTPSessions(ctx context.Context) ([]*RTPSession, error)
	ListClientSessions(ctx context.Context) ([]*ClientSession, error)
	ListRegistrations(ctx context.Context) ([]*Registration, error)

	// Utility methods
	Ping(ctx context.Context) error
	Stats(ctx context.Context) (*StorageStats, error)
	Cleanup(ctx context.Context) error
	Close() error
	// Add to the StateStorage interface
	ListAMIActionIDs(ctx context.Context) ([]string, error)
}

// Helper functions for JSON encoding/decoding
func marshalJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func unmarshalJSON(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

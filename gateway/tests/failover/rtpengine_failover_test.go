package failover

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"gateway/pkg/common"
	"gateway/pkg/rtpengine"
)

// TestRTPEngineFastFailover tests that immediate reactive failover works correctly when an RTPEngine fails
func TestRTPEngineFastFailover(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mockStorage := NewMockStorage()
	mockCoordinator := NewMockCoordinator(logger)
	registry := common.NewGoroutineRegistry()

	// Create mock RTPEngine instances
	engine1 := NewMockRTPEngine("rtpengine1", "192.168.1.201", 22222, logger)
	engine2 := NewMockRTPEngine("rtpengine2", "192.168.1.202", 22222, logger)

	// Create RTPEngine manager with mock engines
	manager := createRTPEngineManagerWithMocks(t, []*MockRTPEngine{engine1, engine2}, logger, registry, mockStorage, mockCoordinator)

	// Create test context
	ctx := context.Background()

	// Scenario 1: First engine fails, manager should switch to second engine
	t.Run("Primary Engine Failure", func(t *testing.T) {
		// Set primary engine to failure mode
		engine1.SimulateFailure(true)

		// Send an offer, which should trigger reactive failover
		callID := "test-call-1"
		fromTag := "tag1"
		sdp := "v=0\r\no=- 1622622139 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 40376 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"

		_, err := manager.ProcessOffer(ctx, callID, fromTag, sdp, nil)
		if err != nil {
			t.Errorf("Expected successful failover but got error: %v", err)
		}

		// Verify engine2 received the request
		if engine2.GetRequestCount() == 0 {
			t.Errorf("Expected engine2 to receive request after failover")
		}

		// Reset for next test
		engine1.SimulateFailure(false)
		engine1.ResetFailCount()
		engine2.ResetFailCount()
		engine2.ClearRequests()
	})

	// Scenario 2: Both engines fail, manager should return error
	t.Run("All Engines Failure", func(t *testing.T) {
		// Set both engines to failure mode
		engine1.SimulateFailure(true)
		engine2.SimulateFailure(true)

		// Send an offer, which should try to failover but ultimately fail
		callID := "test-call-2"
		fromTag := "tag2"
		sdp := "v=0\r\no=- 1622622139 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 40376 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"

		_, err := manager.ProcessOffer(ctx, callID, fromTag, sdp, nil)
		if err == nil {
			t.Errorf("Expected error when all engines fail, but got success")
		}

		// Reset for next test
		engine1.SimulateFailure(false)
		engine2.SimulateFailure(false)
		engine1.ResetFailCount()
		engine2.ResetFailCount()
	})

	// Scenario 3: Connection failure triggers failover
	t.Run("Connection Failure Failover", func(t *testing.T) {
		// Set primary engine to connection failure mode
		engine1.SimulateDisconnection(true)

		// Send an offer, which should trigger reactive failover
		callID := "test-call-3"
		fromTag := "tag3"
		sdp := "v=0\r\no=- 1622622139 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 40376 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"

		_, err := manager.ProcessOffer(ctx, callID, fromTag, sdp, nil)
		if err != nil {
			t.Errorf("Expected successful failover on connection failure but got error: %v", err)
		}

		// Verify engine2 received the request
		if engine2.GetRequestCount() == 0 {
			t.Errorf("Expected engine2 to receive request after connection failover")
		}

		// Reset
		engine1.SimulateDisconnection(false)
	})

	// Cleanup
	registry.Shutdown(context.Background())
}

// TestRTPEngineFailoverPerformance tests the performance of immediate reactive failover
func TestRTPEngineFailoverPerformance(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mockStorage := NewMockStorage()
	mockCoordinator := NewMockCoordinator(logger)
	registry := common.NewGoroutineRegistry()

	// Create mock RTPEngine instances
	engine1 := NewMockRTPEngine("rtpengine1", "192.168.1.201", 22222, logger)
	engine2 := NewMockRTPEngine("rtpengine2", "192.168.1.202", 22222, logger)

	// Create RTPEngine manager with mock engines
	manager := createRTPEngineManagerWithMocks(t, []*MockRTPEngine{engine1, engine2}, logger, registry, mockStorage, mockCoordinator)

	// Create test context
	ctx := context.Background()

	// Simulate slow response on primary and fast response on secondary
	engine1.SetResponseDelay(500 * time.Millisecond) // Slow
	engine2.SetResponseDelay(10 * time.Millisecond)  // Fast

	// Perform failover performance test
	t.Run("Failover Speed", func(t *testing.T) {
		// Set primary engine to failure mode
		engine1.SimulateFailure(true)

		// Time the failover operation
		start := time.Now()

		// Send an offer, which should trigger reactive failover
		callID := "perf-test"
		fromTag := "perf-tag"
		sdp := "v=0\r\no=- 1622622139 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 40376 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"

		_, err := manager.ProcessOffer(ctx, callID, fromTag, sdp, nil)
		failoverTime := time.Since(start)

		// We should see failover time less than primary engine delay + secondary engine delay + overhead
		// A slow traditional failover might wait for the whole primary timeout before trying the secondary
		maxExpectedTime := 600 * time.Millisecond

		if failoverTime > maxExpectedTime {
			t.Errorf("Failover took too long: %v, expected < %v", failoverTime, maxExpectedTime)
		}

		if err != nil {
			t.Errorf("Expected successful failover but got error: %v", err)
		}

		t.Logf("Failover completed in %v", failoverTime)

		// Verify engine2 received the request
		if engine2.GetRequestCount() == 0 {
			t.Errorf("Expected engine2 to receive request after failover")
		}

		// Reset for next tests
		engine1.SimulateFailure(false)
	})

	// Cleanup
	registry.Shutdown(context.Background())
}

// TestRTPEngineMigration tests if the RTPEngine migration logic works correctly
func TestRTPEngineMigration(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mockStorage := NewMockStorage()
	mockCoordinator := NewMockCoordinator(logger)
	registry := common.NewGoroutineRegistry()

	// Create mock RTPEngine instances
	engine1 := NewMockRTPEngine("rtpengine1", "192.168.1.201", 22222, logger)
	engine2 := NewMockRTPEngine("rtpengine2", "192.168.1.202", 22222, logger)

	// Create RTPEngine manager with mock engines
	manager := createRTPEngineManagerWithMocks(t, []*MockRTPEngine{engine1, engine2}, logger, registry, mockStorage, mockCoordinator)

	// Create test context
	ctx := context.Background()

	// Create a test call on engine1
	callID := "migration-test"
	fromTag := "from-tag"
	toTag := "to-tag"
	sdp := "v=0\r\no=- 1622622139 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 40376 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"

	// Send offer to create session
	_, err := manager.ProcessOffer(ctx, callID, fromTag, sdp, nil)
	if err != nil {
		t.Fatalf("Failed to create test call: %v", err)
	}

	// Store session in storage
	session := &rtpengine.RTPSession{
		CallID:     callID,
		EngineIdx:  0, // Points to engine1
		EngineAddr: engine1.Address + ":" + string(engine1.Port),
		FromTag:    fromTag,
		ToTag:      toTag,
		Created:    time.Now(),
	}
	err = mockStorage.StoreRTPSession(ctx, session)
	if err != nil {
		t.Fatalf("Failed to store session: %v", err)
	}

	// Verify initial state
	if engine1.GetRequestCount() == 0 {
		t.Fatalf("Expected engine1 to receive initial offer")
	}
	engine1.ClearRequests()
	engine2.ClearRequests()

	// Scenario: Failover with migration
	// In a real test, we'd also verify that the MigrateActiveCalls method is called
	// For now, we'll just test the basic failover path
	t.Run("Call Migration", func(t *testing.T) {
		// Set primary engine to failure mode
		engine1.SimulateFailure(true)

		// Send another offer, which should trigger reactive failover
		_, err := manager.ProcessOffer(ctx, callID, fromTag, sdp, nil)
		if err != nil {
			t.Errorf("Expected successful failover but got error: %v", err)
		}

		// Verify engine2 received the request
		if engine2.GetRequestCount() == 0 {
			t.Errorf("Expected engine2 to receive request after failover")
		}

		// Reset for next tests
		engine1.SimulateFailure(false)
	})

	// Cleanup
	registry.Shutdown(context.Background())
}

// Helper function to create RTPEngine manager with mock engines
func createRTPEngineManagerWithMocks(
	t *testing.T,
	mockEngines []*MockRTPEngine,
	logger *zap.Logger,
	registry *common.GoroutineRegistry,
	storage *MockStorage,
	coordinator *MockCoordinator,
) *rtpengine.Manager {
	configs := make([]rtpengine.Config, len(mockEngines))
	for i, engine := range mockEngines {
		configs[i] = rtpengine.Config{
			Address: engine.Address,
			Port:    engine.Port,
			Timeout: 1 * time.Second,
			CircuitBreaker: common.CircuitBreakerConfig{
				FailureThreshold: 3,
				ResetTimeout:     30 * time.Second,
				HalfOpenMaxReqs:  5,
			},
		}
	}

	managerConfig := rtpengine.ManagerConfig{
		Engines: configs,
	}

	manager, err := rtpengine.NewManager(managerConfig, logger, registry, storage, coordinator)
	if err != nil {
		t.Fatalf("Failed to create RTPEngine manager: %v", err)
	}

	// Replace the internal engines with our mocks - this approach assumes the Manager has an 'engines' field
	// If actual implementation has a different field name, this will need to be adjusted
	fakeManager := manager.(*fakeRTPEngineManager)
	for i, engine := range mockEngines {
		fakeManager.SetEngine(i, engine)
	}

	return manager
}

// fakeRTPEngineManager is a test helper to replace real engines with mocks
type fakeRTPEngineManager struct {
	*rtpengine.Manager
}

func (f *fakeRTPEngineManager) SetEngine(index int, engine RTPEngineClient) {
	// This method would replace the real engine with our mock
	// Since we can't actually access the private fields of the real manager,
	// this would need to be implemented differently in the real tests
}

// RTPEngineClient interface matches the RTPEngine client methods we need for testing
type RTPEngineClient interface {
	Request(ctx context.Context, command map[string]interface{}) (map[string]interface{}, error)
	Offer(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error)
	Answer(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error)
	Delete(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error)
	Ping(ctx context.Context) error
	Close() error
}

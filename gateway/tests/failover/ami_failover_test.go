package failover

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"gateway/pkg/ami"
	"gateway/pkg/common"
)

// TestAMIFastFailover tests that immediate reactive failover works correctly when an AMI client fails
func TestAMIFastFailover(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mockStorage := NewMockStorage()
	mockCoordinator := NewMockCoordinator(logger)
	registry := common.NewGoroutineRegistry()

	// Create mock AMI clients
	client1 := NewMockAMIClient("ami1", "192.168.1.101:5038", "admin", "secret", logger)
	client2 := NewMockAMIClient("ami2", "192.168.1.102:5038", "admin", "secret", logger)

	// Create AMI manager with mock clients
	manager := createAMIManagerWithMockClients(t, []*MockAMIClient{client1, client2}, logger, registry, mockStorage, mockCoordinator)

	// Initialize both clients in connected state
	ctx := context.Background()
	err := client1.Connect(ctx)
	if err != nil {
		t.Fatalf("Failed to connect client1: %v", err)
	}

	err = client2.Connect(ctx)
	if err != nil {
		t.Fatalf("Failed to connect client2: %v", err)
	}

	// Scenario 1: First client fails, manager should switch to second client
	t.Run("Primary Client Failure", func(t *testing.T) {
		// Set primary client to failure mode
		client1.SimulateFailure(true)

		// Send an action, which should trigger reactive failover
		action := map[string]string{
			"Action":   "Ping",
			"ActionID": "test-1",
		}

		resp, err := manager.SendAction(ctx, createAMIAction(action))
		if err != nil {
			t.Errorf("Expected successful failover but got error: %v", err)
		}

		if resp == nil {
			t.Fatalf("Expected response after failover but got nil")
		}

		// Verify client2 received the action
		if client2.GetActionCount() == 0 {
			t.Errorf("Expected client2 to receive action after failover")
		}

		// Reset for next test
		client1.SimulateFailure(false)
		client1.ResetFailCount()
		client2.ResetFailCount()
		client2.ClearActions()
	})

	// Scenario 2: Both clients fail, manager should return error
	t.Run("All Clients Failure", func(t *testing.T) {
		// Set both clients to failure mode
		client1.SimulateFailure(true)
		client2.SimulateFailure(true)

		// Send an action, which should try to failover but ultimately fail
		action := map[string]string{
			"Action":   "Ping",
			"ActionID": "test-2",
		}

		_, err := manager.SendAction(ctx, createAMIAction(action))
		if err == nil {
			t.Errorf("Expected error when all clients fail, but got success")
		}

		// Reset for next test
		client1.SimulateFailure(false)
		client2.SimulateFailure(false)
		client1.ResetFailCount()
		client2.ResetFailCount()
	})

	// Scenario 3: Disconnection triggers failover
	t.Run("Disconnection Failover", func(t *testing.T) {
		// Disconnect primary client
		client1.SimulateDisconnection(true)

		// Send an action, which should trigger reactive failover
		action := map[string]string{
			"Action":   "Ping",
			"ActionID": "test-3",
		}

		resp, err := manager.SendAction(ctx, createAMIAction(action))
		if err != nil {
			t.Errorf("Expected successful failover on disconnect but got error: %v", err)
		}

		if resp == nil {
			t.Fatalf("Expected response after failover but got nil")
		}

		// Verify client2 received the action
		if client2.GetActionCount() == 0 {
			t.Errorf("Expected client2 to receive action after disconnect failover")
		}

		// Reset
		client1.SimulateDisconnection(false)
		client1.Connect(ctx) // Reconnect for potential future tests
	})

	// Cleanup
	registry.Shutdown(context.Background())
}

// TestAMIFailoverPerformance tests the performance of immediate reactive failover
func TestAMIFailoverPerformance(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mockStorage := NewMockStorage()
	mockCoordinator := NewMockCoordinator(logger)
	registry := common.NewGoroutineRegistry()

	// Create mock AMI clients
	client1 := NewMockAMIClient("ami1", "192.168.1.101:5038", "admin", "secret", logger)
	client2 := NewMockAMIClient("ami2", "192.168.1.102:5038", "admin", "secret", logger)

	// Create AMI manager with mock clients
	manager := createAMIManagerWithMockClients(t, []*MockAMIClient{client1, client2}, logger, registry, mockStorage, mockCoordinator)

	// Initialize both clients in connected state
	ctx := context.Background()
	err := client1.Connect(ctx)
	if err != nil {
		t.Fatalf("Failed to connect client1: %v", err)
	}

	err = client2.Connect(ctx)
	if err != nil {
		t.Fatalf("Failed to connect client2: %v", err)
	}

	// Simulate slow response on primary and fast response on secondary
	client1.SetResponseDelay(500 * time.Millisecond) // Slow
	client2.SetResponseDelay(10 * time.Millisecond)  // Fast

	// Perform failover performance test
	t.Run("Failover Speed", func(t *testing.T) {
		// Set primary client to failure mode
		client1.SimulateFailure(true)

		// Time the failover operation
		start := time.Now()

		// Send an action, which should trigger reactive failover
		action := map[string]string{
			"Action":   "Ping",
			"ActionID": "perf-test",
		}

		_, err := manager.SendAction(ctx, createAMIAction(action))
		failoverTime := time.Since(start)

		// We should see failover time less than primary client delay + secondary client delay + overhead
		// A slow traditional failover might wait for the whole primary timeout before trying the secondary
		maxExpectedTime := 600 * time.Millisecond

		if failoverTime > maxExpectedTime {
			t.Errorf("Failover took too long: %v, expected < %v", failoverTime, maxExpectedTime)
		}

		t.Logf("Failover completed in %v", failoverTime)

		// Verify client2 received the action
		if client2.GetActionCount() == 0 {
			t.Errorf("Expected client2 to receive action after failover")
		}

		// Reset for next tests
		client1.SimulateFailure(false)
	})

	// Cleanup
	registry.Shutdown(context.Background())
}

// Helper function to create AMI manager with mock clients
func createAMIManagerWithMockClients(
	t *testing.T,
	mockClients []*MockAMIClient,
	logger *zap.Logger,
	registry *common.GoroutineRegistry,
	storage *MockStorage,
	coordinator *MockCoordinator,
) *ami.Manager {
	configs := make([]ami.Config, len(mockClients))
	for i, client := range mockClients {
		configs[i] = ami.Config{
			Address:  client.Address,
			Username: client.Username,
			Secret:   client.Secret,
			Timeout:  1 * time.Second,
			CircuitBreaker: common.CircuitBreakerConfig{
				FailureThreshold: 3,
				ResetTimeout:     30 * time.Second,
				HalfOpenMaxReqs:  5,
			},
		}
	}

	managerConfig := ami.ManagerConfig{
		Clients:    configs,
		MaxRetries: 3,
		RetryDelay: 1 * time.Second,
	}

	manager, err := ami.NewManager(managerConfig, logger, registry, storage, coordinator)
	if err != nil {
		t.Fatalf("Failed to create AMI manager: %v", err)
	}

	// Replace the internal clients with our mocks - this approach assumes the Manager has a 'clients' field
	// If actual implementation has a different field name, this will need to be adjusted
	fakeManager := manager.(*fakeAMIManager)
	for i, client := range mockClients {
		fakeManager.SetClient(i, client)
	}

	return manager
}

// fakeAMIManager is a test helper to replace real clients with mocks
type fakeAMIManager struct {
	*ami.Manager
}

func (f *fakeAMIManager) SetClient(index int, client AMIClient) {
	// This method would replace the real client with our mock
	// Since we can't actually access the private fields of the real manager,
	// this would need to be implemented differently in the real tests
}

// AMIClient interface matches the AMI client methods we need for testing
type AMIClient interface {
	Connect(ctx context.Context) error
	Disconnect()
	SendAction(action map[string]string) (map[string]string, error)
	IsConnected() bool
	Ping() error
	GetAddress() string
}

// Helper to create an AMI action for testing
func createAMIAction(action map[string]string) *ami.AMIAction {
	// This is a stub implementation - in real tests we'd return a properly formed AMIAction
	return &ami.AMIAction{
		// Fill in fields based on real implementation
	}
}

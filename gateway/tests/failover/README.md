# Gateway Failover Test Suite

This package provides comprehensive testing for the failover mechanisms in the Gateway project. The tests focus on verifying that the immediate reactive failover implementation works correctly and efficiently, improving the reliability of the system.

## Purpose

The failover tests validate:

1. **Fast Reactive Failover** - Tests that failover happens immediately on error detection rather than waiting for health checks
2. **Failover Performance** - Ensures that failover happens quickly enough to avoid service disruption
3. **Complete Failure Handling** - Verifies the system behaves correctly when all backends are unhealthy
4. **Reconnection Logic** - Tests how the system handles disconnection and reconnection scenarios
5. **Migration Capabilities** - For RTPEngine, tests the migration of active calls between engines

## Test Components

The test suite includes mocks and tests for the following components:

### Mock Implementations

- `mock_ami.go` - Mock implementation of AMI clients for testing
- `mock_rtpengine.go` - Mock implementation of RTPEngine for testing 
- `mock_storage.go` - Mock implementation of storage for testing
- `mock_coordinator.go` - Mock implementation of coordinator for testing

### Test Files

- `ami_failover_test.go` - Tests for AMI client failover
- `rtpengine_failover_test.go` - Tests for RTPEngine failover

## Running the Tests

To run the full test suite:

```bash
go test -v ./tests/failover/...
```

To run a specific test:

```bash
go test -v ./tests/failover -run TestAMIFastFailover
```

## Test Scenarios

The test suite covers the following scenarios:

1. **Primary Backend Failure**: Tests failover when the primary backend fails
2. **All Backends Failure**: Tests behavior when all backends are unavailable 
3. **Disconnection Handling**: Tests failover when a backend disconnects
4. **Failover Speed**: Tests the performance of immediate reactive failover
5. **Call Migration**: For RTPEngine, tests the migration of active calls between engines

## Integration with CI/CD

These tests are designed to be part of the continuous integration pipeline. They help ensure that modifications to the codebase don't negatively impact the reliability features of the gateway.

## Extending the Tests

When adding new failover mechanisms to the gateway, corresponding tests should be added to this package. The mock implementations can be extended to support new functionality as needed.

## Notes for Developers

- The tests use mock implementations that simulate failure conditions without requiring real backend services
- Some tests may need to be adapted as the actual implementation evolves
- Pay attention to timing-sensitive tests, which may need adjustment based on the environment 
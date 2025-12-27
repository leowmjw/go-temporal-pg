#!/bin/bash

set -e

echo "Starting end-to-end tests..."

# Wait for all services to be ready
echo "Waiting for services to be ready..."
./test/e2e/wait-for-services.sh

# Setup test environment
echo "Setting up test environment..."
./test/e2e/setup-test-env.sh

# Run the integration tests
echo "Running integration tests..."
./integration-tests -test.v -test.timeout=30m

# Run specific end-to-end scenarios
echo "Running end-to-end scenarios..."

# Scenario 1: Happy path upgrade
echo "Testing happy path upgrade..."
./test/e2e/test-happy-path.sh

# Scenario 2: Upgrade with rollback
echo "Testing upgrade with rollback..."
./test/e2e/test-rollback-scenario.sh

# Scenario 3: Health check failure
echo "Testing health check failure scenario..."
./test/e2e/test-health-check-failure.sh

# Scenario 4: Concurrent upgrades
echo "Testing concurrent upgrades..."
./test/e2e/test-concurrent-upgrades.sh

# Scenario 5: Large dataset upgrade
echo "Testing large dataset upgrade..."
./test/e2e/test-large-dataset.sh

# Cleanup
echo "Cleaning up test environment..."
./test/e2e/cleanup-test-env.sh

echo "All end-to-end tests completed successfully!"

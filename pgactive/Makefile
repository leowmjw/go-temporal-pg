# Makefile for pgactive-upgrade project

.PHONY: help build test test-unit test-integration test-e2e clean docker-build docker-test lint fmt vet mod-tidy run-local setup-test-env teardown-test-env

# Default target
help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# Build targets
build: ## Build the application binary
	@echo "Building pgactive-upgrade..."
	go build -o bin/pgactive-upgrade ./cmd/main.go

build-test: ## Build test binaries
	@echo "Building test binaries..."
	go test -c -o bin/unit-tests ./internal/...
	go test -c -o bin/integration-tests ./test/integration/

# Test targets
test: test-unit test-integration ## Run all tests

test-unit: ## Run unit tests with coverage
	@echo "Running unit tests..."
	go test -v -race -coverprofile=coverage.out ./internal/...
	go tool cover -html=coverage.out -o coverage.html

test-integration: ## Run integration tests (requires Temporal server)
	@echo "Running integration tests..."
	go test -v -timeout=30m ./test/integration/

test-e2e: ## Run end-to-end tests using Docker Compose
	@echo "Running end-to-end tests..."
	docker-compose -f docker-compose.test.yml up --build --abort-on-container-exit
	docker-compose -f docker-compose.test.yml down -v

# Docker targets
docker-build: ## Build Docker image for testing
	@echo "Building Docker image..."
	docker build -f Dockerfile.test -t pgactive-upgrade:test .

docker-test: docker-build ## Run tests in Docker environment
	@echo "Running tests in Docker..."
	docker-compose -f docker-compose.test.yml up --build --abort-on-container-exit
	docker-compose -f docker-compose.test.yml down -v

# Development targets
run-local: ## Run the application locally (requires local Temporal server)
	@echo "Running pgactive-upgrade locally..."
	TEMPORAL_HOST=localhost:7233 go run ./cmd/main.go

lint: ## Run golangci-lint
	@echo "Running linter..."
	golangci-lint run

fmt: ## Format Go code
	@echo "Formatting code..."
	go fmt ./...

vet: ## Run go vet
	@echo "Running go vet..."
	go vet ./...

mod-tidy: ## Tidy Go modules
	@echo "Tidying modules..."
	go mod tidy

# Test environment management
setup-test-env: ## Start test environment (Temporal, PostgreSQL, etc.)
	@echo "Starting test environment..."
	docker-compose -f docker-compose.test.yml up -d temporal postgres-source postgres-target localstack
	@echo "Waiting for services to be ready..."
	sleep 30
	@echo "Test environment is ready!"

teardown-test-env: ## Stop and clean up test environment
	@echo "Tearing down test environment..."
	docker-compose -f docker-compose.test.yml down -v
	docker system prune -f

# Monitoring
start-monitoring: ## Start monitoring stack (Prometheus, Grafana, Jaeger)
	@echo "Starting monitoring stack..."
	docker-compose -f docker-compose.test.yml up -d prometheus grafana jaeger

stop-monitoring: ## Stop monitoring stack
	@echo "Stopping monitoring stack..."
	docker-compose -f docker-compose.test.yml stop prometheus grafana jaeger

# Mock generation
generate-mocks: ## Generate mocks for testing
	@echo "Generating mocks..."
	go generate ./...
	mockgen -source=internal/activities/activities.go -destination=internal/activities/mocks/mock_activities.go

# Clean targets
clean: ## Clean build artifacts and test files
	@echo "Cleaning..."
	rm -rf bin/
	rm -f coverage.out coverage.html
	rm -f *.log
	docker-compose -f docker-compose.test.yml down -v 2>/dev/null || true
	docker system prune -f

clean-all: clean ## Clean everything including Docker volumes and images
	@echo "Deep cleaning..."
	docker volume prune -f
	docker image prune -a -f

# Database management
reset-test-db: ## Reset test databases
	@echo "Resetting test databases..."
	docker-compose -f docker-compose.test.yml restart postgres-source postgres-target
	sleep 10

# Utility targets
check-deps: ## Check for required dependencies
	@echo "Checking dependencies..."
	@command -v go >/dev/null 2>&1 || { echo "Go is required but not installed"; exit 1; }
	@command -v docker >/dev/null 2>&1 || { echo "Docker is required but not installed"; exit 1; }
	@command -v docker-compose >/dev/null 2>&1 || { echo "Docker Compose is required but not installed"; exit 1; }
	@echo "All dependencies are available!"

logs: ## Show logs from test environment
	docker-compose -f docker-compose.test.yml logs -f

logs-app: ## Show application logs only
	docker-compose -f docker-compose.test.yml logs -f pgactive-upgrade

# Development workflow
dev-setup: check-deps mod-tidy setup-test-env ## Set up development environment
	@echo "Development environment is ready!"
	@echo "Access points:"
	@echo "  - Temporal Web UI: http://localhost:8233"
	@echo "  - Grafana: http://localhost:3000 (admin/admin)"
	@echo "  - Prometheus: http://localhost:9090"
	@echo "  - Jaeger: http://localhost:16686"

dev-test: test-unit ## Run development tests
	@echo "Development tests completed!"

# Release targets
release-test: test docker-test ## Run full test suite for release
	@echo "Release tests completed successfully!"

# Benchmark tests
benchmark: ## Run benchmark tests
	@echo "Running benchmark tests..."
	go test -bench=. -benchmem ./...

# Security scanning
security-scan: ## Run security vulnerability scan
	@echo "Running security scan..."
	go list -json -m all | nancy sleuth

# Documentation
docs: ## Generate documentation
	@echo "Generating documentation..."
	go doc -all ./... > docs/api.md

.DEFAULT_GOAL := help

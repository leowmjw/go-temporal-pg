#!/bin/bash

set -e

# Function to wait for a service to be ready
wait_for_service() {
    local host=$1
    local port=$2
    local service_name=$3
    local max_attempts=${4:-30}
    
    echo "Waiting for $service_name at $host:$port..."
    
    for i in $(seq 1 $max_attempts); do
        if nc -z "$host" "$port" 2>/dev/null; then
            echo "$service_name is ready!"
            return 0
        fi
        echo "Attempt $i/$max_attempts: $service_name not ready yet..."
        sleep 2
    done
    
    echo "ERROR: $service_name did not become ready in time"
    return 1
}

# Function to wait for HTTP endpoint
wait_for_http() {
    local url=$1
    local service_name=$2
    local max_attempts=${3:-30}
    
    echo "Waiting for $service_name HTTP endpoint at $url..."
    
    for i in $(seq 1 $max_attempts); do
        if curl -f -s "$url" >/dev/null 2>&1; then
            echo "$service_name HTTP endpoint is ready!"
            return 0
        fi
        echo "Attempt $i/$max_attempts: $service_name HTTP endpoint not ready yet..."
        sleep 2
    done
    
    echo "ERROR: $service_name HTTP endpoint did not become ready in time"
    return 1
}

# Function to wait for PostgreSQL
wait_for_postgres() {
    local host=$1
    local port=$2
    local user=$3
    local db=$4
    local service_name=$5
    local max_attempts=${6:-30}
    
    echo "Waiting for PostgreSQL $service_name at $host:$port..."
    
    for i in $(seq 1 $max_attempts); do
        if PGPASSWORD="testpass" psql -h "$host" -p "$port" -U "$user" -d "$db" -c "SELECT 1;" >/dev/null 2>&1; then
            echo "PostgreSQL $service_name is ready!"
            return 0
        fi
        echo "Attempt $i/$max_attempts: PostgreSQL $service_name not ready yet..."
        sleep 2
    done
    
    echo "ERROR: PostgreSQL $service_name did not become ready in time"
    return 1
}

# Wait for all required services
echo "Waiting for all services to be ready..."

# PostgreSQL instances
wait_for_postgres "postgres-source" "5432" "postgres" "testdb" "source"
wait_for_postgres "postgres-target" "5433" "postgres" "testdb" "target"
wait_for_postgres "postgres-temporal" "5434" "temporal" "temporal" "temporal"

# Temporal server
wait_for_service "temporal" "7233" "Temporal gRPC"
wait_for_http "http://temporal:8233" "Temporal Web UI"

# LocalStack
wait_for_http "http://localstack:4566/_localstack/health" "LocalStack"

# Additional service checks
echo "Running additional service health checks..."

# Check Temporal cluster health
echo "Checking Temporal cluster health..."
for i in $(seq 1 10); do
    if docker exec pgactive-upgrade_temporal_1 tctl --address temporal:7233 cluster health 2>/dev/null; then
        echo "Temporal cluster is healthy!"
        break
    fi
    echo "Attempt $i/10: Temporal cluster not healthy yet..."
    sleep 3
done

# Check LocalStack services
echo "Checking LocalStack services..."
curl -f "http://localstack:4566/_localstack/health" | jq '.'

# Verify PostgreSQL extensions
echo "Verifying PostgreSQL configurations..."
PGPASSWORD="testpass" psql -h "postgres-source" -p "5432" -U "postgres" -d "testdb" -c "SHOW wal_level; SHOW max_replication_slots;"
PGPASSWORD="testpass" psql -h "postgres-target" -p "5433" -U "postgres" -d "testdb" -c "SHOW wal_level; SHOW max_replication_slots;"

echo "All services are ready for testing!"

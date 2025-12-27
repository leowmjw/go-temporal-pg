#!/bin/bash

set -e

echo "=== Testing Happy Path Upgrade Scenario ==="

# Test configuration
WORKFLOW_ID="e2e-happy-path-$(date +%Y%m%d-%H%M%S)"
SOURCE_DB="test-source-db"
TARGET_VERSION="15.4"

# Function to call Temporal CLI
temporal_cli() {
    docker exec pgactive-upgrade_temporal_1 tctl --address temporal:7233 "$@"
}

# Function to start workflow
start_workflow() {
    local workflow_id=$1
    local input=$2
    
    echo "Starting workflow: $workflow_id"
    temporal_cli workflow start \
        --taskqueue pgactive-upgrade \
        --type RollingUpgradeWorkflow \
        --workflow_id "$workflow_id" \
        --input "$input"
}

# Function to query workflow progress
query_progress() {
    local workflow_id=$1
    
    temporal_cli workflow query \
        --workflow_id "$workflow_id" \
        --query_type progress 2>/dev/null || echo '{"status":"unknown"}'
}

# Function to wait for workflow completion
wait_for_completion() {
    local workflow_id=$1
    local timeout=${2:-1800} # 30 minutes default
    local start_time=$(date +%s)
    
    echo "Waiting for workflow $workflow_id to complete (timeout: ${timeout}s)..."
    
    while true; do
        local current_time=$(date +%s)
        local elapsed=$((current_time - start_time))
        
        if [ $elapsed -ge $timeout ]; then
            echo "ERROR: Workflow timed out after ${timeout} seconds"
            return 1
        fi
        
        # Get workflow status
        local status=$(temporal_cli workflow describe --workflow_id "$workflow_id" --json | jq -r '.WorkflowExecutionInfo.Status // "UNKNOWN"')
        
        case $status in
            "COMPLETED")
                echo "Workflow completed successfully!"
                return 0
                ;;
            "FAILED"|"TERMINATED"|"CANCELED"|"TIMED_OUT")
                echo "ERROR: Workflow ended with status: $status"
                temporal_cli workflow show --workflow_id "$workflow_id"
                return 1
                ;;
            "RUNNING")
                # Query progress
                local progress=$(query_progress "$workflow_id")
                local phase=$(echo "$progress" | jq -r '.phase // "unknown"')
                local percent=$(echo "$progress" | jq -r '.percent // 0')
                echo "Workflow running - Phase: $phase, Progress: $percent%"
                ;;
        esac
        
        sleep 10
    done
}

# Prepare workflow input
WORKFLOW_INPUT=$(cat <<EOF
{
  "source_db_instance_id": "$SOURCE_DB",
  "target_version": "$TARGET_VERSION",
  "shift_percentages": [25, 25, 50],
  "subnets": ["subnet-12345", "subnet-67890"],
  "security_group_ids": ["sg-test123"],
  "instance_class": "db.r6g.large",
  "backup_retention_days": 7,
  "tags": {
    "Environment": "test",
    "Purpose": "e2e-testing",
    "TestCase": "happy-path"
  }
}
EOF
)

echo "Workflow input:"
echo "$WORKFLOW_INPUT" | jq '.'

# Start the upgrade workflow
start_workflow "$WORKFLOW_ID" "$WORKFLOW_INPUT"

# Monitor progress with periodic queries
echo "Monitoring workflow progress..."
for i in {1..10}; do
    sleep 30
    progress=$(query_progress "$WORKFLOW_ID")
    echo "Progress check $i: $progress"
    
    # Parse progress
    status=$(echo "$progress" | jq -r '.status // "unknown"')
    if [ "$status" = "completed" ]; then
        echo "Workflow completed based on progress query!"
        break
    fi
done

# Wait for final completion
wait_for_completion "$WORKFLOW_ID" 1800

# Get final workflow result
echo "Getting final workflow result..."
RESULT=$(temporal_cli workflow show --workflow_id "$WORKFLOW_ID" --output json)
echo "Final result:"
echo "$RESULT" | jq '.WorkflowExecutionInfo.Result'

# Verify result
FINAL_STATUS=$(echo "$RESULT" | jq -r '.WorkflowExecutionInfo.Result.payloads[0].data' | base64 -d | jq -r '.status')
FINAL_PERCENT=$(echo "$RESULT" | jq -r '.WorkflowExecutionInfo.Result.payloads[0].data' | base64 -d | jq -r '.percent')

if [ "$FINAL_STATUS" = "completed" ] && [ "$FINAL_PERCENT" = "100" ]; then
    echo "✅ Happy path test PASSED!"
    echo "   - Final Status: $FINAL_STATUS"
    echo "   - Final Progress: $FINAL_PERCENT%"
else
    echo "❌ Happy path test FAILED!"
    echo "   - Expected: status=completed, percent=100"
    echo "   - Actual: status=$FINAL_STATUS, percent=$FINAL_PERCENT"
    exit 1
fi

# Additional validations
echo "Running additional validations..."

# Check workflow history for expected activities
echo "Validating workflow history..."
HISTORY=$(temporal_cli workflow show --workflow_id "$WORKFLOW_ID" --output json)

# Expected activities in order
EXPECTED_ACTIVITIES=(
    "ValidateInput"
    "ProvisionTargetDB"
    "ConfigurePgactiveParams"
    "InstallPgactiveExtension"
    "WaitForSync"
    "TrafficShiftPhase"
    "RunHealthChecks"
    "Cutover"
    "OptionallyDetachOld"
    "DecommissionSource"
)

echo "Checking for expected activities..."
for activity in "${EXPECTED_ACTIVITIES[@]}"; do
    if echo "$HISTORY" | jq -r '.History.events[].activityTaskCompletedEventAttributes.activityType.name // empty' | grep -q "$activity"; then
        echo "✅ Found activity: $activity"
    else
        echo "❌ Missing activity: $activity"
    fi
done

# Check for child workflow execution
if echo "$HISTORY" | jq -r '.History.events[].childWorkflowExecutionCompletedEventAttributes.workflowType.name // empty' | grep -q "InitReplicationGroupWorkflow"; then
    echo "✅ Found child workflow: InitReplicationGroupWorkflow"
else
    echo "❌ Missing child workflow: InitReplicationGroupWorkflow"
fi

echo "=== Happy Path Upgrade Test Completed Successfully ==="

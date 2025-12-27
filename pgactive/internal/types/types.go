package types

import "time"

// UpgradeInput contains all parameters for the rolling upgrade workflow
type UpgradeInput struct {
	SourceDBInstanceID   string            `json:"source_db_instance_id"`
	TargetVersion       string            `json:"target_version"`
	Subnets             []string          `json:"subnets"`
	ShiftPercentages    []int             `json:"shift_percentages"`
	ReplicationFilters  []string          `json:"replication_filters"`
	InstanceClass       string            `json:"instance_class,omitempty"`
	SecurityGroupIDs    []string          `json:"security_group_ids"`
	BackupRetentionDays int               `json:"backup_retention_days,omitempty"`
	Tags                map[string]string `json:"tags,omitempty"`
}

// ProgressResponse represents the current state of the upgrade
type ProgressResponse struct {
	Phase               string    `json:"phase"`
	Percent             int       `json:"percent"`
	ReplicationLagBytes int64     `json:"replication_lag_bytes"`
	LastUpdated         time.Time `json:"last_updated"`
	Status              string    `json:"status"`
	Message             string    `json:"message,omitempty"`
}

// ActivityInput represents generic activity input with DB identifiers
type ActivityInput struct {
	SourceDBInstanceID string `json:"source_db_instance_id"`
	TargetDBInstanceID string `json:"target_db_instance_id"`
	ReplicationGroup   string `json:"replication_group,omitempty"`
}

// TrafficShiftInput for gradual traffic shifting
type TrafficShiftInput struct {
	ActivityInput
	ShiftPercentage int `json:"shift_percentage"`
	Phase           int `json:"phase"`
}

// HealthCheckInput for post-shift validation
type HealthCheckInput struct {
	ActivityInput
	CheckType string `json:"check_type"`
	Timeout   int    `json:"timeout_seconds"`
}

// RollbackSignal for workflow rollback
type RollbackSignal struct {
	Reason    string    `json:"reason"`
	Timestamp time.Time `json:"timestamp"`
	RequestID string    `json:"request_id"`
}

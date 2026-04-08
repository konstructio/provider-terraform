package metrics

import (
	"time"
)

// TerraformOperationTimer helps with timing Terraform CLI operations.
type TerraformOperationTimer struct {
	Workspace string
	Operation string
	StartTime time.Time
}

// NewTerraformOperationTimer creates a new timer for a Terraform CLI operation.
func NewTerraformOperationTimer(workspace, operation string) *TerraformOperationTimer {
	if workspace == "" {
		workspace = "unknown"
	}
	return &TerraformOperationTimer{
		Workspace: workspace,
		Operation: operation,
		StartTime: time.Now(),
	}
}

// RecordSuccess records a successful Terraform operation.
func (t *TerraformOperationTimer) RecordSuccess() {
	t.record("success")
}

// RecordFailure records a failed Terraform operation.
func (t *TerraformOperationTimer) RecordFailure() {
	t.record("failure")
}

func (t *TerraformOperationTimer) record(status string) {
	duration := time.Since(t.StartTime)
	TerraformOperationsTotal.WithLabelValues(t.Workspace, t.Operation, status).Inc()
	TerraformOperationDuration.WithLabelValues(t.Workspace, t.Operation).Observe(duration.Seconds())
}

package domain

import "time"

// Task status values. "interrupted" is distinct from "failed": it records that
// the owning process stopped before the work reached a terminal state, which is
// recoverable information rather than an error in the work itself.
const (
	TaskQueued      = "queued"
	TaskRunning     = "running"
	TaskCompleted   = "completed"
	TaskFailed      = "failed"
	TaskCancelled   = "cancelled"
	TaskInterrupted = "interrupted"
)

// AnalysisTask is the durable record behind the Web analysis endpoint. The
// evidence it produces is already persisted separately; this tracks the request
// itself so progress and failure survive a restart.
type AnalysisTask struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	AssetID   string    `json:"asset_id"`
	Status    string    `json:"status"`
	CacheKey  string    `json:"cache_key,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (t AnalysisTask) Terminal() bool {
	switch t.Status {
	case TaskCompleted, TaskFailed, TaskCancelled, TaskInterrupted:
		return true
	}
	return false
}

type AnalysisRun struct {
	ProjectID        string            `json:"project_id"`
	AssetID          string            `json:"asset_id"`
	AssetContentHash string            `json:"asset_content_hash"`
	CacheKey         string            `json:"cache_key"`
	AnalyzerVersion  string            `json:"analyzer_version"`
	Provider         string            `json:"provider"`
	Parameters       map[string]any    `json:"parameters,omitempty"`
	Status           string            `json:"status"`
	Stages           map[string]string `json:"stages,omitempty"`
	EvidenceIDs      []string          `json:"evidence_ids,omitempty"`
	Error            string            `json:"error,omitempty"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

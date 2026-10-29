package model

import (
	"errors"
	"time"
)

// Training runs link datasets to the models trained on them (migration
// 000027): a run TRAINED_ON the dataset versions it read, and a model
// PRODUCED_BY its run.

type TrainingSource string

const (
	SourceSDK    TrainingSource = "SDK"
	SourceMLflow TrainingSource = "MLFLOW"
	SourceWandb  TrainingSource = "WANDB"
	SourceOther  TrainingSource = "OTHER"
)

type TrainingStatus string

const (
	RunRunning  TrainingStatus = "RUNNING"
	RunFinished TrainingStatus = "FINISHED"
	RunFailed   TrainingStatus = "FAILED"
)

var (
	ErrTrainingRunNotFound = errors.New("training run not found")
	// ErrModelExists: the organization already has this model name and version.
	ErrModelExists = errors.New("a model with this name and version already exists")
)

type TrainingRun struct {
	ID         string
	TenantID   string
	Name       string
	Source     TrainingSource
	ExternalID string
	URL        string
	Status     TrainingStatus
	Params     map[string]string
	Metrics    map[string]float64
	VersionIDs []string
	Models     []Model
	CreatedBy  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt *time.Time
}

type Model struct {
	ID        string
	RunID     string
	Name      string
	Version   string
	URI       string
	CreatedBy string
	CreatedAt time.Time
}

type TrainingRunPage struct {
	Runs       []TrainingRun
	NextCursor string
}

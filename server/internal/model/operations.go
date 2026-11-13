package model

import "time"

// OperationsStatus is one organization's background work at one moment:
// what is waiting, what is running and how long the oldest item has
// waited. Counts only; it names no files.
type OperationsStatus struct {
	At time.Time

	// ProcessingWaiting jobs are not finished yet (verification, scanning or
	// promotion still to do); ProcessingHeld ones ran out of attempts and
	// wait for an operator.
	ProcessingWaiting    int64
	ProcessingHeld       int64
	OldestProcessingWait time.Duration

	IngestionJobsActive int64
	EntriesWaiting      int64
	EntriesInFlight     int64

	ImportsActive      int64
	ValidationsQueued  int64
	ValidationsRunning int64
	DeletionsScheduled int64

	// SharedAssets are available assets stored as no copy of their own:
	// their bytes equal an object already stored. SharedBytes is what
	// copies of them would have taken.
	SharedAssets int64
	SharedBytes  int64

	// Workers are the heartbeats written in the last day, by every worker
	// process of the platform: they are shared by all organizations.
	Workers []WorkerHeartbeat
}

// WorkerHeartbeat is one worker process's last sign of life.
type WorkerHeartbeat struct {
	Kind       string
	Instance   string
	StartedAt  time.Time
	LastSeenAt time.Time
}

// Worker heartbeats are written every WorkerBeatInterval; a process silent
// for WorkerStaleAfter is taken as stopped.
const (
	WorkerBeatInterval = 30 * time.Second
	WorkerStaleAfter   = 2 * time.Minute
)

// WorkerKinds are the workers that write heartbeats, in the order a status
// page lists them. A kind with no live process is still reported.
var WorkerKinds = []string{"upload", "processing", "packing", "dataset", "outbox", "cleanup", "reconciliation", "compaction", "webhook"}

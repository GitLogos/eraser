package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/eraser-privacy/eraser/internal/history"

	"github.com/google/uuid"
)

// JobStatus represents the status of a background job
type JobStatus string

const (
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusCancelled JobStatus = "cancelled"
	JobStatusPaused    JobStatus = "paused" // Paused due to daily limit
	JobStatusError     JobStatus = "error"  // Stopped due to auth/config error
)

// Job represents a background email sending job
type Job struct {
	ID            string    `json:"id"`
	Status        JobStatus `json:"status"`
	Progress      int       `json:"progress"`
	Sent          int       `json:"sent"`
	Failed        int       `json:"failed"`
	Total         int       `json:"total"`
	CurrentBroker string    `json:"current_broker"`
	CurrentProfile string   `json:"current_profile"` // ID of the profile currently being sent on behalf of
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at,omitempty"`
	Error         string    `json:"error,omitempty"`
	ErrorType     string    `json:"error_type,omitempty"`  // "auth", "rate_limit", etc.
	DailyLimit    int       `json:"daily_limit,omitempty"` // Max emails per day
	DaySent       int       `json:"day_sent,omitempty"`    // Emails sent today

	ctx                  context.Context
	cancelFunc           context.CancelFunc
	mu                   sync.Mutex
	consecutiveAuthFails int // Track consecutive auth failures
}

// Update updates the job progress
func (j *Job) Update(sent, failed int, currentBroker, currentProfile string) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.Sent = sent
	j.Failed = failed
	j.CurrentBroker = currentBroker
	j.CurrentProfile = currentProfile
	if j.Total > 0 {
		j.Progress = ((sent + failed) * 100) / j.Total
	}
}

// Complete marks the job as completed
func (j *Job) Complete() {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.Status = JobStatusCompleted
	j.CompletedAt = time.Now()
	j.Progress = 100
	j.CurrentBroker = ""
	j.CurrentProfile = ""
}

// StopWithError stops the job due to an error
func (j *Job) StopWithError(errorType, errorMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.Status = JobStatusCompleted
	j.CompletedAt = time.Now()
	j.Error = errorMsg
	j.ErrorType = errorType
	j.CurrentBroker = ""
	j.CurrentProfile = ""
}

// RecordAuthFailure records an auth failure and returns true if job should stop
func (j *Job) RecordAuthFailure() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.consecutiveAuthFails++
	return j.consecutiveAuthFails >= 3
}

// ResetAuthFailures resets the consecutive auth failure counter
func (j *Job) ResetAuthFailures() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.consecutiveAuthFails = 0
}

// Cancel cancels the job
func (j *Job) Cancel() {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.Status == JobStatusRunning {
		j.Status = JobStatusCancelled
		j.CompletedAt = time.Now()
		if j.cancelFunc != nil {
			j.cancelFunc()
		}
	}
}

// IsCancelled returns true if the job was cancelled
func (j *Job) IsCancelled() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Status == JobStatusCancelled
}

// Context returns the job's context
func (j *Job) Context() context.Context {
	return j.ctx
}

// ToJSON returns the job data for JSON serialization
func (j *Job) ToJSON() map[string]interface{} {
	j.mu.Lock()
	defer j.mu.Unlock()

	return map[string]interface{}{
		"id":              j.ID,
		"status":          j.Status,
		"progress":        j.Progress,
		"sent":            j.Sent,
		"failed":          j.Failed,
		"total":           j.Total,
		"current_broker":  j.CurrentBroker,
		"current_profile": j.CurrentProfile,
		"started_at":      j.StartedAt,
		"completed_at":    j.CompletedAt,
		"error":           j.Error,
		"error_type":      j.ErrorType,
		"daily_limit":     j.DailyLimit,
		"day_sent":        j.DaySent,
	}
}

// JobManager manages background jobs
type JobManager struct {
	jobs map[string]*Job
	mu   sync.RWMutex
}

// NewJobManager creates a new job manager
func NewJobManager() *JobManager {
	return &JobManager{
		jobs: make(map[string]*Job),
	}
}

// Create creates a new job with the given total count
func (jm *JobManager) Create(total int) *Job {
	jm.mu.Lock()
	defer jm.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())

	job := &Job{
		ID:         uuid.New().String(),
		Status:     JobStatusRunning,
		Progress:   0,
		Sent:       0,
		Failed:     0,
		Total:      total,
		StartedAt:  time.Now(),
		ctx:        ctx,
		cancelFunc: cancel,
	}

	jm.jobs[job.ID] = job
	return job
}

// Get returns a job by ID, or nil if not found
func (jm *JobManager) Get(id string) *Job {
	jm.mu.RLock()
	defer jm.mu.RUnlock()

	return jm.jobs[id]
}

// GetActive returns the currently running job, or nil if none
func (jm *JobManager) GetActive() *Job {
	jm.mu.RLock()
	defer jm.mu.RUnlock()

	for _, job := range jm.jobs {
		if job.Status == JobStatusRunning {
			return job
		}
	}
	return nil
}

// Cleanup removes completed jobs older than the specified duration
func (jm *JobManager) Cleanup(maxAge time.Duration) {
	jm.mu.Lock()
	defer jm.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	for id, job := range jm.jobs {
		if job.Status != JobStatusRunning && job.CompletedAt.Before(cutoff) {
			delete(jm.jobs, id)
		}
	}
}

// RemainingItem is a (profile_id, broker_id) pair in the resume queue.
// Before multi-profile support this was a bare string (broker_id only); the
// Load() path below handles migration of old files.
type RemainingItem struct {
	ProfileID string `json:"profile_id"`
	BrokerID  string `json:"broker_id"`
}

// PersistentJobState represents a job that can be saved/loaded from disk.
// Version 2 (this struct): RemainingItems carries (profile_id, broker_id)
// pairs so resume knows which profile each item was queued for.
type PersistentJobState struct {
	Version          int             `json:"version"` // 2 = multi-profile shape; 0/missing = legacy single-profile
	ID               string          `json:"id"`
	Status           JobStatus       `json:"status"`
	Sent             int             `json:"sent"`
	Failed           int             `json:"failed"`
	Total            int             `json:"total"`
	StartedAt        time.Time       `json:"started_at"`
	RemainingItems   []RemainingItem `json:"remaining_items"`
	Search           string          `json:"search"` // Original filter params
	Category         string          `json:"category"`
	Region           string          `json:"region"`
	StatusFilter     string          `json:"status_filter"`
	ProfileIDs       []string        `json:"profile_ids"` // Which profiles this job was created for
}

// currentShapeVersion identifies the v2 multi-profile shape on disk. Bump
// this constant if the shape changes again in the future; Load() uses it to
// decide whether to reset or migrate.
const currentShapeVersion = 2

// JobPersistence handles saving/loading job state
type JobPersistence struct {
	dataDir string
}

// NewJobPersistence creates a new job persistence handler
func NewJobPersistence(dataDir string) *JobPersistence {
	return &JobPersistence{dataDir: dataDir}
}

func (jp *JobPersistence) filePath() string {
	return filepath.Join(jp.dataDir, "pending_job.json")
}

// Save saves the job state to disk
func (jp *JobPersistence) Save(state *PersistentJobState) error {
	if err := os.MkdirAll(jp.dataDir, 0700); err != nil {
		return err
	}

	// Always stamp the current shape version on write.
	state.Version = currentShapeVersion

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(jp.filePath(), data, 0600)
}

// Load loads a pending job state from disk, returns nil if none exists.
// If the file was written by an older (pre-multi-profile) version, this
// migrates in-memory by mapping every remaining broker to DefaultProfileID.
// The migrated state is returned but NOT auto-saved — Save() gets called
// once processing resumes, upgrading the file naturally.
func (jp *JobPersistence) Load() (*PersistentJobState, error) {
	data, err := os.ReadFile(jp.filePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var state PersistentJobState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}

	if state.Version == currentShapeVersion {
		return &state, nil
	}

	// Legacy shape path: the old struct used `remaining_brokers: []string`.
	// Unmarshal into a transitional struct to grab that field, then synthesise
	// the (profile_id, broker_id) pairs under DefaultProfileID.
	var legacy struct {
		ID               string    `json:"id"`
		Status           JobStatus `json:"status"`
		Sent             int       `json:"sent"`
		Failed           int       `json:"failed"`
		Total            int       `json:"total"`
		StartedAt        time.Time `json:"started_at"`
		RemainingBrokers []string  `json:"remaining_brokers"`
		Search           string    `json:"search"`
		Category         string    `json:"category"`
		Region           string    `json:"region"`
		StatusFilter     string    `json:"status_filter"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		// If we can't even parse the legacy shape, it's irrecoverable — clear
		// the file so the user can start fresh rather than wedge the server.
		log.Printf("pending_job.json is unreadable (%v); clearing to recover", err)
		_ = os.Remove(jp.filePath())
		return nil, nil
	}

	items := make([]RemainingItem, len(legacy.RemainingBrokers))
	for i, id := range legacy.RemainingBrokers {
		items[i] = RemainingItem{ProfileID: history.DefaultProfileID, BrokerID: id}
	}

	log.Printf("Migrated legacy pending_job.json (v1→v%d): %d remaining items mapped to profile %q",
		currentShapeVersion, len(items), history.DefaultProfileID)

	return &PersistentJobState{
		Version:        currentShapeVersion,
		ID:             legacy.ID,
		Status:         legacy.Status,
		Sent:           legacy.Sent,
		Failed:         legacy.Failed,
		Total:          legacy.Total,
		StartedAt:      legacy.StartedAt,
		RemainingItems: items,
		Search:         legacy.Search,
		Category:       legacy.Category,
		Region:         legacy.Region,
		StatusFilter:   legacy.StatusFilter,
		ProfileIDs:     []string{history.DefaultProfileID},
	}, nil
}

// Clear removes the saved job state
func (jp *JobPersistence) Clear() error {
	err := os.Remove(jp.filePath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// String returns a short description of a RemainingItem for logs.
func (r RemainingItem) String() string {
	return fmt.Sprintf("%s/%s", r.ProfileID, r.BrokerID)
}

package bulkops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// Config holds bulk operation settings
type Config struct {
	// Global concurrency limits
	MaxGlobalConcurrent int `json:"max_global_concurrent"` // Total concurrent operations across all jobs
	MaxPerJob           int `json:"max_per_job"`           // Max concurrent within a single bulk job
	MaxPerAP            int `json:"max_per_ap"`            // Max concurrent STAs per AP during fanout

	// Retry settings
	MaxRetries        int           `json:"max_retries"`
	InitialBackoff    time.Duration `json:"initial_backoff"`
	MaxBackoff        time.Duration `json:"max_backoff"`
	BackoffMultiplier float64       `json:"backoff_multiplier"`

	// Timeouts
	OperationTimeout time.Duration `json:"operation_timeout"`

	// Rate limiting
	MinDelayBetweenOps time.Duration `json:"min_delay_between_ops"` // Minimum delay between starting operations
}

// DefaultConfig returns sensible defaults
func DefaultConfig() *Config {
	return &Config{
		MaxGlobalConcurrent: 10,
		MaxPerJob:           5,
		MaxPerAP:            3,
		MaxRetries:          3,
		InitialBackoff:      2 * time.Second,
		MaxBackoff:          60 * time.Second,
		BackoffMultiplier:   2.0,
		OperationTimeout:    5 * time.Minute,
		MinDelayBetweenOps:  500 * time.Millisecond,
	}
}

// OpType represents the type of bulk operation
type OpType string

const (
	OpUpgrade OpType = "upgrade"
	OpBackup  OpType = "backup"
	OpReboot  OpType = "reboot"
	OpConfig  OpType = "config"
	OpRefresh OpType = "refresh"
)

// OpResult represents the result of a single operation
type OpResult struct {
	DeviceID    int           `json:"device_id"`
	DeviceIP    string        `json:"device_ip"`
	Hostname    string        `json:"hostname,omitempty"`
	Status      string        `json:"status"` // "success", "failed", "skipped", "pending"
	Message     string        `json:"message,omitempty"`
	Error       string        `json:"error,omitempty"`
	Attempts    int           `json:"attempts"`
	Duration    time.Duration `json:"duration_ms"`
	StartedAt   time.Time     `json:"started_at,omitempty"`
	CompletedAt time.Time     `json:"completed_at,omitempty"`
}

// DryRunResult represents compatibility check results
type DryRunResult struct {
	DeviceID   int      `json:"device_id"`
	DeviceIP   string   `json:"device_ip"`
	Hostname   string   `json:"hostname,omitempty"`
	Compatible bool     `json:"compatible"`
	CurrentVer string   `json:"current_version,omitempty"`
	TargetVer  string   `json:"target_version,omitempty"`
	Flavor     string   `json:"flavor,omitempty"`
	Issues     []string `json:"issues,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// Job represents a bulk operation job
type Job struct {
	ID         string      `json:"id"`
	Type       OpType      `json:"type"`
	DeviceIDs  []int       `json:"device_ids"`
	Parameters interface{} `json:"parameters,omitempty"`
	DryRun     bool        `json:"dry_run"`

	// Progress
	Total     int   `json:"total"`
	Completed int32 `json:"completed"`
	Succeeded int32 `json:"succeeded"`
	Failed    int32 `json:"failed"`
	Skipped   int32 `json:"skipped"`

	// Results
	Results       []OpResult     `json:"results,omitempty"`
	DryRunResults []DryRunResult `json:"dry_run_results,omitempty"`

	// Timing
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`

	// Control
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
}

// OperationFunc is the function signature for device operations
type OperationFunc func(ctx context.Context, deviceID int, params interface{}) error

// CompatibilityCheckFunc checks if an operation can be performed on a device
type CompatibilityCheckFunc func(deviceID int, params interface{}) (*DryRunResult, error)

// Controller manages bulk operations with concurrency control
type Controller struct {
	db     *sql.DB
	config *Config

	// Global semaphore for all operations
	globalSem chan struct{}

	// Per-AP semaphores for fanout operations
	apSems   map[int]chan struct{}
	apSemsMu sync.Mutex

	// Active jobs
	jobs   map[string]*Job
	jobsMu sync.RWMutex

	// Metrics
	totalOps   int64
	successOps int64
	failedOps  int64
	retriedOps int64
}

// NewController creates a new bulk operations controller
func NewController(db *sql.DB) *Controller {
	c := &Controller{
		db:     db,
		config: DefaultConfig(),
		apSems: make(map[int]chan struct{}),
		jobs:   make(map[string]*Job),
	}
	c.loadConfig()
	c.globalSem = make(chan struct{}, c.config.MaxGlobalConcurrent)
	return c
}

func (c *Controller) loadConfig() {
	var configJSON string
	err := c.db.QueryRow(`SELECT value FROM settings WHERE key = 'bulk_ops_config'`).Scan(&configJSON)
	if err == nil && configJSON != "" {
		var cfg Config
		if json.Unmarshal([]byte(configJSON), &cfg) == nil {
			c.config = &cfg
		}
	}
}

// UpdateConfig updates the bulk operations configuration
func (c *Controller) UpdateConfig(cfg *Config) error {
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	_, err = c.db.Exec(`
		INSERT INTO settings (key, value) VALUES ('bulk_ops_config', $1)
		ON CONFLICT (key) DO UPDATE SET value = $1
	`, string(configJSON))
	if err != nil {
		return err
	}

	c.config = cfg

	// Resize global semaphore if needed
	c.globalSem = make(chan struct{}, cfg.MaxGlobalConcurrent)

	return nil
}

// GetConfig returns current configuration
func (c *Controller) GetConfig() *Config {
	return c.config
}

// getAPSem returns (or creates) a semaphore for an AP
func (c *Controller) getAPSem(apID int) chan struct{} {
	c.apSemsMu.Lock()
	defer c.apSemsMu.Unlock()

	sem, ok := c.apSems[apID]
	if !ok {
		sem = make(chan struct{}, c.config.MaxPerAP)
		c.apSems[apID] = sem
	}
	return sem
}

// DryRun performs compatibility checks without executing operations
func (c *Controller) DryRun(ctx context.Context, opType OpType, deviceIDs []int, params interface{}, checkFn CompatibilityCheckFunc) ([]DryRunResult, error) {
	results := make([]DryRunResult, 0, len(deviceIDs))
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Use limited concurrency for checks too
	sem := make(chan struct{}, c.config.MaxPerJob)

	for _, deviceID := range deviceIDs {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			result, err := checkFn(id, params)
			if err != nil {
				result = &DryRunResult{
					DeviceID:   id,
					Compatible: false,
					Issues:     []string{err.Error()},
				}
			}

			mu.Lock()
			results = append(results, *result)
			mu.Unlock()
		}(deviceID)
	}

	wg.Wait()
	return results, nil
}

// Execute runs a bulk operation with full concurrency control
func (c *Controller) Execute(ctx context.Context, jobID string, opType OpType, deviceIDs []int, params interface{}, opFn OperationFunc) (*Job, error) {
	ctx, cancel := context.WithCancel(ctx)

	job := &Job{
		ID:         jobID,
		Type:       opType,
		DeviceIDs:  deviceIDs,
		Parameters: params,
		Total:      len(deviceIDs),
		Results:    make([]OpResult, 0, len(deviceIDs)),
		StartedAt:  time.Now(),
		ctx:        ctx,
		cancel:     cancel,
	}

	c.jobsMu.Lock()
	c.jobs[jobID] = job
	c.jobsMu.Unlock()

	go c.runJob(job, opFn)

	return job, nil
}

func (c *Controller) runJob(job *Job, opFn OperationFunc) {
	defer func() {
		job.CompletedAt = time.Now()
		job.cancel()
	}()

	var wg sync.WaitGroup
	jobSem := make(chan struct{}, c.config.MaxPerJob)
	lastOpTime := time.Now()
	var lastOpMu sync.Mutex

	for _, deviceID := range job.DeviceIDs {
		select {
		case <-job.ctx.Done():
			return
		default:
		}

		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// Acquire job-level semaphore
			select {
			case jobSem <- struct{}{}:
				defer func() { <-jobSem }()
			case <-job.ctx.Done():
				return
			}

			// Acquire global semaphore
			select {
			case c.globalSem <- struct{}{}:
				defer func() { <-c.globalSem }()
			case <-job.ctx.Done():
				return
			}

			// Rate limiting - ensure minimum delay between operations
			lastOpMu.Lock()
			elapsed := time.Since(lastOpTime)
			if elapsed < c.config.MinDelayBetweenOps {
				time.Sleep(c.config.MinDelayBetweenOps - elapsed)
			}
			lastOpTime = time.Now()
			lastOpMu.Unlock()

			// Execute with retry
			result := c.executeWithRetry(job.ctx, id, job.Parameters, opFn)

			// Update job
			job.mu.Lock()
			job.Results = append(job.Results, result)
			job.mu.Unlock()

			atomic.AddInt32(&job.Completed, 1)
			switch result.Status {
			case "success":
				atomic.AddInt32(&job.Succeeded, 1)
			case "failed":
				atomic.AddInt32(&job.Failed, 1)
			case "skipped":
				atomic.AddInt32(&job.Skipped, 1)
			}

			atomic.AddInt64(&c.totalOps, 1)
			if result.Status == "success" {
				atomic.AddInt64(&c.successOps, 1)
			} else if result.Status == "failed" {
				atomic.AddInt64(&c.failedOps, 1)
			}
		}(deviceID)
	}

	wg.Wait()
}

func (c *Controller) executeWithRetry(ctx context.Context, deviceID int, params interface{}, opFn OperationFunc) OpResult {
	result := OpResult{
		DeviceID:  deviceID,
		StartedAt: time.Now(),
	}

	backoff := c.config.InitialBackoff

	for attempt := 1; attempt <= c.config.MaxRetries; attempt++ {
		result.Attempts = attempt

		// Create timeout context for this attempt
		opCtx, cancel := context.WithTimeout(ctx, c.config.OperationTimeout)

		err := opFn(opCtx, deviceID, params)
		cancel()

		if err == nil {
			result.Status = "success"
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			return result
		}

		result.Error = err.Error()

		// Check if context was cancelled (job cancelled)
		if ctx.Err() != nil {
			result.Status = "skipped"
			result.Message = "Job cancelled"
			return result
		}

		// Check if we should retry
		if attempt < c.config.MaxRetries {
			if isRetryable(err) {
				atomic.AddInt64(&c.retriedOps, 1)
				log.Printf("Device %d: attempt %d failed, retrying in %v: %v", deviceID, attempt, backoff, err)

				select {
				case <-time.After(backoff):
					// Calculate next backoff with jitter
					backoff = time.Duration(float64(backoff) * c.config.BackoffMultiplier)
					if backoff > c.config.MaxBackoff {
						backoff = c.config.MaxBackoff
					}
					// Add jitter (+/-10%)
					jitter := time.Duration(float64(backoff) * 0.1 * (0.5 - float64(time.Now().UnixNano()%100)/100))
					backoff += jitter
				case <-ctx.Done():
					result.Status = "skipped"
					result.Message = "Job cancelled during backoff"
					return result
				}
				continue
			}
		}

		// Final failure
		result.Status = "failed"
		result.Message = fmt.Sprintf("Failed after %d attempts", attempt)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result
	}

	return result
}

// isRetryable determines if an error should trigger a retry
func isRetryable(err error) bool {
	errStr := err.Error()

	// Network/timeout errors are retryable
	retryablePatterns := []string{
		"timeout",
		"connection refused",
		"connection reset",
		"no such host",
		"temporary failure",
		"network is unreachable",
		"i/o timeout",
		"EOF",
		"broken pipe",
		"503",
		"502",
		"504",
	}

	for _, pattern := range retryablePatterns {
		if contains(errStr, pattern) {
			return true
		}
	}

	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsLower(s, substr))
}

func containsLower(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if matchesLower(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

func matchesLower(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// ExecuteFanout runs operations on STAs first, then AP, with per-AP concurrency control
func (c *Controller) ExecuteFanout(ctx context.Context, jobID string, apID int, staIDs []int, params interface{}, opFn OperationFunc) (*Job, error) {
	allDevices := append(staIDs, apID)

	ctx, cancel := context.WithCancel(ctx)

	job := &Job{
		ID:         jobID,
		Type:       OpUpgrade,
		DeviceIDs:  allDevices,
		Parameters: params,
		Total:      len(allDevices),
		Results:    make([]OpResult, 0, len(allDevices)),
		StartedAt:  time.Now(),
		ctx:        ctx,
		cancel:     cancel,
	}

	c.jobsMu.Lock()
	c.jobs[jobID] = job
	c.jobsMu.Unlock()

	go c.runFanoutJob(job, apID, staIDs, opFn)

	return job, nil
}

func (c *Controller) runFanoutJob(job *Job, apID int, staIDs []int, opFn OperationFunc) {
	defer func() {
		job.CompletedAt = time.Now()
		job.cancel()
	}()

	apSem := c.getAPSem(apID)

	// Phase 1: Upgrade all STAs with per-AP concurrency limit
	var wg sync.WaitGroup
	for _, staID := range staIDs {
		select {
		case <-job.ctx.Done():
			return
		default:
		}

		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// Acquire AP-level semaphore
			select {
			case apSem <- struct{}{}:
				defer func() { <-apSem }()
			case <-job.ctx.Done():
				return
			}

			// Also acquire global semaphore
			select {
			case c.globalSem <- struct{}{}:
				defer func() { <-c.globalSem }()
			case <-job.ctx.Done():
				return
			}

			result := c.executeWithRetry(job.ctx, id, job.Parameters, opFn)

			job.mu.Lock()
			job.Results = append(job.Results, result)
			job.mu.Unlock()

			atomic.AddInt32(&job.Completed, 1)
			if result.Status == "success" {
				atomic.AddInt32(&job.Succeeded, 1)
			} else {
				atomic.AddInt32(&job.Failed, 1)
			}
		}(staID)
	}

	wg.Wait()

	// Check if we should continue to AP
	if job.ctx.Err() != nil {
		return
	}

	// Check STA success rate - only upgrade AP if most STAs succeeded
	staSuccessRate := float64(job.Succeeded) / float64(len(staIDs))
	if staSuccessRate < 0.5 && len(staIDs) > 0 {
		result := OpResult{
			DeviceID:  apID,
			Status:    "skipped",
			Message:   fmt.Sprintf("Skipped AP upgrade: only %.0f%% of STAs succeeded", staSuccessRate*100),
			StartedAt: time.Now(),
		}
		result.CompletedAt = time.Now()

		job.mu.Lock()
		job.Results = append(job.Results, result)
		job.mu.Unlock()

		atomic.AddInt32(&job.Completed, 1)
		atomic.AddInt32(&job.Skipped, 1)
		return
	}

	// Phase 2: Upgrade AP
	select {
	case c.globalSem <- struct{}{}:
		defer func() { <-c.globalSem }()
	case <-job.ctx.Done():
		return
	}

	result := c.executeWithRetry(job.ctx, apID, job.Parameters, opFn)

	job.mu.Lock()
	job.Results = append(job.Results, result)
	job.mu.Unlock()

	atomic.AddInt32(&job.Completed, 1)
	if result.Status == "success" {
		atomic.AddInt32(&job.Succeeded, 1)
	} else {
		atomic.AddInt32(&job.Failed, 1)
	}
}

// CancelJob cancels a running job
func (c *Controller) CancelJob(jobID string) error {
	c.jobsMu.RLock()
	job, ok := c.jobs[jobID]
	c.jobsMu.RUnlock()

	if !ok {
		return fmt.Errorf("job not found: %s", jobID)
	}

	job.cancel()
	return nil
}

// GetJob returns job status
func (c *Controller) GetJob(jobID string) (*Job, bool) {
	c.jobsMu.RLock()
	defer c.jobsMu.RUnlock()
	job, ok := c.jobs[jobID]
	return job, ok
}

// CleanupOldJobs removes completed jobs older than the given duration
func (c *Controller) CleanupOldJobs(maxAge time.Duration) int {
	c.jobsMu.Lock()
	defer c.jobsMu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	removed := 0

	for id, job := range c.jobs {
		if !job.CompletedAt.IsZero() && job.CompletedAt.Before(cutoff) {
			delete(c.jobs, id)
			removed++
		}
	}

	return removed
}

// Stats returns operation statistics
func (c *Controller) Stats() map[string]int64 {
	return map[string]int64{
		"total_operations":    atomic.LoadInt64(&c.totalOps),
		"successful":          atomic.LoadInt64(&c.successOps),
		"failed":              atomic.LoadInt64(&c.failedOps),
		"retried":             atomic.LoadInt64(&c.retriedOps),
		"active_global_slots": int64(len(c.globalSem)),
	}
}

// CalculateBackoff calculates exponential backoff with jitter
func CalculateBackoff(attempt int, initial, max time.Duration, multiplier float64) time.Duration {
	backoff := float64(initial) * math.Pow(multiplier, float64(attempt-1))
	if backoff > float64(max) {
		backoff = float64(max)
	}
	// Add jitter (+/-20%)
	jitter := backoff * 0.2 * (0.5 - float64(time.Now().UnixNano()%100)/100)
	return time.Duration(backoff + jitter)
}

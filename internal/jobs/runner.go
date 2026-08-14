package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/yellowman/wavecontrol/internal/firmware"
	"github.com/yellowman/wavecontrol/internal/websocket"
)

// JobType identifies the type of job
type JobType string

const (
	JobUpgrade       JobType = "upgrade"
	JobBulkUpgrade   JobType = "bulk_upgrade"
	JobFanoutUpgrade JobType = "fanout_upgrade"
	JobBackup        JobType = "backup"
	JobRestore       JobType = "restore"
	JobReboot        JobType = "reboot"
	JobRefresh       JobType = "refresh"
)

// JobStatus represents the status of a job
type JobStatus string

const (
	StatusPending   JobStatus = "pending"
	StatusRunning   JobStatus = "running"
	StatusRebooting JobStatus = "rebooting"
	StatusCompleted JobStatus = "completed"
	StatusSkipped   JobStatus = "skipped"
	StatusFailed    JobStatus = "failed"
	StatusCancelled JobStatus = "cancelled"
)

// EventType identifies the type of job event
type EventType string

const (
	EventStarted      EventType = "started"
	EventProgress     EventType = "progress"
	EventStepComplete EventType = "step_complete"
	EventWarning      EventType = "warning"
	EventError        EventType = "error"
	EventCompleted    EventType = "completed"
)

// JobRun represents a job execution instance
type JobRun struct {
	ID             string          `json:"id"`
	JobType        JobType         `json:"job_type"`
	Status         JobStatus       `json:"status"`
	Progress       int             `json:"progress"`
	TotalSteps     int             `json:"total_steps"`
	CompletedSteps int             `json:"completed_steps"`
	DeviceIDs      []int           `json:"device_ids,omitempty"`
	Parameters     json.RawMessage `json:"parameters,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	ErrorMessage   string          `json:"error_message,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	CreatedBy      int             `json:"created_by,omitempty"`
}

// JobEvent represents a progress event for a job
type JobEvent struct {
	ID             int             `json:"id"`
	JobID          string          `json:"job_id"`
	EventTime      time.Time       `json:"event_time"`
	EventType      EventType       `json:"event_type"`
	DeviceID       *int            `json:"device_id,omitempty"`
	DeviceHostname string          `json:"device_hostname,omitempty"`
	DeviceIP       string          `json:"device_ip,omitempty"`
	Message        string          `json:"message"`
	Data           json.RawMessage `json:"data,omitempty"`
}

// UpgradeParams contains parameters for upgrade jobs
type UpgradeParams struct {
	FirmwareFile    string `json:"firmware_file,omitempty"`
	FirmwareVersion string `json:"firmware_version,omitempty"` // Resolves to file per device flavor
	Force           bool   `json:"force"`
	Fanout          bool   `json:"fanout"`
}

// BackupParams contains parameters for backup jobs
type BackupParams struct {
	IncludeConfig bool `json:"include_config"`
}

// Runner manages async job execution
type Runner struct {
	db        *sql.DB
	fwService *firmware.Service
	wsHub     *websocket.Hub

	mu      sync.Mutex
	running map[string]context.CancelFunc // Active jobs with cancel functions
	jobSem  chan struct{}                 // Concurrency limiter
	maxJobs int
	wg      sync.WaitGroup

	// For graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc
}

// dbExec executes a query and logs any error (non-critical operations)
// Returns (result, error) for callers that need to check
func dbExec(db *sql.DB, query string, args ...any) (sql.Result, error) {
	result, err := db.Exec(query, args...)
	if err != nil {
		log.Printf("DB exec error: %v", err)
	}
	return result, err
}

// dbExecIgnore executes a query, logs errors but doesn't return them (fire-and-forget)
func dbExecIgnore(db *sql.DB, query string, args ...any) {
	if _, err := db.Exec(query, args...); err != nil {
		log.Printf("DB exec error: %v", err)
	}
}

// sanitizeIPForPath makes an IP address safe for filesystem paths
func sanitizeIPForPath(ip string) string {
	return strings.ReplaceAll(ip, ":", "-")
}

// NewRunner creates a new job runner
func NewRunner(db *sql.DB, fwService *firmware.Service, wsHub *websocket.Hub) *Runner {
	maxJobs := 10
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{
		db:        db,
		fwService: fwService,
		wsHub:     wsHub,
		running:   make(map[string]context.CancelFunc),
		jobSem:    make(chan struct{}, maxJobs),
		maxJobs:   maxJobs,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// Start initializes the runner and recovers pending jobs from previous runs
func (r *Runner) Start() {
	// Mark any previously "running" jobs as failed (server crashed)
	dbExecIgnore(r.db, `
		UPDATE job_runs SET status = 'failed', error_message = 'Server restarted during execution'
		WHERE status = 'running'
	`)

	// Resume pending jobs
	rows, err := r.db.Query(`
		SELECT id FROM job_runs WHERE status = 'pending' ORDER BY created_at
	`)
	if err != nil {
		log.Printf("Job runner: failed to query pending jobs: %v", err)
		return
	}
	defer rows.Close()

	var pendingIDs []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			pendingIDs = append(pendingIDs, id)
		}
	}

	if len(pendingIDs) > 0 {
		log.Printf("Job runner: resuming %d pending jobs", len(pendingIDs))
		for _, id := range pendingIDs {
			go r.executeJob(id)
		}
	}
}

// Stop gracefully shuts down the runner
func (r *Runner) Stop() {
	r.cancel()
	r.wg.Wait()
}

// StartJob creates a new job and starts it in the background
// Returns the job ID immediately
func (r *Runner) StartJob(ctx context.Context, jobType JobType, deviceIDs []int, params interface{}, userID int) (string, error) {
	// Serialize parameters
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("marshal params: %w", err)
	}

	// Calculate total steps based on job type and device count
	totalSteps := len(deviceIDs)
	if totalSteps == 0 {
		totalSteps = 1
	}

	// Create job record
	jobID := uuid.New().String()
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO job_runs (id, job_type, status, total_steps, device_ids, parameters, created_by)
		VALUES ($1, $2, 'pending', $3, $4, $5, $6)
	`, jobID, jobType, totalSteps, pq.Array(deviceIDs), paramsJSON, userID)
	if err != nil {
		return "", fmt.Errorf("create job: %w", err)
	}

	// Try to start immediately
	go r.executeJob(jobID)

	return jobID, nil
}

// executeJob runs a job (called in goroutine)
func (r *Runner) executeJob(jobID string) {
	// Panic recovery - ensure we mark job as failed if something goes wrong
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("Job %s: PANIC: %v", jobID, rec)
			errMsg := fmt.Sprintf("internal error: %v", rec)
			r.updateStatus(jobID, StatusFailed, &errMsg)
			dbExecIgnore(r.db, `UPDATE job_runs SET completed_at = NOW() WHERE id = $1`, jobID)
			r.logEvent(jobID, EventError, nil, errMsg, nil)
		}
	}()

	log.Printf("Job %s: starting execution", jobID)

	// Acquire semaphore - block until slot available or context cancelled
	select {
	case r.jobSem <- struct{}{}:
		// Got slot
		log.Printf("Job %s: acquired semaphore slot", jobID)
	case <-r.ctx.Done():
		// Runner is shutting down
		log.Printf("Job %s: runner shutting down, aborting", jobID)
		return
	}
	defer func() { <-r.jobSem }()

	// Create cancellable context linked to runner context
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()

	// Register running job
	r.mu.Lock()
	r.running[jobID] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.running, jobID)
		r.mu.Unlock()
	}()

	r.wg.Add(1)
	defer r.wg.Done()

	// Load job from DB
	job, err := r.GetJob(ctx, jobID)
	if err != nil {
		log.Printf("Job %s: load failed: %v", jobID, err)
		return
	}

	if job.Status != StatusPending {
		log.Printf("Job %s: unexpected status %s, skipping", jobID, job.Status)
		return
	}

	// Mark as running
	log.Printf("Job %s: marking as running, type=%s, devices=%v", jobID, job.JobType, job.DeviceIDs)
	r.updateStatus(jobID, StatusRunning, nil)
	dbExecIgnore(r.db, `UPDATE job_runs SET started_at = NOW() WHERE id = $1`, jobID)
	r.logEvent(jobID, EventStarted, nil, "Job started", nil)

	// Update local job status and broadcast (no need to reload from DB)
	job.Status = StatusRunning
	r.broadcastJobUpdate(job)
	log.Printf("Job %s: broadcast running status", jobID)

	// Execute based on type
	var result interface{}
	log.Printf("Job %s: executing job type %s", jobID, job.JobType)
	switch job.JobType {
	case JobUpgrade:
		result, err = r.runUpgradeJob(ctx, job)
	case JobBulkUpgrade:
		result, err = r.runBulkUpgradeJob(ctx, job)
	case JobFanoutUpgrade:
		result, err = r.runFanoutUpgradeJob(ctx, job)
	case JobBackup:
		result, err = r.runBackupJob(ctx, job)
	case JobReboot:
		result, err = r.runRebootJob(ctx, job)
	case JobRefresh:
		result, err = r.runRefreshJob(ctx, job)
	default:
		err = fmt.Errorf("unknown job type: %s", job.JobType)
	}

	log.Printf("Job %s: execution finished, err=%v", jobID, err)

	// Update final status
	if err != nil {
		if ctx.Err() == context.Canceled {
			log.Printf("Job %s: marking as cancelled", jobID)
			r.updateStatus(jobID, StatusCancelled, nil)
			r.logEvent(jobID, EventCompleted, nil, "Job cancelled", nil)
		} else {
			log.Printf("Job %s: marking as failed: %v", jobID, err)
			errMsg := err.Error()
			r.updateStatus(jobID, StatusFailed, &errMsg)
			r.logEvent(jobID, EventError, nil, errMsg, nil)
		}
	} else {
		// Check if upgrade was skipped (already at target version)
		finalStatus := StatusCompleted
		finalMessage := "Job completed successfully"

		// Check result for skipped status or upgrade success
		if result != nil {
			log.Printf("Job %s: result type=%T", jobID, result)

			// Single upgrade result
			if upgradeResult, ok := result.(*firmware.UpgradeResult); ok {
				log.Printf("Job %s: upgrade result status=%s message=%s", jobID, upgradeResult.Status, upgradeResult.Message)
				if upgradeResult.Status == "skipped" {
					finalStatus = StatusSkipped
					finalMessage = "Upgrade skipped: " + upgradeResult.Message
				} else if upgradeResult.Status == "success" {
					finalStatus = StatusRebooting
					finalMessage = "Device rebooting"
				}
			}

			// Bulk/fanout upgrade results
			if results, ok := result.([]*firmware.UpgradeResult); ok {
				allSkipped := len(results) > 0
				anySuccess := false
				var skipMsg string
				for _, r := range results {
					if r == nil {
						continue
					}
					log.Printf("Job %s: bulk result status=%s message=%s", jobID, r.Status, r.Message)
					if r.Status != "skipped" {
						allSkipped = false
					} else {
						skipMsg = r.Message
					}
					if r.Status == "success" {
						anySuccess = true
					}
				}
				if allSkipped {
					finalStatus = StatusSkipped
					finalMessage = "Upgrade skipped: " + skipMsg
				} else if anySuccess {
					finalStatus = StatusRebooting
					finalMessage = "Devices rebooting"
				}
			}
		}

		log.Printf("Job %s: marking as %s", jobID, finalStatus)
		r.updateStatus(jobID, finalStatus, nil)
		if result != nil {
			resultJSON, _ := json.Marshal(result)
			dbExecIgnore(r.db, `UPDATE job_runs SET result = $1 WHERE id = $2`, resultJSON, jobID)
		}
		r.logEvent(jobID, EventCompleted, nil, finalMessage, nil)
	}

	dbExecIgnore(r.db, `UPDATE job_runs SET completed_at = NOW() WHERE id = $1`, jobID)

	// Final broadcast with updated status - use background context since job ctx may be cancelled
	job, err = r.GetJob(context.Background(), jobID)
	if err != nil {
		log.Printf("Job %s: failed to load for final broadcast: %v", jobID, err)
		return
	}
	r.broadcastJobUpdate(job)
	log.Printf("Job %s: final broadcast sent, status=%s", jobID, job.Status)
}

// runUpgradeJob upgrades a single device
func (r *Runner) runUpgradeJob(ctx context.Context, job *JobRun) (interface{}, error) {
	log.Printf("Job %s: runUpgradeJob starting, devices=%v", job.ID, job.DeviceIDs)

	if len(job.DeviceIDs) == 0 {
		return nil, fmt.Errorf("no device specified")
	}

	var params UpgradeParams
	if err := json.Unmarshal(job.Parameters, &params); err != nil {
		log.Printf("Job %s: failed to unmarshal params: %v", job.ID, err)
	}
	log.Printf("Job %s: params firmware_file=%q firmware_version=%q force=%v",
		job.ID, params.FirmwareFile, params.FirmwareVersion, params.Force)

	deviceID := job.DeviceIDs[0]
	r.logEvent(job.ID, EventProgress, &deviceID, "Starting firmware upgrade", nil)

	// Use version if specified, otherwise fall back to file
	firmwareRef := params.FirmwareFile
	if params.FirmwareVersion != "" {
		firmwareRef = params.FirmwareVersion
	}

	if firmwareRef == "" {
		log.Printf("Job %s: no firmware specified, will auto-detect by flavor", job.ID)
	}

	log.Printf("Job %s: calling UpgradeDevice deviceID=%d firmwareRef=%q", job.ID, deviceID, firmwareRef)
	result, err := r.fwService.UpgradeDevice(ctx, int64(deviceID), firmwareRef, params.Force)
	if err != nil {
		log.Printf("Job %s: UpgradeDevice failed: %v", job.ID, err)
		r.logEvent(job.ID, EventError, &deviceID, fmt.Sprintf("Upgrade failed: %v", err), nil)
		return result, err
	}

	if result == nil {
		log.Printf("Job %s: UpgradeDevice returned nil result", job.ID)
		return nil, fmt.Errorf("upgrade returned no result")
	}

	log.Printf("Job %s: UpgradeDevice completed status=%s", job.ID, result.Status)
	r.updateProgress(job.ID, 1, 1)
	r.logEvent(job.ID, EventStepComplete, &deviceID, fmt.Sprintf("Upgrade %s: %s", result.Status, result.Message), nil)
	return result, nil
}

// runBulkUpgradeJob upgrades multiple devices
// Always upgrades STAs before their parent APs to avoid connectivity loss
func (r *Runner) runBulkUpgradeJob(ctx context.Context, job *JobRun) (interface{}, error) {
	var params UpgradeParams
	json.Unmarshal(job.Parameters, &params)

	// Use version if specified, otherwise fall back to file
	firmwareRef := params.FirmwareFile
	if params.FirmwareVersion != "" {
		firmwareRef = params.FirmwareVersion
	}

	// Get device parent info to order STAs before APs
	type deviceInfo struct {
		ID       int
		ParentID *int
	}
	devices := make([]deviceInfo, 0, len(job.DeviceIDs))

	for _, devID := range job.DeviceIDs {
		var parentID *int
		r.db.QueryRowContext(ctx, `SELECT parent_id FROM devices WHERE id = $1`, devID).Scan(&parentID)
		devices = append(devices, deviceInfo{ID: devID, ParentID: parentID})
	}

	// Sort: STAs first (devices with parent_id), then APs (devices without parent_id)
	// This ensures STAs are upgraded while their AP is still up
	sort.Slice(devices, func(i, j int) bool {
		iIsSTA := devices[i].ParentID != nil
		jIsSTA := devices[j].ParentID != nil
		if iIsSTA && !jIsSTA {
			return true // STA before AP
		}
		if !iIsSTA && jIsSTA {
			return false // AP after STA
		}
		return devices[i].ID < devices[j].ID // stable sort by ID
	})

	// Set total_steps for progress visibility (same as fanout)
	totalDevices := len(devices)
	dbExecIgnore(r.db, `UPDATE job_runs SET total_steps = $1 WHERE id = $2`, totalDevices, job.ID)

	var results []*firmware.UpgradeResult
	var mu sync.Mutex
	var wg sync.WaitGroup
	completed := 0

	// Process devices with limited concurrency
	sem := make(chan struct{}, 5) // 5 concurrent upgrades

	for _, dev := range devices {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(devID int) {
			defer wg.Done()
			defer func() { <-sem }()

			r.logEvent(job.ID, EventProgress, &devID, "Starting upgrade", nil)

			result, err := r.fwService.UpgradeDevice(ctx, int64(devID), firmwareRef, params.Force)
			if err != nil {
				r.logEvent(job.ID, EventWarning, &devID, fmt.Sprintf("Upgrade failed: %v", err), nil)
				if result == nil {
					result = &firmware.UpgradeResult{
						DeviceID: int64(devID),
						Status:   "failed",
						Message:  err.Error(),
					}
				}
			} else if result != nil {
				r.logEvent(job.ID, EventStepComplete, &devID, fmt.Sprintf("Upgrade %s", result.Status), nil)
			}

			mu.Lock()
			results = append(results, result)
			completed++
			r.updateProgress(job.ID, completed, len(job.DeviceIDs))
			mu.Unlock()
		}(dev.ID)
	}

	wg.Wait()
	return results, nil
}

// runFanoutUpgradeJob upgrades an AP and all its connected STAs
// STAs are upgraded first while AP is still up, then AP is upgraded last
func (r *Runner) runFanoutUpgradeJob(ctx context.Context, job *JobRun) (interface{}, error) {
	var params UpgradeParams
	json.Unmarshal(job.Parameters, &params)

	// Use version if specified, otherwise fall back to file
	firmwareRef := params.FirmwareFile
	if params.FirmwareVersion != "" {
		firmwareRef = params.FirmwareVersion
	}

	if len(job.DeviceIDs) == 0 {
		return nil, fmt.Errorf("no AP specified")
	}

	apID := job.DeviceIDs[0]

	// Get STAs for this AP
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM devices WHERE parent_id = $1`, apID)
	if err != nil {
		return nil, fmt.Errorf("get STAs: %w", err)
	}
	defer rows.Close()

	var staIDs []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			continue
		}
		staIDs = append(staIDs, id)
	}

	totalDevices := 1 + len(staIDs)
	dbExecIgnore(r.db, `UPDATE job_runs SET total_steps = $1 WHERE id = $2`, totalDevices, job.ID)

	var results []*firmware.UpgradeResult
	completed := 0

	// Upgrade STAs first (while AP is still up)
	r.logEvent(job.ID, EventProgress, nil, fmt.Sprintf("Upgrading %d STAs first", len(staIDs)), nil)
	for _, staID := range staIDs {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		r.logEvent(job.ID, EventProgress, &staID, "Upgrading STA", nil)
		result, err := r.fwService.UpgradeDevice(ctx, int64(staID), firmwareRef, params.Force)
		if err != nil {
			r.logEvent(job.ID, EventWarning, &staID, fmt.Sprintf("STA upgrade failed: %v", err), nil)
		} else if result != nil {
			r.logEvent(job.ID, EventStepComplete, &staID, fmt.Sprintf("STA upgrade %s", result.Status), nil)
		}
		if result != nil {
			results = append(results, result)
		}
		completed++
		r.updateProgress(job.ID, completed, totalDevices)
	}

	// Now upgrade the AP
	r.logEvent(job.ID, EventProgress, &apID, "Upgrading AP", nil)
	result, err := r.fwService.UpgradeDevice(ctx, int64(apID), firmwareRef, params.Force)
	if err != nil {
		r.logEvent(job.ID, EventError, &apID, fmt.Sprintf("AP upgrade failed: %v", err), nil)
	} else if result != nil {
		r.logEvent(job.ID, EventStepComplete, &apID, fmt.Sprintf("AP upgrade %s", result.Status), nil)
	}
	if result != nil {
		results = append(results, result)
	}
	completed++
	r.updateProgress(job.ID, completed, totalDevices)

	return results, nil
}

// runBackupJob backs up device configs to filesystem
func (r *Runner) runBackupJob(ctx context.Context, job *JobRun) (interface{}, error) {
	var params BackupParams
	json.Unmarshal(job.Parameters, &params)

	type BackupResult struct {
		DeviceID int    `json:"device_id"`
		Status   string `json:"status"`
		Message  string `json:"message,omitempty"`
		Path     string `json:"path,omitempty"`
	}

	// Get backup directory from settings
	backupPath := "backups"
	r.db.QueryRow(`SELECT value FROM settings WHERE key = 'backup_dir'`).Scan(&backupPath)

	var results []BackupResult
	completed := 0

	// Set total_steps for progress visibility (same as fanout)
	dbExecIgnore(r.db, `UPDATE job_runs SET total_steps = $1 WHERE id = $2`, len(job.DeviceIDs), job.ID)

	for _, deviceID := range job.DeviceIDs {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		r.logEvent(job.ID, EventProgress, &deviceID, "Backing up config", nil)

		// Get device info including hostname and parent
		var ip, hostname, username, password string
		var parentID sql.NullInt64
		err := r.db.QueryRowContext(ctx, `
			SELECT host(ip_address), COALESCE(hostname, ''), COALESCE(username, ''), COALESCE(password, ''), parent_id
			FROM devices WHERE id = $1
		`, deviceID).Scan(&ip, &hostname, &username, &password, &parentID)

		if err != nil {
			results = append(results, BackupResult{DeviceID: deviceID, Status: "failed", Message: "device not found"})
			continue
		}

		// Fetch config
		config, err := r.fwService.FetchConfig(ip, username, password)
		if err != nil {
			r.logEvent(job.ID, EventWarning, &deviceID, fmt.Sprintf("Backup failed: %v", err), nil)
			results = append(results, BackupResult{DeviceID: deviceID, Status: "failed", Message: err.Error()})
		} else {
			// Save to filesystem with same structure as API backup
			safeIP := sanitizeIPForPath(ip)
			var dirPath string
			if parentID.Valid {
				// STA - get parent AP's IP
				var apIP string
				r.db.QueryRow(`SELECT host(ip_address) FROM devices WHERE id = $1`, parentID.Int64).Scan(&apIP)
				if apIP == "" {
					apIP = "unknown-ap"
				}
				safeAPIP := sanitizeIPForPath(apIP)
				dirPath = filepath.Join(backupPath, safeAPIP, safeIP)
			} else {
				dirPath = filepath.Join(backupPath, safeIP)
			}

			if err := os.MkdirAll(dirPath, 0755); err != nil {
				results = append(results, BackupResult{DeviceID: deviceID, Status: "failed", Message: "mkdir: " + err.Error()})
				continue
			}

			// Filename: hostname_timestamp.cfg
			name := hostname
			if name == "" {
				name = strings.ReplaceAll(safeIP, ".", "-")
			}
			name = strings.ReplaceAll(name, "/", "-")
			name = strings.ReplaceAll(name, "\\", "-")
			name = strings.ReplaceAll(name, ":", "-")

			timestamp := time.Now().Format("20060102-150405")
			filename := filepath.Join(dirPath, fmt.Sprintf("%s_%s.cfg", name, timestamp))

			if err := os.WriteFile(filename, config, 0644); err != nil {
				results = append(results, BackupResult{DeviceID: deviceID, Status: "failed", Message: "write: " + err.Error()})
			} else {
				r.logEvent(job.ID, EventStepComplete, &deviceID, fmt.Sprintf("Config backed up to %s", filename), nil)
				results = append(results, BackupResult{DeviceID: deviceID, Status: "success", Path: filename})
			}
		}

		completed++
		r.updateProgress(job.ID, completed, len(job.DeviceIDs))
	}

	return results, nil
}

// runRebootJob reboots devices
func (r *Runner) runRebootJob(ctx context.Context, job *JobRun) (interface{}, error) {
	type RebootResult struct {
		DeviceID int    `json:"device_id"`
		Status   string `json:"status"`
		Message  string `json:"message,omitempty"`
	}

	var results []RebootResult
	completed := 0

	// Set total_steps for progress visibility (same as fanout)
	dbExecIgnore(r.db, `UPDATE job_runs SET total_steps = $1 WHERE id = $2`, len(job.DeviceIDs), job.ID)

	for _, deviceID := range job.DeviceIDs {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		r.logEvent(job.ID, EventProgress, &deviceID, "Rebooting device", nil)

		result, err := r.fwService.RebootDeviceByID(ctx, int64(deviceID))
		if err != nil {
			r.logEvent(job.ID, EventWarning, &deviceID, fmt.Sprintf("Reboot failed: %v", err), nil)
			results = append(results, RebootResult{DeviceID: deviceID, Status: "failed", Message: err.Error()})
		} else {
			msg := "Reboot initiated"
			if result != nil && result.API != "" {
				msg = fmt.Sprintf("Reboot initiated via %s", result.API)
			}
			r.logEvent(job.ID, EventStepComplete, &deviceID, msg, nil)
			results = append(results, RebootResult{DeviceID: deviceID, Status: "success", Message: msg})
		}

		completed++
		r.updateProgress(job.ID, completed, len(job.DeviceIDs))
	}

	return results, nil
}

// runRefreshJob triggers device refresh
func (r *Runner) runRefreshJob(ctx context.Context, job *JobRun) (interface{}, error) {
	// This would integrate with the poller to force immediate refresh
	r.logEvent(job.ID, EventProgress, nil, fmt.Sprintf("Refreshing %d devices", len(job.DeviceIDs)), nil)
	// TODO: integrate with poller.RefreshDevices()
	return map[string]int{"refreshed": len(job.DeviceIDs)}, nil
}

// CancelJob cancels a running job
func (r *Runner) CancelJob(jobID string) error {
	r.mu.Lock()
	cancel, ok := r.running[jobID]
	r.mu.Unlock()

	if ok {
		log.Printf("Job %s: cancelling running job", jobID)
		cancel()
		return nil
	}

	// Job not running, check if pending
	result, err := r.db.Exec(`
		UPDATE job_runs SET status = 'cancelled', completed_at = NOW()
		WHERE id = $1 AND status = 'pending'
	`, jobID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		// Check what state the job is in for better error message
		var status string
		err := r.db.QueryRow(`SELECT status FROM job_runs WHERE id = $1`, jobID).Scan(&status)
		if err != nil {
			return fmt.Errorf("job %s not found", jobID)
		}
		return fmt.Errorf("job %s cannot be cancelled (status: %s)", jobID, status)
	}
	log.Printf("Job %s: cancelled pending job", jobID)
	return nil
}

// GetJob retrieves a job by ID
func (r *Runner) GetJob(ctx context.Context, jobID string) (*JobRun, error) {
	job := &JobRun{}
	var deviceIDs pq.Int64Array
	var startedAt, completedAt sql.NullTime
	var result, params sql.NullString
	var errMsg sql.NullString
	var createdBy sql.NullInt64

	err := r.db.QueryRowContext(ctx, `
		SELECT id, job_type, status, progress, total_steps, completed_steps,
		       device_ids, parameters, result, error_message,
		       created_at, started_at, completed_at, created_by
		FROM job_runs WHERE id = $1
	`, jobID).Scan(
		&job.ID, &job.JobType, &job.Status, &job.Progress, &job.TotalSteps, &job.CompletedSteps,
		&deviceIDs, &params, &result, &errMsg,
		&job.CreatedAt, &startedAt, &completedAt, &createdBy,
	)
	if err != nil {
		return nil, err
	}

	job.DeviceIDs = make([]int, len(deviceIDs))
	for i, id := range deviceIDs {
		job.DeviceIDs[i] = int(id)
	}
	if params.Valid {
		job.Parameters = json.RawMessage(params.String)
	}
	if result.Valid {
		job.Result = json.RawMessage(result.String)
	}
	if errMsg.Valid {
		job.ErrorMessage = errMsg.String
	}
	if startedAt.Valid {
		job.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		job.CompletedAt = &completedAt.Time
	}
	if createdBy.Valid {
		job.CreatedBy = int(createdBy.Int64)
	}

	return job, nil
}

// GetJobEvents retrieves events for a job
func (r *Runner) GetJobEvents(ctx context.Context, jobID string, limit int) ([]JobEvent, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.job_id, e.event_time, e.event_type, e.device_id, e.message, e.data,
		       d.hostname, host(d.ip_address)
		FROM job_events e
		LEFT JOIN devices d ON e.device_id = d.id
		WHERE e.job_id = $1
		ORDER BY e.event_time ASC
		LIMIT $2
	`, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []JobEvent
	for rows.Next() {
		var ev JobEvent
		var deviceID sql.NullInt64
		var data sql.NullString
		var hostname, ip sql.NullString

		err := rows.Scan(&ev.ID, &ev.JobID, &ev.EventTime, &ev.EventType, &deviceID, &ev.Message, &data, &hostname, &ip)
		if err != nil {
			continue
		}
		if deviceID.Valid {
			id := int(deviceID.Int64)
			ev.DeviceID = &id
		}
		if hostname.Valid {
			ev.DeviceHostname = hostname.String
		}
		if ip.Valid {
			ev.DeviceIP = ip.String
		}
		if data.Valid {
			ev.Data = json.RawMessage(data.String)
		}
		events = append(events, ev)
	}

	return events, nil
}

// ListJobs returns recent jobs
func (r *Runner) ListJobs(ctx context.Context, status string, limit int) ([]JobRun, error) {
	if limit <= 0 {
		limit = 50
	}

	query := `
		SELECT id, job_type, status, progress, total_steps, completed_steps,
		       device_ids, parameters, result, error_message,
		       created_at, started_at, completed_at, created_by
		FROM job_runs
	`
	args := []interface{}{}

	if status != "" {
		query += " WHERE status = $1"
		args = append(args, status)
	}

	query += " ORDER BY created_at DESC LIMIT " + fmt.Sprintf("%d", limit)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []JobRun
	for rows.Next() {
		job := JobRun{}
		var deviceIDs pq.Int64Array
		var startedAt, completedAt sql.NullTime
		var result, params sql.NullString
		var errMsg sql.NullString
		var createdBy sql.NullInt64

		err := rows.Scan(
			&job.ID, &job.JobType, &job.Status, &job.Progress, &job.TotalSteps, &job.CompletedSteps,
			&deviceIDs, &params, &result, &errMsg,
			&job.CreatedAt, &startedAt, &completedAt, &createdBy,
		)
		if err != nil {
			continue
		}

		job.DeviceIDs = make([]int, len(deviceIDs))
		for i, id := range deviceIDs {
			job.DeviceIDs[i] = int(id)
		}
		if params.Valid {
			job.Parameters = json.RawMessage(params.String)
		}
		if result.Valid {
			job.Result = json.RawMessage(result.String)
		}
		if errMsg.Valid {
			job.ErrorMessage = errMsg.String
		}
		if startedAt.Valid {
			job.StartedAt = &startedAt.Time
		}
		if completedAt.Valid {
			job.CompletedAt = &completedAt.Time
		}
		if createdBy.Valid {
			job.CreatedBy = int(createdBy.Int64)
		}

		jobs = append(jobs, job)
	}

	return jobs, nil
}

// Shutdown waits for running jobs to complete
func (r *Runner) Shutdown(timeout time.Duration) {
	// Cancel all running jobs
	r.mu.Lock()
	for _, cancel := range r.running {
		cancel()
	}
	r.mu.Unlock()

	// Wait with timeout
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("Job runner: all jobs completed")
	case <-time.After(timeout):
		log.Println("Job runner: shutdown timeout, some jobs may not have completed")
	}
}

// Helper methods

func (r *Runner) updateStatus(jobID string, status JobStatus, errMsg *string) {
	if errMsg != nil {
		dbExecIgnore(r.db, `UPDATE job_runs SET status = $1, error_message = $2 WHERE id = $3`, status, *errMsg, jobID)
	} else {
		dbExecIgnore(r.db, `UPDATE job_runs SET status = $1 WHERE id = $2`, status, jobID)
	}
}

func (r *Runner) updateProgress(jobID string, completed, total int) {
	progress := 0
	if total > 0 {
		progress = (completed * 100) / total
	}
	dbExecIgnore(r.db, `UPDATE job_runs SET progress = $1, completed_steps = $2 WHERE id = $3`, progress, completed, jobID)

	// Broadcast progress
	r.broadcastJobProgress(jobID, progress, completed, total)
}

func (r *Runner) logEvent(jobID string, eventType EventType, deviceID *int, message string, data interface{}) {
	var dataJSON interface{}
	if data != nil {
		dataJSON, _ = json.Marshal(data)
	}

	dbExecIgnore(r.db, `
		INSERT INTO job_events (job_id, event_type, device_id, message, data)
		VALUES ($1, $2, $3, $4, $5)
	`, jobID, eventType, deviceID, message, dataJSON)

	// Broadcast event
	r.broadcastJobEvent(jobID, eventType, deviceID, message, data)
}

func (r *Runner) broadcastJobUpdate(job *JobRun) {
	if job == nil {
		log.Printf("broadcastJobUpdate: job is nil")
		return
	}
	if r.wsHub == nil {
		log.Printf("Job %s: wsHub is nil, cannot broadcast", job.ID)
		return
	}

	// Build target info for display
	targetInfo := ""
	if len(job.DeviceIDs) == 1 {
		// Single device - look up hostname and IP
		var hostname, ip sql.NullString
		r.db.QueryRow(`SELECT hostname, host(ip_address) FROM devices WHERE id = $1`, job.DeviceIDs[0]).Scan(&hostname, &ip)
		if hostname.Valid && hostname.String != "" {
			targetInfo = hostname.String
			if ip.Valid && ip.String != "" {
				targetInfo += " (" + ip.String + ")"
			}
		} else if ip.Valid {
			targetInfo = ip.String
		}
	} else if len(job.DeviceIDs) > 1 {
		targetInfo = fmt.Sprintf("%d devices", len(job.DeviceIDs))
	}

	log.Printf("Job %s: broadcasting job_update status=%s progress=%d target=%s", job.ID, job.Status, job.Progress, targetInfo)
	r.wsHub.Broadcast(websocket.Message{
		Type: websocket.MsgJobUpdate,
		Data: map[string]interface{}{
			"job_id":          job.ID,
			"job_type":        job.JobType,
			"status":          job.Status,
			"progress":        job.Progress,
			"total_steps":     job.TotalSteps,
			"completed_steps": job.CompletedSteps,
			"error_message":   job.ErrorMessage,
			"target":          targetInfo,
		},
	})
}

func (r *Runner) broadcastJobProgress(jobID string, progress, completed, total int) {
	if r.wsHub == nil {
		log.Printf("Job %s: wsHub is nil, cannot broadcast progress", jobID)
		return
	}
	log.Printf("Job %s: broadcasting job_progress progress=%d completed=%d total=%d", jobID, progress, completed, total)
	r.wsHub.Broadcast(websocket.Message{
		Type: "job_progress",
		Data: map[string]interface{}{
			"job_id":          jobID,
			"progress":        progress,
			"completed_steps": completed,
			"total_steps":     total,
		},
	})
}

func (r *Runner) broadcastJobEvent(jobID string, eventType EventType, deviceID *int, message string, data interface{}) {
	if r.wsHub == nil {
		log.Printf("Job %s: wsHub is nil, cannot broadcast event", jobID)
		return
	}
	log.Printf("Job %s: broadcasting job_event type=%s message=%s", jobID, eventType, message)
	r.wsHub.Broadcast(websocket.Message{
		Type: "job_event",
		Data: map[string]interface{}{
			"job_id":     jobID,
			"event_type": eventType,
			"device_id":  deviceID,
			"message":    message,
			"data":       data,
		},
	})
}

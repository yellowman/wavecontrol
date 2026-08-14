package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/yellowman/wavecontrol/internal/firmware"
	"github.com/yellowman/wavecontrol/internal/websocket"
)

// JobType identifies the type of scheduled job
type JobType string

const (
	JobUpgrade JobType = "upgrade"
	JobReboot  JobType = "reboot"
	JobRefresh JobType = "refresh"
)

// JobStatus represents the status of a job
type JobStatus string

const (
	StatusPending   JobStatus = "pending"
	StatusRunning   JobStatus = "running"
	StatusCompleted JobStatus = "completed"
	StatusFailed    JobStatus = "failed"
	StatusCancelled JobStatus = "cancelled"
	StatusBlocked   JobStatus = "blocked" // Blocked by maintenance window
)

// dbExecIgnore executes a query, logs errors but doesn't return them (fire-and-forget)
func dbExecIgnore(db *sql.DB, query string, args ...any) {
	if _, err := db.Exec(query, args...); err != nil {
		log.Printf("DB exec error: %v", err)
	}
}

// ScheduledJob represents a scheduled job from the database
type ScheduledJob struct {
	ID               int             `json:"id"`
	JobType          JobType         `json:"job_type"`
	DeviceIDs        []int           `json:"device_ids"`
	Parameters       json.RawMessage `json:"parameters"`
	ScheduledAt      time.Time       `json:"scheduled_at"`
	RepeatCron       string          `json:"repeat_cron,omitempty"`
	LastRun          *time.Time      `json:"last_run,omitempty"`
	NextRun          *time.Time      `json:"next_run,omitempty"`
	Status           JobStatus       `json:"status"`
	Progress         int             `json:"progress"`
	TotalDevices     int             `json:"total_devices"`
	CompletedDevices int             `json:"completed_devices"`
	ErrorMessage     string          `json:"error_message,omitempty"`
	CreatedBy        int             `json:"created_by"`
	CreatedAt        time.Time       `json:"created_at"`
}

// UpgradeParams contains parameters for upgrade jobs
type UpgradeParams struct {
	FirmwareFile    string `json:"firmware_file,omitempty"`    // Specific file (deprecated)
	FirmwareVersion string `json:"firmware_version,omitempty"` // Version string - resolves per device flavor
	Force           bool   `json:"force"`
	Fanout          bool   `json:"fanout"` // For APs: upgrade STAs first
}

// MaintenanceWindow represents a maintenance window
type MaintenanceWindow struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Scope     string    `json:"scope"` // global, region, site
	RegionID  *int      `json:"region_id,omitempty"`
	SiteID    *int      `json:"site_id,omitempty"`
	DayOfWeek []int     `json:"day_of_week"` // 0=Sun, 6=Sat
	StartTime string    `json:"start_time"`  // HH:MM
	EndTime   string    `json:"end_time"`    // HH:MM
	Timezone  string    `json:"timezone"`
	AllowJobs []string  `json:"allow_jobs"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// Scheduler manages scheduled jobs
type Scheduler struct {
	db        *sql.DB
	fwService *firmware.Service
	wsHub     *websocket.Hub

	mu      sync.Mutex
	running bool
	wg      sync.WaitGroup

	// Concurrency control
	jobSem            chan struct{}
	maxConcurrentJobs int
	checkInterval     time.Duration

	// Maintenance window checking
	respectMaintenance bool

	// Running job cancellation (best-effort, in-process)
	runMu          sync.Mutex
	runningCancels map[int]context.CancelFunc
}

// NewScheduler creates a new scheduler
func NewScheduler(db *sql.DB, fwService *firmware.Service, wsHub *websocket.Hub) *Scheduler {
	s := &Scheduler{
		db:                 db,
		fwService:          fwService,
		wsHub:              wsHub,
		maxConcurrentJobs:  5,
		checkInterval:      10 * time.Second,
		respectMaintenance: true,
		runningCancels:     make(map[int]context.CancelFunc),
	}
	s.loadSettings()
	s.jobSem = make(chan struct{}, s.maxConcurrentJobs)
	return s
}

// loadSettings loads scheduler settings from database
func (s *Scheduler) loadSettings() {
	var val string

	if s.db.QueryRow(`SELECT value FROM settings WHERE key = 'scheduler_max_concurrent'`).Scan(&val) == nil {
		if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 50 {
			s.maxConcurrentJobs = n
		}
	}

	if s.db.QueryRow(`SELECT value FROM settings WHERE key = 'scheduler_check_interval'`).Scan(&val) == nil {
		if n, err := strconv.Atoi(val); err == nil && n >= 5 && n <= 300 {
			s.checkInterval = time.Duration(n) * time.Second
		}
	}

	if s.db.QueryRow(`SELECT value FROM settings WHERE key = 'scheduler_respect_maintenance'`).Scan(&val) == nil {
		s.respectMaintenance = val == "true"
	}

	log.Printf("Scheduler: maxConcurrent=%d, checkInterval=%v, respectMaintenance=%v",
		s.maxConcurrentJobs, s.checkInterval, s.respectMaintenance)
}

// ReloadSettings reloads settings from database (call after settings change)
func (s *Scheduler) ReloadSettings() {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldMax := s.maxConcurrentJobs
	s.loadSettings()

	// Resize semaphore if max changed
	if s.maxConcurrentJobs != oldMax {
		s.jobSem = make(chan struct{}, s.maxConcurrentJobs)
		log.Printf("Scheduler: resized job semaphore to %d", s.maxConcurrentJobs)
	}
}

// Start begins the scheduler loop
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()

	log.Printf("Scheduler started (max concurrent: %d)", s.maxConcurrentJobs)

	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
			s.wg.Wait()
			log.Println("Scheduler stopped")
			return

		case <-ticker.C:
			s.checkAndRunJobs(ctx)
		}
	}
}

// checkAndRunJobs checks for due jobs and runs them
func (s *Scheduler) checkAndRunJobs(ctx context.Context) {
	now := time.Now()

	// Find jobs that are due - only fetch as many as we can run
	// Use FOR UPDATE SKIP LOCKED to prevent multiple instances from grabbing same job
	// Include 'blocked' jobs so we can re-check maintenance windows
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_type, device_ids, parameters, scheduled_at, repeat_cron, status, created_by
		FROM scheduled_jobs
		WHERE status IN ('pending', 'blocked')
		  AND ((next_run IS NULL AND scheduled_at <= $1)
		   OR (next_run IS NOT NULL AND next_run <= $1))
		ORDER BY COALESCE(next_run, scheduled_at)
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	`, now, s.maxConcurrentJobs*2) // Fetch extra in case some are blocked
	if err != nil {
		log.Printf("Scheduler query error: %v", err)
		return
	}
	defer rows.Close()

	var jobs []ScheduledJob
	for rows.Next() {
		var job ScheduledJob
		var deviceIDs pq.Int64Array
		var repeatCron sql.NullString

		err := rows.Scan(
			&job.ID, &job.JobType, &deviceIDs, &job.Parameters,
			&job.ScheduledAt, &repeatCron, &job.Status, &job.CreatedBy,
		)
		if err != nil {
			log.Printf("Scheduler scan error: %v", err)
			continue
		}

		if repeatCron.Valid {
			job.RepeatCron = repeatCron.String
		}
		job.DeviceIDs = make([]int, len(deviceIDs))
		for i, id := range deviceIDs {
			job.DeviceIDs[i] = int(id)
		}

		jobs = append(jobs, job)
	}
	rows.Close()

	// Process each job
	for _, job := range jobs {
		// Check maintenance windows BEFORE claiming
		if s.respectMaintenance {
			inWindow, nextWindowStart := s.checkMaintenanceWindow(ctx, job)
			if !inWindow {
				// Block the job until next window
				s.blockJobForMaintenance(job.ID, nextWindowStart)
				continue
			}
		}

		// Try to acquire semaphore (non-blocking)
		select {
		case s.jobSem <- struct{}{}:
			// Got a slot, claim the job atomically
			claimedJob, ok := s.claimJob(ctx, job.ID)
			if !ok {
				// Job was claimed by another instance or cancelled
				<-s.jobSem // Release slot
				continue
			}

			s.wg.Add(1)
			go func(j ScheduledJob) {
				defer s.wg.Done()
				defer func() { <-s.jobSem }() // Release semaphore when done
				s.runJob(ctx, j)
			}(claimedJob)

		default:
			// No slots available, skip this cycle
			log.Printf("Job concurrency limit reached (%d), deferring", s.maxConcurrentJobs)
			return
		}
	}
}

// checkMaintenanceWindow checks if job can run and returns next window start if not
func (s *Scheduler) checkMaintenanceWindow(ctx context.Context, job ScheduledJob) (inWindow bool, nextStart *time.Time) {
	// Get device site/region info for the first device (as representative)
	if len(job.DeviceIDs) == 0 {
		return true, nil // No devices = global job, always allowed
	}

	var siteID, regionID sql.NullInt64
	s.db.QueryRowContext(ctx, `
		SELECT d.site_id, s.region_id 
		FROM devices d
		LEFT JOIN sites s ON d.site_id = s.id
		WHERE d.id = $1
	`, job.DeviceIDs[0]).Scan(&siteID, &regionID)

	now := time.Now()
	currentDOW := int(now.Weekday()) // 0 = Sunday

	// Check for applicable maintenance windows (most specific first)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, start_time, end_time, timezone, allow_jobs, day_of_week
		FROM maintenance_windows
		WHERE enabled = true
		  AND (
		    (scope = 'global')
		    OR (scope = 'region' AND region_id = $1)
		    OR (scope = 'site' AND site_id = $2)
		  )
		ORDER BY 
		  CASE scope WHEN 'site' THEN 1 WHEN 'region' THEN 2 ELSE 3 END
	`, regionID, siteID)
	if err != nil {
		log.Printf("Maintenance window query error: %v", err)
		return true, nil // Allow on error
	}
	defer rows.Close()

	var earliestNextWindow *time.Time

	for rows.Next() {
		var mw MaintenanceWindow
		var startTime, endTime string
		var allowJobs pq.StringArray
		var daysOfWeek pq.Int64Array

		if err := rows.Scan(&mw.ID, &mw.Scope, &startTime, &endTime, &mw.Timezone, &allowJobs, &daysOfWeek); err != nil {
			continue
		}

		// Check if job type is allowed
		jobAllowed := false
		for _, jt := range allowJobs {
			if jt == string(job.JobType) {
				jobAllowed = true
				break
			}
		}
		if !jobAllowed {
			continue
		}

		// Load timezone
		loc, err := time.LoadLocation(mw.Timezone)
		if err != nil {
			loc = time.UTC
		}
		nowLocal := now.In(loc)

		// Check day of week
		dayMatch := len(daysOfWeek) == 0 // Empty = any day
		for _, d := range daysOfWeek {
			if int(d) == currentDOW {
				dayMatch = true
				break
			}
		}

		// Parse times
		start, _ := time.ParseInLocation("15:04:05", startTime, loc)
		end, _ := time.ParseInLocation("15:04:05", endTime, loc)

		// Adjust to today's date
		start = time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), start.Hour(), start.Minute(), 0, 0, loc)
		end = time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), end.Hour(), end.Minute(), 0, 0, loc)

		// Handle overnight windows (e.g., 22:00 - 06:00)
		if end.Before(start) {
			end = end.Add(24 * time.Hour)
		}

		if dayMatch && nowLocal.After(start) && nowLocal.Before(end) {
			return true, nil // Within maintenance window
		}

		// Calculate next window start for this window
		nextWindow := start
		if nowLocal.After(start) {
			nextWindow = nextWindow.Add(24 * time.Hour)
		}
		// Adjust for day of week if specified
		if len(daysOfWeek) > 0 && !dayMatch {
			// Find next matching day
			for i := 1; i <= 7; i++ {
				checkDay := (currentDOW + i) % 7
				for _, d := range daysOfWeek {
					if int(d) == checkDay {
						nextWindow = nextWindow.Add(time.Duration(i) * 24 * time.Hour)
						break
					}
				}
			}
		}

		nextWindowUTC := nextWindow.UTC()
		if earliestNextWindow == nil || nextWindowUTC.Before(*earliestNextWindow) {
			earliestNextWindow = &nextWindowUTC
		}
	}

	// No matching window found - check if any windows exist
	var windowCount int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_windows WHERE enabled = true`).Scan(&windowCount)

	// If no maintenance windows defined, allow all jobs
	if windowCount == 0 {
		return true, nil
	}

	return false, earliestNextWindow
}

// blockJobForMaintenance marks a job as blocked until next maintenance window
func (s *Scheduler) blockJobForMaintenance(jobID int, nextWindow *time.Time) {
	if nextWindow != nil {
		dbExecIgnore(s.db, `
			UPDATE scheduled_jobs 
			SET status = 'blocked', next_run = $1, error_message = 'Waiting for maintenance window'
			WHERE id = $2
		`, nextWindow, jobID)
	} else {
		dbExecIgnore(s.db, `
			UPDATE scheduled_jobs 
			SET status = 'blocked', error_message = 'No applicable maintenance window'
			WHERE id = $1
		`, jobID)
	}
}

// claimJob atomically claims a job by updating its status to 'running'
// Returns the job if successfully claimed, false otherwise
func (s *Scheduler) claimJob(ctx context.Context, jobID int) (ScheduledJob, bool) {
	var job ScheduledJob
	var deviceIDs pq.Int64Array
	var repeatCron sql.NullString

	// Atomic update: only succeeds if job is still pending (or blocked waiting for maintenance)
	err := s.db.QueryRowContext(ctx, `
		UPDATE scheduled_jobs
		SET status = 'running', error_message = ''
		WHERE id = $1 AND status IN ('pending', 'blocked')
		RETURNING id, job_type, device_ids, parameters, scheduled_at, repeat_cron, status, created_by
	`, jobID).Scan(
		&job.ID, &job.JobType, &deviceIDs, &job.Parameters,
		&job.ScheduledAt, &repeatCron, &job.Status, &job.CreatedBy,
	)

	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("Failed to claim job %d: %v", jobID, err)
		}
		return job, false
	}

	if repeatCron.Valid {
		job.RepeatCron = repeatCron.String
	}

	// Convert pq.Int64Array to []int
	job.DeviceIDs = make([]int, len(deviceIDs))
	for i, id := range deviceIDs {
		job.DeviceIDs[i] = int(id)
	}

	return job, true
}

// runJob executes a single scheduled job
func (s *Scheduler) runJob(ctx context.Context, job ScheduledJob) {
	log.Printf("Running scheduled job %d: %s (%d devices)", job.ID, job.JobType, len(job.DeviceIDs))

	// Create a per-job context so we can cancel individual running jobs.
	jobCtx, cancel := context.WithCancel(ctx)
	s.runMu.Lock()
	s.runningCancels[job.ID] = cancel
	s.runMu.Unlock()
	defer func() {
		cancel()
		s.runMu.Lock()
		delete(s.runningCancels, job.ID)
		s.runMu.Unlock()
	}()

	// Initialize progress tracking
	totalDevices := len(job.DeviceIDs)
	s.updateJobProgress(job.ID, 0, totalDevices, 0, "")

	// Notify via WebSocket
	s.broadcastJobStatus(job.ID, string(StatusRunning), 0, totalDevices, 0, "")

	var err error
	switch job.JobType {
	case JobUpgrade:
		err = s.runUpgradeJob(jobCtx, job)
	case JobReboot:
		err = s.runRebootJob(jobCtx, job)
	case JobRefresh:
		err = s.runRefreshJob(jobCtx, job)
	default:
		err = fmt.Errorf("unknown job type: %s", job.JobType)
	}

	// Update status
	status := StatusCompleted
	var errMsg string
	if err != nil {
		// If we were explicitly cancelled, report cancelled instead of failed.
		if errors.Is(err, context.Canceled) {
			// Only treat as cancelled if the DB status is already 'cancelled' (user request)
			var dbStatus string
			_ = s.db.QueryRowContext(ctx, `SELECT status FROM scheduled_jobs WHERE id = $1`, job.ID).Scan(&dbStatus)
			if dbStatus == string(StatusCancelled) {
				status = StatusCancelled
				errMsg = "Cancelled"
				log.Printf("Job %d cancelled", job.ID)
			} else {
				status = StatusFailed
				errMsg = err.Error()
				log.Printf("Job %d failed (context cancelled): %v", job.ID, err)
			}
		} else {
			status = StatusFailed
			errMsg = err.Error()
			log.Printf("Job %d failed: %v", job.ID, err)
		}
	} else {
		log.Printf("Job %d completed successfully", job.ID)
	}

	s.updateJobStatus(job.ID, status, &errMsg)

	// Handle repeat jobs
	if job.RepeatCron != "" && status == StatusCompleted {
		nextRun := s.calculateNextRun(job.RepeatCron)
		if nextRun != nil {
			s.scheduleNextRun(job.ID, *nextRun)
		}
	}

	// Notify via WebSocket
	s.broadcastJobStatus(job.ID, string(status), 100, totalDevices, totalDevices, errMsg)
}

// broadcastJobStatus sends job status update via WebSocket
func (s *Scheduler) broadcastJobStatus(jobID int, status string, progress, total, completed int, errMsg string) {
	if s.wsHub == nil {
		return
	}
	s.wsHub.BroadcastJobUpdate(jobID, status, map[string]interface{}{
		"progress":          progress,
		"total_devices":     total,
		"completed_devices": completed,
		"error":             errMsg,
	})
}

// updateJobProgress updates progress in database
func (s *Scheduler) updateJobProgress(jobID, progress, total, completed int, errMsg string) {
	dbExecIgnore(s.db, `
		UPDATE scheduled_jobs 
		SET progress = $1, total_devices = $2, completed_devices = $3, error_message = NULLIF($4, '')
		WHERE id = $5
	`, progress, total, completed, errMsg, jobID)
}

// runUpgradeJob runs a firmware upgrade job with progress tracking
func (s *Scheduler) runUpgradeJob(ctx context.Context, job ScheduledJob) error {
	var params UpgradeParams
	if err := json.Unmarshal(job.Parameters, &params); err != nil {
		return fmt.Errorf("invalid upgrade params: %w", err)
	}

	// Build full device list (including STAs if fanout)
	allDevices := []int{}
	for _, deviceID := range job.DeviceIDs {
		var parentID sql.NullInt64
		s.db.QueryRowContext(ctx, `SELECT parent_id FROM devices WHERE id = $1`, deviceID).Scan(&parentID)

		// If fanout and this is an AP, add STAs first
		if params.Fanout && !parentID.Valid {
			staIDs, err := s.getSTAIDs(ctx, deviceID)
			if err == nil && len(staIDs) > 0 {
				allDevices = append(allDevices, staIDs...)
			}
		}
		allDevices = append(allDevices, deviceID)
	}

	totalDevices := len(allDevices)
	completedDevices := 0

	// Process each device with progress updates
	for i, deviceID := range allDevices {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Update progress before starting this device
		progress := int(float64(i) / float64(totalDevices) * 100)
		s.updateJobProgress(job.ID, progress, totalDevices, completedDevices, "")
		s.broadcastJobStatus(job.ID, string(StatusRunning), progress, totalDevices, completedDevices, "")

		// Upgrade this device
		result := s.upgradeDevice(ctx, job.ID, deviceID, params)
		completedDevices++

		// Broadcast per-device result
		if s.wsHub != nil {
			s.wsHub.BroadcastDeviceUpdate(deviceID, "", map[string]interface{}{
				"id":      deviceID,
				"job_id":  job.ID,
				"action":  "upgrade",
				"status":  result.Status,
				"message": result.Message,
			})
		}
	}

	// Final progress update
	s.updateJobProgress(job.ID, 100, totalDevices, completedDevices, "")

	return nil
}

// upgradeDevice upgrades a single device and returns the result
func (s *Scheduler) upgradeDevice(ctx context.Context, jobID, deviceID int, params UpgradeParams) *firmware.UpgradeResult {
	var ip, mac, flavor string
	err := s.db.QueryRowContext(ctx, `
		SELECT ip_address::text, mac, COALESCE(flavor, '')
		FROM devices WHERE id = $1
	`, deviceID).Scan(&ip, &mac, &flavor)
	if err != nil {
		log.Printf("Job %d: device %d lookup failed: %v", jobID, deviceID, err)
		return &firmware.UpgradeResult{
			DeviceID: int64(deviceID),
			Status:   "failed",
			Message:  fmt.Sprintf("device lookup failed: %v", err),
		}
	}

	// Determine firmware reference - prefer version (resolves per device flavor)
	firmwareRef := params.FirmwareVersion
	if firmwareRef == "" {
		firmwareRef = params.FirmwareFile
	}

	// If still empty, auto-select by flavor
	if firmwareRef == "" && flavor != "" {
		fw := s.fwService.FindByFlavor(flavor)
		if fw != nil {
			firmwareRef = fw.Name
		}
	}

	if firmwareRef == "" {
		log.Printf("Job %d: no firmware found for device %d (flavor: %s)", jobID, deviceID, flavor)
		return &firmware.UpgradeResult{
			DeviceID: int64(deviceID),
			Status:   "skipped",
			Message:  "no matching firmware found",
		}
	}

	// Run upgrade - UpgradeDevice handles both filename and version strings
	result, err := s.fwService.UpgradeDevice(ctx, int64(deviceID), firmwareRef, params.Force)
	if err != nil {
		log.Printf("Job %d: upgrade failed for device %d: %v", jobID, deviceID, err)
		result = &firmware.UpgradeResult{
			DeviceID: int64(deviceID),
			Status:   "failed",
			Message:  err.Error(),
		}
	}

	// Log result
	s.db.ExecContext(ctx, `
		INSERT INTO changelog (device_mac, change, "user")
		VALUES ($1, $2, $3)
	`, mac, fmt.Sprintf("Scheduled upgrade %s: %s", result.Status, result.Message), nil)

	return result
}

// runRebootJob runs a reboot job
func (s *Scheduler) runRebootJob(ctx context.Context, job ScheduledJob) error {
	for _, deviceID := range job.DeviceIDs {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		result, err := s.fwService.RebootDeviceByID(ctx, int64(deviceID))
		status := "success"
		apiUsed := "unknown_api"
		mac := ""
		if result != nil {
			apiUsed = result.API
			mac = result.DeviceMAC
		}
		if err != nil {
			status = "failed"
			log.Printf("Job %d: reboot device %d failed: %v", job.ID, deviceID, err)
		}

		s.db.ExecContext(ctx, `
			INSERT INTO changelog (device_mac, change)
			VALUES ($1, $2)
		`, mac, fmt.Sprintf("Scheduled reboot %s via %s", status, apiUsed))
	}

	return nil
}

// runRefreshJob triggers a poll refresh for devices
func (s *Scheduler) runRefreshJob(ctx context.Context, job ScheduledJob) error {
	// This would trigger the poller to refresh specific devices
	// For now, just log it - actual implementation would call poller.RefreshDevice()
	log.Printf("Job %d: refresh job for %d devices", job.ID, len(job.DeviceIDs))
	return nil
}

// getSTAIDs returns STA device IDs for an AP
func (s *Scheduler) getSTAIDs(ctx context.Context, apID int) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM devices WHERE parent_id = $1
	`, apID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// updateJobStatus updates the status of a job
func (s *Scheduler) updateJobStatus(jobID int, status JobStatus, errMsg *string) {
	var errMsgVal interface{}
	if errMsg != nil && *errMsg != "" {
		errMsgVal = *errMsg
	}

	if status == StatusCompleted || status == StatusFailed {
		dbExecIgnore(s.db, `
			UPDATE scheduled_jobs 
			SET status = $1, last_run = NOW(), error_message = $2
			WHERE id = $3
		`, status, errMsgVal, jobID)
	} else {
		dbExecIgnore(s.db, `
			UPDATE scheduled_jobs SET status = $1, error_message = $2 WHERE id = $3
		`, status, errMsgVal, jobID)
	}
}

// scheduleNextRun schedules the next run for a repeating job
func (s *Scheduler) scheduleNextRun(jobID int, nextRun time.Time) {
	dbExecIgnore(s.db, `
		UPDATE scheduled_jobs 
		SET status = 'pending', next_run = $1
		WHERE id = $2
	`, nextRun, jobID)
}

// calculateNextRun calculates the next run time from a cron expression
// Simplified cron: supports @daily, @hourly, @weekly, or interval like "30m", "1h", "24h"
func (s *Scheduler) calculateNextRun(cron string) *time.Time {
	now := time.Now()
	var next time.Time

	switch cron {
	case "@hourly":
		next = now.Add(time.Hour)
	case "@daily":
		next = now.Add(24 * time.Hour)
	case "@weekly":
		next = now.Add(7 * 24 * time.Hour)
	default:
		// Try parsing as duration
		d, err := time.ParseDuration(cron)
		if err != nil {
			return nil
		}
		next = now.Add(d)
	}

	return &next
}

// CreateJob creates a new scheduled job
func (s *Scheduler) CreateJob(jobType JobType, deviceIDs []int, params interface{}, scheduledAt time.Time, repeatCron string, userID int) (int, error) {
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return 0, err
	}

	var jobID int
	var repeatCronVal interface{}
	if repeatCron != "" {
		repeatCronVal = repeatCron
	}

	// Use pq.Array for PostgreSQL INTEGER[] type
	err = s.db.QueryRow(`
		INSERT INTO scheduled_jobs (job_type, device_ids, parameters, scheduled_at, repeat_cron, status, created_by)
		VALUES ($1, $2, $3, $4, $5, 'pending', $6)
		RETURNING id
	`, jobType, pq.Array(deviceIDs), paramsJSON, scheduledAt, repeatCronVal, userID).Scan(&jobID)

	return jobID, err
}

// CancelJob cancels a scheduled job
func (s *Scheduler) CancelJob(jobID int) error {
	// Allow cancelling jobs that have not completed yet.
	result, err := s.db.Exec(`
		UPDATE scheduled_jobs
		SET status = 'cancelled', error_message = COALESCE(NULLIF(error_message, ''), 'Cancelled by user')
		WHERE id = $1 AND status IN ('pending', 'blocked', 'running')
	`, jobID)
	if err != nil {
		return err
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("job %d not found or not cancellable", jobID)
	}

	// Best-effort: if this job is running in this process, cancel its context.
	s.runMu.Lock()
	if cancel, ok := s.runningCancels[jobID]; ok {
		cancel()
	}
	s.runMu.Unlock()

	// Broadcast status update so UIs can refresh immediately.
	if s.wsHub != nil {
		s.wsHub.BroadcastJobUpdate(jobID, string(StatusCancelled), map[string]interface{}{
			"progress":          0,
			"total_devices":     0,
			"completed_devices": 0,
			"error":             "Cancelled",
		})
	}

	return nil
}

// UpdateJobSchedule updates a scheduled job's next scheduled time and/or repeat cron.
// Only jobs in pending/blocked status can be edited.
//
// Semantics:
// - scheduledAt (if provided) becomes the next run time.
// - repeatCron (if provided) replaces the existing repeat_cron (empty string clears it).
// - next_run is cleared so the scheduler uses the updated scheduled_at.
// - blocked jobs are returned to pending (editing overrides maintenance-block state).
func (s *Scheduler) UpdateJobSchedule(jobID int, scheduledAt *time.Time, repeatCron *string) (ScheduledJob, error) {
	// Load existing job fields so we can support partial updates.
	var cur ScheduledJob
	var deviceIDs pq.Int64Array
	var curRepeat sql.NullString
	var curStatus string
	var curNextRun sql.NullTime
	if err := s.db.QueryRow(`
		SELECT id, job_type, device_ids, parameters, scheduled_at, repeat_cron, status, created_by, created_at, next_run
		FROM scheduled_jobs
		WHERE id = $1
	`, jobID).Scan(
		&cur.ID, &cur.JobType, &deviceIDs, &cur.Parameters, &cur.ScheduledAt, &curRepeat, &curStatus, &cur.CreatedBy, &cur.CreatedAt, &curNextRun,
	); err != nil {
		return ScheduledJob{}, err
	}

	if curRepeat.Valid {
		cur.RepeatCron = curRepeat.String
	}
	cur.Status = JobStatus(curStatus)
	if curNextRun.Valid {
		cur.NextRun = &curNextRun.Time
	}
	cur.DeviceIDs = make([]int, len(deviceIDs))
	for i, id := range deviceIDs {
		cur.DeviceIDs[i] = int(id)
	}

	if cur.Status != StatusPending && cur.Status != StatusBlocked {
		return ScheduledJob{}, fmt.Errorf("job %d not editable (status=%s)", jobID, cur.Status)
	}

	newScheduled := cur.ScheduledAt
	if scheduledAt != nil {
		newScheduled = *scheduledAt
	}

	newRepeat := cur.RepeatCron
	if repeatCron != nil {
		newRepeat = *repeatCron
	}

	// Normalize repeat cron storage: empty => NULL
	var repeatVal interface{}
	if newRepeat != "" {
		repeatVal = newRepeat
	}

	// Clear next_run so the scheduler uses scheduled_at, and reset status to pending.
	_, err := s.db.Exec(`
		UPDATE scheduled_jobs
		SET scheduled_at = $1,
			repeat_cron = $2,
			next_run = NULL,
			status = 'pending',
			error_message = ''
		WHERE id = $3 AND status IN ('pending','blocked')
	`, newScheduled, repeatVal, jobID)
	if err != nil {
		return ScheduledJob{}, err
	}

	// Return updated job snapshot.
	cur.ScheduledAt = newScheduled
	cur.RepeatCron = newRepeat
	cur.NextRun = nil
	cur.Status = StatusPending
	cur.ErrorMessage = ""

	// Broadcast update so UIs refresh immediately.
	if s.wsHub != nil {
		s.wsHub.BroadcastJobUpdate(jobID, string(StatusPending), map[string]interface{}{
			"scheduled_at": cur.ScheduledAt,
			"next_run":     nil,
			"repeat_cron":  cur.RepeatCron,
			"status":       string(StatusPending),
		})
	}

	return cur, nil
}

// ListJobs returns scheduled jobs with progress info
func (s *Scheduler) ListJobs(status string, limit int) ([]ScheduledJob, error) {
	query := `
		SELECT id, job_type, device_ids, parameters, scheduled_at, 
		       repeat_cron, last_run, next_run, status, 
		       COALESCE(progress, 0), COALESCE(total_devices, 0), COALESCE(completed_devices, 0),
		       COALESCE(error_message, ''), created_by, created_at
		FROM scheduled_jobs
	`
	args := []interface{}{}

	if status != "" {
		query += " WHERE status = $1"
		args = append(args, status)
	}

	query += " ORDER BY COALESCE(next_run, scheduled_at) DESC LIMIT " + fmt.Sprintf("%d", limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []ScheduledJob
	for rows.Next() {
		var job ScheduledJob
		var deviceIDs pq.Int64Array
		var repeatCron sql.NullString
		var lastRun, nextRun sql.NullTime

		err := rows.Scan(
			&job.ID, &job.JobType, &deviceIDs, &job.Parameters,
			&job.ScheduledAt, &repeatCron, &lastRun, &nextRun,
			&job.Status, &job.Progress, &job.TotalDevices, &job.CompletedDevices,
			&job.ErrorMessage, &job.CreatedBy, &job.CreatedAt,
		)
		if err != nil {
			continue
		}

		if repeatCron.Valid {
			job.RepeatCron = repeatCron.String
		}
		if lastRun.Valid {
			job.LastRun = &lastRun.Time
		}
		if nextRun.Valid {
			job.NextRun = &nextRun.Time
		}

		// Convert pq.Int64Array to []int
		job.DeviceIDs = make([]int, len(deviceIDs))
		for i, id := range deviceIDs {
			job.DeviceIDs[i] = int(id)
		}

		jobs = append(jobs, job)
	}

	return jobs, nil
}

// MaintenanceWindow CRUD operations

// ListMaintenanceWindows returns all maintenance windows
func (s *Scheduler) ListMaintenanceWindows() ([]MaintenanceWindow, error) {
	rows, err := s.db.Query(`
		SELECT id, name, scope, region_id, site_id, day_of_week,
		       start_time::text, end_time::text, timezone, allow_jobs, enabled, created_at
		FROM maintenance_windows
		ORDER BY scope, name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var windows []MaintenanceWindow
	for rows.Next() {
		var mw MaintenanceWindow
		var regionID, siteID sql.NullInt64
		var daysOfWeek pq.Int64Array
		var allowJobs pq.StringArray

		err := rows.Scan(
			&mw.ID, &mw.Name, &mw.Scope, &regionID, &siteID, &daysOfWeek,
			&mw.StartTime, &mw.EndTime, &mw.Timezone, &allowJobs, &mw.Enabled, &mw.CreatedAt,
		)
		if err != nil {
			continue
		}

		if regionID.Valid {
			id := int(regionID.Int64)
			mw.RegionID = &id
		}
		if siteID.Valid {
			id := int(siteID.Int64)
			mw.SiteID = &id
		}

		mw.DayOfWeek = make([]int, len(daysOfWeek))
		for i, d := range daysOfWeek {
			mw.DayOfWeek[i] = int(d)
		}

		mw.AllowJobs = []string(allowJobs)
		windows = append(windows, mw)
	}

	return windows, nil
}

// CreateMaintenanceWindow creates a new maintenance window
func (s *Scheduler) CreateMaintenanceWindow(mw MaintenanceWindow, userID int) (int, error) {
	var id int
	var regionID, siteID interface{}
	if mw.RegionID != nil {
		regionID = *mw.RegionID
	}
	if mw.SiteID != nil {
		siteID = *mw.SiteID
	}

	err := s.db.QueryRow(`
		INSERT INTO maintenance_windows 
			(name, scope, region_id, site_id, day_of_week, start_time, end_time, timezone, allow_jobs, enabled, created_by)
		VALUES ($1, $2, $3, $4, $5, $6::time, $7::time, $8, $9, $10, $11)
		RETURNING id
	`, mw.Name, mw.Scope, regionID, siteID, pq.Array(mw.DayOfWeek),
		mw.StartTime, mw.EndTime, mw.Timezone, pq.Array(mw.AllowJobs), mw.Enabled, userID).Scan(&id)

	return id, err
}

// UpdateMaintenanceWindow updates an existing maintenance window
func (s *Scheduler) UpdateMaintenanceWindow(id int, mw MaintenanceWindow) error {
	var regionID, siteID interface{}
	if mw.RegionID != nil {
		regionID = *mw.RegionID
	}
	if mw.SiteID != nil {
		siteID = *mw.SiteID
	}

	_, err := s.db.Exec(`
		UPDATE maintenance_windows SET
			name = $1, scope = $2, region_id = $3, site_id = $4, day_of_week = $5,
			start_time = $6::time, end_time = $7::time, timezone = $8, allow_jobs = $9, 
			enabled = $10, updated_at = NOW()
		WHERE id = $11
	`, mw.Name, mw.Scope, regionID, siteID, pq.Array(mw.DayOfWeek),
		mw.StartTime, mw.EndTime, mw.Timezone, pq.Array(mw.AllowJobs), mw.Enabled, id)

	return err
}

// DeleteMaintenanceWindow deletes a maintenance window
func (s *Scheduler) DeleteMaintenanceWindow(id int) error {
	result, err := s.db.Exec(`DELETE FROM maintenance_windows WHERE id = $1`, id)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("maintenance window %d not found", id)
	}
	return nil
}

// GetConcurrencySettings returns current scheduler concurrency settings
func (s *Scheduler) GetConcurrencySettings() map[string]interface{} {
	return map[string]interface{}{
		"max_concurrent_jobs": s.maxConcurrentJobs,
		"check_interval_sec":  int(s.checkInterval.Seconds()),
		"respect_maintenance": s.respectMaintenance,
	}
}

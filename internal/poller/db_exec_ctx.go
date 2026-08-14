package poller

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// dbCtx carries enough information to log DB failures with device/host context.
type dbCtx struct {
	Op       string
	Host     string
	DeviceID int64
	MAC      string
}

// dbCtxForJob returns a DB context populated from a poll job.
func dbCtxForJob(job pollJob, op string) dbCtx {
	return dbCtx{Op: op, Host: job.IP, DeviceID: job.DeviceID, MAC: job.MAC}
}

// dbCtxForDevice returns a DB context when you only have device id (e.g., child updates).
func dbCtxForDevice(deviceID int64, op string) dbCtx {
	return dbCtx{Op: op, DeviceID: deviceID}
}

// dbCtxForMAC returns a DB context when the MAC is the primary identifier.
func dbCtxForMAC(mac, host, op string, deviceID int64) dbCtx {
	return dbCtx{Op: op, Host: host, DeviceID: deviceID, MAC: mac}
}

// dbCtxForOp returns a DB context for operations not tied to a single device.
func dbCtxForOp(op string) dbCtx {
	return dbCtx{Op: op}
}

func (c dbCtx) prefix() string {
	parts := make([]string, 0, 3)
	if c.Op != "" {
		parts = append(parts, "op="+c.Op)
	}
	if c.Host != "" {
		parts = append(parts, "host="+c.Host)
	}
	if c.DeviceID != 0 {
		parts = append(parts, fmt.Sprintf("device_id=%d", c.DeviceID))
	}
	if c.MAC != "" {
		parts = append(parts, "mac="+c.MAC)
	}
	return strings.Join(parts, " ")
}

const dbLogSuppressInterval = 5 * time.Minute

type dbErrState struct {
	lastLogged time.Time
	suppressed int
}

var (
	dbErrMu     sync.Mutex
	dbErrStates = map[string]*dbErrState{}
)

func shortKeyString(s string, max int) string {
	if max <= 0 || s == "" {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func dbErrorKey(ctx dbCtx, err error, query string) string {
	q := compactQuery(query)

	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return fmt.Sprintf("%s|%s|%d|%s|%s|%s|%s",
			ctx.Op, ctx.Host, ctx.DeviceID, ctx.MAC, string(pqErr.Code), pqErr.Constraint, q)
	}

	// Fallback key: type + shortened message + query
	return fmt.Sprintf("%s|%s|%d|%s|%T|%s|%s",
		ctx.Op, ctx.Host, ctx.DeviceID, ctx.MAC, err, shortKeyString(err.Error(), 120), q)
}

func redactIndexSet(idxs []int) map[int]struct{} {
	if len(idxs) == 0 {
		return nil
	}
	m := make(map[int]struct{}, len(idxs))
	for _, i := range idxs {
		m[i] = struct{}{}
	}
	return m
}

func formatDBArgs(args []any, redactIdx map[int]struct{}) string {
	if len(args) == 0 {
		return ""
	}

	parts := make([]string, 0, len(args))
	for i, a := range args {
		if redactIdx != nil {
			if _, ok := redactIdx[i]; ok {
				parts = append(parts, fmt.Sprintf("$%d=<redacted>", i+1))
				continue
			}
		}
		parts = append(parts, fmt.Sprintf("$%d=%s", i+1, formatDBArg(a)))
	}
	return strings.Join(parts, ", ")
}

func formatDBArg(a any) string {
	if a == nil {
		return "NULL"
	}

	// Avoid dumping huge values (e.g., arrays) by default.
	if _, ok := a.(driver.Valuer); ok {
		return fmt.Sprintf("<%T>", a)
	}

	switch v := a.(type) {
	case string:
		s := strings.ReplaceAll(v, "\n", "\\n")
		s = shortKeyString(s, 96)
		return fmt.Sprintf("%q", s)
	case []byte:
		return fmt.Sprintf("<bytes len=%d>", len(v))
	case time.Time:
		return v.Format(time.RFC3339)
	case []string:
		if len(v) == 0 {
			return "[]"
		}
		preview := make([]string, 0, 3)
		for i := 0; i < len(v) && i < 3; i++ {
			preview = append(preview, fmt.Sprintf("%q", shortKeyString(v[i], 24)))
		}
		if len(v) > 3 {
			return fmt.Sprintf("[len=%d %s ...]", len(v), strings.Join(preview, ", "))
		}
		return fmt.Sprintf("[%s]", strings.Join(preview, ", "))
	default:
		return fmt.Sprintf("%v", a)
	}
}

func logDBExecError(ctx dbCtx, err error, query string, args []any, redactIdx []int) {
	key := dbErrorKey(ctx, err, query)
	now := time.Now()

	var suppressed int
	shouldLog := false

	dbErrMu.Lock()
	st := dbErrStates[key]
	if st == nil {
		st = &dbErrState{lastLogged: now}
		dbErrStates[key] = st
		shouldLog = true
	} else if now.Sub(st.lastLogged) >= dbLogSuppressInterval {
		suppressed = st.suppressed
		st.suppressed = 0
		st.lastLogged = now
		shouldLog = true
	} else {
		st.suppressed++
	}
	dbErrMu.Unlock()

	if !shouldLog {
		return
	}

	prefix := ctx.prefix()
	if prefix != "" {
		prefix += " "
	}

	q := compactQuery(query)
	argStr := formatDBArgs(args, redactIndexSet(redactIdx))
	if argStr != "" {
		argStr = " args: " + argStr
	}

	if suppressed > 0 {
		log.Printf("DB exec error: %v (%ssuppressed=%d query: %s%s)", err, prefix, suppressed, q, argStr)
		return
	}
	log.Printf("DB exec error: %v (%squery: %s%s)", err, prefix, q, argStr)
}

// dbExecCtx executes a query and logs any error (with context + suppression).
func dbExecCtx(db *sql.DB, ctx dbCtx, query string, args ...any) (sql.Result, error) {
	result, err := db.Exec(query, args...)
	if err != nil {
		logDBExecError(ctx, err, query, args, nil)
	}
	return result, err
}

// dbExecIgnoreCtx executes a query and logs errors but doesn't return them (fire-and-forget).
func dbExecIgnoreCtx(db *sql.DB, ctx dbCtx, query string, args ...any) {
	if _, err := db.Exec(query, args...); err != nil {
		logDBExecError(ctx, err, query, args, nil)
	}
}

// dbExecCtxRedact is the same as dbExecCtx but redacts arg values at specified 0-based indexes.
func dbExecCtxRedact(db *sql.DB, ctx dbCtx, query string, redactIdx []int, args ...any) (sql.Result, error) {
	result, err := db.Exec(query, args...)
	if err != nil {
		logDBExecError(ctx, err, query, args, redactIdx)
	}
	return result, err
}

// dbExecIgnoreCtxRedact is the same as dbExecIgnoreCtx but redacts arg values at specified 0-based indexes.
func dbExecIgnoreCtxRedact(db *sql.DB, ctx dbCtx, query string, redactIdx []int, args ...any) {
	if _, err := db.Exec(query, args...); err != nil {
		logDBExecError(ctx, err, query, args, redactIdx)
	}
}

// dbQueryRowCtx scans a single row and logs any error (with context + suppression).
// It intentionally does NOT log sql.ErrNoRows, because that is often used for control flow.
func dbQueryRowCtx(db *sql.DB, ctx dbCtx, query string, dest []any, args ...any) error {
	err := db.QueryRow(query, args...).Scan(dest...)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		logDBExecError(ctx, err, query, args, nil)
	}
	return err
}

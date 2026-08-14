//go:build unix

package main

import (
	"io"
	"log"
	"log/syslog"
	"os"
)

// initLogging sets up logging based on debug mode.
// Configures both custom loggers and the default log package.
func initLogging() {
	if debugMode {
		// Debug mode: everything to stderr with timestamps
		errLog = log.New(os.Stderr, "", log.LstdFlags)
		debugLog = log.New(os.Stderr, "", log.LstdFlags)
		// Also configure default logger for internal packages
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
		log.SetPrefix("")
		return
	}

	// Daemon mode: route different severities to syslog.
	// Many installations rotate out LOG_INFO aggressively, so the default logger
	// (used by internal packages) is routed to LOG_WARNING.
	infoWriter, errInfo := syslog.New(syslog.LOG_INFO|syslog.LOG_DAEMON, "wavecontrol")
	warnWriter, errWarn := syslog.New(syslog.LOG_WARNING|syslog.LOG_DAEMON, "wavecontrol")
	errWriter, errErr := syslog.New(syslog.LOG_ERR|syslog.LOG_DAEMON, "wavecontrol")

	if errInfo != nil || errWarn != nil || errErr != nil {
		// Fall back to stderr if syslog unavailable
		errLog = log.New(os.Stderr, "wavecontrol: ", 0)
		debugLog = nil // No debug output in daemon mode
		log.SetOutput(os.Stderr)
		log.SetPrefix("wavecontrol: ")
		log.SetFlags(0)
		return
	}

	// Critical errors (LOG_ERR)
	errLog = log.New(errWriter, "", 0)
	debugLog = nil // No debug output in daemon mode

	// Default logger for internal packages (LOG_WARNING)
	log.SetOutput(warnWriter)
	log.SetPrefix("")
	log.SetFlags(0)

	// Keep the info writer for future use (avoid unused warning)
	_ = infoWriter
}

// discardWriter is used to suppress debug output in daemon mode
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

var _ io.Writer = discardWriter{}

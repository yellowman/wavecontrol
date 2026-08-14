//go:build windows

package main

import (
	"log"
	"os"
)

// initLogging sets up logging based on debug mode
// On Windows, we log to stderr in all modes since syslog isn't available
// For production Windows deployments, consider using Windows Event Log
func initLogging() {
	if debugMode {
		// Debug mode: everything to stderr with timestamps
		errLog = log.New(os.Stderr, "", log.LstdFlags)
		debugLog = log.New(os.Stderr, "", log.LstdFlags)
		// Also configure default logger for internal packages
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	} else {
		// Daemon mode on Windows: still log to stderr
		// When run as a Windows Service, stderr goes to the service log
		errLog = log.New(os.Stderr, "wavecontrol: ", log.LstdFlags)
		debugLog = nil // No debug output in daemon mode
		// Configure default logger for internal packages
		log.SetOutput(os.Stderr)
		log.SetPrefix("wavecontrol: ")
		log.SetFlags(log.LstdFlags)
	}
}

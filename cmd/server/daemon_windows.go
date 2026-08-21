//go:build windows

package main

// daemonize on Windows - no-op, runs in foreground
// Use Windows Service for production deployments
func daemonize() error {
	// Windows doesn't support Unix-style daemonization
	// Run as Windows Service for production, or -d for development
	return nil
}

// dropPrivileges on Windows is deliberately a no-op. Earlier versions changed
// to a user profile directory here, which made relative web/, firmware/, and
// backup paths disappear depending on which account launched the process.
// Working-directory selection is now explicit and handled in main before any
// runtime paths are opened.
func dropPrivileges(username string, unchrooted bool) error {
	return nil
}

// platformSecure on Windows - no-op
func platformSecure() error {
	// No pledge/unveil equivalent on Windows
	return nil
}

//go:build windows

package main

import (
	"fmt"
	"os"
	"os/user"
)

// daemonize on Windows - no-op, runs in foreground
// Use Windows Service for production deployments
func daemonize() error {
	// Windows doesn't support Unix-style daemonization
	// Run as Windows Service for production, or -d for development
	return nil
}

// dropPrivileges on Windows - just chdir to working directory
func dropPrivileges(username string, unchrooted bool) error {
	// Windows doesn't support chroot or privilege dropping like Unix
	// But we can at least chdir to a sensible working directory

	// If username specified, try to find their home directory
	if username != "" {
		if u, err := user.Lookup(username); err == nil && u.HomeDir != "" {
			if err := os.Chdir(u.HomeDir); err != nil {
				return fmt.Errorf("chdir %s: %w", u.HomeDir, err)
			}
			return nil
		}
	}

	// Otherwise try current user's home
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		// Use a wavecontrol subdirectory if it exists
		wcDir := u.HomeDir + "\\wavecontrol"
		if info, err := os.Stat(wcDir); err == nil && info.IsDir() {
			os.Chdir(wcDir)
			return nil
		}
		// Fall back to home directory
		os.Chdir(u.HomeDir)
	}

	return nil
}

// platformSecure on Windows - no-op
func platformSecure() error {
	// No pledge/unveil equivalent on Windows
	return nil
}

//go:build openbsd

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// daemonize forks to background if not already a daemon child
func daemonize() error {
	// Check if we're already the daemon child
	if os.Getenv("WAVECONTROL_DAEMON") == "1" {
		// We are the child, detach from terminal
		syscall.Setsid()
		return nil
	}

	// Fork child process
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = append(os.Environ(), "WAVECONTROL_DAEMON=1")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("fork: %w", err)
	}

	// Parent exits
	os.Exit(0)
	return nil
}

// dropPrivileges drops root privileges to specified user or auto-detected user
// Default behavior: chroot to user's home directory
// If unchrooted is true, just chdir without chroot
func dropPrivileges(username string, unchrooted bool) error {
	// Only drop privileges if running as root
	if os.Getuid() != 0 {
		return nil
	}

	// Find target user
	var targetUser *user.User
	var err error

	if username != "" {
		targetUser, err = user.Lookup(username)
		if err != nil {
			return fmt.Errorf("user %s not found: %w", username, err)
		}
	} else {
		// Try users in order: _wavecontrol, www, nobody
		for _, name := range []string{"_wavecontrol", "www", "nobody"} {
			targetUser, err = user.Lookup(name)
			if err == nil {
				break
			}
		}
		if targetUser == nil {
			return fmt.Errorf("no suitable user found (_wavecontrol, www, nobody)")
		}
	}

	uid, err := strconv.Atoi(targetUser.Uid)
	if err != nil {
		return fmt.Errorf("parse uid: %w", err)
	}
	gid, err := strconv.Atoi(targetUser.Gid)
	if err != nil {
		return fmt.Errorf("parse gid: %w", err)
	}

	// Try to get www group for chgrp
	wwwGid := gid
	if wwwGroup, err := user.LookupGroup("www"); err == nil {
		if g, err := strconv.Atoi(wwwGroup.Gid); err == nil {
			wwwGid = g
		}
	}

	// Chroot to user's home directory by default
	// Use -U flag to skip chroot and just chdir
	if targetUser.HomeDir == "" {
		return fmt.Errorf("user %s has no home directory", targetUser.Username)
	}

	if unchrooted {
		// Just chdir, no chroot
		if err := syscall.Chdir(targetUser.HomeDir); err != nil {
			return fmt.Errorf("chdir %s: %w", targetUser.HomeDir, err)
		}
	} else {
		// Chroot to user's home directory
		if err := syscall.Chroot(targetUser.HomeDir); err != nil {
			return fmt.Errorf("chroot %s: %w", targetUser.HomeDir, err)
		}
		if err := syscall.Chdir("/"); err != nil {
			return fmt.Errorf("chdir /: %w", err)
		}
	}

	// Set supplementary groups
	if err := syscall.Setgroups([]int{gid, wwwGid}); err != nil {
		// Not fatal, continue
	}

	// Set GID first (must be done before setuid)
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid %d: %w", gid, err)
	}

	// Set UID
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid %d: %w", uid, err)
	}

	return nil
}

// platformSecure applies OpenBSD-specific security measures (pledge)
func platformSecure() error {
	// Pledge promises:
	// - stdio: standard I/O
	// - rpath: read files (config, web assets)
	// - wpath: write files (logs, pid)
	// - cpath: create files
	// - inet: network sockets
	// - dns: DNS resolution
	// - proc: fork (for daemonization, already done)
	// - getpw: user lookup (already done)
	//
	// After initialization, we can drop to minimal set
	promises := "stdio rpath wpath cpath inet dns"

	if err := unix.Pledge(promises, ""); err != nil {
		return fmt.Errorf("pledge: %w", err)
	}

	return nil
}

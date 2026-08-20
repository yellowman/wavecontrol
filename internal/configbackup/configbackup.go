// Package configbackup centralizes secure filesystem handling for device
// configuration backups.
package configbackup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	directoryMode = 0o700
	fileMode      = 0o600
	maxReadSize   = 64 << 20 // Device configurations should be far smaller than 64 MiB.
)

// EnsureRoot creates the configured backup root and returns its canonical path.
// Existing directories are tightened to owner-only permissions.
func EnsureRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("backup root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve backup root: %w", err)
	}
	if err := os.MkdirAll(abs, directoryMode); err != nil {
		return "", fmt.Errorf("create backup root: %w", err)
	}
	if err := os.Chmod(abs, directoryMode); err != nil {
		return "", fmt.Errorf("secure backup root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize backup root: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("stat backup root: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("backup root is not a directory")
	}
	return real, nil
}

// EnsureDir creates an owner-only directory below root. Every relative part
// must be a single path component.
func EnsureDir(root string, parts ...string) (string, error) {
	realRoot, err := EnsureRoot(root)
	if err != nil {
		return "", err
	}
	cleanParts := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "." || part == ".." || filepath.Base(part) != part || strings.ContainsAny(part, `/\\`) {
			return "", fmt.Errorf("invalid backup path component %q", part)
		}
		cleanParts = append(cleanParts, part)
	}
	dir := filepath.Join(append([]string{realRoot}, cleanParts...)...)
	if !within(realRoot, dir) {
		return "", errors.New("backup directory escapes root")
	}
	if err := os.MkdirAll(dir, directoryMode); err != nil {
		return "", fmt.Errorf("create backup directory: %w", err)
	}
	// MkdirAll preserves permissive modes on existing directories. Tighten each
	// application-controlled directory in the requested path.
	current := realRoot
	for _, part := range cleanParts {
		current = filepath.Join(current, part)
		if err := os.Chmod(current, directoryMode); err != nil {
			return "", fmt.Errorf("secure backup directory: %w", err)
		}
	}
	return dir, nil
}

// Write creates a collision-resistant backup file without following or
// overwriting an existing path.
func Write(dir, baseName string, data []byte) (string, error) {
	baseName = SafeName(baseName)
	if baseName == "" {
		baseName = "device"
	}
	for attempt := 0; attempt < 10; attempt++ {
		stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
		name := fmt.Sprintf("%s_%s.cfg", baseName, stamp)
		if attempt > 0 {
			name = fmt.Sprintf("%s_%s_%d.cfg", baseName, stamp, attempt)
		}
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create backup: %w", err)
		}
		writeErr := writeAllAndSync(f, data)
		closeErr := f.Close()
		if writeErr != nil {
			_ = os.Remove(path)
			return "", writeErr
		}
		if closeErr != nil {
			_ = os.Remove(path)
			return "", fmt.Errorf("close backup: %w", closeErr)
		}
		return path, nil
	}
	return "", errors.New("could not allocate a unique backup filename")
}

func writeAllAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync backup: %w", err)
	}
	return nil
}

// ResolveExisting validates and canonicalizes an existing regular .cfg file
// below root. Symlinks are allowed only when their final target remains within
// the configured root.
func ResolveExisting(root, requested string) (string, os.FileInfo, error) {
	realRoot, err := EnsureRoot(root)
	if err != nil {
		return "", nil, err
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", nil, errors.New("backup path is empty")
	}
	var abs string
	if filepath.IsAbs(requested) {
		abs = filepath.Clean(requested)
	} else {
		// API responses expose paths relative to the configured backup root so
		// callers do not need, or learn, the server's absolute filesystem path.
		abs = filepath.Join(realRoot, filepath.Clean(requested))
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, fmt.Errorf("resolve backup path: %w", err)
	}
	if !within(realRoot, real) {
		return "", nil, errors.New("backup path escapes configured root")
	}
	if !strings.EqualFold(filepath.Ext(real), ".cfg") {
		return "", nil, errors.New("backup file must have .cfg extension")
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", nil, fmt.Errorf("stat backup: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, errors.New("backup path is not a regular file")
	}
	return real, info, nil
}

// ReadExisting resolves and reads an existing backup with a defensive size
// limit so a local filesystem mistake cannot make the server allocate without
// bound.
func ReadExisting(root, requested string) ([]byte, string, os.FileInfo, error) {
	real, info, err := ResolveExisting(root, requested)
	if err != nil {
		return nil, "", nil, err
	}
	if info.Size() > maxReadSize {
		return nil, "", nil, fmt.Errorf("backup exceeds %d-byte limit", maxReadSize)
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, "", nil, fmt.Errorf("open backup: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxReadSize+1))
	if err != nil {
		return nil, "", nil, fmt.Errorf("read backup: %w", err)
	}
	if len(data) > maxReadSize {
		return nil, "", nil, fmt.Errorf("backup exceeds %d-byte limit", maxReadSize)
	}
	return data, real, info, nil
}

// SafeName converts a hostname or other label into a conservative filename
// component. It intentionally permits only portable characters.
func SafeName(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	b.Grow(len(value))
	lastDash := false
	for _, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), ".-_ ")
}

func within(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

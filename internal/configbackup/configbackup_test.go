package configbackup

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteUsesPrivateModesAndUniqueNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backups")
	dir, err := EnsureDir(root, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	first, err := Write(dir, "radio/name", []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Write(dir, "radio/name", []byte("two"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("expected unique filenames")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(first)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("file mode = %o, want 600", got)
		}
		info, err = os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("directory mode = %o, want 700", got)
		}
	}
}

func TestResolveExistingRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges vary on Windows")
	}
	base := t.TempDir()
	root := filepath.Join(base, "backups")
	if _, err := EnsureRoot(root); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.cfg")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape.cfg")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveExisting(root, link); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected escape rejection, got %v", err)
	}
}

func TestResolveExistingAllowsFileInsideRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backups")
	dir, err := EnsureDir(root, "ap")
	if err != nil {
		t.Fatal(err)
	}
	path, err := Write(dir, "radio", []byte("config"))
	if err != nil {
		t.Fatal(err)
	}
	data, resolved, _, err := ReadExisting(root, path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "config" || resolved == "" {
		t.Fatalf("unexpected result: %q %q", data, resolved)
	}
}

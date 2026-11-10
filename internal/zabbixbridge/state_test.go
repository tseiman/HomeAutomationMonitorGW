package zabbixbridge

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCursorStoreAtomicPrivateAndSymlinkSafe(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "cursor")
	s, err := openCursorStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Save("v1.AAAAAAAAAAAAAAAAAAAAAA.1"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("mode=%v", info.Mode())
	}
	got, err := s.Load()
	if err != nil || got != "v1.AAAAAAAAAAAAAAAAAAAAAA.1" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(d, "target")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("v1.AAAAAAAAAAAAAAAAAAAAAA.2"); err == nil {
		t.Fatal("replaced symlink")
	}
	body, _ := os.ReadFile(target)
	if string(body) != "old" {
		t.Fatalf("target changed: %q", body)
	}
}

func TestCursorStoreRetainsOldCursorWhenPreRenameFails(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "cursor")
	s, err := openCursorStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Save("old"); err != nil {
		t.Fatal(err)
	}
	s.beforeRename = func() error { return os.ErrPermission }
	if err := s.Save("new"); err == nil {
		t.Fatal("save succeeded")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "old\n" {
		t.Fatalf("body=%q err=%v", body, err)
	}
}

func TestCursorStoreRejectsUnsafeParentAndConcurrentOwner(t *testing.T) {
	d := t.TempDir()
	if err := os.Chmod(d, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := openCursorStore(filepath.Join(d, "cursor")); err == nil {
		t.Fatal("accepted world-writable state parent")
	}
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := openCursorStore(filepath.Join(d, "cursor"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := openCursorStore(filepath.Join(d, "cursor")); err == nil {
		t.Fatal("second owner acquired lock")
	}
}

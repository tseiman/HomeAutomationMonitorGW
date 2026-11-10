package zabbixbridge

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type cursorStore struct {
	path         string
	dir          *os.File
	lock         *os.File
	beforeRename func() error
}

func checkCursorPath(path string) error {
	return validateCursorPath(path, true)
}

func validateCursorPath(path string, allowMissingParent bool) error {
	dirPath := filepath.Dir(path)
	info, err := os.Lstat(dirPath)
	if allowMissingParent && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("unsafe state parent")
	}
	if target, err := os.Lstat(path); err == nil {
		targetStat, statOK := target.Sys().(*syscall.Stat_t)
		if !target.Mode().IsRegular() || target.Mode().Perm()&0o077 != 0 || !statOK || targetStat.Nlink != 1 || targetStat.Uid != uint32(os.Geteuid()) {
			return fmt.Errorf("unsafe cursor target")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func openCursorStore(path string) (*cursorStore, error) {
	dirPath := filepath.Dir(path)
	if err := validateCursorPath(path, false); err != nil {
		return nil, err
	}
	dir, err := os.Open(dirPath)
	if err != nil {
		return nil, err
	}
	lockPath := path + ".lock"
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		dir.Close()
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), lockPath)
	li, err := lock.Stat()
	if err != nil {
		lock.Close()
		dir.Close()
		return nil, fmt.Errorf("inspect lock file: %w", err)
	}
	lockStat, lockStatOK := li.Sys().(*syscall.Stat_t)
	if !li.Mode().IsRegular() || li.Mode().Perm()&0o077 != 0 || !lockStatOK || lockStat.Nlink != 1 || lockStat.Uid != uint32(os.Geteuid()) {
		lock.Close()
		dir.Close()
		return nil, fmt.Errorf("unsafe lock file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		dir.Close()
		return nil, fmt.Errorf("cursor state is locked: %w", err)
	}
	return &cursorStore{path: path, dir: dir, lock: lock}, nil
}
func (s *cursorStore) Close() error {
	var errs []error
	if s.lock != nil {
		errs = append(errs, s.lock.Close())
	}
	if s.dir != nil {
		errs = append(errs, s.dir.Close())
	}
	return errors.Join(errs...)
}
func (s *cursorStore) inspectTarget() error {
	info, err := os.Lstat(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ok || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("unsafe cursor target")
	}
	return nil
}
func (s *cursorStore) Load() (string, error) {
	if err := s.inspectTarget(); err != nil {
		return "", err
	}
	fd, err := unix.Open(s.path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), s.path)
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	if len(b) > 4096 {
		return "", fmt.Errorf("cursor file too large")
	}
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return string(b), nil
}
func (s *cursorStore) Save(cursor string) error {
	if err := s.inspectTarget(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".cursor-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(cursor + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if s.beforeRename != nil {
		if err := s.beforeRename(); err != nil {
			return err
		}
	}
	if err := s.inspectTarget(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return err
	}
	committed = true
	if err := s.dir.Sync(); err != nil {
		return fmt.Errorf("cursor committed but directory fsync failed: %w", err)
	}
	return nil
}

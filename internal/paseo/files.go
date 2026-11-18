package paseo

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Error is a refusal with a message meant for the user, the port of
// paseo_handoff.HandoffError.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func refuse(msg string) error { return &Error{Msg: msg} }

func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// privateDir creates path (0700) and checks that it is a real directory the
// current user owns with no group or other access.
func privateDir(path string) (string, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || !ownedByMe(info) || info.Mode().Perm()&0o077 != 0 {
		return "", refuse("Watcher directory must be an owned private directory")
	}
	return path, nil
}

// readObject reads a JSON object file, keeping key order and number literals.
func readObject(path string) (*Object, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	o, err := DecodeObject(data)
	if errors.Is(err, errExpectedObject) {
		return nil, refuse("Expected a JSON object")
	}
	return o, err
}

// atomicWrite replaces path with value as json.dump(value, indent=2) plus a
// newline: a 0600 temporary file in the same directory, fsync, rename. An
// existing path must be a regular file the current user owns.
func atomicWrite(path string, value any) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || !ownedByMe(info) {
			return refuse("Refusing unsafe registration file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	text, err := DumpIndent(value, 2)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name) // a no-op after the rename
	if _, err := f.WriteString(text + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// lockFile takes flock(LOCK_EX) on path (created 0600, never through a
// symlink) and returns the release function. The old Python workers take the
// same lock, so the two never tick one registration at once.
func lockFile(path string) (func(), error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || int(st.Uid) != os.Getuid() {
		syscall.Close(fd)
		return nil, refuse("Refusing unsafe lock file")
	}
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return func() { syscall.Close(fd) }, nil
}

func lockPath(path string) string { return withSuffix(path, ".lock") }

// ReadObject reads a registration file, keeping key order and numbers.
func ReadObject(path string) (*Object, error) { return readObject(path) }

// AtomicWrite replaces a registration file (0600, fsync, rename).
func AtomicWrite(path string, value any) error { return atomicWrite(path, value) }

// LockFile takes the registration lock at path, waiting for it.
func LockFile(path string) (func(), error) { return lockFile(path) }

// LockPath is the lock file of a registration file.
func LockPath(path string) string { return lockPath(path) }

// TryLockFile takes the lock at path without waiting: ok is false when
// someone else holds it.
func TryLockFile(path string) (unlock func(), ok bool, err error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, false, err
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return nil, false, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || int(st.Uid) != os.Getuid() {
		syscall.Close(fd)
		return nil, false, refuse("Refusing unsafe lock file")
	}
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err != syscall.EINTR {
			break
		}
	}
	if err == syscall.EWOULDBLOCK {
		syscall.Close(fd)
		return nil, false, nil
	}
	if err != nil {
		syscall.Close(fd)
		return nil, false, err
	}
	return func() { syscall.Close(fd) }, true, nil
}

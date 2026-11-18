//go:build unix

package service

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// ErrAlreadyRunning means another agent holds the lock.
var ErrAlreadyRunning = errors.New("service: agent is already running")

// Lock is a single-instance lock: an exclusive flock on a file that also holds the PID.
type Lock struct{ f *os.File }

// AcquireLock takes the lock or returns ErrAlreadyRunning.
func AcquireLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, errors.New("service: open lock file")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, errors.New("service: lock file")
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock; closing the file also releases it if the process dies.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
		l.f.Close()
	}
}

// RunningPID reports whether an agent holds the lock and its PID.
func RunningPID(path string) (pid int, running bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return 0, false
	}
	buf := make([]byte, 32)
	n, _ := f.Read(buf)
	pid, err = strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil || pid <= 0 {
		return 0, true
	}
	return pid, true
}

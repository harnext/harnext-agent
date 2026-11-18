//go:build windows

package service

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// ErrAlreadyRunning means another agent holds the lock.
var ErrAlreadyRunning = errors.New("service: agent is already running")

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errLockViolation        = syscall.Errno(33)
	errIOPending            = syscall.Errno(997)
	// The lock covers one byte far beyond the PID text: Windows locks are mandatory, so locking
	// byte 0 would stop RunningPID from reading the PID.
	lockOffset = 0x40000000
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

func lockByte(f *os.File, flags uintptr) error {
	ol := syscall.Overlapped{Offset: lockOffset}
	r, _, e := procLockFileEx.Call(f.Fd(), flags, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		return e
	}
	return nil
}

func unlockByte(f *os.File) {
	ol := syscall.Overlapped{Offset: lockOffset}
	_, _, _ = procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
}

func isContended(err error) bool {
	return errors.Is(err, errLockViolation) || errors.Is(err, errIOPending)
}

// Lock is a single-instance lock: an exclusive LockFileEx byte lock on a file that also holds the PID.
type Lock struct{ f *os.File }

// AcquireLock takes the lock or returns ErrAlreadyRunning.
func AcquireLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, errors.New("service: open lock file")
	}
	if err := lockByte(f, lockfileExclusiveLock|lockfileFailImmediately); err != nil {
		f.Close()
		if isContended(err) {
			return nil, ErrAlreadyRunning
		}
		return nil, errors.New("service: lock file")
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock; the OS also releases it if the process dies.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		unlockByte(l.f)
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
	// A shared probe of the lock byte fails only while the exclusive holder lives.
	err = lockByte(f, lockfileFailImmediately)
	if err == nil {
		unlockByte(f)
		return 0, false
	}
	if !isContended(err) {
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

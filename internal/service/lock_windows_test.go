//go:build windows

package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLockWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")
	if _, running := RunningPID(path); running {
		t.Fatal("running before lock")
	}
	l, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(path); err != ErrAlreadyRunning {
		t.Fatalf("second acquire = %v", err)
	}
	if pid, running := RunningPID(path); !running || pid != os.Getpid() {
		t.Fatalf("RunningPID = %d, %v", pid, running)
	}
	l.Release()
	if _, running := RunningPID(path); running {
		t.Fatal("running after release")
	}
	l2, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	l2.Release()
}

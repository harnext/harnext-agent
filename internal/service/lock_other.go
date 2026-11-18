//go:build !unix && !windows

package service

import "errors"

// ErrAlreadyRunning is never returned on this platform (no locking; supported on unix and windows only).
var ErrAlreadyRunning = errors.New("service: agent is already running")

type Lock struct{}

func AcquireLock(string) (*Lock, error) { return &Lock{}, nil }

func (*Lock) Release() {}

func RunningPID(string) (int, bool) { return 0, false }

// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

//go:build !darwin && !linux

package ptyrelay

import (
	"errors"
	"os"
	"os/exec"
)

// OpenPty is not supported on this platform.
func OpenPty() (master, slave *os.File, err error) {
	return nil, nil, errors.New("ptyrelay: unsupported platform")
}

// SameTerminal is not supported on this platform.
func SameTerminal(a, b *os.File) bool { return false }

// Run is not supported on this platform.
func Run(cmd *exec.Cmd, opt Options) (Result, error) {
	return Result{}, errors.New("ptyrelay: unsupported platform")
}

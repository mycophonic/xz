// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin || dragonfly || freebsd || (linux && !appengine) || netbsd || openbsd || illumos
// +build darwin dragonfly freebsd linux,!appengine netbsd openbsd illumos

// Package term provides the IsTerminal function.
package term

import "golang.org/x/sys/unix"

// IsTerminal returns true if the given file descriptor is a terminal.
func IsTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), ioctlGetTermios)
	return err == nil
}

// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build illumos
// +build illumos

package term

import "golang.org/x/sys/unix"

const ioctlGetTermios = unix.TCGETS

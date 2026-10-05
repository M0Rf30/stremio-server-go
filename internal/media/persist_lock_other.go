// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package media

import "io"

// persistLockSupported reports whether lockPersistDir actually excludes
// other processes on this platform.
const persistLockSupported = false

// lockPersistDir is a no-op on platforms without flock or a Windows-style
// exclusive open (e.g. Solaris, AIX; none of them are release targets):
// running two servers on one persist dir there is unsupported.
func lockPersistDir(string) (io.Closer, error) { return noLock{}, nil }

type noLock struct{}

func (noLock) Close() error { return nil }

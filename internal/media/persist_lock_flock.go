// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package media

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// persistLockSupported reports whether lockPersistDir actually excludes
// other processes on this platform.
const persistLockSupported = true

// lockPersistDir takes a non-blocking exclusive flock on path (created 0600
// if missing) so two server processes can't share one persist dir: the
// second one's loadPersisted would otherwise delete the first one's
// in-progress *.tmp* files and sessions. The lock lives as long as the
// returned file is open (CloseHLS closes it) and is released by the kernel
// if the process dies. flock locks are per open file description, so a
// second open in the same process conflicts too.
func lockPersistDir(path string) (io.Closer, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errPersistLocked
		}
		return nil, err
	}
	return f, nil
}

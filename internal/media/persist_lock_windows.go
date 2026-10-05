// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

//go:build windows

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

// errorSharingViolation is ERROR_SHARING_VIOLATION, which package syscall
// does not name.
const errorSharingViolation syscall.Errno = 32

// lockPersistDir opens path (created if missing) with share mode 0, i.e. no
// other handle to it may be opened until this one is closed. That is
// Windows' native exclusive-open lock: a second server process (or a second
// open in this one) gets ERROR_SHARING_VIOLATION. The lock lives as long as
// the returned file is open (CloseHLS closes it) and is released by the OS
// if the process dies. os.OpenFile can't do this because it always opens
// with FILE_SHARE_READ|FILE_SHARE_WRITE.
func lockPersistDir(path string) (io.Closer, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // no sharing: this is the lock
		nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errorSharingViolation) {
			return nil, errPersistLocked
		}
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

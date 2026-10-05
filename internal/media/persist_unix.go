// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

//go:build unix

package media

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// securePersistDir enforces MED-2's intent for the predictable persist
// directory (fi is its Lstat result, already known to be a real directory):
// it must be owned by this process's effective uid, so a co-resident user
// can't have pre-created it, and it is then tightened to 0700 so no other
// user can list session ids or read segments in it.
func securePersistDir(dir string, fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine owner")
	}
	if uid := os.Geteuid(); uint64(st.Uid) != uint64(uid) {
		return fmt.Errorf("owned by uid %d, not by this process (uid %d)", st.Uid, uid)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod 0700: %w", err)
		}
	}
	return nil
}

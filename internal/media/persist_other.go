// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

//go:build !unix

package media

import "io/fs"

// securePersistDir is a no-op off POSIX. On Windows, fs.FileMode permission
// bits don't reflect the NTFS ACLs that actually govern access (a directory
// always reports 0777), and there is no uid to compare. The persist
// directory inherits WORK_DIR's ACLs, which is why the README asks for a
// dedicated WORK_DIR.
func securePersistDir(string, fs.FileInfo) error { return nil }

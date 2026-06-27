//go:build 386 || arm || mips || mipsle

package archive

// On 32-bit platforms the 7zip decompressor dependency does not compile (it
// uses constants that overflow a 32-bit int), so 7zip archives are reported as
// unsupported rather than breaking the build. All other formats remain
// available on these platforms.

import "errors"

func openSevenZip(_ string) (Reader, error) {
	return nil, errors.New("archive: 7zip is not supported on 32-bit builds")
}

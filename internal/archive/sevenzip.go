//go:build !386 && !arm && !mips && !mipsle

package archive

// 7zip support is built only on 64-bit platforms. The decompressor dependency
// chain contains 32-bit-int-overflowing constants that do not compile where
// int is 32 bits (e.g. linux/arm, linux/386); those platforms use the stub in
// sevenzip_stub.go instead.

import (
	"fmt"
	"io"

	"github.com/bodgit/sevenzip"
)

type szReader struct {
	rc    *sevenzip.ReadCloser
	index map[string]*sevenzip.File // normName → file; built once in openSevenZip for O(1) Open
}

func openSevenZip(fpath string) (Reader, error) {
	rc, err := sevenzip.OpenReader(fpath)
	if err != nil {
		return nil, err
	}
	// Build index once; first-wins matches the previous linear-scan behaviour.
	idx := make(map[string]*sevenzip.File, len(rc.File))
	for _, f := range rc.File {
		if n := normName(f.Name); n != "" {
			if _, ok := idx[n]; !ok {
				idx[n] = f
			}
		}
	}
	return &szReader{rc: rc, index: idx}, nil
}

func (r *szReader) List() ([]Entry, error) {
	out := make([]Entry, 0, len(r.rc.File))
	for _, f := range r.rc.File {
		fi := f.FileInfo()
		out = append(out, Entry{
			Name:  normName(f.Name),
			Size:  fi.Size(),
			IsDir: fi.IsDir(),
		})
	}
	return out, nil
}

func (r *szReader) Open(name string) (io.ReadCloser, error) {
	// O(1) index lookup built during openSevenZip; avoids O(N×normName) per call.
	if f, ok := r.index[name]; ok {
		return f.Open()
	}
	return nil, fmt.Errorf("archive: %q not found in 7zip archive", name)
}

func (r *szReader) Close() error { return r.rc.Close() }

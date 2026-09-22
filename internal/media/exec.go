package media

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
)

// ffprobeOutputLimit is the maximum ffprobe stdout this process will buffer
// before treating a still-growing process as hostile and killing it. ffprobe
// JSON output for even a pathological multi-thousand-stream file is a few
// hundred KB; 8 MiB is a generous ceiling that still forecloses an
// unbounded-memory DoS from a crafted or malicious input (MED-1).
const ffprobeOutputLimit = 8 << 20 // 8 MiB

// errOutputTooLarge is returned by runCapped when a subprocess's stdout
// exceeds the configured limit.
var errOutputTooLarge = errors.New("media: subprocess output exceeded size limit")

// runCapped runs cmd and returns its stdout, mirroring exec.Cmd.Output()
// (stderr is attached to the returned *exec.ExitError on a non-zero exit)
// but enforcing a hard cap of limit bytes instead of buffering an unbounded
// amount. If the subprocess writes more than limit bytes to stdout, it is
// killed immediately and errOutputTooLarge is returned.
//
// cmd.Stdout and cmd.Stderr must be unset when this is called.
func runCapped(cmd *exec.Cmd, limit int64) ([]byte, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// Read up to limit+1 bytes: if we actually get limit+1, the subprocess
	// exceeded the cap and is killed immediately rather than left to block on
	// a full pipe until its own timeout.
	data, readErr := io.ReadAll(io.LimitReader(stdout, limit+1))
	overflow := int64(len(data)) > limit
	if overflow {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()

	if overflow {
		return nil, errOutputTooLarge
	}
	if readErr != nil {
		return nil, readErr
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			ee.Stderr = stderr.Bytes()
		}
		return nil, waitErr
	}
	return data, nil
}

//go:build darwin

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package fifo

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openFifoFile opens fn for the fifo data path (flag already has
// O_CREAT/O_NONBLOCK stripped by openFifo; nonblockRequested reports
// whether the caller passed O_NONBLOCK).
//
// Darwin's os.OpenFile keeps FIFOs off the kqueue-based runtime poller,
// since kqueue can miss a last-writer-close event and hang a Read waiting
// for EOF (golang.org/issue/24164). The cost: Close can't interrupt an
// in-progress Read/Write on an unregistered fd either - it's a bare
// close(2) that a concurrently blocked read(2)/write(2) never observes.
//
// O_RDWR fifos have no EOF to miss: holding our own fd open for both
// directions guarantees a writer always exists. So for that mode only,
// it's safe to reopen with O_NONBLOCK and hand the fd to os.NewFile
// instead of os.OpenFile - os.OpenFile's FIFO exclusion only applies to
// descriptors it opens itself, so this registers the fd with the poller
// anyway, letting Close interrupt it like on Linux. O_RDONLY/O_WRONLY
// keep going through os.OpenFile unchanged.
func openFifoFile(fn string, flag int, nonblockRequested bool) (*os.File, error) {
	if !nonblockRequested || flag&syscall.O_RDWR == 0 {
		return os.OpenFile(fn, flag, 0)
	}

	// Re-add O_NONBLOCK (stripped by openFifo): it's what makes this
	// fd pollable once os.NewFile wraps it below.
	for {
		fd, err := unix.Open(fn, flag|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: fn, Err: err}
		}
		return os.NewFile(uintptr(fd), fn), nil
	}
}

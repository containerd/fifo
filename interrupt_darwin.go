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
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

// interruptRead wakes a Read blocked in read(2) on an O_RDWR fifo, which
// Close can't otherwise interrupt: os.OpenFile keeps darwin fifos off the
// kqueue poller, since XNU only notifies a fifo's knotes when read(2) or
// write(2) returns (golang.org/issue/24164), so os.File.Close defers close(2)
// until the read returns. It marks the fifo as closing for Read, then writes
// one byte to the fifo through the fifo's own fd, made nonblocking so the
// write can't block on a full buffer. A full buffer means the reader isn't
// blocked, and its next Read sees readClosing.
//
// The byte goes into the fifo: another process reading the same fifo may
// receive it.
//
// O_RDONLY fifos are left alone: they get EOF when the last writer closes.
func interruptRead(f *fifo) {
	if f.flag&syscall.O_RDWR == 0 {
		return
	}
	atomic.StoreInt32(&f.readClosing, 1)

	// SyscallConn, not Fd: Fd puts the file back in blocking mode.
	rc, err := f.file.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		if unix.SetNonblock(int(fd), true) != nil {
			return
		}
		_, _ = unix.Write(int(fd), []byte{0})
	})
}

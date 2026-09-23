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
	"io"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	spliceMax   int64 = 1 << 62
	spliceToEOF int64 = -1
)

var (
	spliceSupported int32 = 1
	bufPool               = &sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 1<<20)
			return &buf
		},
	}
)

func (f *fifo) ReadFrom(r io.Reader) (int64, error) {
	if f.flag&(syscall.O_WRONLY|syscall.O_RDWR) == 0 {
		return 0, ErrWrToRDONLY
	}
	select {
	case <-f.opened:
		return f.readFrom(r)
	default:
	}
	select {
	case <-f.opened:
		return f.readFrom(r)
	case <-f.closed:
		return 0, ErrWriteClosed
	}
}

func (f *fifo) readFrom(r io.Reader) (int64, error) {
	if atomic.LoadInt32(&spliceSupported) != 1 {
		return copyBuffer(f.file, r)
	}

	remain := spliceToEOF
	lr, ok := r.(*io.LimitedReader)
	if ok {
		remain, r = lr.N, lr.R
		if remain <= 0 {
			return 0, nil
		}
	}

	rscI, ok := r.(syscall.Conn)
	if !ok {
		return copyBuffer(f.file, r)
	}

	rsc, err := rscI.SyscallConn()
	if err != nil {
		if lr != nil {
			r = lr
		}
		return copyBuffer(f.file, r)
	}

	wsc, err := f.SyscallConn()
	if err != nil {
		if lr != nil {
			r = lr
		}
		return copyBuffer(f.file, r)
	}

	handled, written, err := doRawCopy(rsc, wsc, remain)
	if err != nil {
		return written, err
	}
	if !handled {
		if lr != nil {
			r = lr
		}
		return copyBuffer(f.file, r)
	}

	return written, nil
}

func (f *fifo) WriteTo(w io.Writer) (int64, error) {
	if f.flag&syscall.O_WRONLY > 0 {
		return 0, ErrRdFrmWRONLY
	}
	select {
	case <-f.opened:
		return f.writeTo(w)
	default:
	}

	select {
	case <-f.opened:
		return f.writeTo(w)
	case <-f.closed:
		return 0, ErrWriteClosed
	}
}

func (f *fifo) writeTo(w io.Writer) (int64, error) {
	if atomic.LoadInt32(&spliceSupported) != 1 {
		return copyBuffer(w, f.file)
	}

	wscI, ok := w.(syscall.Conn)
	if !ok {
		return copyBuffer(w, f.file)
	}

	wsc, err := wscI.SyscallConn()
	if err != nil {
		return copyBuffer(w, f.file)
	}

	rsc, err := f.SyscallConn()
	if err != nil {
		return copyBuffer(w, f.file)
	}

	handled, written, err := doRawCopy(rsc, wsc, spliceToEOF)
	if err != nil {
		return written, err
	}
	if !handled {
		return copyBuffer(w, f.file)
	}
	return written, err
}

func doRawCopy(rsc, wsc syscall.RawConn, remain int64) (handled bool, written int64, _ error) {
	var (
		spliceErr error
		writeErr  error
		copyToEOF = remain == spliceToEOF
	)

	if remain == spliceToEOF {
		remain = spliceMax
	}

	// Hear the RawConn Read/Write methods allow us to utilize the go runtime
	// poller to wait for the file descriptors to be ready for reads or writes.
	//
	// Read/Write will sleep the goroutine until the file descriptor is ready.
	// Once we are inside the function we've passed in, we know the FD is ready.
	//
	// Read/Write both run the function they are passed repeatedly until the
	// function returns true.
	err := rsc.Read(func(rfd uintptr) bool {
		err := wsc.Write(func(wfd uintptr) bool {
			for copyToEOF || remain > 0 {
				// We always use NONBLOCK here. If the file descriptor(s) is not
				// opened with O_NONBLOCK then splice just blocks like normal.
				// If they opened with O_NONBLOCK, then `unix.Splice` returns
				// with EAGAIN when either the read or write would block.
				n, err := unix.Splice(int(rfd), nil, int(wfd), nil, int(remain), unix.SPLICE_F_MOVE|unix.SPLICE_F_NONBLOCK)
				if n > 0 {
					written += n
					if !copyToEOF {
						remain -= n
					}
				}

				switch err {
				case unix.ENOSYS:
					// splice not supported on kernel
					atomic.StoreInt32(&spliceSupported, 0)
					return true
				case syscall.EINVAL, syscall.EOPNOTSUPP, syscall.EPERM:
					// In all these cases, there is no data transferred
					return true
				case nil:
					handled = true
					if n == 0 {
						// At EOF
						return true
					}
				case unix.EINTR:
					continue
				case unix.EAGAIN:
					// Normally we'd want to return false here, because this just means
					// we need it to wait for the fd to be ready again, however we don't know
					// which fd needs to be waited on.
					// So, break out of the write func and let `Read` return false so we end up
					// waiting for both fd's to be ready.
					return true
				default:
					spliceErr = err
					handled = true
					return true
				}
			}
			return true
		})
		if err != nil {
			writeErr = err
			return true
		}
		if spliceErr != nil {
			// I don't like this but it made the linter happy. All hail the mighty linter.
			// If splice returned EAGAIN we should return false so we can
			// wait for the FD to be ready again. Otherwise we just want to exit
			// early.
			return spliceErr != unix.EAGAIN
		}

		return true
	})

	if spliceErr != nil {
		return handled, written, spliceErr
	}
	if writeErr != nil {
		return handled, written, writeErr
	}
	return handled, written, err
}

func copyBuffer(w io.Writer, r io.Reader) (int64, error) {
	buf := bufPool.Get().(*[]byte)
	n, err := io.CopyBuffer(w, r, *buf)
	bufPool.Put(buf)
	return n, err
}

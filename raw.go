// +build go1.12

package fifo

import (
	"syscall"

	"github.com/pkg/errors"
)

// SyscallConn provides raw access to the fifo's underlying filedescrptor.
// See syscall.Conn for guarentees provided by this interface.
func (f *fifo) SyscallConn() (syscall.RawConn, error) {
	select {
	case <-f.opened:
		return newRawConn(f)
	default:
	}

	if !f.block {
		rc := &rawConn{f: f, ready: make(chan struct{})}
		go func() {
			select {
			case <-f.closed:
				return
			case <-f.opened:
				rc.raw, rc.err = f.file.SyscallConn()
				close(rc.ready)
			}
		}()

		return rc, nil
	}

	select {
	case <-f.opened:
		return newRawConn(f)
	case <-f.closed:
		return nil, errors.New("fifo closed")
	}
}

// newRawConn creates a new syscall.RawConn from a fifo
//
// Note that this assumes the fifo is open
// It is recommended to only call this through `fifo.SyscallConn`
func newRawConn(f *fifo) (syscall.RawConn, error) {
	raw, err := f.file.SyscallConn()
	if err != nil {
		return nil, err
	}

	ready := make(chan struct{})
	close(ready)
	return &rawConn{f: f, raw: raw, ready: ready}, nil
}

type rawConn struct {
	f     *fifo
	ready chan struct{}
	raw   syscall.RawConn
	err   error
}

func (r *rawConn) Control(f func(fd uintptr)) error {
	select {
	case <-r.f.closed:
		return errors.New("control of closed fifo")
	case <-r.ready:
	}

	if r.err != nil {
		return r.err
	}

	return r.raw.Control(f)
}

func (r *rawConn) Read(f func(fd uintptr) (done bool)) error {
	if r.f.flag&syscall.O_WRONLY > 0 {
		return errors.New("reading from write-only fifo")
	}

	select {
	case <-r.f.closed:
		return errors.New("reading of a closed fifo")
	case <-r.ready:
	}

	if r.err != nil {
		return r.err
	}

	return r.raw.Read(f)
}

func (r *rawConn) Write(f func(fd uintptr) (done bool)) error {
	if r.f.flag&(syscall.O_WRONLY|syscall.O_RDWR) == 0 {
		return errors.New("writing to read-only fifo")
	}

	select {
	case <-r.f.closed:
		return errors.New("writing to a closed fifo")
	case <-r.ready:
	}

	if r.err != nil {
		return r.err
	}

	return r.raw.Write(f)
}

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
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openRDWRWithWriter opens an O_RDWR reader on fn, like containerd's
// openShimLog, plus a blocking O_WRONLY writer, like a shim's fd 2.
func openRDWRWithWriter(t *testing.T, fn string) (io.ReadCloser, *os.File) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	r, err := OpenFifo(ctx, fn, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NONBLOCK, 0o700)
	require.NoError(t, err)

	w, err := os.OpenFile(fn, os.O_WRONLY, 0)
	require.NoError(t, err)

	return r, w
}

// TestFifoRDWRBigWrite: a reader parked in Read must receive a single
// blocking write larger than the fifo's free buffer space (8192 bytes).
// XNU only posts a fifo's kqueue knotes when write(2) returns, and such a
// write sleeps until the reader drains, so a kqueue-polled reader hangs.
func TestFifoRDWRBigWrite(t *testing.T) {
	tmpdir, err := os.MkdirTemp("", "fifos")
	require.NoError(t, err)
	defer os.RemoveAll(tmpdir)

	r, w := openRDWRWithWriter(t, filepath.Join(tmpdir, t.Name()))
	defer r.Close()
	defer w.Close()

	data := make([]byte, 16635)
	for i := range data {
		data[i] = byte(i)
	}

	read := make(chan []byte, 1)
	go func() {
		buf := make([]byte, len(data))
		n, _ := io.ReadFull(r, buf)
		read <- buf[:n]
	}()
	time.Sleep(100 * time.Millisecond) // let the reader park in Read

	written := make(chan error, 1)
	go func() {
		_, err := w.Write(data)
		written <- err
	}()

	select {
	case err := <-written:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Write hung")
	}
	select {
	case got := <-read:
		assert.Equal(t, data, got)
	case <-time.After(3 * time.Second):
		t.Fatal("Read hung")
	}
}

// TestFifoORDWRReader is the darwin counterpart of TestFifoORDWR, following
// OpenFifo's advice: the O_RDWR end only reads, writers open O_WRONLY.
func TestFifoORDWRReader(t *testing.T) {
	tmpdir, err := os.MkdirTemp("", "fifos")
	require.NoError(t, err)
	defer os.RemoveAll(tmpdir)

	leakCheckWg = &sync.WaitGroup{}
	defer func() {
		leakCheckWg = nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fn := filepath.Join(tmpdir, "f0")

	// No writer yet: the O_RDWR open must not block.
	r, err := OpenFifo(ctx, fn, syscall.O_RDWR|syscall.O_CREAT, 0o600)
	require.NoError(t, err)

	b := make([]byte, 32)
	for _, msg := range []string{"foobar", "barbaz"} {
		w, err := OpenFifo(ctx, fn, syscall.O_WRONLY, 0)
		require.NoError(t, err)

		_, err = w.Write([]byte(msg))
		assert.NoError(t, err)
		assert.NoError(t, w.Close())

		// The previous writer closing didn't end the stream.
		n, err := r.Read(b)
		assert.NoError(t, err)
		assert.Equal(t, msg, string(b[:n]))
	}

	assert.NoError(t, r.Close())
	_, err = r.Read(b)
	assert.Equal(t, ErrReadClosed, err)

	assert.NoError(t, checkWgDone(leakCheckWg))
}

// copyUntilClose closes r while io.Copy is parked in r.Read and returns the
// copy's error.
func copyUntilClose(t *testing.T, r io.ReadCloser) error {
	t.Helper()

	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, r)
		copied <- err
	}()
	time.Sleep(10 * time.Millisecond) // let the copy park in Read

	closed := make(chan error, 1)
	go func() {
		closed <- r.Close()
	}()
	select {
	case err := <-closed:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung")
	}

	select {
	case err := <-copied:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Read hung after Close")
		return nil
	}
}

// TestFifoRDWRCloseWhileReadingNoWriter: Close interrupts a Read on an
// O_RDWR fifo nobody else ever opened. The reader is its own writer, so it
// never sees EOF.
func TestFifoRDWRCloseWhileReadingNoWriter(t *testing.T) {
	tmpdir, err := os.MkdirTemp("", "fifos")
	require.NoError(t, err)
	defer os.RemoveAll(tmpdir)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	r, err := OpenFifo(ctx, filepath.Join(tmpdir, t.Name()), syscall.O_RDWR|syscall.O_CREAT|syscall.O_NONBLOCK, 0o700)
	require.NoError(t, err)

	assert.Equal(t, ErrReadClosed, copyUntilClose(t, r))
}

// TestFifoRDWRCloseWhileReadingSilentWriter: Close interrupts a Read while
// a writer holds the fifo open without writing. copyUntilClose fails if the
// reading goroutine doesn't return; leakCheckWg covers openFifo's own.
func TestFifoRDWRCloseWhileReadingSilentWriter(t *testing.T) {
	tmpdir, err := os.MkdirTemp("", "fifos")
	require.NoError(t, err)
	defer os.RemoveAll(tmpdir)

	leakCheckWg = &sync.WaitGroup{}
	defer func() {
		leakCheckWg = nil
	}()

	fn := filepath.Join(tmpdir, t.Name())
	for i := 0; i < 200; i++ {
		r, w := openRDWRWithWriter(t, fn)
		err := copyUntilClose(t, r)
		require.NoError(t, w.Close())
		require.Equal(t, ErrReadClosed, err, "iteration %d", i)
	}

	assert.NoError(t, checkWgDone(leakCheckWg))
}

// TestFifoRDWRCloseFullBuffer: Close must not block when the fifo buffer is
// full, so interruptRead's write can't complete. Nobody is blocked in Read
// then; the next Read sees the close, and the blocked writer gets EPIPE.
func TestFifoRDWRCloseFullBuffer(t *testing.T) {
	tmpdir, err := os.MkdirTemp("", "fifos")
	require.NoError(t, err)
	defer os.RemoveAll(tmpdir)

	r, w := openRDWRWithWriter(t, filepath.Join(tmpdir, t.Name()))
	defer w.Close()

	written := make(chan error, 1)
	go func() {
		_, err := w.Write(make([]byte, 64<<10))
		written <- err
	}()
	select {
	case err := <-written:
		t.Fatalf("Write should have blocked, but got %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	closed := make(chan error, 1)
	go func() {
		closed <- r.Close()
	}()
	select {
	case err := <-closed:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung")
	}

	_, err = r.Read(make([]byte, 32))
	assert.Equal(t, ErrReadClosed, err)

	select {
	case err := <-written:
		assert.True(t, errors.Is(err, syscall.EPIPE), "got %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Write hung after Close")
	}
}

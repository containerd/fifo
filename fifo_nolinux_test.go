//go:build !linux && !windows

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
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFifoCloseAfterRm(t *testing.T) {
	tmpdir, err := os.MkdirTemp("", "fifos")
	assert.NoError(t, err)
	defer os.RemoveAll(tmpdir)

	// non-linux version of this test leaks a goroutine

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f, err := OpenFifo(ctx, filepath.Join(tmpdir, "f0"), syscall.O_RDONLY|syscall.O_CREAT|syscall.O_NONBLOCK, 0o600)
	assert.NoError(t, err)

	time.Sleep(time.Second) // non-linux doesn't allow removing before syscall has been called. will cause an error.

	err = os.RemoveAll(filepath.Join(tmpdir, "f0"))
	assert.NoError(t, err)

	cerr := make(chan error)

	go func() {
		b := make([]byte, 32)
		_, err := f.Read(b)
		cerr <- err
	}()

	select {
	case err := <-cerr:
		t.Fatalf("read should have blocked, but got %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	err = f.Close()
	assert.NoError(t, err)

	select {
	case err := <-cerr:
		assert.EqualError(t, err, "reading from a closed fifo")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("read should have been unblocked")
	}

	cancel()
	ctx, cancel = context.WithCancel(context.Background())
	cerr = make(chan error)
	go func() {
		_, err = OpenFifo(ctx, filepath.Join(tmpdir, "f1"), syscall.O_WRONLY|syscall.O_CREAT, 0o600)
		cerr <- err
	}()

	select {
	case err := <-cerr:
		t.Fatalf("open should have blocked, but got %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	err = os.RemoveAll(filepath.Join(tmpdir, "f1"))
	cancel()

	select {
	case err := <-cerr:
		assert.Error(t, err)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("open should have been unblocked")
	}
}

// TestFifoRDWRCloseWhileReading guards against closing an O_RDWR fifo
// must unblock a concurrent, already-blocked Read. On Darwin, os.OpenFile
// leaves fifo descriptors off the runtime poller (see golang/go#24164),
// so without openFifoFile's O_RDWR-specific workaround, Close's underlying
// file.Close() would never interrupt the blocked read(2) syscall and this
// will hang instead of failing fast.
func TestFifoRDWRCloseWhileReading(t *testing.T) {
	tmpdir, err := os.MkdirTemp("", "fifos")
	assert.NoError(t, err)
	defer os.RemoveAll(tmpdir)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	fn := filepath.Join(tmpdir, t.Name())

	rw, err := OpenFifo(ctx, fn, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NONBLOCK, 0o600)
	assert.NoError(t, err)

	// A second, independent writer keeps the pipe from ever reaching EOF on its own,
	// so any unblocking of the reader below can only come from Close().
	w, err := OpenFifo(ctx, fn, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	assert.NoError(t, err)
	defer w.Close()

	read := make(chan error, 1)
	go func() {
		buf := make([]byte, 32)
		_, err := rw.Read(buf)
		read <- err
	}()

	select {
	case err := <-read:
		t.Fatalf("read should have blocked, but got %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	closeErr := make(chan error, 1)
	go func() {
		closeErr <- rw.Close()
	}()

	select {
	case err := <-closeErr:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close should not block")
	}

	select {
	case err := <-read:
		assert.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Read should have unblocked after Close")
	}
}

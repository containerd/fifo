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
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestReadFrom(t *testing.T) {
	dir, err := ioutil.TempDir("", t.Name())
	assert.NoError(t, err)
	defer os.RemoveAll(dir)

	ctx := context.Background()
	data := strings.Repeat("This is a test, this is only a test.", 1000)

	// For these test cases we only call ReadFrom and validate there is no error and the
	// amouont of data it copied is what we put into it.
	// The main test runner will validate the data is correct on each case.
	cases := map[string]func(*testing.T, io.ReaderFrom){
		"regular file": func(t *testing.T, w io.ReaderFrom) {
			f, err := os.OpenFile(filepath.Join(dir, "data"), os.O_RDWR|os.O_CREATE, 0600)
			assert.NoError(t, err)
			defer func() {
				f.Close()
				os.RemoveAll(f.Name())
			}()

			_, err = f.WriteString(data)
			assert.NoError(t, err)

			_, err = f.Seek(0, io.SeekStart)
			assert.NoError(t, err)

			n, err := w.ReadFrom(f)
			w.(io.Closer).Close()
			assert.NoError(t, err)
			assert.Equal(t, n, int64(len(data)))
		},
		"fifo": func(t *testing.T, w io.ReaderFrom) {
			// Tests fifo<->fifo copy works.
			buf := strings.NewReader(data)

			fifoW, err := OpenFifo(ctx, filepath.Join(dir, "fifo2"), os.O_RDWR|os.O_CREATE, 0600)
			assert.NoError(t, err)
			defer fifoW.Close()

			fifoR, err := OpenFifo(ctx, filepath.Join(dir, "fifo2"), os.O_RDONLY, 0600)
			assert.NoError(t, err)
			defer fifoR.Close()

			go func() {
				io.Copy(fifoW, buf)
				fifoW.Close()
			}()

			n, err := w.ReadFrom(fifoR)
			w.(io.Closer).Close()
			assert.NoError(t, err)
			assert.Equal(t, n, buf.Size())
		},
		"unix conn": func(t *testing.T, w io.ReaderFrom) {
			sock := filepath.Join(dir, t.Name())
			err := os.MkdirAll(filepath.Dir(sock), 0755)
			assert.NoError(t, err)

			l, err := net.Listen("unix", sock)
			assert.NoError(t, err)

			go func() {
				defer l.Close()
				conn, err := l.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				io.Copy(conn, strings.NewReader(data))
			}()

			conn, err := net.Dial("unix", filepath.Join(dir, t.Name()))
			assert.NoError(t, err)
			defer conn.Close()

			n, err := w.ReadFrom(conn)
			w.(io.Closer).Close()
			assert.NoError(t, err)
			assert.Equal(t, n, int64(len(data)))
		},
		"userspace": func(t *testing.T, w io.ReaderFrom) {
			// Tests that copying from userspace explicitly works.
			data := strings.NewReader(data)
			n, err := w.ReadFrom(data)
			w.(io.Closer).Close()
			assert.NoError(t, err)
			assert.Equal(t, n, data.Size())
		},
		"limited reader": func(t *testing.T, w io.ReaderFrom) {
			// Makes sure we don't read too much from a file wrapped in a limited reader
			// This is important because normally we'd just try to splice as much data as possible,
			// If the user wraps with a LimitedReader, we still want to the benefits of splice but need
			// to limit the splice call to the value limited in the LimitedReader.
			f, err := os.OpenFile(filepath.Join(dir, "data"), os.O_RDWR|os.O_CREATE, 0600)
			assert.NoError(t, err)
			defer func() {
				f.Close()
				os.RemoveAll(f.Name())
			}()

			// Write data twice, will limit the reader to just one.
			written, err := f.WriteString(data + data)
			assert.NoError(t, err)
			assert.Equal(t, written, len(data+data))

			_, err = f.Seek(0, io.SeekStart)
			assert.NoError(t, err)

			n, err := w.ReadFrom(io.LimitReader(f, int64(len(data))))
			w.(io.Closer).Close()
			assert.NoError(t, err)
			assert.Equal(t, n, int64(len(data)))
		},
	}

	buf := bytes.NewBuffer(nil)
	for name, testCase := range cases {
		name := name
		testCase := testCase

		t.Run(name, func(t *testing.T) {
			buf.Reset()
			pipeW, err := OpenFifo(ctx, filepath.Join(dir, "fifo"), os.O_RDWR|os.O_CREATE, 0600)
			assert.NoError(t, err)
			defer pipeW.Close()

			pipeR, err := OpenFifo(ctx, filepath.Join(dir, "fifo"), os.O_RDONLY, 0600)
			assert.NoError(t, err)
			defer pipeR.Close()

			done := make(chan struct{})
			go func() {
				io.Copy(buf, pipeR)
				close(done)
			}()

			testCase(t, pipeW.(io.ReaderFrom))

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("timeout waiting for copy")
				// Force copy to end and wait for it
				// We have already failed the test above but there's still some value in seeing the results of the copy below.
				pipeR.Close()
				<-done
			}
			assert.Equal(t, len(data), buf.Len())
			assert.Equal(t, buf.String(), data)
		})
	}
}

func TestWriteTo(t *testing.T) {
	dir, err := ioutil.TempDir("", t.Name())
	assert.NoError(t, err)
	defer os.RemoveAll(dir)

	ctx := context.Background()

	pipeW, err := OpenFifo(ctx, filepath.Join(dir, "fifo"), os.O_RDWR|os.O_CREATE, 0600)
	assert.NoError(t, err)
	defer pipeW.Close()

	pipeR, err := OpenFifo(ctx, filepath.Join(dir, "fifo"), os.O_RDONLY, 0600)
	assert.NoError(t, err)
	defer pipeR.Close()

	f, err := os.OpenFile(filepath.Join(dir, "data"), os.O_RDWR|os.O_CREATE, 0600)
	assert.NoError(t, err)
	defer f.Close()

	data := strings.Repeat("This is a test, this is only a test.", 100)

	done := make(chan struct{})
	go func() {
		io.Copy(pipeW, strings.NewReader(data))
		pipeW.Close()
		close(done)
	}()

	_, err = f.Seek(0, io.SeekStart)
	assert.NoError(t, err)

	n, err := pipeR.(io.WriterTo).WriteTo(f)
	pipeR.Close()
	assert.NoError(t, err)
	assert.Equal(t, int64(len(data)), n)

	<-done

	_, err = f.Seek(0, io.SeekStart)
	assert.NoError(t, err)

	buf := bytes.NewBuffer(nil)
	_, err = io.Copy(buf, f)
	assert.NoError(t, err)
	assert.Equal(t, buf.String(), data)
}

// +build !windows

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
	"io"
	"os"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

// openFifoDup2 is same as OpenFifo, but additionally creates a copy of the FIFO file descriptor with dup2 syscall.
func openFifoDup2(ctx context.Context, fn string, flag int, perm os.FileMode, fd int) (io.ReadWriteCloser, error) {
	f, err := openFifo(ctx, fn, flag, perm)
	if err != nil {
		return nil, errors.Wrap(err, "fifo error")
	}

	if err := unix.Dup2(int(f.file.Fd()), fd); err != nil {
		_ = f.Close()
		return nil, errors.Wrap(err, "dup2 error")
	}

	return f, nil
}

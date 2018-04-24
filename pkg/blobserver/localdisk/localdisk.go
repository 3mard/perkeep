/*
Copyright 2011 The Perkeep Authors

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

/*
Package localdisk registers the "filesystem" blobserver storage type,
storing blobs in a forest of sharded directories at the specified root.

Example low-level config:

     "/storage/": {
         "handler": "storage-filesystem",
         "handlerArgs": {
            "path": "/var/camlistore/blobs"
          }
     },

*/
package localdisk // import "perkeep.org/pkg/blobserver/localdisk"

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"

	"perkeep.org/internal/osutil"
	"perkeep.org/pkg/blob"
	"perkeep.org/pkg/blobserver"
	"perkeep.org/pkg/blobserver/files"
	"perkeep.org/pkg/blobserver/local"

	"go4.org/jsonconfig"
	"go4.org/syncutil"
)

// DiskStorage implements the blobserver.Storage interface using the
// local filesystem.
type DiskStorage struct {
	root string

	fs files.VFS

	// dirLockMu must be held for writing when deleting an empty directory
	// and for read when receiving blobs.
	dirLockMu *sync.RWMutex

	// gen will be nil if partition != ""
	gen *local.Generationer

	// tmpFileGate limits the number of temporary files open at the same
	// time, so we don't run into the max set by ulimit. It is nil on
	// systems (Windows) where we don't know the maximum number of open
	// file descriptors.
	tmpFileGate *syncutil.Gate

	// statGate limits how many pending Stat calls we have in flight.
	statGate *syncutil.Gate
}

func (ds *DiskStorage) String() string {
	return fmt.Sprintf("\"filesystem\" file-per-blob at %s", ds.root)
}

// IsDir reports whether root is a localdisk (file-per-blob) storage directory.
func IsDir(root string) (bool, error) {
	if osutil.DirExists(filepath.Join(root, "sha1")) {
		return true, nil
	}
	if osutil.DirExists(filepath.Join(root, blob.RefFromString("").HashName())) {
		return true, nil
	}
	return false, nil
}

const (
	// We refuse to create a DiskStorage when the user's ulimit is lower than
	// minFDLimit. 100 is ridiculously low, but the default value on OSX is 256, and we
	// don't want to fail by default, so our min value has to be lower than 256.
	minFDLimit         = 100
	recommendedFDLimit = 1024
)

// New returns a new local disk storage implementation at the provided
// root directory, which must already exist.
func New(root string) (*DiskStorage, error) {
	// Local disk.
	fi, err := os.Stat(root)
	if os.IsNotExist(err) {
		// As a special case, we auto-created the "packed" directory for subpacked.
		if filepath.Base(root) == "packed" {
			if err := os.Mkdir(root, 0700); err != nil {
				return nil, fmt.Errorf("failed to mkdir packed directory: %v", err)
			}
			fi, err = os.Stat(root)
		} else {
			return nil, fmt.Errorf("Storage root %q doesn't exist", root)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("Failed to stat directory %q: %v", root, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("storage root %q exists but is not a directory", root)
	}
	ds := &DiskStorage{
		fs:        osFS{},
		root:      root,
		dirLockMu: new(sync.RWMutex),
		gen:       local.NewGenerationer(root),
		statGate:  syncutil.NewGate(10), // arbitrary, but bounded; be more clever later?
	}
	if err := ds.migrate3to2(); err != nil {
		return nil, fmt.Errorf("Error updating localdisk format: %v", err)
	}
	if _, _, err := ds.StorageGeneration(); err != nil {
		return nil, fmt.Errorf("Error initialization generation for %q: %v", root, err)
	}
	ul, err := osutil.MaxFD()
	if err != nil {
		if err == osutil.ErrNotSupported {
			// Do not set the gate on Windows, since we don't know the ulimit.
			return ds, nil
		}
		return nil, err
	}
	if ul < minFDLimit {
		return nil, fmt.Errorf("the max number of open file descriptors on your system (ulimit -n) is too low. Please fix it with 'ulimit -S -n X' with X being at least %d", recommendedFDLimit)
	}
	// Setting the gate to 80% of the ulimit, to leave a bit of room for other file ops happening in Perkeep.
	// TODO(mpl): make this used and enforced Perkeep-wide. Issue #837.
	ds.tmpFileGate = syncutil.NewGate(int(ul * 80 / 100))
	err = ds.checkFS()
	if err != nil {
		return nil, err
	}
	return ds, nil
}

func newFromConfig(_ blobserver.Loader, config jsonconfig.Obj) (storage blobserver.Storage, err error) {
	path := config.RequiredString("path")
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return New(path)
}

func init() {
	blobserver.RegisterStorageConstructor("filesystem", blobserver.StorageConstructor(newFromConfig))
}

func (ds *DiskStorage) tryRemoveDir(dir string) {
	ds.dirLockMu.Lock()
	defer ds.dirLockMu.Unlock()
	ds.fs.RemoveDir(dir) // ignore error
}

func (ds *DiskStorage) Fetch(ctx context.Context, br blob.Ref) (io.ReadCloser, uint32, error) {
	return ds.fetch(ctx, br, 0, -1)
}

func (ds *DiskStorage) SubFetch(ctx context.Context, br blob.Ref, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length < 0 {
		return nil, blob.ErrNegativeSubFetch
	}
	rc, _, err := ds.fetch(ctx, br, offset, length)
	return rc, err
}

// u32 converts n to an uint32, or panics if n is out of range
func u32(n int64) uint32 {
	if n < 0 || n > math.MaxUint32 {
		panic("bad size " + fmt.Sprint(n))
	}
	return uint32(n)
}

// length -1 means entire file
func (ds *DiskStorage) fetch(ctx context.Context, br blob.Ref, offset, length int64) (rc io.ReadCloser, size uint32, err error) {
	// TODO: use ctx, if the os package ever supports that.
	fileName := ds.blobPath(br)
	stat, err := ds.fs.Stat(fileName)
	if os.IsNotExist(err) {
		return nil, 0, os.ErrNotExist
	}
	size = u32(stat.Size())
	file, err := ds.fs.Open(fileName)
	if err != nil {
		if os.IsNotExist(err) {
			err = os.ErrNotExist
		}
		return nil, 0, err
	}
	// normal Fetch
	if length < 0 && offset == 0 {
		return file, size, nil
	}
	// SubFetch:
	if offset < 0 || offset > stat.Size() {
		if offset < 0 {
			return nil, 0, blob.ErrNegativeSubFetch
		}
		return nil, 0, blob.ErrOutOfRangeOffsetSubFetch
	}
	if offset != 0 {
		if at, err := file.Seek(offset, io.SeekStart); err != nil || at != offset {
			file.Close()
			return nil, 0, fmt.Errorf("localdisk: error seeking to %d: got %v, %v", offset, at, err)
		}
	}
	return struct {
		io.Reader
		io.Closer
	}{
		Reader: io.LimitReader(file, length),
		Closer: file,
	}, 0 /* unused */, nil
}

func (ds *DiskStorage) RemoveBlobs(ctx context.Context, blobs []blob.Ref) error {
	for _, blob := range blobs {
		fileName := ds.blobPath(blob)
		err := ds.fs.Remove(fileName)
		switch {
		case err == nil:
			continue
		case os.IsNotExist(err):
			// deleting already-deleted file; harmless.
			continue
		default:
			return err
		}
	}
	return nil
}

// checkFS verifies the DiskStorage root storage path
// operations include: stat, read/write file, mkdir, delete (files and directories)
func (ds *DiskStorage) checkFS() (ret error) {
	tempdir, err := ioutil.TempDir(ds.root, "")
	if err != nil {
		return fmt.Errorf("localdisk check: unable to create tempdir in %s, err=%v", ds.root, err)
	}
	defer func() {
		err := os.RemoveAll(tempdir)
		if err != nil {
			cleanErr := fmt.Errorf("localdisk check: unable to clean temp dir: %v", err)
			if ret == nil {
				ret = cleanErr
			} else {
				log.Printf("WARNING: %v", cleanErr)
			}
		}
	}()

	tempfile := filepath.Join(tempdir, "FILE.tmp")
	filename := filepath.Join(tempdir, "FILE")
	data := []byte("perkeep rocks")
	err = ioutil.WriteFile(tempfile, data, 0644)
	if err != nil {
		return fmt.Errorf("localdisk check: unable to write into %s, err=%v", ds.root, err)
	}

	out, err := ioutil.ReadFile(tempfile)
	if err != nil {
		return fmt.Errorf("localdisk check: unable to read from %s, err=%v", tempfile, err)
	}
	if bytes.Compare(out, data) != 0 {
		return fmt.Errorf("localdisk check: tempfile contents didn't match, got=%q", out)
	}
	if _, err := os.Lstat(filename); !os.IsNotExist(err) {
		return fmt.Errorf("localdisk check: didn't expect file to exist, Lstat had other error, err=%v", err)
	}
	if err := os.Rename(tempfile, filename); err != nil {
		return fmt.Errorf("localdisk check: rename failed, err=%v", err)
	}
	if _, err := os.Lstat(filename); err != nil {
		return fmt.Errorf("localdisk check: after rename passed Lstat had error, err=%v", err)
	}
	return nil
}

// osFS implements the files.VFS interface using the os package and
// the host filesystem.
type osFS struct{}

func (osFS) Remove(path string) error                     { return os.Remove(path) }
func (osFS) RemoveDir(path string) error                  { return os.Remove(path) }
func (osFS) Stat(path string) (os.FileInfo, error)        { return os.Stat(path) }
func (osFS) Lstat(path string) (os.FileInfo, error)       { return os.Lstat(path) }
func (osFS) Open(path string) (files.ReadableFile, error) { return os.Open(path) }
func (osFS) MkdirAll(path string, perm os.FileMode) error { return os.MkdirAll(path, perm) }
func (osFS) Rename(oldname, newname string) error         { return os.Rename(oldname, newname) }

func (osFS) TempFile(dir, prefix string) (files.WritableFile, error) {
	f, err := ioutil.TempFile(dir, prefix)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (osFS) ReadDirNames(dir string) ([]string, error) {
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.Readdirnames(-1)
}

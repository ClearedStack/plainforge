package store

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/clearedstack/plainforge/internal/pferr"
)

// errNotFound is returned inside this package when an object is not stored.
var errNotFound = errors.New("object not stored")

// storageErr turns a file system failure into a public error. Only the
// plain reason from the operating system is kept; the path is dropped.
func storageErr(doing string, err error) error {
	var pe *pferr.Error
	if errors.As(err, &pe) {
		return err
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return pferr.Parsef("repository storage failed while %s: %s", doing, errno.Error())
	}
	return pferr.Parsef("repository storage failed while %s", doing)
}

// objdir is a directory of loose objects.
type objdir struct{ dir string }

func (d objdir) path(o OID) string {
	s := o.String()
	return filepath.Join(d.dir, s[:2], s[2:])
}

func (d objdir) has(o OID) bool {
	_, err := os.Stat(d.path(o))
	return err == nil
}

// open starts reading an object and returns its kind, its stated size and a
// reader positioned at its content.
func (d objdir) open(o OID) (Type, int, io.ReadCloser, *os.File, error) {
	f, err := os.Open(d.path(o))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, 0, nil, nil, errNotFound
		}
		return 0, 0, nil, nil, storageErr("opening an object", err)
	}
	bad := func() (Type, int, io.ReadCloser, *os.File, error) {
		f.Close()
		return 0, 0, nil, nil, pferr.Parsef("a stored object is damaged")
	}
	zr, err := zlib.NewReader(bufio.NewReader(f))
	if err != nil {
		return bad()
	}
	var hdr []byte
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(zr, one); err != nil || len(hdr) > 32 {
			return bad()
		}
		if one[0] == 0 {
			break
		}
		hdr = append(hdr, one[0])
	}
	name, sizeText, ok := bytes.Cut(hdr, []byte(" "))
	t, okType := ParseType(string(name))
	size, err := strconv.Atoi(string(sizeText))
	if !ok || !okType || err != nil || size < 0 {
		return bad()
	}
	return t, size, zr, f, nil
}

func (d objdir) typeOf(o OID) (Type, error) {
	t, _, zr, f, err := d.open(o)
	if err != nil {
		return 0, err
	}
	zr.Close()
	f.Close()
	return t, nil
}

// read returns an object and checks that its content still matches its name.
func (d objdir) read(o OID) (Type, []byte, error) {
	t, size, zr, f, err := d.open(o)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	defer zr.Close()
	if size > MaxObjectSize {
		return 0, nil, pferr.Needsf("a stored object is larger than Plainforge handles")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(zr, data); err != nil {
		return 0, nil, pferr.Parsef("a stored object is damaged")
	}
	if n, _ := zr.Read(make([]byte, 1)); n != 0 {
		return 0, nil, pferr.Parsef("a stored object is damaged")
	}
	if Hash(t, data) != o {
		return 0, nil, pferr.Parsef("a stored object is damaged")
	}
	return t, data, nil
}

// write stores an object as a loose file and returns its name. The file is
// written under a temporary name and moved into place, so a reader never
// sees half an object.
func (d objdir) write(t Type, data []byte, tmpDir string) (OID, error) {
	o := Hash(t, data)
	final := d.path(o)
	if _, err := os.Stat(final); err == nil {
		return o, nil
	}
	tmp, err := os.CreateTemp(tmpDir, "obj-*")
	if err != nil {
		return o, storageErr("creating an object", err)
	}
	defer os.Remove(tmp.Name())
	zw := zlib.NewWriter(tmp)
	_, err = zw.Write(header(t, len(data)))
	if err == nil {
		_, err = zw.Write(data)
	}
	if err == nil {
		err = zw.Close()
	}
	if err == nil {
		err = tmp.Chmod(0o444)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return o, storageErr("writing an object", err)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return o, storageErr("writing an object", err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return o, storageErr("writing an object", err)
	}
	return o, nil
}

// syncFile makes sure a file has reached the disk.
func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

package store

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
)

const (
	IO_CONTEXT_TYPE_MERGE   = 1
	IO_CONTEXT_TYPE_READ    = 2
	IO_CONTEXT_TYPE_FLUSH   = 4
	IO_CONTEXT_TYPE_DEFAULT = 8
)

type IOContextType int

var (
	IO_CONTEXT_READ = NewIOContextBool(false)
)

type IOContext struct {
	context IOContextType
	// mergeInfo MergeInfo
	// flushInfo FlushInfo
	readOnce bool
}

func NewIOContextBool(readOnce bool) IOContext {
	return IOContext{IOContextType(IO_CONTEXT_TYPE_READ), readOnce}
}

type Lock struct {
	self interface{}
}

type LockFactory struct {
	self       interface{}
	lockPrefix string
	Make       func(name string) Lock
	Clear      func(name string) error
}

type FSLockFactory struct {
	*LockFactory
	lockDir string // can not be set twice
}

type Directory struct {
	isOpen      bool
	lockFactory LockFactory
	ListAll     func() (paths []string, err error)
	OpenInput   func(name string, context IOContext) (in *IndexInput, err error)
	LockID      func() string
}

func (d *Directory) SetLockFactory(lockFactory LockFactory) {
	d.lockFactory = lockFactory
	d.lockFactory.lockPrefix = d.LockID()
}

func (d *Directory) ensureOpen() {
	if !d.isOpen {
		panic("this Directory is closed")
	}
}

type FSDirectory struct {
	*Directory
	path      string
	chunkSize int
}

func (d *FSDirectory) SetLockFactory(lockFactory LockFactory) {
	d.Directory.SetLockFactory(lockFactory)

	// for filesystem based LockFactory, delete the lockPrefix, if the locks are placed
	// in index dir. If no index dir is given, set ourselves
	if lf, ok := lockFactory.self.(*FSLockFactory); ok {
		if lf.lockDir == "" {
			lf.lockDir = d.path
			lf.lockPrefix = ""
		} else if lf.lockDir == d.path {
			lf.lockPrefix = ""
		}
	}
}

func newFSDirectory(path string) (d *FSDirectory, err error) {
	d = &FSDirectory{chunkSize: math.MaxInt32}
	if f, err := os.Open(path); err == nil {
		fi, err := f.Stat()
		if err != nil {
			return d, err
		}
		if !fi.IsDir() {
			return d, errors.New(fmt.Sprintf("file '%v' exists but is not a directory", path))
		}
	}

	super := Directory{ListAll: func() (paths []string, err error) {
		d.ensureOpen()
		return ListAll(d.path)
	}, LockID: func() string {
		d.ensureOpen()
		var digest int
		for _, ch := range d.path {
			digest = 31*digest + int(ch)
		}
		return fmt.Sprintf("lucene-%v", strconv.FormatUint(uint64(digest), 10))
	}}
	d.Directory = &super
	// TODO default to native lock factory
	d.SetLockFactory(*(NewSimpleFSLockFactory(path).LockFactory))
	return d, nil
}

// TODO support lock factory
func OpenFSDirectory(path string) (d FSDirectory, err error) {
	// TODO support native implementations
	super, err := NewSimpleFSDirectory(path)
	if err != nil {
		return d, err
	}
	return *(super.FSDirectory), nil
}

func ListAll(path string) (paths []string, err error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, errors.New(fmt.Sprintf("directory '%v' does not exist", path))
	} else if err != nil {
		panic(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if !fi.IsDir() {
		return nil, errors.New(fmt.Sprintf("file '%v' exists but is not a directory", path))
	}

	// Exclude subdirs
	return f.Readdirnames(0)
}

type DataInput struct {
	ReadByte func() byte
}

func (in *DataInput) ReadInt() int {
	return (int(in.ReadByte()&0xFF) << 24) | (int(in.ReadByte()&0xFF) << 16) | (int(in.ReadByte()&0xFF) << 8) | int(in.ReadByte()&0xFF)
}

func (in *DataInput) ReadLong() int64 {
	return ((int64(in.ReadInt())) << 32) | (int64(in.ReadInt()) & 0xFFFFFFFF)
}

type IndexInput struct {
	*DataInput
	desc   string
	Length func() int64
}

func newIndexInput(desc string) *IndexInput {
	if desc == "" {
		panic("resourceDescription must not be null")
	}
	super := &DataInput{}
	return &IndexInput{DataInput: super, desc: desc}
}

type BufferedIndexInput struct {
	*IndexInput
	bufferSize     int
	buffer         []byte
	bufferStart    int64
	bufferLength   int
	bufferPosition int
	seekInternal   func(pos int64)
	readInternal   func(b []byte, offset, length int) error
}

func newBufferedIndexInput(desc string, context IOContext) *BufferedIndexInput {
	return newBufferedIndexInputBySize(desc, bufferSize(context))
}

func newBufferedIndexInputBySize(desc string, bufferSize int) *BufferedIndexInput {
	super := newIndexInput(desc)
	checkBufferSize(bufferSize)
	return &BufferedIndexInput{IndexInput: super, bufferSize: bufferSize}
}

func (in *BufferedIndexInput) newBuffer(newBuffer []byte) {
	// Subclasses can do something here
	in.buffer = newBuffer
}

func checkBufferSize(bufferSize int) {
	if bufferSize <= 0 {
		panic(fmt.Sprintf("bufferSize must be greater than 0 (got %v)", bufferSize))
	}
}

func bufferSize(context IOContext) int {
	switch context.context {
	case IO_CONTEXT_TYPE_MERGE:
		// The normal read buffer size defaults to 1024, but
		// increasing this during merging seems to yield
		// performance gains.  However we don't want to increase
		// it too much because there are quite a few
		// BufferedIndexInputs created during merging.  See
		// LUCENE-888 for details.
		return 4096
	default:
		return 1024
	}
}

func (in *BufferedIndexInput) ReadByte() (b byte, err error) {
	if in.bufferPosition >= in.bufferLength {
		err = in.refill()
		if err != nil {
			return 0, err
		}
	}
	b = in.buffer[in.bufferPosition]
	in.bufferPosition++
	return b, nil
}

func (in *BufferedIndexInput) refill() error {
	start := in.bufferStart + int64(in.bufferPosition)
	end := start + int64(in.bufferSize)
	if end > in.Length() { // don't read past EOF
		end = in.Length()
	}
	newLength := int(end - start)
	if newLength <= 0 {
		return errors.New(fmt.Sprintf("read past EOF: %v", in))
	}

	if in.buffer == nil {
		in.newBuffer(make([]byte, in.bufferSize)) // allocate buffer lazily
		in.seekInternal(int64(in.bufferStart))
	}
	in.readInternal(in.buffer, 0, newLength)
	in.bufferLength = newLength
	in.bufferStart = start
	in.bufferPosition = 0
	return nil
}

func (in *BufferedIndexInput) FilePointer() int64 {
	return in.bufferStart + int64(in.bufferPosition)
}

type FSIndexInput struct {
	*BufferedIndexInput
	file      *os.File
	isClone   bool
	chunkSize int
	off       int64
	end       int64
}

func newFSIndexInput(desc, path string, context IOContext, chunkSize int) (in *FSIndexInput, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	super := newBufferedIndexInput(desc, context)
	in = &FSIndexInput{super, f, false, chunkSize, 0, fi.Size()}
	super.Length = func() int64 {
		return in.end - in.off
	}
	return in, nil
}

func (in *FSIndexInput) Close() {
	// only close the file if this is not a clone
	if !in.isClone {
		in.file.Close()
	}
}
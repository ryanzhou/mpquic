// Package logfile is a minimal size-rotated log file (used by the Windows
// service, which has no stderr).
package logfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// File appends to a log file and rotates it to "<path>.1" when it exceeds
// MaxSize bytes (one old file is kept).
type File struct {
	path    string
	maxSize int64
	mu      sync.Mutex
	f       *os.File
	size    int64
}

// Open opens (or creates) path for appending, creating its directory.
func Open(path string, maxSize int64) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	l := &File{path: path, maxSize: maxSize}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *File) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

// Write implements io.Writer.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return 0, os.ErrClosed
	}
	if l.maxSize > 0 && l.size > 0 && l.size+int64(len(p)) > l.maxSize {
		l.f.Close()
		old := l.path + ".1"
		os.Remove(old)
		os.Rename(l.path, old)
		if err := l.open(); err != nil {
			l.f = nil
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// Close closes the file.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Tail returns up to the last n bytes of the log (including the rotated
// file if the current one is shorter), starting at a line boundary.
func Tail(path string, n int64) (string, error) {
	cur, err := readTail(path, n)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if int64(len(cur)) < n {
		if prev, err := readTail(path+".1", n-int64(len(cur))); err == nil {
			cur = append(prev, cur...)
		}
	}
	if int64(len(cur)) >= n {
		// drop the partial first line
		for i, c := range cur {
			if c == '\n' {
				cur = cur[i+1:]
				break
			}
		}
	}
	if len(cur) > 0 {
		return string(cur), nil
	}
	return "", err
}

func readTail(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := st.Size() - n
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package webdavfs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"

	"github.com/tailscale/gowebdav"
)

type readOnlyFile struct {
	io.ReadCloser
	name      string
	initialFI fs.FileInfo
	fi        fs.FileInfo
	client    *gowebdav.Client
	mu        sync.RWMutex
}

// Readdir implements webdav.File.
func (f *readOnlyFile) Readdir(count int) ([]fs.FileInfo, error) {
	return nil, &os.PathError{
		Op:   "readdir",
		Path: f.fi.Name(),
		Err:  errors.New("is a file"), // TODO(oxtoacart): make sure this and below errors match what a regular os.File does
	}
}

// Seek implements webdav.File.
func (f *readOnlyFile) Seek(offset int64, whence int) (int64, error) {
	err := f.statIfNecessary()
	if err != nil {
		return 0, err
	}

	switch whence {
	case io.SeekEnd:
		if offset == 0 {
			// seek to end is usually done to check size, let's play along
			return f.fi.Size(), nil
		}
	case io.SeekStart:
		if offset == 0 {
			// this is usually done to start reading after getting size
			return 0, nil
		}
	}

	// unknown seek scenario, error out
	return 0, &os.PathError{
		Op:   "seek",
		Path: f.fi.Name(),
		Err:  errors.New("seek not supported"),
	}
}

// Stat implements webdav.File, returning either the FileInfo with which this
// file was initialized, or the more recently fetched FileInfo if available.
func (f *readOnlyFile) Stat() (fs.FileInfo, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.fi != nil {
		return f.fi, nil
	}
	return f.initialFI, nil
}

// Read implements webdav.File.
func (f *readOnlyFile) Read(p []byte) (int, error) {
	err := f.initReaderIfNecessary()
	if err != nil {
		return 0, err
	}

	n, err := f.ReadCloser.Read(p)
	return n, err
}

// Write implements webdav.File.
func (f *readOnlyFile) Write(p []byte) (int, error) {
	return 0, &os.PathError{
		Op:   "write",
		Path: f.fi.Name(),
		Err:  errors.New("read-only"),
	}
}

// Close implements webdav.File.
func (f *readOnlyFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.ReadCloser == nil {
		return nil
	}
	return f.ReadCloser.Close()
}

// statIfNecessary lazily initializes the FileInfo, bypassing the stat cache to
// make sure we have fresh info before trying to read the file.
func (f *readOnlyFile) statIfNecessary() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fi == nil {
		ctxWithTimeout, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()

		var err error
		f.fi, err = f.client.Stat(ctxWithTimeout, f.name)
		if err != nil {
			return translateWebDAVError(err)
		}
	}

	return nil
}

// initReaderIfNecessary initializes the Reader if it hasn't been opened yet. We do
// this lazily because golang.org/x/net/webdav often opens files in read-only
// mode without ever actually reading from them, so we can improve performance
// by avoiding the round-trip to the server.
func (f *readOnlyFile) initReaderIfNecessary() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.ReadCloser == nil {
		var err error
		f.ReadCloser, err = f.client.ReadStream(context.Background(), f.name)
		if err != nil {
			return translateWebDAVError(err)
		}
	}

	return nil
}

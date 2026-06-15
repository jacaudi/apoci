package oci

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"sync"

	"cuelabs.dev/go/oci/ociregistry"
)

// diskBlobWriter is an ociregistry.BlobWriter that stages a chunked blob upload
// on disk instead of buffering the whole blob in memory: each chunk is appended
// to a temp file and folded into a running SHA-256, and Commit streams the file
// into the blob store.
type diskBlobWriter struct {
	uuid    string
	maxSize int64
	// commit persists the staged content (read from the rewound temp file).
	commit func(dig ociregistry.Digest, data io.Reader) (ociregistry.Descriptor, error)
	// onDone runs once when the upload reaches a terminal state (committed or
	// canceled), used to drop registry bookkeeping for any outcome.
	onDone func()

	mu   sync.Mutex
	file *os.File
	path string
	hash hash.Hash
	size int64
	done bool // committed or canceled; further writes are rejected
}

func (w *diskBlobWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return 0, fmt.Errorf("%w: upload already finalized", ociregistry.ErrBlobUploadInvalid)
	}
	if w.size+int64(len(p)) > w.maxSize {
		return 0, fmt.Errorf("%w: blob exceeds maximum size (%d bytes)", ociregistry.ErrBlobUploadInvalid, w.maxSize)
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	_, _ = w.hash.Write(p[:n])
	if err != nil {
		return n, fmt.Errorf("writing upload chunk: %w", err)
	}
	return n, nil
}

func (w *diskBlobWriter) Size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

func (w *diskBlobWriter) ChunkSize() int { return 8 * 1024 }

func (w *diskBlobWriter) ID() string { return w.uuid }

// Path returns the staging file path, or "" once Commit or Cancel released it.
func (w *diskBlobWriter) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return ""
	}
	return w.path
}

// Close is a no-op: the framework calls it after every PATCH chunk, but the
// staging file must survive to be resumed by later requests and finalized in
// Commit. The file is released in Commit or Cancel.
func (w *diskBlobWriter) Close() error { return nil }

func (w *diskBlobWriter) Commit(dig ociregistry.Digest) (ociregistry.Descriptor, error) {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return ociregistry.Descriptor{}, fmt.Errorf("%w: upload already finalized", ociregistry.ErrBlobUploadInvalid)
	}
	w.done = true
	computed := ociregistry.Digest("sha256:" + hex.EncodeToString(w.hash.Sum(nil)))
	f := w.file
	// Release w.mu before cleanup and the commit callback (which takes
	// r.uploadsMu) so the two locks never nest.
	w.mu.Unlock()
	defer w.cleanup()

	if computed != dig {
		return ociregistry.Descriptor{}, fmt.Errorf("digest mismatch: expected %s, got %s: %w", dig, computed, ociregistry.ErrDigestInvalid)
	}
	if f == nil {
		return ociregistry.Descriptor{}, fmt.Errorf("%w: upload has no staged data", ociregistry.ErrBlobUploadInvalid)
	}
	if err := f.Sync(); err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("syncing upload: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("rewinding upload: %w", err)
	}
	return w.commit(dig, f)
}

// Cancel discards the upload and removes the staging file. Safe to call repeatedly.
func (w *diskBlobWriter) Cancel() error {
	w.mu.Lock()
	w.done = true
	w.mu.Unlock()
	w.cleanup()
	return nil
}

func (w *diskBlobWriter) cleanup() {
	w.mu.Lock()
	if w.file == nil {
		w.mu.Unlock()
		return
	}
	_ = w.file.Close()
	_ = os.Remove(w.path)
	w.file = nil
	w.mu.Unlock()
	// Run onDone outside w.mu: it takes the registry lock, and Commit
	// deliberately avoids nesting w.mu inside that lock.
	if w.onDone != nil {
		w.onDone()
	}
}

// newDiskBlobWriter creates a staging file in dir. A random ID is generated when uuid is empty.
func newDiskBlobWriter(dir, uuid string, maxSize int64, commit func(dig ociregistry.Digest, data io.Reader) (ociregistry.Descriptor, error)) (*diskBlobWriter, error) {
	f, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		return nil, fmt.Errorf("creating upload staging file: %w", err)
	}
	if uuid == "" {
		uuid = newUploadID()
	}
	return &diskBlobWriter{
		uuid:    uuid,
		maxSize: maxSize,
		commit:  commit,
		file:    f,
		path:    f.Name(),
		hash:    sha256.New(),
	}, nil
}

func newUploadID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("oci: reading random upload id: %v", err))
	}
	return hex.EncodeToString(b)
}

package replication

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"cuelabs.dev/go/oci/ociregistry"
)

const (
	testStripPrefix = "reg.example.com/"
	testLocalRepo   = "reg.example.com/team/app"
)

// fakeSource serves manifests and blobs from in-memory maps.
type fakeSource struct {
	manifests map[string]blob
	blobs     map[string]blob
}

type blob struct {
	data      []byte
	mediaType string
}

type fakeBlobReader struct {
	io.ReadCloser
	desc ociregistry.Descriptor
}

func (f fakeBlobReader) Descriptor() ociregistry.Descriptor { return f.desc }

func (s *fakeSource) GetManifest(_ context.Context, _ string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {
	b, ok := s.manifests[string(digest)]
	if !ok {
		return nil, fmt.Errorf("manifest %s not found", digest)
	}
	return reader(b, digest), nil
}

func (s *fakeSource) GetBlob(_ context.Context, _ string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {
	b, ok := s.blobs[string(digest)]
	if !ok {
		return nil, fmt.Errorf("blob %s not found", digest)
	}
	return reader(b, digest), nil
}

func reader(b blob, digest ociregistry.Digest) ociregistry.BlobReader {
	return fakeBlobReader{
		ReadCloser: io.NopCloser(strings.NewReader(string(b.data))),
		desc:       ociregistry.Descriptor{Digest: digest, Size: int64(len(b.data)), MediaType: b.mediaType},
	}
}

// targetRegistry records what a replication client pushes to it.
type targetRegistry struct {
	mu        sync.Mutex
	blobs     map[string]bool
	manifests map[string][]byte // "repo/ref" -> body
}

func newTargetRegistry() *targetRegistry {
	return &targetRegistry{blobs: map[string]bool{}, manifests: map[string][]byte{}}
}

func (tr *targetRegistry) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/v2/":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead && strings.Contains(p, "/blobs/"):
			dgst := p[strings.LastIndex(p, "/")+1:]
			tr.mu.Lock()
			ok := tr.blobs[dgst]
			tr.mu.Unlock()
			if ok {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/blobs/uploads/"):
			w.Header().Set("Location", p+"uuid-1")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && strings.Contains(p, "/blobs/uploads/"):
			tr.mu.Lock()
			tr.blobs[r.URL.Query().Get("digest")] = true
			tr.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && strings.Contains(p, "/manifests/"):
			body, _ := io.ReadAll(r.Body)
			key := strings.TrimPrefix(p, "/v2/")
			tr.mu.Lock()
			tr.manifests[key] = body
			tr.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return mux
}

func TestWorkerReplicatesImage(t *testing.T) {
	target := newTargetRegistry()
	srv := httptest.NewServer(target.handler())
	defer srv.Close()

	// Build an image manifest with a config blob and one layer.
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	layerData := []byte("layer-bytes")
	configDigest := string(godigest.FromBytes(configData))
	layerDigest := string(godigest.FromBytes(layerData))

	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: godigest.Digest(configDigest), Size: int64(len(configData))},
		Layers:    []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: godigest.Digest(layerDigest), Size: int64(len(layerData))}},
	}
	manifest.SchemaVersion = 2
	manifestData, err := json.Marshal(manifest)
	require.NoError(t, err)
	manifestDigest := string(godigest.FromBytes(manifestData))

	src := &fakeSource{
		manifests: map[string]blob{manifestDigest: {manifestData, ocispec.MediaTypeImageManifest}},
		blobs: map[string]blob{
			configDigest: {configData, ocispec.MediaTypeImageConfig},
			layerDigest:  {layerData, ocispec.MediaTypeImageLayerGzip},
		},
	}

	w := NewWorker(Config{
		Targets: []Target{{
			Name:        "test",
			Endpoint:    srv.URL,
			Auth:        authNone,
			StripPrefix: testStripPrefix,
		}},
		Source:    src,
		QueueSize: 10,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	w.process(context.Background(), job{
		targetIdx: 0,
		repo:      testLocalRepo,
		tag:       "v1",
		digest:    manifestDigest,
		mediaType: ocispec.MediaTypeImageManifest,
	})

	target.mu.Lock()
	defer target.mu.Unlock()
	require.True(t, target.blobs[configDigest], "config blob should be pushed")
	require.True(t, target.blobs[layerDigest], "layer blob should be pushed")
	require.Contains(t, target.manifests, "team/app/manifests/"+manifestDigest, "manifest pushed by digest")
	require.Contains(t, target.manifests, "team/app/manifests/v1", "manifest pushed by tag")

	st := w.Status()
	require.Len(t, st, 1)
	require.Equal(t, int64(1), st[0].Replicated)
	require.Equal(t, int64(0), st[0].Failed)
	require.Empty(t, st[0].LastError)
}

func TestWorkerSkipsExistingBlob(t *testing.T) {
	target := newTargetRegistry()
	configData := []byte(`{}`)
	configDigest := string(godigest.FromBytes(configData))
	target.blobs[configDigest] = true // pretend it already exists

	srv := httptest.NewServer(target.handler())
	defer srv.Close()

	manifest := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: ocispec.Descriptor{Digest: godigest.Digest(configDigest), Size: int64(len(configData))}}
	manifest.SchemaVersion = 2
	manifestData, _ := json.Marshal(manifest)
	manifestDigest := string(godigest.FromBytes(manifestData))

	src := &fakeSource{
		manifests: map[string]blob{manifestDigest: {manifestData, ocispec.MediaTypeImageManifest}},
		blobs:     map[string]blob{configDigest: {configData, ocispec.MediaTypeImageConfig}},
	}
	w := NewWorker(Config{
		Targets:   []Target{{Name: "t", Endpoint: srv.URL, Auth: authNone}},
		Source:    src,
		QueueSize: 10,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := w.replicate(context.Background(), 0, "app", "", manifestDigest, ocispec.MediaTypeImageManifest, map[string]bool{})
	require.NoError(t, err)
	require.Contains(t, target.manifests, "app/manifests/"+manifestDigest)
}

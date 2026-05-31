package replication

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// addImage registers an image manifest (config + one layer) in src and returns
// its digest.
func addImage(src *fakeSource, config, layer []byte) string {
	cd := string(godigest.FromBytes(config))
	ld := string(godigest.FromBytes(layer))
	m := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: godigest.Digest(cd), Size: int64(len(config))},
		Layers:    []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: godigest.Digest(ld), Size: int64(len(layer))}},
	}
	m.SchemaVersion = 2
	data, _ := json.Marshal(m)
	md := string(godigest.FromBytes(data))
	src.manifests[md] = blob{data, ocispec.MediaTypeImageManifest}
	src.blobs[cd] = blob{config, ocispec.MediaTypeImageConfig}
	src.blobs[ld] = blob{layer, ocispec.MediaTypeImageLayerGzip}
	return md
}

func TestWorkerReplicatesIndex(t *testing.T) {
	src := &fakeSource{manifests: map[string]blob{}, blobs: map[string]blob{}}
	child1 := addImage(src, []byte(`{"arch":"amd64"}`), []byte("layer-amd64"))
	child2 := addImage(src, []byte(`{"arch":"arm64"}`), []byte("layer-arm64"))

	index := ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{MediaType: ocispec.MediaTypeImageManifest, Digest: godigest.Digest(child1)},
			{MediaType: ocispec.MediaTypeImageManifest, Digest: godigest.Digest(child2)},
		},
	}
	index.SchemaVersion = 2
	indexData, _ := json.Marshal(index)
	indexDigest := string(godigest.FromBytes(indexData))
	src.manifests[indexDigest] = blob{indexData, ocispec.MediaTypeImageIndex}

	target := newTargetRegistry()
	srv := httptest.NewServer(target.handler())
	defer srv.Close()

	w := NewWorker(Config{
		Targets:   []Target{{Name: "t", Endpoint: srv.URL, Auth: authNone}},
		Source:    src,
		QueueSize: 10,
	}, discardLogger())

	err := w.replicate(context.Background(), 0, "app", "latest", indexDigest, ocispec.MediaTypeImageIndex, map[string]bool{})
	require.NoError(t, err)

	target.mu.Lock()
	defer target.mu.Unlock()
	// Both child manifests, the index (by digest + tag), and every layer/config.
	require.Contains(t, target.manifests, "app/manifests/"+child1)
	require.Contains(t, target.manifests, "app/manifests/"+child2)
	require.Contains(t, target.manifests, "app/manifests/"+indexDigest)
	require.Contains(t, target.manifests, "app/manifests/latest")
	require.Len(t, target.blobs, 4, "two configs + two layers")
}

// failTarget serves /v2/ and HEAD/blobs but lets the test inject failures for
// the upload-start and manifest-put steps.
type failTarget struct {
	emptyLocation bool
	manifestPUT   int // status code for PUT manifests (0 => 201)
}

func (f failTarget) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(testV2Root, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == testV2Root:
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusNotFound) // blob missing -> triggers upload
		case r.Method == http.MethodPost: // start upload
			if !f.emptyLocation {
				w.Header().Set("Location", p+"uuid")
			}
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && strings.Contains(p, "/manifests/"):
			code := f.manifestPUT
			if code == 0 {
				code = http.StatusCreated
			}
			w.WriteHeader(code)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	})
	return mux
}

func TestWorkerEmptyLocationFails(t *testing.T) {
	src := &fakeSource{manifests: map[string]blob{}, blobs: map[string]blob{}}
	digest := addImage(src, []byte(`{}`), []byte("layer"))

	srv := httptest.NewServer(failTarget{emptyLocation: true}.handler())
	defer srv.Close()

	w := NewWorker(Config{
		Targets:   []Target{{Name: "t", Endpoint: srv.URL, Auth: authNone}},
		Source:    src,
		QueueSize: 10,
	}, discardLogger())

	err := w.replicate(context.Background(), 0, "app", "", digest, ocispec.MediaTypeImageManifest, map[string]bool{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Location")
}

func TestWorkerManifestPutFailureRecordsStatus(t *testing.T) {
	src := &fakeSource{manifests: map[string]blob{}, blobs: map[string]blob{}}
	digest := addImage(src, []byte(`{}`), []byte("layer"))

	srv := httptest.NewServer(failTarget{manifestPUT: http.StatusInternalServerError}.handler())
	defer srv.Close()

	w := NewWorker(Config{
		Targets:   []Target{{Name: "t", Endpoint: srv.URL, Auth: authNone}},
		Source:    src,
		QueueSize: 10,
	}, discardLogger())

	w.process(context.Background(), job{targetIdx: 0, repo: "app", tag: "v1", digest: digest, mediaType: ocispec.MediaTypeImageManifest})

	st := w.Status()
	require.Len(t, st, 1)
	require.Equal(t, int64(0), st[0].Replicated)
	require.Equal(t, int64(1), st[0].Failed)
	require.NotEmpty(t, st[0].LastError)
}

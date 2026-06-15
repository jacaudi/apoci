package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGoFetcher_EscapesVersion verifies that module versions are bang-escaped on
// the wire (uppercase L -> !l) per the GOPROXY protocol, otherwise upstreams 404
// on any version containing uppercase letters.
func TestGoFetcher_EscapesVersion(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	f := NewGoFetcher([]string{srv.URL}, 5*time.Second, 1<<20)

	if _, err := f.FetchInfo(context.Background(), "example.com/mod", "v2.0.0-Beta"); err != nil {
		t.Fatalf("FetchInfo: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/@v/v2.0.0-!beta.info") {
		t.Fatalf("version not escaped, got path %q", gotPath)
	}

	if _, err := f.FetchZip(context.Background(), "example.com/mod", "v2.0.0-Beta"); err != nil {
		t.Fatalf("FetchZip: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/@v/v2.0.0-!beta.zip") {
		t.Fatalf("version not escaped, got path %q", gotPath)
	}
}

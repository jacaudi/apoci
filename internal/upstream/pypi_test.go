package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPyPIFetchProjectParsesPEP691(t *testing.T) {
	var gotPath, gotAccept string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", pep691MediaType)
		_, _ = w.Write([]byte(`{"files":[{"filename":"foo_bar-1.0-py3-none-any.whl",` +
			`"url":"` + srv.URL + `/files/foo_bar-1.0-py3-none-any.whl",` +
			`"hashes":{"sha256":"abc123"},` +
			`"requires-python":">=3.9"}]}`))
	}))
	defer srv.Close()

	f := NewPyPIFetcher([]string{srv.URL}, 5*time.Second, 1<<20)

	proj, err := f.FetchProject(context.Background(), "foo-bar")
	require.NoError(t, err)
	require.Equal(t, "/simple/foo-bar/", gotPath)
	require.Contains(t, gotAccept, "application/vnd.pypi.simple.v1+json")

	require.Len(t, proj.Files, 1)
	file := proj.Files[0]
	require.Equal(t, "foo_bar-1.0-py3-none-any.whl", file.Filename)
	require.Equal(t, srv.URL+"/files/foo_bar-1.0-py3-none-any.whl", file.URL)
	require.Equal(t, map[string]string{"sha256": "abc123"}, file.Hashes)
	require.Equal(t, ">=3.9", file.RequiresPython)
}

func TestPyPIFetchProjectAll404ReturnsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := NewPyPIFetcher([]string{srv.URL}, 5*time.Second, 1<<20)

	_, err := f.FetchProject(context.Background(), "nonexistent")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrProjectNotFound))
}

func TestPyPIFetchProjectWalksUpstreamsInOrder(t *testing.T) {
	var queried []string

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queried = append(queried, "first")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer first.Close()

	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queried = append(queried, "second")
		w.Header().Set("Content-Type", pep691MediaType)
		_, _ = w.Write([]byte(`{"files":[{"filename":"pkg-1.0.tar.gz","url":"https://example.com/pkg-1.0.tar.gz","hashes":{},"requires-python":""}]}`))
	}))
	defer second.Close()

	f := NewPyPIFetcher([]string{first.URL, second.URL}, 5*time.Second, 1<<20)

	proj, err := f.FetchProject(context.Background(), "pkg")
	require.NoError(t, err)
	require.Equal(t, []string{"first", "second"}, queried)
	require.Len(t, proj.Files, 1)
	require.Equal(t, "pkg-1.0.tar.gz", proj.Files[0].Filename)
}

func TestPyPIFetchProjectIndexSizeCap(t *testing.T) {
	oversized := strings.Repeat("a", (1<<20)+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", pep691MediaType)
		_, _ = w.Write([]byte(`{"files":[{"filename":"x","url":"y","hashes":{},"requires-python":"` + oversized + `"}]}`))
	}))
	defer srv.Close()

	f := NewPyPIFetcher([]string{srv.URL}, 5*time.Second, 1<<20)

	_, err := f.FetchProject(context.Background(), "big")
	require.Error(t, err)
	require.Contains(t, err.Error(), "max size")
}

func TestPyPIFetchFileBoundedAndFetched(t *testing.T) {
	data := []byte("0123456789")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/files/x.whl", r.URL.Path)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	f := NewPyPIFetcher([]string{srv.URL}, 5*time.Second, 1<<20)

	got, err := f.FetchFile(context.Background(), srv.URL+"/files/x.whl")
	require.NoError(t, err)
	require.Equal(t, data, got)

	small := NewPyPIFetcher([]string{srv.URL}, 5*time.Second, 5)
	_, err = small.FetchFile(context.Background(), srv.URL+"/files/x.whl")
	require.Error(t, err)
	require.Contains(t, err.Error(), "max size")
}

func TestPyPIFetcherDisabledWithNoUpstreams(t *testing.T) {
	f := NewPyPIFetcher(nil, 5*time.Second, 1<<20)
	require.False(t, f.Enabled())
}

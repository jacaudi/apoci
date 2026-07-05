package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/version"
)

// pypiIndexMaxBytes bounds a simple-index metadata response. Project indexes
// are small; the multi-hundred-MiB cap for distribution files must not apply
// to metadata.
const pypiIndexMaxBytes = 1 << 20

const pep691MediaType = "application/vnd.pypi.simple.v1+json"

// ErrProjectNotFound means every configured upstream returned 404 for the
// project — the caller should surface its normal not-found path.
var ErrProjectNotFound = errors.New("project not found upstream")

// PyPIProjectFile is one distribution file in a PEP 691 project response.
type PyPIProjectFile struct {
	Filename       string            `json:"filename"`
	URL            string            `json:"url"`
	Hashes         map[string]string `json:"hashes"`
	RequiresPython string            `json:"requires-python"`
}

// PyPIProject is the subset of a PEP 691 project response apoci consumes.
type PyPIProject struct {
	Files []PyPIProjectFile `json:"files"`
}

// PyPIFetcher pulls project metadata and distribution files from upstream
// PEP 691 simple indexes (pypi.org), walking the configured list in order.
// Same shape as GoFetcher: no auth, own circuit breaker, coalesced fetches.
type PyPIFetcher struct {
	client  *http.Client
	bases   []string
	maxFile int64
	circuit *circuitBreaker
	sf      singleflight.Group
}

// NewPyPIFetcher creates a PyPI upstream fetcher. bases are index roots such
// as "https://pypi.org"; an empty list yields a fetcher that always misses.
func NewPyPIFetcher(bases []string, fetchTimeout time.Duration, maxFileSize int64) *PyPIFetcher {
	cleaned := make([]string, 0, len(bases))
	for _, b := range bases {
		if b = strings.TrimRight(strings.TrimSpace(b), "/"); b != "" {
			cleaned = append(cleaned, b)
		}
	}
	return &PyPIFetcher{
		client:  newStreamingHTTPClient(fetchTimeout),
		bases:   cleaned,
		maxFile: maxFileSize,
		circuit: newCircuitBreaker(),
	}
}

// Enabled reports whether any upstream index is configured.
func (f *PyPIFetcher) Enabled() bool { return len(f.bases) > 0 }

// FetchProject returns the PEP 691 project metadata for a PEP 503-normalized
// name, coalescing concurrent identical fetches. All-404 → ErrProjectNotFound.
func (f *PyPIFetcher) FetchProject(ctx context.Context, normalizedName string) (*PyPIProject, error) {
	v, err, _ := f.sf.Do("index\x00"+normalizedName, func() (any, error) {
		return f.fetchProjectUncoalesced(ctx, normalizedName)
	})
	if err != nil {
		return nil, err
	}
	return v.(*PyPIProject), nil
}

func (f *PyPIFetcher) fetchProjectUncoalesced(ctx context.Context, name string) (*PyPIProject, error) {
	notFound := 0
	var lastErr error
	for _, base := range f.bases {
		reqURL := base + "/simple/" + url.PathEscape(name) + "/"
		resp, err := f.do(ctx, base, reqURL, pep691MediaType)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			notFound++
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("upstream %s returned %d", base, resp.StatusCode)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, pypiIndexMaxBytes+1))
		_ = resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("reading upstream %s: %w", base, err)
			continue
		}
		if int64(len(data)) > pypiIndexMaxBytes {
			return nil, fmt.Errorf("upstream index response exceeds max size")
		}
		var proj PyPIProject
		if err := json.Unmarshal(data, &proj); err != nil {
			lastErr = fmt.Errorf("parsing upstream %s index: %w", base, err)
			continue
		}
		return &proj, nil
	}
	if notFound == len(f.bases) && notFound > 0 {
		return nil, ErrProjectNotFound
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no upstream index configured")
	}
	return nil, lastErr
}

// FetchFile GETs a distribution file by the absolute URL the project index
// gave (pypi.org hosts files on files.pythonhosted.org), bounded by maxFile.
func (f *PyPIFetcher) FetchFile(ctx context.Context, fileURL string) ([]byte, error) {
	u, err := url.Parse(fileURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid upstream file URL %q", fileURL)
	}
	// Circuit-key by host: file hosts differ from index hosts.
	resp, err := f.do(ctx, u.Scheme+"://"+u.Host, fileURL, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream file %s returned %d", fileURL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, f.maxFile+1))
	if err != nil {
		return nil, fmt.Errorf("reading upstream file: %w", err)
	}
	if int64(len(data)) > f.maxFile {
		return nil, fmt.Errorf("upstream file exceeds max size")
	}
	return data, nil
}

// do issues one GET with the circuit breaker keyed by circuitKey, mirroring
// GoFetcher.do's failure accounting.
func (f *PyPIFetcher) do(ctx context.Context, circuitKey, reqURL, accept string) (*http.Response, error) {
	if f.circuit.isOpen(circuitKey) {
		return nil, fmt.Errorf("circuit open for upstream %s", circuitKey)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", version.UserAgent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		if f.circuit.recordFailure(circuitKey) {
			metrics.UpstreamCircuitOpen.WithLabelValues(circuitKey).Set(1)
		}
		return nil, fmt.Errorf("fetching %s: %w", reqURL, err)
	}
	if resp.StatusCode >= 500 {
		if f.circuit.recordFailure(circuitKey) {
			metrics.UpstreamCircuitOpen.WithLabelValues(circuitKey).Set(1)
		}
	} else if wasOpen := f.circuit.recordSuccess(circuitKey); wasOpen {
		metrics.UpstreamCircuitOpen.WithLabelValues(circuitKey).Set(0)
	}
	return resp, nil
}

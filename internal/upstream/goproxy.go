package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/sync/singleflight"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/version"
)

// newHTTPClient builds the HTTP client shared by the OCI and goproxy fetchers:
// a SafeDialContext transport (SSRF guard) with the configured fetch timeout.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: validate.SafeDialContext,
		},
	}
}

// newStreamingHTTPClient is for responses whose body is streamed to disk
// (blobs, module zips). It must NOT set http.Client.Timeout, which would cap
// the whole transfer and abort large legitimate downloads mid-stream; instead
// it bounds connection setup and time-to-first-byte and relies on the request
// context for the overall deadline.
func newStreamingHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           validate.SafeDialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
		},
	}
}

// GoFetcher proxies requests to upstream Go module proxies (GOPROXY protocol),
// walking the configured proxy list in order. It reuses the package's circuit
// breaker and the shared SafeDialContext HTTP client.
type GoFetcher struct {
	client    *http.Client
	proxies   []string
	maxModule int64
	circuit   *circuitBreaker
	sf        singleflight.Group
}

// NewGoFetcher creates a Go module proxy fetcher. proxies are base URLs such as
// "https://proxy.golang.org"; an empty list yields a fetcher that always misses.
func NewGoFetcher(proxies []string, fetchTimeout time.Duration, maxModuleSize int64) *GoFetcher {
	cleaned := make([]string, 0, len(proxies))
	for _, p := range proxies {
		if p = strings.TrimRight(strings.TrimSpace(p), "/"); p != "" {
			cleaned = append(cleaned, p)
		}
	}
	return &GoFetcher{
		client:    newStreamingHTTPClient(fetchTimeout),
		proxies:   cleaned,
		maxModule: maxModuleSize,
		circuit:   newCircuitBreaker(),
	}
}

// Enabled reports whether any upstream proxy is configured.
func (f *GoFetcher) Enabled() bool { return len(f.proxies) > 0 }

func (f *GoFetcher) FetchInfo(ctx context.Context, mod, ver string) ([]byte, error) {
	escVer, err := module.EscapeVersion(ver)
	if err != nil {
		return nil, fmt.Errorf("escaping version %q: %w", ver, err)
	}
	return f.fetchBytes(ctx, mod, "@v/"+escVer+".info")
}

func (f *GoFetcher) FetchList(ctx context.Context, mod string) ([]byte, error) {
	return f.fetchBytes(ctx, mod, "@v/list")
}

func (f *GoFetcher) FetchLatest(ctx context.Context, mod string) ([]byte, error) {
	return f.fetchBytes(ctx, mod, "@latest")
}

// FetchZip returns the module zip bytes, bounded by maxModule.
func (f *GoFetcher) FetchZip(ctx context.Context, mod, ver string) ([]byte, error) {
	escVer, err := module.EscapeVersion(ver)
	if err != nil {
		return nil, fmt.Errorf("escaping version %q: %w", ver, err)
	}
	return f.fetchBytes(ctx, mod, "@v/"+escVer+".zip")
}

// fetchBytes coalesces concurrent identical fetches (same module+suffix) so a
// burst of `go get` requests for the same path issues a single upstream call.
func (f *GoFetcher) fetchBytes(ctx context.Context, mod, suffix string) ([]byte, error) {
	v, err, _ := f.sf.Do(mod+"\x00"+suffix, func() (any, error) {
		return f.fetchBytesUncoalesced(ctx, mod, suffix)
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

// fetchBytesUncoalesced GETs an escaped module path + suffix from each proxy in
// turn, returning the first 200 body. The body is bounded by maxModule.
func (f *GoFetcher) fetchBytesUncoalesced(ctx context.Context, mod, suffix string) ([]byte, error) {
	var lastErr error
	for _, base := range f.proxies {
		resp, err := f.do(ctx, base, mod, suffix)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("upstream %s returned %d", base, resp.StatusCode)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, f.maxModule+1))
		_ = resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("reading upstream %s: %w", base, err)
			continue
		}
		if int64(len(data)) > f.maxModule {
			return nil, fmt.Errorf("upstream response exceeds max size")
		}
		return data, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no upstream proxy configured")
	}
	return nil, lastErr
}

// do issues a single GET to base + escaped(module) + "/" + suffix, applying the
// circuit breaker (keyed by proxy base) around the request.
func (f *GoFetcher) do(ctx context.Context, base, mod, suffix string) (*http.Response, error) {
	if f.circuit.isOpen(base) {
		return nil, fmt.Errorf("circuit open for upstream %s", base)
	}

	escaped, err := module.EscapePath(mod)
	if err != nil {
		return nil, fmt.Errorf("escaping module path %q: %w", mod, err)
	}
	reqURL := base + "/" + escaped + "/" + suffix

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", version.UserAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		if f.circuit.recordFailure(base) {
			metrics.UpstreamCircuitOpen.WithLabelValues(base).Set(1)
		}
		return nil, fmt.Errorf("fetching %s: %w", reqURL, err)
	}
	if resp.StatusCode >= 500 {
		if f.circuit.recordFailure(base) {
			metrics.UpstreamCircuitOpen.WithLabelValues(base).Set(1)
		}
	} else if wasOpen := f.circuit.recordSuccess(base); wasOpen {
		metrics.UpstreamCircuitOpen.WithLabelValues(base).Set(0)
	}
	return resp, nil
}

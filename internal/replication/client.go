// Package replication pushes locally-pushed artifacts out to non-apoci OCI
// registries (GHCR, Quay, ...) as they arrive — the outbound counterpart to
// upstream proxying.
package replication

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/version"
)

const (
	authNone  = "none"
	authBasic = "basic"
	authToken = "token"
)

// Target is the push destination configuration.
type Target struct {
	Name          string
	Endpoint      string
	Auth          string
	Username      string
	Password      string
	Insecure      bool
	RepoGlobs     []string
	StripPrefix   string
	DestNamespace string
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// Client pushes blobs and manifests to one OCI distribution target.
type Client struct {
	target Target
	http   *http.Client

	mu        sync.Mutex
	challenge *challenge // discovered once, lazily
	tokens    map[string]cachedToken
}

type challenge struct {
	realm   string
	service string
}

func NewClient(t Target, timeout time.Duration) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Block dials to private/link-local IPs (incl. the remote-supplied token
	// realm), matching the upstream fetcher. Honors validate.AllowPrivateIPs.
	transport.DialContext = validate.SafeDialContext
	if t.Insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // operator opted in via target.insecure
	}
	return &Client{
		target: t,
		http:   &http.Client{Timeout: timeout, Transport: transport},
		tokens: make(map[string]cachedToken),
	}
}

// BlobExists reports whether the target already holds the blob.
func (c *Client) BlobExists(ctx context.Context, repo, digest string) (bool, error) {
	req, err := c.newRequest(ctx, http.MethodHead, fmt.Sprintf("/v2/%s/blobs/%s", repo, digest), nil)
	if err != nil {
		return false, err
	}
	if err := c.authorize(ctx, req, repo); err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("blob HEAD %s: status %d", digest, resp.StatusCode)
	}
}

// PushBlob uploads a blob via the two-step monolithic upload flow.
func (c *Client) PushBlob(ctx context.Context, repo, digest string, size int64, body io.Reader) error {
	start, err := c.newRequest(ctx, http.MethodPost, fmt.Sprintf("/v2/%s/blobs/uploads/", repo), nil)
	if err != nil {
		return err
	}
	if err := c.authorize(ctx, start, repo); err != nil {
		return err
	}
	resp, err := c.http.Do(start)
	if err != nil {
		return fmt.Errorf("starting blob upload: %w", err)
	}
	location := resp.Header.Get("Location")
	drain(resp)
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("starting blob upload: status %d", resp.StatusCode)
	}

	uploadURL, err := c.resolveUpload(location, digest)
	if err != nil {
		return err
	}

	put, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, body)
	if err != nil {
		return err
	}
	put.Header.Set("Content-Type", "application/octet-stream")
	put.Header.Set("User-Agent", version.UserAgent)
	put.ContentLength = size
	if err := c.authorize(ctx, put, repo); err != nil {
		return err
	}
	putResp, err := c.http.Do(put)
	if err != nil {
		return fmt.Errorf("uploading blob: %w", err)
	}
	defer drain(putResp)
	if putResp.StatusCode != http.StatusCreated {
		return fmt.Errorf("uploading blob %s: status %d", digest, putResp.StatusCode)
	}
	return nil
}

// PutManifest uploads a manifest under the given reference (tag or digest).
func (c *Client) PutManifest(ctx context.Context, repo, reference, mediaType string, body []byte) error {
	req, err := c.newRequest(ctx, http.MethodPut, fmt.Sprintf("/v2/%s/manifests/%s", repo, reference), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mediaType)
	req.ContentLength = int64(len(body))
	if err := c.authorize(ctx, req, repo); err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("putting manifest: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("putting manifest %s: status %d", reference, resp.StatusCode)
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.target.Endpoint, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", version.UserAgent)
	return req, nil
}

// resolveUpload turns the upload Location (possibly relative) into an absolute
// URL with the digest query parameter appended.
func (c *Client) resolveUpload(location, digest string) (string, error) {
	if location == "" {
		return "", fmt.Errorf("blob upload: empty Location header")
	}
	base, err := url.Parse(strings.TrimRight(c.target.Endpoint, "/"))
	if err != nil {
		return "", err
	}
	loc, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("parsing upload Location: %w", err)
	}
	abs := base.ResolveReference(loc)
	q := abs.Query()
	q.Set("digest", digest)
	abs.RawQuery = q.Encode()
	return abs.String(), nil
}

func drain(resp *http.Response) {
	// Fully consume the body so the transport can reuse the keep-alive
	// connection. These are the target registry's control-API responses, not
	// attacker-streamed blobs, so they are safe to read to completion.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func (c *Client) authorize(ctx context.Context, req *http.Request, repo string) error {
	switch c.target.Auth {
	case authNone:
		return nil
	case authBasic:
		req.SetBasicAuth(c.target.Username, c.target.Password)
		return nil
	case authToken:
		token, err := c.getToken(ctx, repo)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	default:
		return fmt.Errorf("unknown auth type %q", c.target.Auth)
	}
}

// getToken fetches a pull,push-scoped bearer token via the Docker registry v2
// token flow, caching per repo until shortly before expiry.
func (c *Client) getToken(ctx context.Context, repo string) (string, error) {
	c.mu.Lock()
	if ct, ok := c.tokens[repo]; ok && time.Now().Before(ct.expiresAt) {
		c.mu.Unlock()
		return ct.token, nil
	}
	c.mu.Unlock()

	ch, err := c.discoverChallenge(ctx)
	if err != nil {
		return "", err
	}

	tokenURL, err := url.Parse(ch.realm)
	if err != nil {
		return "", fmt.Errorf("parsing realm %q: %w", ch.realm, err)
	}
	q := tokenURL.Query()
	if ch.service != "" {
		q.Set("service", ch.service)
	}
	q.Set("scope", fmt.Sprintf("repository:%s:pull,push", repo))
	tokenURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", version.UserAgent)
	if c.target.Username != "" {
		req.SetBasicAuth(c.target.Username, c.target.Password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching token: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decoding token: %w", err)
	}
	token := tr.Token
	if token == "" {
		token = tr.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("empty token in response")
	}
	expiresIn := tr.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 60
	}
	c.mu.Lock()
	c.tokens[repo] = cachedToken{token: token, expiresAt: time.Now().Add(time.Duration(expiresIn-5) * time.Second)}
	c.mu.Unlock()
	return token, nil
}

// discoverChallenge reads the Bearer realm/service from a GET /v2/ 401 response.
func (c *Client) discoverChallenge(ctx context.Context) (*challenge, error) {
	c.mu.Lock()
	if c.challenge != nil {
		ch := c.challenge
		c.mu.Unlock()
		return ch, nil
	}
	c.mu.Unlock()

	req, err := c.newRequest(ctx, http.MethodGet, "/v2/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probing /v2/: %w", err)
	}
	hdr := resp.Header.Get("WWW-Authenticate")
	drain(resp)

	ch := parseChallenge(hdr)
	if ch.realm == "" {
		return nil, fmt.Errorf("no Bearer realm in WWW-Authenticate challenge")
	}
	c.mu.Lock()
	c.challenge = &ch
	c.mu.Unlock()
	return &ch, nil
}

// parseChallenge extracts realm and service from a `Bearer realm="...",service="..."` header.
func parseChallenge(header string) challenge {
	var ch challenge
	rest, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return ch
	}
	for part := range strings.SplitSeq(rest, ",") {
		key, val, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		val = strings.Trim(val, `"`)
		switch key {
		case "realm":
			ch.realm = val
		case "service":
			ch.service = val
		}
	}
	return ch
}

// DestRepo maps a local repo path to the target repo path using the configured
// strip prefix and destination namespace.
func (t Target) DestRepo(localRepo string) string {
	repo := localRepo
	if t.StripPrefix != "" {
		repo = strings.TrimPrefix(repo, t.StripPrefix)
	}
	repo = strings.Trim(repo, "/")
	if t.DestNamespace != "" {
		repo = strings.Trim(t.DestNamespace, "/") + "/" + repo
	}
	return repo
}

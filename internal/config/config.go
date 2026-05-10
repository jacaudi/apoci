package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	cenv "github.com/caarlos0/env/v11"
	"gopkg.in/yaml.v3"
)

const (
	AutoAcceptNone   = "none"
	AutoAcceptMutual = "mutual"
	AutoAcceptAll    = "all"

	StorageTypeLocal = "local"
	StorageTypeS3    = "s3"
)

type Config struct {
	Endpoint      string `yaml:"endpoint"      env:"APOCI_ENDPOINT"`
	Name          string `yaml:"name"          env:"APOCI_NAME"`
	Listen        string `yaml:"listen"        env:"APOCI_LISTEN"`
	DataDir       string `yaml:"dataDir"       env:"APOCI_DATA_DIR"`
	KeyPath       string `yaml:"keyPath"       env:"APOCI_KEY_PATH"`
	LogLevel      string `yaml:"logLevel"      env:"APOCI_LOG_LEVEL"`
	LogFormat     string `yaml:"logFormat"     env:"APOCI_LOG_FORMAT"`
	ImmutableTags string `yaml:"immutableTags" env:"APOCI_IMMUTABLE_TAGS"`
	RegistryToken string `yaml:"registryToken" env:"APOCI_REGISTRY_TOKEN"`
	AdminToken    string `yaml:"adminToken"    env:"APOCI_ADMIN_TOKEN"`
	AccountDomain string `yaml:"accountDomain" env:"APOCI_ACCOUNT_DOMAIN"`

	Database Database `yaml:"database"   envPrefix:"APOCI_DB_"`
	Storage  Storage  `yaml:"storage"    envPrefix:"APOCI_STORAGE_"`
	TLS      *TLS     `yaml:"tls,omitempty"`

	Peering       Peering       `yaml:"peering"    envPrefix:"APOCI_PEERING_"`
	Federation    Federation    `yaml:"federation" envPrefix:"APOCI_FEDERATION_"`
	Limits        Limits        `yaml:"limits"     envPrefix:"APOCI_"`
	RateLimits    RateLimits    `yaml:"rateLimits" envPrefix:"APOCI_RATELIMIT_"`
	Metrics       Metrics       `yaml:"metrics"       envPrefix:"APOCI_METRICS_"`
	GC            GC            `yaml:"gc"            envPrefix:"APOCI_GC_"`
	Notifications Notifications `yaml:"notifications" envPrefix:"APOCI_NOTIFICATIONS_"`
	Upstreams     Upstreams     `yaml:"upstreams"     envPrefix:"APOCI_UPSTREAMS_"`
	UI            UI            `yaml:"ui"            envPrefix:"APOCI_UI_"`
	Backends      Backends      `yaml:"backends"      envPrefix:"APOCI_BACKENDS_"`

	Domain string `yaml:"-" env:"-"`
}

type Storage struct {
	Type string    `yaml:"type" env:"TYPE"` // "local" (default) or "s3"
	S3   S3Storage `yaml:"s3"   envPrefix:"S3_"`
}

type S3Storage struct {
	Bucket         string `yaml:"bucket"         env:"BUCKET"`
	Region         string `yaml:"region"         env:"REGION"`
	Endpoint       string `yaml:"endpoint"       env:"ENDPOINT"` // custom endpoint for S3-compatible stores (e.g. MinIO)
	AccessKey      string `yaml:"accessKey"      env:"ACCESS_KEY"`
	SecretKey      string `yaml:"secretKey"      env:"SECRET_KEY"`
	Prefix         string `yaml:"prefix"         env:"PREFIX"`           // key prefix inside the bucket
	ForcePathStyle bool   `yaml:"forcePathStyle" env:"FORCE_PATH_STYLE"` // required for MinIO and some compatible stores
	TempDir        string `yaml:"tempDir"        env:"TEMP_DIR"`         // upload staging dir; defaults to os.TempDir()
}

type Database struct {
	Driver       string `yaml:"driver"       env:"DRIVER"`         // "sqlite" (default) or "postgres"
	DSN          string `yaml:"dsn"          env:"DSN"`            // connection string; required for postgres, ignored for sqlite
	MaxOpenConns int    `yaml:"maxOpenConns" env:"MAX_OPEN_CONNS"` // max open connections (0 = driver default: 4 for sqlite, 25 for postgres)
	MaxIdleConns int    `yaml:"maxIdleConns" env:"MAX_IDLE_CONNS"` // max idle connections (0 = driver default: 4 for sqlite, 10 for postgres)
}

type TLS struct {
	Cert string `yaml:"cert" env:"APOCI_TLS_CERT"`
	Key  string `yaml:"key"  env:"APOCI_TLS_KEY"`
}

type Peering struct {
	HealthCheckInterval time.Duration `yaml:"healthCheckInterval" env:"HEALTH_CHECK_INTERVAL"`
	FetchTimeout        time.Duration `yaml:"fetchTimeout"        env:"FETCH_TIMEOUT"`
}

const (
	DefaultMaxManifestSize int64 = 10 * 1024 * 1024  // 10 MB
	DefaultMaxBlobSize     int64 = 512 * 1024 * 1024 // 512 MB
)

type Federation struct {
	AutoAccept                string        `yaml:"autoAccept"             env:"AUTO_ACCEPT"`                      // "none" (default), "mutual", "all"
	AllowInsecureHTTP         bool          `yaml:"allowInsecureHTTP"      env:"ALLOW_INSECURE_HTTP"`              // allow plain HTTP federation (testing only)
	AllowedDomains            []string      `yaml:"allowedDomains"         env:"ALLOWED_DOMAINS" envSeparator:","` // always auto-accept from these domains
	BlockedDomains            []string      `yaml:"blockedDomains"         env:"BLOCKED_DOMAINS" envSeparator:","` // silently drop all activities from these domains
	BlockedActors             []string      `yaml:"blockedActors"          env:"BLOCKED_ACTORS"  envSeparator:","` // silently drop all activities from these actor URLs
	OutgoingFollowPendingTTL  time.Duration `yaml:"outgoingFollowPendingTTL"  env:"OUTGOING_FOLLOW_PENDING_TTL"`   // TTL for pending outgoing follows (default: 7 days)
	OutgoingFollowRejectedTTL time.Duration `yaml:"outgoingFollowRejectedTTL" env:"OUTGOING_FOLLOW_REJECTED_TTL"`  // TTL for rejected outgoing follows (default: 24 hours)
}

type Limits struct {
	MaxManifestSize int64 `yaml:"maxManifestSize" env:"MAX_MANIFEST_SIZE"`
	MaxBlobSize     int64 `yaml:"maxBlobSize"     env:"MAX_BLOB_SIZE"`
}

type RateLimits struct {
	InboxRate         float64  `yaml:"inboxRate"         env:"INBOX_RATE"`                          // requests per second for inbox
	InboxBurst        int      `yaml:"inboxBurst"        env:"INBOX_BURST"`                         // burst capacity for inbox
	RegistryPushRate  float64  `yaml:"registryPushRate"  env:"REGISTRY_PUSH_RATE"`                  // requests per second for registry push
	RegistryPushBurst int      `yaml:"registryPushBurst" env:"REGISTRY_PUSH_BURST"`                 // burst capacity for registry push
	TrustedIPs        []string `yaml:"trustedIPs"        env:"TRUSTED_IPS"        envSeparator:","` // IPs/CIDRs that bypass rate limiting
}

type Metrics struct {
	Enabled bool   `yaml:"enabled" env:"ENABLED"`
	Listen  string `yaml:"listen"  env:"LISTEN"`
	Token   string `yaml:"token"   env:"TOKEN"`
}

type Notifications struct {
	URLs   []string `yaml:"urls"   env:"URLS"   envSeparator:","`
	Events []string `yaml:"events" env:"EVENTS" envSeparator:","`
}

type GC struct {
	Enabled                *bool         `yaml:"enabled"                env:"ENABLED"`
	Interval               time.Duration `yaml:"interval"               env:"INTERVAL"`
	StalePeerBlobAge       time.Duration `yaml:"stalePeerBlobAge"       env:"STALE_PEER_BLOB_AGE"`
	OrphanBatchSize        int           `yaml:"orphanBatchSize"        env:"ORPHAN_BATCH_SIZE"`
	BlobGCGracePeriod      time.Duration `yaml:"blobGCGracePeriod"      env:"BLOB_GC_GRACE_PERIOD"`
	UntaggedManifestAge    time.Duration `yaml:"untaggedManifestAge"    env:"UNTAGGED_MANIFEST_AGE"`
	UntaggedBatchSize      int           `yaml:"untaggedBatchSize"      env:"UNTAGGED_BATCH_SIZE"`
	RetentionTagsPerCycle  int           `yaml:"retentionTagsPerCycle"  env:"RETENTION_TAGS_PER_CYCLE"`
	DiskUsageThreshold     int           `yaml:"diskUsageThreshold"     env:"DISK_USAGE_THRESHOLD"`
	DiskUsageCheckInterval time.Duration `yaml:"diskUsageCheckInterval" env:"DISK_USAGE_CHECK_INTERVAL"`
	Retention              Retention     `yaml:"retention"              envPrefix:"RETENTION_"`
}

type Retention struct {
	KeepLastN   int               `yaml:"keepLastN"   env:"KEEP_LAST_N"`
	MaxAge      time.Duration     `yaml:"maxAge"      env:"MAX_AGE"`
	PinnedGlobs []string          `yaml:"pinnedGlobs" env:"PINNED_GLOBS" envSeparator:","`
	PerRepo     RepoRetentionList `yaml:"perRepo"     env:"PER_REPO"`
}

type RepoRetention struct {
	Repo        string        `yaml:"repo"        json:"repo"`
	KeepLastN   int           `yaml:"keepLastN"   json:"keepLastN,omitempty"`
	MaxAge      time.Duration `yaml:"maxAge"      json:"maxAge,omitempty"`
	PinnedGlobs []string      `yaml:"pinnedGlobs" json:"pinnedGlobs,omitempty"`
}

// UnmarshalJSON accepts maxAge as a duration string ("720h") in addition to the
// default nanosecond number, so JSON-from-env matches the YAML format.
func (r *RepoRetention) UnmarshalJSON(data []byte) error {
	type raw struct {
		Repo        string          `json:"repo"`
		KeepLastN   int             `json:"keepLastN"`
		MaxAge      json.RawMessage `json:"maxAge"`
		PinnedGlobs []string        `json:"pinnedGlobs"`
	}
	var x raw
	if err := json.Unmarshal(data, &x); err != nil {
		return err
	}
	r.Repo = x.Repo
	r.KeepLastN = x.KeepLastN
	r.PinnedGlobs = x.PinnedGlobs
	if len(x.MaxAge) == 0 || string(x.MaxAge) == "null" {
		return nil
	}
	if x.MaxAge[0] == '"' {
		var s string
		if err := json.Unmarshal(x.MaxAge, &s); err != nil {
			return fmt.Errorf("invalid maxAge: %w", err)
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid maxAge %q: %w", s, err)
		}
		r.MaxAge = d
		return nil
	}
	var ns int64
	if err := json.Unmarshal(x.MaxAge, &ns); err != nil {
		return fmt.Errorf("invalid maxAge: %w", err)
	}
	r.MaxAge = time.Duration(ns)
	return nil
}

type RepoRetentionList []RepoRetention

func (l *RepoRetentionList) UnmarshalText(text []byte) error {
	// Cast to a plain slice so json doesn't see UnmarshalText and reject the array.
	var slice []RepoRetention
	if err := json.Unmarshal(text, &slice); err != nil {
		return err
	}
	*l = slice
	return nil
}

// Upstream configures an external OCI registry for pull-through caching.
type Upstream struct {
	Name     string `yaml:"name"     env:"NAME"`     // registry name, e.g. "docker.io"
	Endpoint string `yaml:"endpoint" env:"ENDPOINT"` // registry URL, e.g. "https://registry-1.docker.io"
	Auth     string `yaml:"auth"     env:"AUTH"`     // "none", "basic", or "token" (default: "token")
	Username string `yaml:"username" env:"USERNAME"` // for basic/token auth
	Password string `yaml:"password" env:"PASSWORD"` // for basic/token auth
	Private  bool   `yaml:"private"  env:"PRIVATE"`  // require auth to pull images cached from this upstream
}

// String returns a string representation with the password redacted.
func (u Upstream) String() string {
	pass := ""
	if u.Password != "" {
		pass = "[REDACTED]"
	}
	return fmt.Sprintf("Upstream{Name:%s Endpoint:%s Auth:%s Username:%s Password:%s}",
		u.Name, u.Endpoint, u.Auth, u.Username, pass)
}

// UpstreamList is a slice of Upstream that can be parsed from a JSON env var.
type UpstreamList []Upstream

// UnmarshalText implements encoding.TextUnmarshaler so cenv can parse
// APOCI_UPSTREAMS_REGISTRIES as a JSON array of upstream registry configs.
func (u *UpstreamList) UnmarshalText(text []byte) error {
	return json.Unmarshal(text, u)
}

// Upstreams holds configuration for upstream registry proxying.
type Upstreams struct {
	Enabled      bool          `yaml:"enabled"      env:"ENABLED"`
	FetchTimeout time.Duration `yaml:"fetchTimeout" env:"FETCH_TIMEOUT"`
	Registries   UpstreamList  `yaml:"registries"   env:"REGISTRIES"`
}

// UI holds configuration for the web UI.
type UI struct {
	Enabled bool `yaml:"enabled" env:"ENABLED"`
}

// Backends configures the per-package-format registries (OCI is wired
// separately and always-on). Each block defaults to enabled with federation on.
type Backends struct {
	NPM   BackendConfig `yaml:"npm"   envPrefix:"NPM_"`
	Cargo BackendConfig `yaml:"cargo" envPrefix:"CARGO_"`
	PyPI  BackendConfig `yaml:"pypi"  envPrefix:"PYPI_"`
}

type BackendConfig struct {
	Enabled  *bool  `yaml:"enabled"  env:"ENABLED"`  // default true
	Federate *bool  `yaml:"federate" env:"FEDERATE"` // default true; false = no outbound publish, no adapter registration
	Token    string `yaml:"token"    env:"TOKEN"`    // optional override; falls back to the global RegistryToken
}

func (b BackendConfig) IsEnabled() bool {
	return b.Enabled == nil || *b.Enabled
}

func (c *Config) BlobDiskPath() string {
	if c.Storage.Type != StorageTypeLocal {
		return ""
	}
	return filepath.Join(c.DataDir, "blobs")
}

func (b BackendConfig) IsFederated() bool {
	return b.Federate == nil || *b.Federate
}

// TokenOr returns the per-backend Token, or fallback when unset.
func (b BackendConfig) TokenOr(fallback string) string {
	if b.Token != "" {
		return b.Token
	}
	return fallback
}

func Load(path string) (*Config, error) {
	cfg := &Config{}

	data, err := os.ReadFile(path) //nolint:gosec // config path is provided by operator
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("reading config file: %w", err)
		}
		// No config file — will rely on env vars and defaults.
	} else {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config file: %w", err)
		}
	}

	// Env vars override YAML values.
	if err := cenv.Parse(cfg); err != nil {
		return nil, fmt.Errorf("parsing environment variables: %w", err)
	}

	// Handle TLS env vars — the pointer may be nil from YAML.
	if cfg.TLS == nil {
		if os.Getenv("APOCI_TLS_CERT") != "" || os.Getenv("APOCI_TLS_KEY") != "" {
			cfg.TLS = &TLS{}
			if err := cenv.Parse(cfg.TLS); err != nil {
				return nil, fmt.Errorf("parsing TLS environment variables: %w", err)
			}
		}
	}

	if err := applyDefaults(cfg); err != nil {
		return nil, err
	}

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

func applyDefaults(cfg *Config) error {
	if err := applyServerDefaults(cfg); err != nil {
		return err
	}
	applyPeeringDefaults(cfg)
	applyLimitsDefaults(cfg)
	applyRateLimitsDefaults(cfg)
	applyGCDefaults(cfg)
	applyUpstreamDefaults(cfg)
	applyFederationDefaults(cfg)
	applyBackendsDefaults(cfg)
	return applyTokenDefaults(cfg)
}

func applyServerDefaults(cfg *Config) error {
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil {
			return fmt.Errorf("invalid endpoint URL: %w", err)
		}
		cfg.Domain = u.Hostname()
	}
	if cfg.Name == "" {
		cfg.Name = cfg.Domain
	}
	if cfg.Listen == "" {
		cfg.Listen = ":5000"
	}
	if cfg.DataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("determining home directory: %w", err)
		}
		cfg.DataDir = filepath.Join(home, ".apoci")
	}
	if cfg.KeyPath == "" {
		cfg.KeyPath = filepath.Join(cfg.DataDir, "ap.key")
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.LogFormat == "" {
		cfg.LogFormat = "json"
	}
	if cfg.Storage.Type == "" {
		cfg.Storage.Type = StorageTypeLocal
	}
	if cfg.Database.Driver == "" {
		cfg.Database.Driver = "sqlite"
	}
	if cfg.Metrics.Listen == "" {
		cfg.Metrics.Listen = ":9090"
	}
	if cfg.Federation.AutoAccept == "" {
		cfg.Federation.AutoAccept = AutoAcceptNone
	}
	if cfg.AccountDomain == "" {
		cfg.AccountDomain = cfg.Domain
	}
	if cfg.ImmutableTags == "" {
		cfg.ImmutableTags = `^v[0-9]`
	}
	return nil
}

func applyPeeringDefaults(cfg *Config) {
	if cfg.Peering.HealthCheckInterval == 0 {
		cfg.Peering.HealthCheckInterval = 30 * time.Second
	}
	if cfg.Peering.FetchTimeout == 0 {
		cfg.Peering.FetchTimeout = 60 * time.Second
	}
}

func applyFederationDefaults(cfg *Config) {
	if cfg.Federation.OutgoingFollowPendingTTL == 0 {
		cfg.Federation.OutgoingFollowPendingTTL = 7 * 24 * time.Hour // 7 days
	}
	if cfg.Federation.OutgoingFollowRejectedTTL == 0 {
		cfg.Federation.OutgoingFollowRejectedTTL = 24 * time.Hour // 24 hours
	}
}

func applyBackendsDefaults(cfg *Config) {
	for _, b := range []*BackendConfig{&cfg.Backends.NPM, &cfg.Backends.Cargo, &cfg.Backends.PyPI} {
		if b.Enabled == nil {
			t := true
			b.Enabled = &t
		}
		if b.Federate == nil {
			t := true
			b.Federate = &t
		}
	}
}

func applyLimitsDefaults(cfg *Config) {
	if cfg.Limits.MaxManifestSize == 0 {
		cfg.Limits.MaxManifestSize = DefaultMaxManifestSize
	}
	if cfg.Limits.MaxBlobSize == 0 {
		cfg.Limits.MaxBlobSize = DefaultMaxBlobSize
	}
}

func applyRateLimitsDefaults(cfg *Config) {
	if cfg.RateLimits.InboxRate == 0 {
		cfg.RateLimits.InboxRate = 30 // 30 req/sec
	}
	if cfg.RateLimits.InboxBurst == 0 {
		cfg.RateLimits.InboxBurst = 100
	}
	if cfg.RateLimits.RegistryPushRate == 0 {
		cfg.RateLimits.RegistryPushRate = 50 // 50 req/sec
	}
	if cfg.RateLimits.RegistryPushBurst == 0 {
		cfg.RateLimits.RegistryPushBurst = 100
	}
}

func applyGCDefaults(cfg *Config) {
	if cfg.GC.Enabled == nil {
		t := true
		cfg.GC.Enabled = &t
	}
	if cfg.GC.Interval == 0 {
		cfg.GC.Interval = 6 * time.Hour
	}
	if cfg.GC.StalePeerBlobAge == 0 {
		cfg.GC.StalePeerBlobAge = 30 * 24 * time.Hour
	}
	if cfg.GC.OrphanBatchSize == 0 {
		cfg.GC.OrphanBatchSize = 500
	}
	if cfg.GC.BlobGCGracePeriod == 0 {
		cfg.GC.BlobGCGracePeriod = time.Hour
	}
	if cfg.GC.UntaggedManifestAge == 0 {
		cfg.GC.UntaggedManifestAge = 7 * 24 * time.Hour
	}
	if cfg.GC.UntaggedBatchSize == 0 {
		cfg.GC.UntaggedBatchSize = 500
	}
	if cfg.GC.RetentionTagsPerCycle == 0 {
		cfg.GC.RetentionTagsPerCycle = 10000
	}
	if cfg.GC.DiskUsageCheckInterval == 0 {
		cfg.GC.DiskUsageCheckInterval = 5 * time.Minute
	}
	if cfg.GC.Retention.PinnedGlobs == nil {
		cfg.GC.Retention.PinnedGlobs = []string{"latest", "v*"}
	}
}

func applyUpstreamDefaults(cfg *Config) {
	if cfg.Upstreams.FetchTimeout == 0 {
		cfg.Upstreams.FetchTimeout = 60 * time.Second
	}
	for i := range cfg.Upstreams.Registries {
		if cfg.Upstreams.Registries[i].Auth == "" {
			cfg.Upstreams.Registries[i].Auth = "token"
		}
	}
}

func applyTokenDefaults(cfg *Config) error {
	if cfg.RegistryToken == "" {
		token, err := loadOrGenerateToken(filepath.Join(cfg.DataDir, "registry.token"))
		if err != nil {
			return fmt.Errorf("setting up registry token: %w", err)
		}
		cfg.RegistryToken = token
	}
	if cfg.AdminToken == "" {
		token, err := loadOrGenerateToken(filepath.Join(cfg.DataDir, "admin.token"))
		if err != nil {
			return fmt.Errorf("setting up admin token: %w", err)
		}
		cfg.AdminToken = token
	}
	return nil
}

func loadOrGenerateToken(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-controlled path
	if err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			return token, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("creating directory for token: %w", err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating random token: %w", err)
	}
	token := hex.EncodeToString(buf)

	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("writing token file: %w", err)
	}

	return token, nil
}

func validate(cfg *Config) error {
	if err := validateEndpoint(cfg); err != nil {
		return err
	}
	if err := validateStorage(cfg); err != nil {
		return err
	}
	if err := validateDatabase(cfg); err != nil {
		return err
	}
	if err := validateLogging(cfg); err != nil {
		return err
	}
	if err := validateFederation(cfg); err != nil {
		return err
	}
	if err := validateRateLimits(cfg); err != nil {
		return err
	}
	if err := validateRegex(cfg); err != nil {
		return err
	}
	if err := validateAccountDomain(cfg); err != nil {
		return err
	}
	if err := validateNonNegative(cfg); err != nil {
		return err
	}
	if err := validateNotificationEvents(cfg); err != nil {
		return err
	}
	if err := validateUpstreams(cfg); err != nil {
		return err
	}
	return nil
}

func validateEndpoint(cfg *Config) error {
	if cfg.Endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}
	if cfg.Domain == "" {
		return fmt.Errorf("could not derive domain from endpoint")
	}
	endpointScheme := strings.ToLower(strings.SplitN(cfg.Endpoint, "://", 2)[0])
	if endpointScheme != "https" && endpointScheme != "http" {
		return fmt.Errorf("endpoint scheme must be 'https' or 'http', got %q", endpointScheme)
	}
	return nil
}

func validateStorage(cfg *Config) error {
	validStorageTypes := map[string]bool{StorageTypeLocal: true, StorageTypeS3: true}
	if !validStorageTypes[cfg.Storage.Type] {
		return fmt.Errorf("storage.type must be 'local' or 's3'")
	}
	if cfg.Storage.Type == StorageTypeS3 {
		if cfg.Storage.S3.Bucket == "" {
			return fmt.Errorf("storage.s3.bucket is required when storage.type is 's3'")
		}
		if cfg.Storage.S3.Region == "" && cfg.Storage.S3.Endpoint == "" {
			return fmt.Errorf("storage.s3.region is required when no custom endpoint is configured")
		}
		if cfg.Storage.S3.Endpoint != "" {
			if _, err := url.ParseRequestURI(cfg.Storage.S3.Endpoint); err != nil {
				return fmt.Errorf("storage.s3.endpoint is not a valid URL: %w", err)
			}
		}
	}
	return nil
}

func validateDatabase(cfg *Config) error {
	validDrivers := map[string]bool{"sqlite": true, "postgres": true}
	if !validDrivers[cfg.Database.Driver] {
		return fmt.Errorf("database.driver must be 'sqlite' or 'postgres'")
	}
	if cfg.Database.Driver == "postgres" && cfg.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required when driver is 'postgres'")
	}
	return nil
}

func validateLogging(cfg *Config) error {
	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[cfg.LogLevel] {
		return fmt.Errorf("logLevel must be one of: debug, info, warn, error")
	}
	validFormats := map[string]bool{"json": true, "text": true}
	if !validFormats[cfg.LogFormat] {
		return fmt.Errorf("logFormat must be 'json' or 'text'")
	}
	return nil
}

func validateFederation(cfg *Config) error {
	validAutoAccept := map[string]bool{AutoAcceptNone: true, AutoAcceptMutual: true, AutoAcceptAll: true}
	if !validAutoAccept[cfg.Federation.AutoAccept] {
		return fmt.Errorf("federation.autoAccept must be 'none', 'mutual', or 'all'")
	}
	return nil
}

func validateRegex(cfg *Config) error {
	if cfg.ImmutableTags != "" {
		if _, err := regexp.Compile(cfg.ImmutableTags); err != nil {
			return fmt.Errorf("invalid immutableTags regex: %w", err)
		}
	}
	return nil
}

func validateAccountDomain(cfg *Config) error {
	if cfg.AccountDomain != cfg.Domain {
		if strings.Contains(cfg.AccountDomain, "/") || strings.Contains(cfg.AccountDomain, ":") {
			return fmt.Errorf("accountDomain must be a bare hostname (no scheme, port, or path)")
		}
	}
	return nil
}

func validateRateLimits(cfg *Config) error {
	if cfg.RateLimits.InboxRate < 0 {
		return fmt.Errorf("rateLimits.inboxRate must not be negative")
	}
	if cfg.RateLimits.InboxBurst < 0 {
		return fmt.Errorf("rateLimits.inboxBurst must not be negative")
	}
	if cfg.RateLimits.RegistryPushRate < 0 {
		return fmt.Errorf("rateLimits.registryPushRate must not be negative")
	}
	if cfg.RateLimits.RegistryPushBurst < 0 {
		return fmt.Errorf("rateLimits.registryPushBurst must not be negative")
	}
	for i, entry := range cfg.RateLimits.TrustedIPs {
		if strings.Contains(entry, "/") {
			if _, _, err := net.ParseCIDR(entry); err != nil {
				return fmt.Errorf("rateLimits.trustedIPs[%d] is not a valid CIDR: %w", i, err)
			}
		} else {
			if net.ParseIP(entry) == nil {
				return fmt.Errorf("rateLimits.trustedIPs[%d] is not a valid IP address", i)
			}
		}
	}
	return nil
}

func validateNonNegative(cfg *Config) error {
	if cfg.Limits.MaxManifestSize < 0 {
		return fmt.Errorf("limits.maxManifestSize must not be negative")
	}
	if cfg.Limits.MaxBlobSize < 0 {
		return fmt.Errorf("limits.maxBlobSize must not be negative")
	}
	if cfg.Peering.HealthCheckInterval < 0 {
		return fmt.Errorf("peering.healthCheckInterval must not be negative")
	}
	if cfg.Peering.FetchTimeout < 0 {
		return fmt.Errorf("peering.fetchTimeout must not be negative")
	}
	if cfg.GC.Interval < 0 {
		return fmt.Errorf("gc.interval must not be negative")
	}
	if cfg.GC.StalePeerBlobAge < 0 {
		return fmt.Errorf("gc.stalePeerBlobAge must not be negative")
	}
	if cfg.GC.OrphanBatchSize < 0 {
		return fmt.Errorf("gc.orphanBatchSize must not be negative")
	}
	if cfg.GC.BlobGCGracePeriod < 0 {
		return fmt.Errorf("gc.blobGCGracePeriod must not be negative")
	}
	if cfg.GC.DiskUsageThreshold < 0 || cfg.GC.DiskUsageThreshold > 100 {
		return fmt.Errorf("gc.diskUsageThreshold must be between 0 and 100")
	}
	if cfg.GC.DiskUsageCheckInterval < 0 {
		return fmt.Errorf("gc.diskUsageCheckInterval must not be negative")
	}
	seen := make(map[string]bool)
	for i, r := range cfg.GC.Retention.PerRepo {
		if r.Repo == "" {
			return fmt.Errorf("gc.retention.perRepo[%d].repo is required", i)
		}
		if seen[r.Repo] {
			return fmt.Errorf("gc.retention.perRepo: duplicate entry for %q", r.Repo)
		}
		if r.KeepLastN < 0 {
			return fmt.Errorf("gc.retention.perRepo[%d].keepLastN must not be negative", i)
		}
		if r.MaxAge < 0 {
			return fmt.Errorf("gc.retention.perRepo[%d].maxAge must not be negative", i)
		}
		seen[r.Repo] = true
	}
	return nil
}

func validateNotificationEvents(cfg *Config) error {
	validNotificationEvents := map[string]bool{
		"peer_health":         true,
		"follow_request":      true,
		"replication_failure": true,
		"gc_error":            true,
	}
	for _, e := range cfg.Notifications.Events {
		if !validNotificationEvents[e] {
			return fmt.Errorf("notifications.events: unknown event %q", e)
		}
	}
	return nil
}

func validateUpstreams(cfg *Config) error {
	if !cfg.Upstreams.Enabled {
		return nil
	}
	validAuthTypes := map[string]bool{"none": true, "basic": true, "token": true}
	seen := make(map[string]bool)
	for i, u := range cfg.Upstreams.Registries {
		if u.Name == "" {
			return fmt.Errorf("upstreams.registries[%d].name is required", i)
		}
		if u.Endpoint == "" {
			return fmt.Errorf("upstreams.registries[%d].endpoint is required", i)
		}
		if _, err := url.ParseRequestURI(u.Endpoint); err != nil {
			return fmt.Errorf("upstreams.registries[%d].endpoint is not a valid URL: %w", i, err)
		}
		if !validAuthTypes[u.Auth] {
			return fmt.Errorf("upstreams.registries[%d].auth must be 'none', 'basic', or 'token'", i)
		}
		if u.Auth == "basic" && (u.Username == "" || u.Password == "") {
			return fmt.Errorf("upstreams.registries[%d] requires username and password for basic auth", i)
		}
		if seen[u.Name] {
			return fmt.Errorf("upstreams.registries: duplicate registry name %q", u.Name)
		}
		seen[u.Name] = true
	}
	if cfg.Upstreams.FetchTimeout < 0 {
		return fmt.Errorf("upstreams.fetchTimeout must not be negative")
	}
	return nil
}

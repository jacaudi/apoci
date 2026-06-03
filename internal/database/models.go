package database

import (
	"time"

	"github.com/uptrace/bun"
)

// Outgoing follow status constants.
const (
	FollowStatusPending  = "pending"
	FollowStatusAccepted = "accepted"
	FollowStatusRejected = "rejected"
)

// Package: (Type, Name) is unique. Name format is backend-specific
// ("foo.com/myapp" for OCI, "@scope/foo" for npm, "com.example:lib" for Maven).
type Package struct {
	bun.BaseModel `bun:"table:packages"`

	ID                    int64      `bun:"id,pk,autoincrement"`
	Type                  string     `bun:"type,notnull"`
	Name                  string     `bun:"name,notnull"`
	OwnerID               string     `bun:"owner_id,notnull"`
	Private               bool       `bun:"private,notnull,default:false"`
	CreatedAt             time.Time  `bun:"created_at,notnull,default:current_timestamp"`
	FederationWithdrawnAt *time.Time `bun:"federation_withdrawn_at"`
}

// PackageVersion's Version is the backend's identifier (digest for OCI,
// semver for npm, GAV.V for Maven). Metadata holds the canonical bytes for
// backends that need them (the manifest body for OCI). MediaType/SizeBytes
// describe Metadata. SubjectDigest/ArtifactType implement OCI referrers.
type PackageVersion struct {
	bun.BaseModel `bun:"table:package_versions"`

	ID            int64     `bun:"id,pk,autoincrement"`
	PackageID     int64     `bun:"package_id,notnull"`
	Version       string    `bun:"version,notnull"`
	Metadata      []byte    `bun:"metadata"`
	MediaType     string    `bun:"media_type,notnull,default:''"`
	SizeBytes     int64     `bun:"size_bytes,notnull,default:0"`
	SourceActor   *string   `bun:"source_actor"`
	SubjectDigest *string   `bun:"subject_digest"`
	ArtifactType  *string   `bun:"artifact_type"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp"`
}

// PackageFile attaches a blob to a version. For OCI Filename == BlobDigest.
type PackageFile struct {
	bun.BaseModel `bun:"table:package_files"`

	ID          int64     `bun:"id,pk,autoincrement"`
	VersionID   int64     `bun:"version_id,notnull"`
	Filename    string    `bun:"filename,notnull"`
	BlobDigest  string    `bun:"blob_digest,notnull"`
	SizeBytes   int64     `bun:"size_bytes,notnull"`
	ContentType *string   `bun:"content_type"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp"`
}

// PackageTag points a name to a version string. Version is denormalized
// (not an FK) to match the prior OCI tag→digest semantics.
type PackageTag struct {
	bun.BaseModel `bun:"table:package_tags"`

	ID        int64     `bun:"id,pk,autoincrement"`
	PackageID int64     `bun:"package_id,notnull"`
	Name      string    `bun:"name,notnull"`
	Version   string    `bun:"version,notnull"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp"`
}

// DeletedVersion tombstones a deleted version so federation cannot re-create
// it. OCI uses a digest-only lookup; other backends key by (type, name).
type DeletedVersion struct {
	bun.BaseModel `bun:"table:deleted_versions"`

	ID          int64     `bun:"id,pk,autoincrement"`
	PackageType string    `bun:"package_type,notnull"`
	PackageName string    `bun:"package_name,notnull"`
	Version     string    `bun:"version,notnull"`
	SourceActor string    `bun:"source_actor,notnull"`
	DeletedAt   time.Time `bun:"deleted_at,notnull,default:current_timestamp"`
}

// Repository is an OCI-shaped DTO over the packages table (type='oci').
type Repository struct {
	ID        int64
	Name      string
	OwnerID   string
	Private   bool
	CreatedAt time.Time
}

// Manifest is an OCI-shaped DTO over package_versions
// (Digest=Version, Content=Metadata).
type Manifest struct {
	ID            int64
	RepositoryID  int64
	Digest        string
	MediaType     string
	SizeBytes     int64
	Content       []byte
	SourceActor   *string
	SubjectDigest *string
	ArtifactType  *string
	CreatedAt     time.Time
}

// Tag is an OCI-shaped DTO over package_tags (ManifestDigest=Version).
type Tag struct {
	ID             int64
	RepositoryID   int64
	Name           string
	ManifestDigest string
	UpdatedAt      time.Time
}

type Blob struct {
	bun.BaseModel `bun:"table:blobs"`

	ID            int64     `bun:"id,pk,autoincrement"`
	Digest        string    `bun:"digest,notnull,unique"`
	SizeBytes     int64     `bun:"size_bytes,notnull"`
	MediaType     *string   `bun:"media_type"`
	StoredLocally bool      `bun:"stored_locally,notnull,default:false"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp"`
}

type PeerBlob struct {
	bun.BaseModel `bun:"table:peer_blobs"`

	ID             int64      `bun:"id,pk,autoincrement"`
	PeerActor      string     `bun:"peer_actor,notnull"`
	BlobDigest     string     `bun:"blob_digest,notnull"`
	PeerEndpoint   string     `bun:"peer_endpoint,notnull"`
	LastVerifiedAt *time.Time `bun:"last_verified_at"`
}

// Actor represents a remote ActivityPub actor. Consolidates peers, follows, and outgoing_follows.
type Actor struct {
	bun.BaseModel `bun:"table:actors"`

	ID           int64   `bun:"id,pk,autoincrement" json:"id"`
	ActorURL     string  `bun:"actor_url,notnull,unique" json:"actor_url"`
	Name         *string `bun:"name" json:"name,omitempty"`
	Alias        *string `bun:"alias" json:"alias,omitempty"`
	Endpoint     string  `bun:"endpoint,notnull" json:"endpoint"`
	PublicKeyPEM *string `bun:"public_key_pem" json:"public_key_pem,omitempty"`

	// Inbound: they follow us
	TheyFollowUs   bool       `bun:"they_follow_us,notnull,default:false" json:"they_follow_us"`
	TheyFollowUsAt *time.Time `bun:"they_follow_us_at" json:"they_follow_us_at,omitempty"`

	// Outbound: we follow them
	WeFollowThem     bool       `bun:"we_follow_them,notnull,default:false" json:"we_follow_them"`
	WeFollowStatus   *string    `bun:"we_follow_status" json:"we_follow_status,omitempty"` // pending, accepted, rejected
	WeFollowAcceptAt *time.Time `bun:"we_follow_accept_at" json:"we_follow_accept_at,omitempty"`

	// Health & replication (for blob fetching)
	IsHealthy         bool       `bun:"is_healthy,notnull,default:true" json:"is_healthy"`
	ReplicationPolicy string     `bun:"replication_policy,notnull,default:'lazy'" json:"replication_policy"`
	LastSeenAt        *time.Time `bun:"last_seen_at" json:"last_seen_at,omitempty"`

	FederationTagGlobs *string `bun:"federation_tag_globs" json:"federation_tag_globs,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

func (a *Actor) HasPendingOutgoingFollow() bool {
	return a != nil && a.WeFollowThem && a.WeFollowStatus != nil && *a.WeFollowStatus == FollowStatusPending
}

func (a *Actor) HasAcceptedOutgoingFollow() bool {
	return a != nil && a.WeFollowThem && a.WeFollowStatus != nil && *a.WeFollowStatus == FollowStatusAccepted
}

func (a *Actor) HasPendingOrAcceptedOutgoingFollow() bool {
	return a.HasPendingOutgoingFollow() || a.HasAcceptedOutgoingFollow()
}

func (a *Actor) GetPublicKeyPEM() string {
	if a == nil || a.PublicKeyPEM == nil {
		return ""
	}
	return *a.PublicKeyPEM
}

func (a *Actor) GetWeFollowStatus() string {
	if a == nil || a.WeFollowStatus == nil {
		return ""
	}
	return *a.WeFollowStatus
}

type FollowRequest struct {
	bun.BaseModel `bun:"table:follow_requests"`

	ID           int64     `bun:"id,pk,autoincrement"`
	ActorURL     string    `bun:"actor_url,notnull,unique"`
	PublicKeyPEM string    `bun:"public_key_pem,notnull"`
	Endpoint     string    `bun:"endpoint,notnull"`
	Alias        *string   `bun:"alias"`
	RequestedAt  time.Time `bun:"requested_at,notnull,default:current_timestamp"`
}

type Activity struct {
	bun.BaseModel `bun:"table:activities"`

	ID          int64     `bun:"id,pk,autoincrement"`
	ActivityID  string    `bun:"activity_id,notnull,unique"`
	Type        string    `bun:"type,notnull"`
	ActorURL    string    `bun:"actor_url,notnull"`
	ObjectJSON  []byte    `bun:"object_json,notnull"`
	PublishedAt time.Time `bun:"published_at,notnull,default:current_timestamp"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp"`
}

type UploadSession struct {
	bun.BaseModel `bun:"table:upload_sessions"`

	ID            int64     `bun:"id,pk,autoincrement"`
	UUID          string    `bun:"uuid,notnull,unique"`
	RepositoryID  int64     `bun:"repository_id,notnull"`
	BytesReceived int64     `bun:"bytes_received,notnull,default:0"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp"`
	ExpiresAt     time.Time `bun:"expires_at,notnull"`
}

type Delivery struct {
	bun.BaseModel `bun:"table:delivery_queue"`

	ID            int64     `bun:"id,pk,autoincrement"`
	ActivityID    string    `bun:"activity_id,notnull"`
	InboxURL      string    `bun:"inbox_url,notnull"`
	ActivityJSON  []byte    `bun:"activity_json,notnull"`
	Attempts      int       `bun:"attempts,notnull,default:0"`
	MaxAttempts   int       `bun:"max_attempts,notnull,default:10"`
	NextAttemptAt time.Time `bun:"next_attempt_at,notnull,default:current_timestamp"`
	LastError     *string   `bun:"last_error"`
	Status        string    `bun:"status,notnull,default:'pending'"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp"`
}

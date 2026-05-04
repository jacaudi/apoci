// Package pkgfed holds helpers shared by the npm, cargo, and pypi federation
// adapters.
package pkgfed

import (
	"context"
	"encoding/json"
	"fmt"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

// LookupOwnedPackage returns nil,nil for an unknown package (allowing
// delete-before-create or yank-before-create to be no-ops) and wraps
// database.ErrPackageOwnerMismatch when the sender doesn't own it.
// Ownership is immutable once set, so this is safe without a transaction.
func LookupOwnedPackage(ctx context.Context, db *database.DB, pkgType, name, actorURL string) (*database.Package, error) {
	pkg, err := db.GetPackage(ctx, pkgType, name)
	if err != nil {
		return nil, fmt.Errorf("lookup package: %w", err)
	}
	if pkg == nil {
		return nil, nil
	}
	if pkg.OwnerID != actorURL {
		return nil, fmt.Errorf("%w: %s/%s owned by %s, not %s", database.ErrPackageOwnerMismatch, pkgType, name, pkg.OwnerID, actorURL)
	}
	return pkg, nil
}

// RemarshalInto round-trips a decoded JSON value through Marshal so it can be
// re-Unmarshaled into a typed target. Needed because map[string]any decoding
// loses the json.RawMessage form of nested objects.
func RemarshalInto(v any, target any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

// RecordPeerBlob is a no-op when the sender's actor URL has no derivable
// endpoint; otherwise it records the (actor, digest, endpoint) tuple in
// peer_blobs so download handlers can later 302-redirect on local-blob miss.
func RecordPeerBlob(ctx context.Context, db *database.DB, actorURL, digest string) error {
	endpoint := activitypub.EndpointFromActorURL(actorURL)
	if endpoint == "" {
		return nil
	}
	return db.PutPeerBlob(ctx, actorURL, digest, endpoint)
}

// Replicator eagerly fetches a content-addressed blob from a peer URL into
// the local blobstore. The interface lives here so backends can accept it via
// Config without importing peering directly.
type Replicator interface {
	ReplicateFromURL(ctx context.Context, sourceURL, digest string)
}

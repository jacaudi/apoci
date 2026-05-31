package oci

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"cuelabs.dev/go/oci/ociregistry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestAttachReferrerAndHasReferrer(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	repo := "test.example.com/test/app"
	subject := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:abc"},"layers":[]}`)
	subjectDesc, err := reg.PushManifest(ctx, repo, "v1", subject, testManifestMediaType)
	require.NoError(t, err)

	const artifactType = "application/vnd.test.report.v1"

	// No referrer yet.
	has, err := reg.HasReferrer(ctx, repo, string(subjectDesc.Digest), artifactType)
	require.NoError(t, err)
	require.False(t, has)

	report := []byte(`{"vulns":0}`)
	annotations := map[string]string{"dev.test.count": "0"}
	refDigest, err := reg.AttachReferrer(ctx, repo, string(subjectDesc.Digest), artifactType, annotations, report, "application/json")
	require.NoError(t, err)
	require.NotEmpty(t, refDigest)

	// Now discoverable by HasReferrer, but only for the matching artifactType.
	has, err = reg.HasReferrer(ctx, repo, string(subjectDesc.Digest), artifactType)
	require.NoError(t, err)
	require.True(t, has)

	has, err = reg.HasReferrer(ctx, repo, string(subjectDesc.Digest), "application/vnd.other.v1")
	require.NoError(t, err)
	require.False(t, has)

	// The stored referrer manifest carries the subject, artifactType and annotations.
	reader, err := reg.GetManifest(ctx, repo, ociregistry.Digest(refDigest))
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	raw, err := io.ReadAll(reader)
	require.NoError(t, err)

	var m ocispec.Manifest
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, artifactType, m.ArtifactType)
	require.NotNil(t, m.Subject)
	require.Equal(t, string(subjectDesc.Digest), string(m.Subject.Digest))
	require.Equal(t, "0", m.Annotations["dev.test.count"])
	require.Len(t, m.Layers, 1)
}

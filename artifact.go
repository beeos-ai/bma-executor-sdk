package bmaexecutor

// Artifact references keep large tool output out of the 64 KiB control frame.
// The executor protocol v1 has no artifact upload endpoint: the deployment
// uploads bytes through its artifact store (or an agent input path) and reports
// only the reference in tool.result. Artifact upload over the executor channel
// is tracked separately and is not part of this release.

// NewArtifactRef builds an artifact reference for tool.result. digest is
// "sha256:<hex>" of the content.
func NewArtifactRef(artifactID, name, mimeType string, sizeBytes int64, digest string) ArtifactRef {
	return ArtifactRef{ArtifactID: artifactID, Name: name, MimeType: mimeType, SizeBytes: sizeBytes, Digest: digest}
}

// NewArtifactContentBlock builds the content block that points at an artifact
// from inside tool.result content.
func NewArtifactContentBlock(ref ArtifactRef) ContentBlock {
	return ContentBlock{Type: ContentBlockTypeArtifact, ArtifactID: ref.ArtifactID, MimeType: ref.MimeType}
}

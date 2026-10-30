package bmaexecutor

// Artifact references keep large tool output out of the 64 KiB control frame.
// The executor protocol v1 has no artifact upload endpoint: the deployment
// uploads bytes through its artifact store (or an agent input path) and reports
// only the reference in tool.result. Artifact upload over the executor channel
// is tracked separately and is not part of this release.

// NewArtifactRef builds an artifact reference for tool.result. Optional fields
// are omitted when empty or non-positive.
func NewArtifactRef(artifactID, name, mimeType string, sizeBytes int64) ArtifactRef {
	ref := ArtifactRef{ArtifactID: artifactID}
	if name != "" {
		ref.Name = &name
	}
	if mimeType != "" {
		ref.MimeType = &mimeType
	}
	if sizeBytes > 0 {
		ref.SizeBytes = &sizeBytes
	}
	return ref
}

// NewArtifactContentBlock builds the content block that points at an artifact
// from inside tool.result content.
func NewArtifactContentBlock(ref ArtifactRef) ContentBlock {
	artifactID := ref.ArtifactID
	block := ContentBlock{
		Type:       ContentBlockTypeArtifact,
		ArtifactID: &artifactID,
	}
	if ref.MimeType != nil {
		mimeType := *ref.MimeType
		block.MimeType = &mimeType
	}
	return block
}

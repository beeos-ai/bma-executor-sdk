package bmaexecutor

import "encoding/json"

// NewArtifactRef builds an artifact reference for tool.result.
func NewArtifactRef(artifactID, name, mimeType string, sizeBytes int64) ArtifactRef {
	return ArtifactRef{ArtifactID: artifactID, Name: name, MimeType: mimeType, SizeBytes: sizeBytes}
}

// NewArtifactContentBlock builds the content block that points at an artifact
// from inside tool.result content.
func NewArtifactContentBlock(ref ArtifactRef) ContentBlock {
	return ContentBlock{Type: ContentBlockTypeArtifact, ArtifactID: ref.ArtifactID, MimeType: ref.MimeType}
}

// NewToolInput encodes a built-in tool input for ToolCallParams.Input.
func NewToolInput(input ToolInput) json.RawMessage {
	raw, _ := json.Marshal(input) // ToolInput has only JSON-safe fields
	return raw
}

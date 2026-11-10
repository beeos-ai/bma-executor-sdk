package bmaexecutor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // registers the decoder image.DecodeConfig reads GIF dimensions with
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// User images. The platform sends an image the user attached as a file block
// of turn.start / turn.input (file_id, name, mime_type, size_bytes; the bytes
// never travel on the control channel). The executor downloads it, checks it
// the way the Anthropic API does and hands it to the agent as an ACP image
// content block (ACP prompt content: image, base64 data + mimeType), which
// the agent may receive only when its initialize reply declares
// promptCapabilities.image. An image that cannot be handed over ends the turn
// with a session.error; it is never dropped silently.
const (
	// acpMaxImageBytes is the Anthropic API limit of one image (5 MB), which
	// the platform also enforces when the image is attached.
	acpMaxImageBytes = 5 * 1024 * 1024
	// acpMaxImageEdge is the longest side the Anthropic API accepts (8000
	// px; an image above it is rejected).
	acpMaxImageEdge = 8000
	// acpMaxPromptImages is the most images the Anthropic API takes in one request.
	acpMaxPromptImages   = 100
	acpImageFetchTimeout = 30 * time.Second
)

// ACPInputFile is one file block the executor must fetch the bytes of.
type ACPInputFile struct {
	SessionID string
	TurnID    string
	// Block is the file block as the platform sent it (FileID is set).
	Block AgentInputBlock
	// Metadata is the turn.start metadata of the turn.
	Metadata map[string]string
}

// ACPInputFiles returns the content of a file block (the caller closes it and
// reads at most the image limit). It sets ACPAgentOptions.InputFiles.
type ACPInputFiles func(ctx context.Context, file ACPInputFile) (io.ReadCloser, error)

// acpImageTypes are the image types of the Anthropic API.
var acpImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// canTakeImages reports whether the agent declared image prompts. The Claude
// Code profile is left out: its image path is not exercised against the
// pinned release yet, so it is not declared and refuses an image visibly.
func (p *acpProcess) canTakeImages() bool {
	return p.init.AgentCapabilities.PromptCapabilities.Image && !p.agent.claude()
}

// fileInputs is the agent capability's file_inputs: the executor can fetch
// the files of file blocks (a host InputFiles, or the tools endpoint the
// platform hands a turn when PlatformTools is on) and the agent takes images.
func (a *ACPAgent) fileInputs(imageAgent bool) bool {
	return imageAgent && (a.opts.InputFiles != nil || a.opts.PlatformTools)
}

// promptContent is the ACP prompt of one input: its text, then its images.
func (a *ACPAgent) promptContent(ctx context.Context, proc *acpProcess, turn *AgentTurn, input []AgentInputBlock) ([]any, error) {
	var texts []string
	var files []AgentInputBlock
	for _, b := range input {
		switch {
		case b.Type == AgentInputBlockTypeText:
			texts = append(texts, b.Text)
		case b.Type == AgentInputBlockTypeFile && strings.HasPrefix(b.MimeType, "image/"):
			files = append(files, b)
		}
	}
	if len(files) == 0 {
		return []any{map[string]any{"type": "text", "text": strings.Join(texts, "\n")}}, nil
	}
	if !proc.canTakeImages() {
		return nil, &AgentError{Code: AgentErrorCodeAgentProtocolError,
			Message: "the input has an image but this agent does not take image input (its initialize reply has promptCapabilities.image false)"}
	}
	if len(files) > acpMaxPromptImages {
		return nil, &AgentError{Code: AgentErrorCodeAgentError,
			Message: fmt.Sprintf("the input has %d images; at most %d are accepted", len(files), acpMaxPromptImages)}
	}
	var content []any
	if text := strings.Join(texts, "\n"); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, f := range files {
		data, err := a.fetchImage(ctx, turn, f)
		if err != nil {
			return nil, &AgentError{Code: AgentErrorCodeAgentError, Message: "image " + imageLabel(f) + ": " + err.Error()}
		}
		content = append(content, map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": f.MimeType})
	}
	return content, nil
}

func imageLabel(b AgentInputBlock) string {
	if b.Name != "" {
		return fmt.Sprintf("%q", b.Name)
	}
	return b.FileID
}

// fetchImage downloads and checks one image.
func (a *ACPAgent) fetchImage(ctx context.Context, turn *AgentTurn, b AgentInputBlock) ([]byte, error) {
	if !acpImageTypes[b.MimeType] {
		return nil, fmt.Errorf("the type %s is not supported (png, jpeg, gif and webp are)", b.MimeType)
	}
	if b.FileID == "" {
		return nil, errors.New("the file block has no file_id")
	}
	if b.SizeBytes > acpMaxImageBytes {
		return nil, fmt.Errorf("it is %d bytes; an image may be at most %d", b.SizeBytes, acpMaxImageBytes)
	}
	ctx, cancel := context.WithTimeout(ctx, acpImageFetchTimeout)
	defer cancel()
	file := ACPInputFile{SessionID: turn.SessionID(), TurnID: turn.TurnID(), Block: b, Metadata: turn.Params().Metadata}
	fetch := a.opts.InputFiles
	if fetch == nil {
		fetch = fetchFromToolsEndpoint
	}
	body, err := fetch(ctx, file)
	if err != nil {
		return nil, fmt.Errorf("could not be downloaded: %w", err)
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, acpMaxImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("could not be downloaded: %w", err)
	}
	if err := checkImage(data, b.MimeType); err != nil {
		return nil, err
	}
	return data, nil
}

// fetchFromToolsEndpoint downloads the file from the platform's tools
// endpoint of the turn (turn.start metadata tools_mcp_url, tools_mcp_token),
// the route the platform's own brain host uses: GET <tools endpoint without
// /mcp>/files/<file_id>/content with the turn's token. A redirect is not
// followed (it would carry the token elsewhere).
func fetchFromToolsEndpoint(ctx context.Context, f ACPInputFile) (io.ReadCloser, error) {
	endpoint, token := f.Metadata[metaToolsURL], f.Metadata[metaToolsToken]
	base, ok := strings.CutSuffix(endpoint, "/mcp")
	if endpoint == "" || token == "" || !ok {
		return nil, errors.New("the platform handed this turn no files endpoint (turn.start metadata tools_mcp_url ending in /mcp and tools_mcp_token), and the host set no ACPAgentOptions.InputFiles")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/files/"+url.PathEscape(f.Block.FileID)+"/content", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("the files endpoint is not reachable")
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, fmt.Errorf("the platform answered HTTP %d", response.StatusCode)
	}
	return response.Body, nil
}

// checkImage applies the Anthropic API's image rules: a supported type whose
// bytes really are that type, at most 5 MB, at most 8000 px on a side.
func checkImage(data []byte, mimeType string) error {
	switch {
	case len(data) == 0:
		return errors.New("it is empty")
	case len(data) > acpMaxImageBytes:
		return fmt.Errorf("it is over %d bytes", acpMaxImageBytes)
	}
	if sniffed := sniffImageType(data); sniffed != mimeType {
		if sniffed == "" {
			return fmt.Errorf("its bytes are not a %s image", mimeType)
		}
		return fmt.Errorf("its bytes are %s, not the declared %s", sniffed, mimeType)
	}
	width, height, err := imageSize(data, mimeType)
	if err != nil {
		return fmt.Errorf("it is not a valid %s image: %w", mimeType, err)
	}
	if width <= 0 || height <= 0 {
		return errors.New("it has no pixels")
	}
	if width > acpMaxImageEdge || height > acpMaxImageEdge {
		return fmt.Errorf("it is %dx%d px; the longest side may be at most %d", width, height, acpMaxImageEdge)
	}
	return nil
}

func sniffImageType(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

func imageSize(data []byte, mimeType string) (width, height int, err error) {
	if mimeType == "image/webp" {
		return webpSize(data)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	return config.Width, config.Height, err
}

// webpSize reads the canvas size from the first chunk of a WebP file (RIFF
// container: VP8 lossy, VP8L lossless, VP8X extended).
func webpSize(data []byte) (int, int, error) {
	short := errors.New("the header is truncated")
	if len(data) < 20 {
		return 0, 0, short
	}
	chunk := data[12:]
	switch string(data[12:16]) {
	case "VP8 ":
		if len(chunk) < 18 || chunk[11] != 0x9d || chunk[12] != 0x01 || chunk[13] != 0x2a {
			return 0, 0, errors.New("bad VP8 frame header")
		}
		return int(binary.LittleEndian.Uint16(chunk[14:])) & 0x3fff, int(binary.LittleEndian.Uint16(chunk[16:])) & 0x3fff, nil
	case "VP8L":
		if len(chunk) < 13 || chunk[8] != 0x2f {
			return 0, 0, errors.New("bad VP8L header")
		}
		bits := binary.LittleEndian.Uint32(chunk[9:])
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, nil
	case "VP8X":
		if len(chunk) < 18 {
			return 0, 0, short
		}
		return 1 + (int(chunk[12]) | int(chunk[13])<<8 | int(chunk[14])<<16), 1 + (int(chunk[15]) | int(chunk[16])<<8 | int(chunk[17])<<16), nil
	}
	return 0, 0, errors.New("unknown chunk " + string(data[12:16]))
}

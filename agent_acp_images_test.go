package bmaexecutor

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// User images (agent_acp_images.go): the file blocks of an input reach the
// agent as ACP image content blocks when it declared promptCapabilities.image,
// and are refused visibly (session.error) otherwise.

func testImage(t testing.TB, kind string, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range min(w, 16) {
		img.Set(x, 0, color.RGBA{R: 200, A: 255})
	}
	var b bytes.Buffer
	var err error
	switch kind {
	case "png":
		err = png.Encode(&b, img)
	case "jpeg":
		err = jpeg.Encode(&b, img, nil)
	case "gif":
		err = gif.Encode(&b, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// webpChunk is a RIFF/WEBP file made of one chunk (header fields only: enough
// for the size check).
func webpChunk(fourcc string, payload []byte) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(4+8+len(payload)))
	b.WriteString("WEBP" + fourcc)
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(payload)))
	b.Write(payload)
	return b.Bytes()
}

func webpVP8X(w, h int) []byte {
	p := []byte{0, 0, 0, 0, byte(w - 1), byte((w - 1) >> 8), byte((w - 1) >> 16), byte(h - 1), byte((h - 1) >> 8), byte((h - 1) >> 16)}
	return webpChunk("VP8X", p)
}

func webpVP8L(w, h int) []byte {
	p := []byte{0x2f, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(p[1:], uint32(w-1)|uint32(h-1)<<14)
	return webpChunk("VP8L", p)
}

func webpVP8(w, h int) []byte {
	p := []byte{0, 0, 0, 0x9d, 0x01, 0x2a, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(p[6:], uint16(w))
	binary.LittleEndian.PutUint16(p[8:], uint16(h))
	return webpChunk("VP8 ", p)
}

func TestCheckImage(t *testing.T) {
	big := bytes.Repeat([]byte{0}, acpMaxImageBytes+1)
	copy(big, "\x89PNG\r\n\x1a\n")
	for _, c := range []struct {
		name string
		data []byte
		mime string
		want string // "" = accepted; otherwise a substring of the error
	}{
		{"png", testImage(t, "png", 10, 10), "image/png", ""},
		{"jpeg", testImage(t, "jpeg", 10, 10), "image/jpeg", ""},
		{"gif", testImage(t, "gif", 10, 10), "image/gif", ""},
		{"webp lossy", webpVP8(640, 480), "image/webp", ""},
		{"webp lossless", webpVP8L(640, 480), "image/webp", ""},
		{"webp extended", webpVP8X(300, 200), "image/webp", ""},
		{"largest side allowed", testImage(t, "png", acpMaxImageEdge, 1), "image/png", ""},
		{"side over 8000 px", testImage(t, "png", acpMaxImageEdge+1, 1), "image/png", "8001x1 px"},
		{"tall side over 8000 px", webpVP8X(10, 9000), "image/webp", "10x9000 px"},
		{"over 5 MB", big, "image/png", "over 5242880 bytes"},
		{"empty", nil, "image/png", "it is empty"},
		{"jpeg declared as png", testImage(t, "jpeg", 4, 4), "image/png", "bytes are image/jpeg, not the declared image/png"},
		{"text declared as png", []byte("not an image"), "image/png", "not a image/png image"},
		{"truncated png", testImage(t, "png", 10, 10)[:20], "image/png", "not a valid image/png"},
		{"truncated webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8X"), "image/webp", "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := checkImage(c.data, c.mime)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("checkImage = %v, want accepted", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("checkImage = %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// filesPlatform serves session files like the platform's tools endpoint.
type filesPlatform struct {
	server *httptest.Server
	mu     sync.Mutex
	files  map[string][]byte
	seen   []string // "<auth> <path>"
}

func newFilesPlatform(t *testing.T, files map[string][]byte) *filesPlatform {
	p := &filesPlatform{files: files}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.seen = append(p.seen, r.Header.Get("Authorization")+" "+r.URL.Path)
		p.mu.Unlock()
		const prefix = "/internal/brain-tools/sessions/sesn_img/files/"
		id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/content")
		data, found := p.files[id]
		switch {
		case r.Header.Get("Authorization") != "Bearer tools-token" || !strings.HasPrefix(r.URL.Path, prefix) || !ok:
			w.WriteHeader(http.StatusUnauthorized)
		case !found:
			w.WriteHeader(http.StatusNotFound)
		default:
			_, _ = w.Write(data)
		}
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *filesPlatform) metadata() map[string]string {
	return map[string]string{"tools_mcp_url": p.server.URL + "/internal/brain-tools/sessions/sesn_img/mcp", "tools_mcp_token": "tools-token"}
}

func fileBlock(id, name, mime string, size int) AgentInputBlock {
	return AgentInputBlock{Type: AgentInputBlockTypeFile, FileID: id, Name: name, MimeType: mime, SizeBytes: int64(size)}
}

// agentMessages are the texts of the agent.message events of a turn.
func agentMessages(t *testing.T, f *acpFixture, turn string) []string {
	t.Helper()
	var out []string
	for _, e := range eventsOf(f.platform.snapshot(), turn) {
		if e.Type == AgentEventTypeAgentMessage {
			for _, c := range eventJSON(t, e)["content"].([]any) {
				out = append(out, c.(map[string]any)["text"].(string))
			}
		}
	}
	return out
}

// reportedImages decodes the images the fake agent got ("mime:base64,...").
func reportedImages(t *testing.T, message string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	list, ok := strings.CutPrefix(message, "images=")
	if !ok {
		t.Fatalf("the agent got no images: %q", message)
	}
	for _, item := range strings.Split(list, ",") {
		mime, data, _ := strings.Cut(item, ":")
		raw, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			t.Fatal(err)
		}
		out[mime] = raw
	}
	return out
}

func TestACPAgentImagesReachTheAgentAsImageBlocks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pngData, gifData := testImage(t, "png", 32, 24), testImage(t, "gif", 8, 8)
	files := newFilesPlatform(t, map[string][]byte{"file_png": pngData, "file_gif": gifData})
	f := startACPFixture(t, ctx, ACPAgentOptions{PlatformTools: true, Env: []string{"BMA_ACP_FAKE_IMAGE=1"}})

	capability, _ := f.executor.AgentCapability()
	if !capability.FileInputs || !capability.Harnesses[0].ImageInputs {
		t.Fatalf("capability file_inputs=%v harness image_inputs=%v, want both true", capability.FileInputs, capability.Harnesses[0].ImageInputs)
	}

	f.start(t, ctx, "sesn_img", "turn_1", "what is this?", func(p *TurnStartParams) {
		p.Input = append(p.Input, fileBlock("file_png", "a.png", "image/png", len(pngData)), fileBlock("file_gif", "b.gif", "image/gif", len(gifData)))
		p.Metadata = files.metadata()
	})
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	messages := agentMessages(t, f, "turn_1")
	if len(messages) != 1 {
		t.Fatalf("messages = %q", messages)
	}
	images := reportedImages(t, messages[0])
	if !bytes.Equal(images["image/png"], pngData) || !bytes.Equal(images["image/gif"], gifData) || len(images) != 2 {
		t.Fatalf("the agent got %d images, png equal=%v gif equal=%v", len(images), bytes.Equal(images["image/png"], pngData), bytes.Equal(images["image/gif"], gifData))
	}
	files.mu.Lock()
	seen := strings.Join(files.seen, "|")
	files.mu.Unlock()
	if want := "Bearer tools-token /internal/brain-tools/sessions/sesn_img/files/file_png/content|Bearer tools-token /internal/brain-tools/sessions/sesn_img/files/file_gif/content"; seen != want {
		t.Fatalf("file requests = %s", seen)
	}
}

// An image-only input has no text block; a turn.input with an image is
// prompted in the same turn; the host's own InputFiles is used when the
// platform hands no tools endpoint.
func TestACPAgentInputFilesHookAndImageOnlyInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pngData := testImage(t, "png", 5, 5)
	jpegData := testImage(t, "jpeg", 6, 6)
	var mu sync.Mutex
	var asked []string
	f := startACPFixture(t, ctx, ACPAgentOptions{Env: []string{"BMA_ACP_FAKE_IMAGE=1"}, InputFiles: func(_ context.Context, file ACPInputFile) (io.ReadCloser, error) {
		mu.Lock()
		asked = append(asked, file.SessionID+"/"+file.TurnID+"/"+file.Block.FileID)
		mu.Unlock()
		return io.NopCloser(bytes.NewReader(map[string][]byte{"file_a": pngData, "file_b": jpegData}[file.Block.FileID])), nil
	}})
	if capability, _ := f.executor.AgentCapability(); !capability.FileInputs {
		t.Fatal("file_inputs is not declared although the host set InputFiles")
	}
	f.start(t, ctx, "sesn_hook", "turn_1", "slow", func(p *TurnStartParams) {})
	if _, err := f.executor.SendTurnInput(ctx, TurnInputParams{SessionID: "sesn_hook", TurnID: "turn_1", InputID: "in_img",
		Input: []AgentInputBlock{fileBlock("file_a", "a.png", "image/png", len(pngData))}}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	var got []string
	for _, m := range agentMessages(t, f, "turn_1") {
		if strings.HasPrefix(m, "images=") {
			got = append(got, m)
		}
	}
	if len(got) != 1 || !bytes.Equal(reportedImages(t, got[0])["image/png"], pngData) {
		t.Fatalf("image messages = %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(asked, ",") != "sesn_hook/turn_1/file_a" {
		t.Fatalf("InputFiles asked for %v", asked)
	}
}

// An image that cannot be handed over ends the turn with a session.error that
// says why; the agent never sees a prompt without it.
func TestACPAgentImageRefusalsAreVisible(t *testing.T) {
	pngData := testImage(t, "png", 4, 4)
	huge := testImage(t, "png", acpMaxImageEdge+1, 1)
	for _, c := range []struct {
		name  string
		env   []string
		tools bool // the platform hands the files endpoint
		block AgentInputBlock
		want  string
		code  string // the session.error type (default agent_error)
	}{
		{"agent takes no image", nil, true, fileBlock("file_png", "a.png", "image/png", len(pngData)),
			"the input has an image but this agent does not take image input", AgentErrorCodeAgentProtocolError},
		{"unsupported type", []string{"BMA_ACP_FAKE_IMAGE=1"}, true, fileBlock("file_png", "a.bmp", "image/bmp", len(pngData)),
			`image "a.bmp": the type image/bmp is not supported`, ""},
		{"declared over 5 MB", []string{"BMA_ACP_FAKE_IMAGE=1"}, true, fileBlock("file_png", "a.png", "image/png", acpMaxImageBytes+1),
			"it is 5242881 bytes; an image may be at most 5242880", ""},
		{"bytes are not the declared type", []string{"BMA_ACP_FAKE_IMAGE=1"}, true, fileBlock("file_png", "a.jpg", "image/jpeg", len(pngData)),
			`image "a.jpg": its bytes are image/png, not the declared image/jpeg`, ""},
		{"side over 8000 px", []string{"BMA_ACP_FAKE_IMAGE=1"}, true, fileBlock("file_huge", "h.png", "image/png", len(huge)),
			"it is 8001x1 px", ""},
		{"file not found", []string{"BMA_ACP_FAKE_IMAGE=1"}, true, fileBlock("file_gone", "g.png", "image/png", 10),
			`image "g.png": could not be downloaded: the platform answered HTTP 404`, ""},
		{"no way to download", []string{"BMA_ACP_FAKE_IMAGE=1"}, false, fileBlock("file_png", "a.png", "image/png", len(pngData)),
			"could not be downloaded: the platform handed this turn no files endpoint", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			files := newFilesPlatform(t, map[string][]byte{"file_png": pngData, "file_huge": huge})
			f := startACPFixture(t, ctx, ACPAgentOptions{Env: c.env})
			f.start(t, ctx, "sesn_img", "turn_1", "look", func(p *TurnStartParams) {
				p.Input = append(p.Input, c.block)
				if c.tools {
					p.Metadata = files.metadata()
				}
			})
			assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeFailed)
			var kind, message string
			for _, e := range eventsOf(f.platform.snapshot(), "turn_1") {
				if e.Type == AgentEventTypeSessionError {
					info := eventJSON(t, e)["error"].(map[string]any)
					kind, message = info["type"].(string), info["message"].(string)
				}
			}
			if want := cmp.Or(c.code, AgentErrorCodeAgentError); kind != want || !strings.Contains(message, c.want) {
				t.Fatalf("session.error = %s %q, want %s containing %q", kind, message, want, c.want)
			}
			if got := agentMessages(t, f, "turn_1"); len(got) != 0 {
				t.Fatalf("the agent was prompted: %q", got)
			}
		})
	}
}

func TestACPAgentFileInputsNeedAnImageAgentAndAFileSource(t *testing.T) {
	for _, c := range []struct {
		name         string
		env          []string
		opts         ACPAgentOptions
		file, harnes bool
	}{
		{"agent without image", nil, ACPAgentOptions{PlatformTools: true}, false, false},
		{"image agent, no file source", []string{"BMA_ACP_FAKE_IMAGE=1"}, ACPAgentOptions{}, false, true},
		{"image agent, tools endpoint", []string{"BMA_ACP_FAKE_IMAGE=1"}, ACPAgentOptions{PlatformTools: true}, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c.opts.Env = c.env
			f := startACPFixture(t, ctx, c.opts)
			capability, _ := f.executor.AgentCapability()
			if capability.FileInputs != c.file || capability.Harnesses[0].ImageInputs != c.harnes {
				t.Fatalf("file_inputs=%v image_inputs=%v, want %v %v", capability.FileInputs, capability.Harnesses[0].ImageInputs, c.file, c.harnes)
			}
		})
	}
}

// --- the real bma-dsh-acp ---

// dshAcpCommand is the argv of the real bma-dsh-acp: BMA_DSH_ACP_BIN (the SEA
// binary or the .mjs bundle), or the pinned npm package (npm ci in
// testdata/bma-dsh-acp). CI sets BMA_REQUIRE_DSH_ACP=1, which turns "not
// installed" into a failure.
func dshAcpCommand(t *testing.T) []string {
	t.Helper()
	bin := os.Getenv("BMA_DSH_ACP_BIN")
	if bin == "" {
		entry, err := filepath.Abs("testdata/bma-dsh-acp/node_modules/@beeos-ai/bma-dsh-acp/lib/main.js")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(entry); err != nil {
			if os.Getenv("BMA_REQUIRE_DSH_ACP") == "1" {
				t.Fatalf("bma-dsh-acp is not installed (npm ci in testdata/bma-dsh-acp): %v", err)
			}
			t.Skipf("bma-dsh-acp is not installed (npm ci in testdata/bma-dsh-acp): %v", err)
		}
		bin = entry
	}
	if !strings.HasSuffix(bin, ".mjs") && !strings.HasSuffix(bin, ".js") {
		return []string{bin}
	}
	node, err := exec.LookPath("node") // the test resolves it; ACPAgent itself never looks on PATH
	if err != nil {
		t.Skip("node not found for the bma-dsh-acp bundle")
	}
	return []string{node, bin}
}

// TestACPAgentUserImageReachesTheModelThroughBmaDshAcp is the end-to-end case:
// a file block of an image goes through the real bma-dsh-acp (0.2.2, which
// declares image:true) and the scripted OpenAI-compatible model endpoint gets
// the image in its request.
func TestACPAgentUserImageReachesTheModelThroughBmaDshAcp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bma-dsh-acp is run with a POSIX path here")
	}
	command := dshAcpCommand(t)
	pngData := testImage(t, "png", 48, 32)
	files := newFilesPlatform(t, map[string][]byte{"file_cat": pngData})

	var mu sync.Mutex
	var userContent [][]map[string]any // the content parts of the last user message of each model request
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for i := len(body.Messages) - 1; i >= 0; i-- {
			if body.Messages[i].Role != "user" {
				continue
			}
			var parts []map[string]any
			if json.Unmarshal(body.Messages[i].Content, &parts) == nil {
				mu.Lock()
				userContent = append(userContent, parts)
				mu.Unlock()
			}
			break
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, delta := range []map[string]any{{"content": "I see "}, {"content": "an image"}, {}} {
			finish := any(nil)
			if len(delta) == 0 {
				finish = "stop"
			}
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer model.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{Command: command, PlatformTools: true,
		Upstream: ACPModelUpstream{BaseURL: model.URL + "/v1", Model: "scripted"}})
	capability, _ := f.executor.AgentCapability()
	if h := capability.Harnesses[0]; h.AgentName != "bma-dsh-acp" || !h.ImageInputs || !capability.FileInputs {
		t.Fatalf("harness = %+v file_inputs=%v, want bma-dsh-acp with image_inputs and file_inputs", h, capability.FileInputs)
	}
	f.start(t, ctx, "sesn_img", "turn_1", "what is in this picture?", func(p *TurnStartParams) {
		p.Input = append(p.Input, fileBlock("file_cat", "cat.png", "image/png", len(pngData)))
		p.Metadata = files.metadata()
	})
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if got := strings.Join(agentMessages(t, f, "turn_1"), ""); got != "I see an image" {
		t.Fatalf("reply = %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(userContent) == 0 {
		t.Fatal("the model got no multi-part user message: the image was not sent")
	}
	var sawText, sawImage bool
	for _, part := range userContent[len(userContent)-1] {
		switch part["type"] {
		case "text":
			sawText = sawText || strings.Contains(fmt.Sprint(part["text"]), "what is in this picture?")
		case "image_url":
			url := fmt.Sprint(part["image_url"].(map[string]any)["url"])
			data, ok := strings.CutPrefix(url, "data:image/png;base64,")
			raw, err := base64.StdEncoding.DecodeString(data)
			if cfg, _, derr := image.DecodeConfig(bytes.NewReader(raw)); ok && err == nil && derr == nil {
				sawImage = cfg.Width == 48 && cfg.Height == 32
			}
		}
	}
	if !sawText || !sawImage {
		t.Fatalf("model request parts text=%v 48x32 png=%v: %v", sawText, sawImage, userContent[len(userContent)-1])
	}
}

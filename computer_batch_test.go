package bmaexecutor

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// screenBackend is a ComputerUseBackend with a w x h screen whose pixel
// (x, y) encodes x and y in its colour, so a crop can be checked. It records
// every call and fails an action whose text is "boom".
type screenBackend struct {
	w, h    int
	mu      sync.Mutex
	actions []ComputerActionRequest
	observe int
}

func (b *screenBackend) Observe(_ context.Context, _ ComputerObserveRequest) ([]ContentBlock, error) {
	b.mu.Lock()
	b.observe++
	b.mu.Unlock()
	img := image.NewRGBA(image.Rect(0, 0, b.w, b.h))
	for y := range b.h {
		for x := range b.w {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), uint8(x / 256), 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return []ContentBlock{
		{Type: ContentBlockTypeImage, MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(out.Bytes())},
		{Type: ContentBlockTypeText, Text: fmt.Sprintf("screen %dx%d", b.w, b.h)},
	}, nil
}

func (b *screenBackend) Action(_ context.Context, req ComputerActionRequest) ([]ContentBlock, error) {
	b.mu.Lock()
	b.actions = append(b.actions, req)
	b.mu.Unlock()
	if req.Text == "boom" {
		return nil, errors.New("the key went missing")
	}
	return []ContentBlock{{Type: ContentBlockTypeText, Text: "did " + req.Action}}, nil
}

func (b *screenBackend) calls() ([]ComputerActionRequest, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]ComputerActionRequest(nil), b.actions...), b.observe
}

func resultImages(t *testing.T, result ToolResultParams) []image.Image {
	t.Helper()
	var out []image.Image
	for _, block := range result.Content {
		if block.Type != ContentBlockTypeImage {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(block.Data)
		if err != nil {
			t.Fatal(err)
		}
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, img)
	}
	return out
}

// TestComputerBatch: the steps of one computer_action run in order, the first
// failure stops the batch, every step is reported and a screenshot ends it
// (Anthropic computer use tool, batch actions).
func TestComputerBatch(t *testing.T) {
	backend := &screenBackend{w: 800, h: 600}
	session, _ := computerSession(t, CoreOptions{ComputerUse: backend})
	result := callComputer(t, session, ToolNameComputerAction, map[string]any{"actions": []map[string]any{
		{"action": "click", "coordinate": []int{10, 20}},
		{"action": "type", "text": "hello"},
		{"action": "key", "text": "Return"},
	}})
	actions, observes := backend.calls()
	if result.IsError || len(actions) != 3 || actions[0].Action != "click" || actions[1].Text != "hello" ||
		actions[2].Action != "key" || observes != 1 {
		t.Fatalf("batch = %+v; actions %+v, observes %d", result, actions, observes)
	}
	if images := resultImages(t, result); len(images) != 1 || images[0].Bounds().Dx() != 800 {
		t.Fatalf("batch images = %d", len(images))
	}
	text := textOf(result)
	for _, want := range []string{"step 1 click: did click", "step 2 type: did type", "step 3 key: did key"} {
		if !strings.Contains(text, want) {
			t.Errorf("batch text lacks %q:\n%s", want, text)
		}
	}

	backend.actions = nil
	result = callComputer(t, session, ToolNameComputerAction, map[string]any{"actions": []map[string]any{
		{"action": "click", "coordinate": []int{10, 20}},
		{"action": "key", "text": "boom"},
		{"action": "type", "text": "never"},
	}})
	actions, observes = backend.calls()
	if !result.IsError || result.Outcome != ToolOutcomeFailed || len(actions) != 2 || observes != 2 {
		t.Fatalf("failing batch = %+v; actions %+v, observes %d", result, actions, observes)
	}
	text = textOf(result)
	for _, want := range []string{"step 2 key failed: the key went missing",
		"step 3 type: Not executed: an earlier computer action in this batch failed."} {
		if !strings.Contains(text, want) {
			t.Errorf("failing batch text lacks %q:\n%s", want, text)
		}
	}
	if len(resultImages(t, result)) != 1 {
		t.Error("a failed batch does not end with a screenshot")
	}
}

// TestComputerBatchRefusals: a batch whose steps do not all check runs none of
// them.
func TestComputerBatchRefusals(t *testing.T) {
	backend := &screenBackend{w: 800, h: 600}
	session, _ := computerSession(t, CoreOptions{ComputerUse: backend})
	for name, input := range map[string]map[string]any{
		"bad step":                      {"actions": []map[string]any{{"action": "type", "text": "x"}, {"action": "click"}}},
		"nested batch":                  {"actions": []map[string]any{{"actions": []map[string]any{{"action": "wait", "duration": 1}}}}},
		"action and batch":              {"action": "type", "text": "x", "actions": []map[string]any{{"action": "type", "text": "y"}}},
		"empty batch":                   {"actions": []map[string]any{}},
		"too long waits":                {"actions": []map[string]any{{"action": "wait", "duration": 6}, {"action": "wait", "duration": 6}}},
		"hold_key without duration":     {"action": "hold_key", "text": "shift"},
		"wait too long":                 {"action": "wait", "duration": 11},
		"mouse_move without coordinate": {"action": "mouse_move"},
	} {
		if result := callComputer(t, session, ToolNameComputerAction, input); !result.IsError {
			t.Errorf("%s = %+v", name, result)
		}
	}
	if actions, observes := backend.calls(); len(actions) != 0 || observes != 0 {
		t.Fatalf("refused calls ran: %+v, %d observes", actions, observes)
	}
}

// TestComputerWaitAndNewActions: wait pauses in Core without the backend;
// mouse_move and hold_key reach the backend with their members.
func TestComputerWaitAndNewActions(t *testing.T) {
	backend := &screenBackend{w: 800, h: 600}
	session, _ := computerSession(t, CoreOptions{ComputerUse: backend})
	started := time.Now()
	if result := callComputer(t, session, ToolNameComputerAction, map[string]any{"action": "wait", "duration": 0.2}); result.IsError {
		t.Fatalf("wait = %+v", result)
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Fatalf("wait returned after %v", elapsed)
	}
	callComputer(t, session, ToolNameComputerAction, map[string]any{"action": "mouse_move", "coordinate": []int{30, 40}})
	callComputer(t, session, ToolNameComputerAction, map[string]any{"action": "hold_key", "text": "shift", "duration": 0.5})
	actions, _ := backend.calls()
	if len(actions) != 2 || actions[0].Action != ComputerActionMouseMove || *actions[0].Coordinate != [2]float64{30, 40} ||
		actions[1].Action != ComputerActionHoldKey || actions[1].Text != "shift" || actions[1].Duration != 500*time.Millisecond {
		t.Fatalf("actions = %+v", actions)
	}
}

// TestComputerScreenshotFitsImageLimitsAndMapsCoordinates: a screenshot over
// the image limit is scaled down to fit it and the model's coordinates are
// mapped back to the screen; a zoom crops the screen at full resolution and
// keeps the full screenshot's coordinate frame.
func TestComputerScreenshotFitsImageLimitsAndMapsCoordinates(t *testing.T) {
	backend := &screenBackend{w: 1920, h: 1080}
	session, _ := computerSession(t, CoreOptions{ComputerUse: backend})
	observe := callComputer(t, session, ToolNameComputerObserve, map[string]any{})
	images := resultImages(t, observe)
	if len(images) != 1 {
		t.Fatalf("observe = %+v", observe)
	}
	w, h := images[0].Bounds().Dx(), images[0].Bounds().Dy()
	if max(w, h) > maxImageLongEdge || w*h > maxImagePixels || w < 1400 {
		t.Fatalf("screenshot %dx%d does not fit %d px / %d pixels tightly", w, h, maxImageLongEdge, maxImagePixels)
	}
	if !strings.Contains(textOf(observe), fmt.Sprintf("scaled to %dx%d from the screen's 1920x1080", w, h)) {
		t.Errorf("observe text = %q", textOf(observe))
	}
	scale := 1920 / float64(w)
	callComputer(t, session, ToolNameComputerAction, map[string]any{"action": "click", "coordinate": []float64{float64(w) / 2, float64(h) / 2}})
	callComputer(t, session, ToolNameComputerAction, map[string]any{"action": "drag",
		"start_coordinate": []float64{0, 0}, "coordinate": []float64{float64(w - 1), float64(h - 1)}})

	// Zoom on the bottom-right quarter: full screen resolution, no rescale.
	zoom := callComputer(t, session, ToolNameComputerObserve, map[string]any{"region": []float64{float64(w) / 2, float64(h) / 2, float64(w), float64(h)}})
	zoomed := resultImages(t, zoom)
	if len(zoomed) != 1 {
		t.Fatalf("zoom = %+v", zoom)
	}
	zb := zoomed[0].Bounds()
	if math.Abs(float64(zb.Dx())-960) > 2 || math.Abs(float64(zb.Dy())-540) > 2 {
		t.Fatalf("zoom is %dx%d, want the 960x540 screen pixels of the region", zb.Dx(), zb.Dy())
	}
	if r, g, _, _ := zoomed[0].At(zb.Min.X, zb.Min.Y).RGBA(); math.Abs(float64(r>>8)-float64(960%256)) > 2 || math.Abs(float64(g>>8)-float64(540%256)) > 2 {
		t.Errorf("zoom top-left pixel is (%d,%d), want the screen's (960,540)", r>>8, g>>8)
	}
	// After the zoom, coordinates are still those of the full screenshot.
	callComputer(t, session, ToolNameComputerAction, map[string]any{"action": "mouse_move", "coordinate": []float64{float64(w) / 4, float64(h) / 4}})

	actions, _ := backend.calls()
	near := func(got [2]float64, x, y float64) bool { return math.Abs(got[0]-x) <= 2 && math.Abs(got[1]-y) <= 2 }
	if len(actions) != 3 || !near(*actions[0].Coordinate, 960, 540) || !near(*actions[1].Start, 0, 0) ||
		!near(*actions[1].Coordinate, 1919, 1079) || !near(*actions[2].Coordinate, 480, 270) {
		t.Fatalf("scale %.4f, actions %+v", scale, actions)
	}
	if actions[0].Frame == nil || actions[0].Frame.Width != w || actions[0].Frame.Scale == 0 {
		t.Errorf("frame = %+v", actions[0].Frame)
	}
}

// TestComputerBatchApproval: a batch needs approval when any step does.
func TestComputerBatchApproval(t *testing.T) {
	backend := &approvingBackend{screenBackend: screenBackend{w: 800, h: 600}}
	_, core := computerSession(t, CoreOptions{ComputerUse: backend})
	quiet := map[string]any{"actions": []any{map[string]any{"action": "click", "coordinate": []any{1.0, 2.0}}}}
	asks := map[string]any{"actions": []any{map[string]any{"action": "click", "coordinate": []any{1.0, 2.0}},
		map[string]any{"action": "key", "text": "Return"}}}
	if core.computerNeedsApproval(context.Background(), "sess_cu", ToolNameComputerAction, quiet) {
		t.Error("a batch of safe steps asks")
	}
	if !core.computerNeedsApproval(context.Background(), "sess_cu", ToolNameComputerAction, asks) {
		t.Error("a batch with a step that asks does not ask")
	}
}

type approvingBackend struct{ screenBackend }

func (b *approvingBackend) NeedsObserveApproval(context.Context, ComputerObserveRequest) bool {
	return false
}
func (b *approvingBackend) NeedsActionApproval(_ context.Context, req ComputerActionRequest) bool {
	return req.Action == ComputerActionKey
}

// TestCuaDriverNewActions: mouse_move is the driver's move_cursor; the driver
// has no key hold, so hold_key is refused.
func TestCuaDriverNewActions(t *testing.T) {
	driver, logPath, err := fakeCua(t)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := computerSession(t, CoreOptions{ComputerUse: driver})
	callComputer(t, session, ToolNameComputerObserve, map[string]any{})
	if result := callComputer(t, session, ToolNameComputerAction, map[string]any{"action": "mouse_move", "coordinate": []int{11, 12}}); result.IsError {
		t.Fatalf("mouse_move = %+v", result)
	}
	if result := callComputer(t, session, ToolNameComputerAction, map[string]any{"action": "hold_key", "text": "shift", "duration": 1}); !result.IsError ||
		!strings.Contains(textOf(result), "hold_key is not available") {
		t.Fatalf("hold_key = %+v", result)
	}
	raw, _ := os.ReadFile(logPath)
	if !strings.Contains(string(raw), `move_cursor {"scope":"desktop","x":11,"y":12}`) {
		t.Fatalf("driver log:\n%s", raw)
	}
}

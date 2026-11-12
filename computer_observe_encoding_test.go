package bmaexecutor

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestObservationEncodingPreservesPixelsAndDefaults(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 321, 177))
	for y := 0; y < 177; y++ {
		for x := 0; x < 321; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, img); err != nil {
		t.Fatal(err)
	}
	original := ContentBlock{Type: ContentBlockTypeImage, MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(raw.Bytes())}
	for _, format := range []string{"", "png"} {
		got, err := encodeObservationImage(original, format)
		if err != nil || got.Data != original.Data || got.MimeType != original.MimeType {
			t.Fatal("default observation changed", err)
		}
	}
	got, err := encodeObservationImage(original, "jpeg")
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(got.Data)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil || decoded.Bounds() != img.Bounds() || got.MimeType != "image/jpeg" {
		t.Fatal("coordinate dimensions changed", err)
	}
	if _, err := encodeObservationImage(original, "webp"); err == nil {
		t.Fatal("unknown format accepted")
	}
	original.Data = base64.StdEncoding.EncodeToString([]byte("invalid PNG"))
	if _, err := encodeObservationImage(original, "jpeg"); err == nil {
		t.Fatal("corrupt image accepted")
	}
	original.Data = "not base64"
	if _, err := encodeObservationImage(original, "jpeg"); err == nil {
		t.Fatal("bad encoding accepted")
	}
}

type observeRequestRecorder struct{ requests []ComputerObserveRequest }

func (r *observeRequestRecorder) Observe(_ context.Context, req ComputerObserveRequest) ([]ContentBlock, error) {
	r.requests = append(r.requests, req)
	return []ContentBlock{{Type: ContentBlockTypeText, Text: "captured"}}, nil
}
func (*observeRequestRecorder) Action(context.Context, ComputerActionRequest) ([]ContentBlock, error) {
	return nil, nil
}
func TestObservationEncodingForwardedOnlyWhenRequested(t *testing.T) {
	recorder := &observeRequestRecorder{}
	session, _ := computerSession(t, CoreOptions{ComputerUse: recorder})
	for _, format := range []string{"", "jpeg"} {
		got := callComputer(t, session, ToolNameComputerObserve, map[string]any{"image_format": format, "pid": 12, "window_id": 34})
		if got.IsError {
			t.Fatalf("observation rejected: %+v", got)
		}
	}
	if len(recorder.requests) != 2 || recorder.requests[0].ImageFormat != "" || recorder.requests[1].ImageFormat != "jpeg" || recorder.requests[1].PID != 12 || recorder.requests[1].WindowID != 34 {
		t.Fatalf("wrong capture requests: %+v", recorder.requests)
	}
	got := callComputer(t, session, ToolNameComputerObserve, map[string]any{"image_format": "invalid"})
	if !got.IsError || len(recorder.requests) != 2 {
		t.Fatal("unsupported format reached capture backend")
	}
}

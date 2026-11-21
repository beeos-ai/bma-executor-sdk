package bmaexecutor

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
)

// Screenshot image limits. A screenshot (or zoom) larger than these is scaled
// down before it reaches the model, and the model's coordinates are mapped
// back with the same factor: providers either reject an oversized image
// (Anthropic's computer use toolset) or downscale it themselves, and then the
// model points into pixels the executor does not know about. 1568 px on the
// long edge and about 1.15 megapixels is the limit every Claude model accepts
// (Anthropic, computer use tool: "Size screenshots to fit image limits");
// OpenAI's high detail fits a 2048 px square and then 768 px on the short
// side, so the same image loses nothing there.
const (
	maxImageLongEdge = 1568
	maxImagePixels   = 1_150_000
)

// maxBatchSteps bounds the steps of one computer_action batch;
// maxComputerDuration bounds hold_key / wait and the durations of one batch
// (seconds), so a call stays inside the platform's tool call timeout.
const (
	maxBatchSteps       = 20
	maxComputerDuration = 10.0
)

// imageScale is the factor that fits w x h into the image limits (at most 1).
func imageScale(w, h int) float64 {
	return min(1, float64(maxImageLongEdge)/float64(max(w, h)), math.Sqrt(maxImagePixels/float64(w*h)))
}

// fitScreenshot scales the first image of an observation down to the image
// limits and notes the scale for the model. scale is screen pixels per
// screenshot pixel (1 when the image already fits or there is none).
func fitScreenshot(blocks []ContentBlock) ([]ContentBlock, float64, error) {
	for i, block := range blocks {
		if block.Type != ContentBlockTypeImage || block.Data == "" {
			continue
		}
		cfg, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, bytes.NewReader([]byte(block.Data))))
		if err != nil || cfg.Width < 1 || cfg.Height < 1 || imageScale(cfg.Width, cfg.Height) == 1 {
			return blocks, 1, nil
		}
		src, mime, err := firstImage(blocks[i : i+1])
		if err != nil {
			return nil, 0, err
		}
		img, _ := fitImage(src)
		out, err := encodeImage(img, mime)
		if err != nil {
			return nil, 0, err
		}
		w, h := img.Bounds().Dx(), img.Bounds().Dy()
		scale := float64(cfg.Width) / float64(w)
		fitted := append([]ContentBlock(nil), blocks...)
		fitted[i] = out
		fitted = append(fitted, ContentBlock{Type: ContentBlockTypeText, Text: fmt.Sprintf(
			"The screenshot is scaled to %dx%d from the screen's %dx%d (1 screenshot pixel = %.4f screen pixels) to fit the model's image limits. "+
				"Give coordinates in pixels of this screenshot; window positions and sizes listed above are screen pixels.",
			w, h, cfg.Width, cfg.Height, scale)})
		return fitted, scale, nil
	}
	return blocks, 1, nil
}

// fitImage scales src down to the image limits; scale is the factor applied.
func fitImage(src image.Image) (image.Image, float64) {
	b := src.Bounds()
	scale := imageScale(b.Dx(), b.Dy())
	if scale == 1 {
		return src, 1
	}
	w, h := max(1, int(float64(b.Dx())*scale)), max(1, int(float64(b.Dy())*scale))
	return downscale(src, w, h), scale
}

// firstImage decodes the first image block; mime is its media type.
func firstImage(blocks []ContentBlock) (image.Image, string, error) {
	for _, block := range blocks {
		if block.Type != ContentBlockTypeImage {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(block.Data)
		if err != nil {
			return nil, "", errors.New("the screenshot is not valid base64")
		}
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, "", fmt.Errorf("the screenshot cannot be decoded: %w", err)
		}
		return img, block.MimeType, nil
	}
	return nil, "", errors.New("the observation returned no screenshot")
}

// encodeImage encodes img as an image block: JPEG for a JPEG source, PNG
// otherwise.
func encodeImage(img image.Image, mime string) (ContentBlock, error) {
	var out bytes.Buffer
	var err error
	if mime == "image/jpeg" {
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: 75})
	} else {
		mime = "image/png"
		err = png.Encode(&out, img)
	}
	if err != nil {
		return ContentBlock{}, err
	}
	return ContentBlock{Type: ContentBlockTypeImage, MimeType: mime, Data: base64.StdEncoding.EncodeToString(out.Bytes())}, nil
}

// downscale resizes src to w x h (both no larger than src) by averaging the
// source pixels each destination pixel covers (box filter), which keeps thin
// text legible where nearest-neighbour sampling drops strokes.
func downscale(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	rgba, ok := src.(*image.RGBA)
	if !ok || b.Min != (image.Point{}) {
		rgba = image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)
	}
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		y0, y1 := y*sh/h, max((y+1)*sh/h, y*sh/h+1)
		for x := range w {
			x0, x1 := x*sw/w, max((x+1)*sw/w, x*sw/w+1)
			var r, g, bl, a, n uint32
			for sy := y0; sy < y1; sy++ {
				row := rgba.Pix[sy*rgba.Stride+x0*4 : sy*rgba.Stride+x1*4]
				for i := 0; i < len(row); i += 4 {
					r, g, bl, a = r+uint32(row[i]), g+uint32(row[i+1]), bl+uint32(row[i+2]), a+uint32(row[i+3])
					n++
				}
			}
			o := y*dst.Stride + x*4
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(a/n)
		}
	}
	return dst
}

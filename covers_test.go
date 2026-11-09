package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func testJPEG(t *testing.T, w, h, quality int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 7), uint8(y * 3), uint8(x ^ y), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestMakeThumb(t *testing.T) {
	// Small cover: returned unchanged, never upscaled.
	small := testJPEG(t, 300, 450, 90)
	if got, _ := makeThumb(small); !bytes.Equal(got, small) {
		t.Error("small cover was re-encoded")
	}
	// Large cover: scaled to thumbWidth, and smaller than the original.
	large := testJPEG(t, 1200, 1800, 92)
	got, err := makeThumb(large)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, _ := image.DecodeConfig(bytes.NewReader(got))
	if cfg.Width != thumbWidth || cfg.Height != 600 {
		t.Errorf("thumb is %dx%d, want %dx600", cfg.Width, cfg.Height, thumbWidth)
	}
	if len(got) >= len(large) {
		t.Errorf("thumb %d bytes >= original %d", len(got), len(large))
	}
	// Large but already tiny in bytes: the original wins.
	tiny := testJPEG(t, 800, 1200, 5)
	if got, _ := makeThumb(tiny); len(got) > len(tiny) {
		t.Errorf("thumb %d bytes > original %d", len(got), len(tiny))
	}
}

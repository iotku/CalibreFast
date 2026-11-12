package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
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
	if got, _ := makeThumb(small, noAbort); !bytes.Equal(got, small) {
		t.Error("small cover was re-encoded")
	}
	// Large cover: scaled to thumbWidth, and smaller than the original.
	large := testJPEG(t, 1200, 1800, 92)
	got, err := makeThumb(large, noAbort)
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
	if got, _ := makeThumb(tiny, noAbort); len(got) > len(tiny) {
		t.Errorf("thumb %d bytes > original %d", len(got), len(tiny))
	}
}

func noAbort() error { return nil }

func TestMakeThumbAbort(t *testing.T) {
	stop := errors.New("stop")
	large := testJPEG(t, 1200, 1800, 92)
	if _, err := makeThumb(large, func() error { return stop }); !errors.Is(err, stop) {
		t.Errorf("err = %v, want the abort error", err)
	}
}

func TestCoverJPEG(t *testing.T) {
	decode := func(t *testing.T, data []byte) (string, int, int) {
		t.Helper()
		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		return format, cfg.Width, cfg.Height
	}

	// A JPEG of a reasonable size is kept byte for byte.
	jpg := testJPEG(t, 1600, 2400, 85)
	if got, err := coverJPEG(jpg); err != nil || !bytes.Equal(got, jpg) {
		t.Errorf("JPEG re-encoded (%v)", err)
	}

	// Too wide: scaled down to coverMaxWidth.
	wide := testJPEG(t, coverMaxWidth+400, 4000, 85)
	got, err := coverJPEG(wide)
	if err != nil {
		t.Fatal(err)
	}
	if f, w, h := decode(t, got); f != "jpeg" || w != coverMaxWidth || h != 3459 {
		t.Errorf("too wide: %s %dx%d", f, w, h)
	}

	// Too big for its width (noise at quality 100): re-encoded at its size.
	noise := image.NewRGBA(image.Rect(0, 0, 1800, 1800))
	for i := range noise.Pix {
		noise.Pix[i] = uint8(i*2654435761>>13) | 3
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, noise, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= coverKeepMaxBytes {
		t.Fatalf("test image is only %d bytes", buf.Len())
	}
	if got, err := coverJPEG(buf.Bytes()); err != nil || len(got) >= buf.Len() {
		t.Errorf("too big: %d bytes, %v", len(got), err)
	} else if f, w, h := decode(t, got); f != "jpeg" || w != 1800 || h != 1800 {
		t.Errorf("too big: %s %dx%d", f, w, h)
	}

	// Not a JPEG: encoded once, at its own size.
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 300, 450))); err != nil {
		t.Fatal(err)
	}
	if got, err := coverJPEG(pngBuf.Bytes()); err != nil {
		t.Error(err)
	} else if f, w, h := decode(t, got); f != "jpeg" || w != 300 || h != 450 {
		t.Errorf("PNG: %s %dx%d", f, w, h)
	}

	// Not an image, or a cut-off JPEG: refused.
	for _, bad := range [][]byte{[]byte("not an image"), jpg[:len(jpg)/2]} {
		if _, err := coverJPEG(bad); err == nil {
			t.Errorf("%d bytes of not an image accepted", len(bad))
		}
	}
}

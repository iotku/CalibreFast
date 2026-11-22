package imageutil

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"testing"

	"golang.org/x/image/draw"
)

const thumbWidth = 400

var coverOpts = CoverOptions{MaxWidth: 2560, KeepMaxBytes: 5 << 20, Quality: 92}

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
	if got, _ := Thumb(small, thumbWidth, 80, nil); !bytes.Equal(got, small) {
		t.Error("small cover was re-encoded")
	}
	// Large cover: scaled to thumbWidth, and smaller than the original.
	large := testJPEG(t, 1200, 1800, 92)
	got, err := Thumb(large, thumbWidth, 80, nil)
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
	if got, _ := Thumb(tiny, thumbWidth, 80, nil); len(got) > len(tiny) {
		t.Errorf("thumb %d bytes > original %d", len(got), len(tiny))
	}
}

func TestMakeThumbAbort(t *testing.T) {
	stop := errors.New("stop")
	large := testJPEG(t, 1200, 1800, 92)
	if _, err := Thumb(large, thumbWidth, 80, func() error { return stop }); !errors.Is(err, stop) {
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
	if got, err := CoverJPEG(jpg, coverOpts); err != nil || !bytes.Equal(got, jpg) {
		t.Errorf("JPEG re-encoded (%v)", err)
	}

	// Too wide: scaled down to coverOpts.MaxWidth.
	wide := testJPEG(t, coverOpts.MaxWidth+400, 4000, 85)
	got, err := CoverJPEG(wide, coverOpts)
	if err != nil {
		t.Fatal(err)
	}
	if f, w, h := decode(t, got); f != "jpeg" || w != coverOpts.MaxWidth || h != 3459 {
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
	if buf.Len() <= coverOpts.KeepMaxBytes {
		t.Fatalf("test image is only %d bytes", buf.Len())
	}
	if got, err := CoverJPEG(buf.Bytes(), coverOpts); err != nil || len(got) >= buf.Len() {
		t.Errorf("too big: %d bytes, %v", len(got), err)
	} else if f, w, h := decode(t, got); f != "jpeg" || w != 1800 || h != 1800 {
		t.Errorf("too big: %s %dx%d", f, w, h)
	}

	// Not a JPEG: encoded once, at its own size.
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 300, 450))); err != nil {
		t.Fatal(err)
	}
	if got, err := CoverJPEG(pngBuf.Bytes(), coverOpts); err != nil {
		t.Error(err)
	} else if f, w, h := decode(t, got); f != "jpeg" || w != 300 || h != 450 {
		t.Errorf("PNG: %s %dx%d", f, w, h)
	}

	// Not an image, or a cut-off JPEG: refused.
	for _, bad := range [][]byte{[]byte("not an image"), jpg[:len(jpg)/2]} {
		if _, err := CoverJPEG(bad, coverOpts); err == nil {
			t.Errorf("%d bytes of not an image accepted", len(bad))
		}
	}
}

func TestPDFImageIsPage(t *testing.T) {
	cases := []struct {
		w, h         int
		pageW, pageH float64
		want         bool
	}{
		{1240, 1754, 595, 842, true},  // A4 scan
		{1754, 1240, 595, 842, true},  // turned with /Rotate
		{1200, 1200, 595, 842, false}, // a square picture on the page
		{200, 283, 595, 842, false},   // a logo of the page's shape
		{1650, 2550, 612, 792, false}, // a taller picture than the Letter page (0.65 vs 0.77)
	}
	for _, c := range cases {
		if got := PDFImageIsPage(c.w, c.h, c.pageW, c.pageH); got != c.want {
			t.Errorf("%dx%d on %vx%v: got %v, want %v", c.w, c.h, c.pageW, c.pageH, got, c.want)
		}
	}
}

func TestResizeToWidth(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 300, 1))
	if got := ResizeToWidth(img, 100).Bounds(); got.Dx() != 100 || got.Dy() != 1 {
		t.Errorf("got %v", got)
	}
	// Far wider than the target (through the halvings), odd sizes, very flat.
	for _, size := range [][2]int{{4001, 3}, {3333, 5001}, {801, 1201}, {1600, 2400}} {
		src := image.NewRGBA(image.Rect(0, 0, size[0], size[1]))
		want := max(1, int(float64(size[1])*400/float64(size[0])+0.5))
		if got := ResizeToWidth(src, 400).Bounds(); got.Dx() != 400 || got.Dy() != want {
			t.Errorf("%v: got %v, want 400x%d", size, got, want)
		}
	}
}

// The quick first steps don't change what the thumbnail looks like: it
// matches Catmull-Rom all the way down, to well past what anyone could see.
func TestResizeToWidthMatchesCatmullRom(t *testing.T) {
	for _, size := range [][2]int{{1600, 2400}, {2560, 3840}, {950, 1400}} {
		src := coverLike(size[0], size[1])
		got := ResizeToWidth(src, 400).(*image.RGBA)
		ref := image.NewRGBA(got.Bounds())
		draw.CatmullRom.Scale(ref, ref.Bounds(), src, src.Bounds(), draw.Src, nil)
		if p := psnr(got, ref); p < 40 {
			t.Errorf("%v: %.1f dB from Catmull-Rom, want at least 40", size, p)
		}
	}
}

// coverLike: smooth gradients with sharp-edged shapes and fine lines, as
// covers have (a title, an illustration), at a cover's proportions.
func coverLike(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{uint8(255 * x / w), uint8(255 * y / h), uint8(128 + 100*math.Sin(float64(x+y)/90)), 255}
			dx, dy := x-w/2, y-h/3
			if dx*dx+dy*dy < (w/4)*(w/4) {
				c = color.RGBA{230, 200, 40, 255}
			}
			if y > h*2/3 && y < h*2/3+h/12 && (x/(w/40))%2 == 0 {
				c = color.RGBA{20, 20, 20, 255} // "lettering"
			}
			img.Set(x, y, c)
		}
	}
	return img
}

func psnr(a, b *image.RGBA) float64 {
	var se float64
	for i := range a.Pix {
		if i%4 == 3 {
			continue
		}
		d := float64(a.Pix[i]) - float64(b.Pix[i])
		se += d * d
	}
	mse := se / float64(len(a.Pix)/4*3)
	if mse == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255/mse)
}

func BenchmarkThumb(b *testing.B) {
	img := coverLike(1600, 2400)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := Thumb(buf.Bytes(), thumbWidth, 80, nil); err != nil {
			b.Fatal(err)
		}
	}
}

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"testing"
)

// A text/diagram page (mostly flat with sharp marks) must come back as PNG —
// smaller AND lossless (no JPEG ringing on glyph edges). A noisy photo must
// come back as JPEG. Proves optimizeImage picks the right encoder per content.
func TestEncoderSelection(t *testing.T) {
	// Text-like: white page with sharp black bars (simulates lines of text).
	doc := image.NewGray(image.Rect(0, 0, 800, 600))
	for y := range 600 {
		for x := range 800 {
			doc.SetGray(x, y, color.Gray{Y: 255})
		}
	}
	for row := 40; row < 600; row += 24 {
		for y := row; y < row+8; y++ {
			for x := 40; x < 700; x += 3 { // dashed bars = "words"
				doc.SetGray(x, y, color.Gray{Y: 0})
			}
		}
	}
	var docBuf bytes.Buffer
	jpeg.Encode(&docBuf, doc, &jpeg.Options{Quality: 82})
	autoBytes, mime := optimizeImage(docBuf.Bytes(), "image/jpeg", modeAuto, 0)
	if mime != "image/png" {
		t.Errorf("text page (auto) should encode as PNG (smaller+lossless), got %s", mime)
	}
	// text mode must be no bigger than auto — binarization is the fewest bytes.
	textBytes, tmime := optimizeImage(docBuf.Bytes(), "image/jpeg", modeText, 0)
	if tmime != "image/png" {
		t.Errorf("text mode should be PNG, got %s", tmime)
	}
	if len(textBytes) > len(autoBytes) {
		t.Errorf("text mode (%d B) should be <= auto (%d B)", len(textBytes), len(autoBytes))
	}

	// Photo-like: full-frame random noise — JPEG must win.
	photo := image.NewRGBA(image.Rect(0, 0, 400, 400))
	for y := range 400 {
		for x := range 400 {
			photo.Set(x, y, color.RGBA{uint8(rand.IntN(256)), uint8(rand.IntN(256)), uint8(rand.IntN(256)), 255})
		}
	}
	var pBuf bytes.Buffer
	jpeg.Encode(&pBuf, photo, &jpeg.Options{Quality: 90})
	_, mime = optimizeImage(pBuf.Bytes(), "image/jpeg", modeColor, 0)
	if mime != "image/jpeg" {
		t.Errorf("noisy photo should encode as JPEG, got %s", mime)
	}
}

// Text mode must binarize only genuinely bimodal images. A document page comes
// back strictly 1-bit; a continuous-tone photo mislabeled text=true must fall
// through to the grayscale path (JPEG for noise) instead of binarized garbage.
func TestTextModeBimodalityGate(t *testing.T) {
	// Document: white page, sharp black bars → bimodal → every pixel 0 or 255.
	doc := image.NewGray(image.Rect(0, 0, 800, 600))
	for i := range doc.Pix {
		doc.Pix[i] = 255
	}
	for row := 40; row < 600; row += 24 {
		for y := row; y < row+8; y++ {
			for x := 40; x < 700; x += 3 {
				doc.SetGray(x, y, color.Gray{Y: 0})
			}
		}
	}
	var docBuf bytes.Buffer
	jpeg.Encode(&docBuf, doc, &jpeg.Options{Quality: 82})
	out, mime := optimizeImage(docBuf.Bytes(), "image/jpeg", modeText, 0)
	if mime != "image/png" {
		t.Fatalf("bimodal doc in text mode should be PNG, got %s", mime)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode text-mode output: %v", err)
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			g := color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y
			if g != 0 && g != 255 {
				t.Fatalf("doc output not binarized: pixel (%d,%d)=%d", x, y, g)
			}
		}
	}

	// Photo: full-frame noise → unimodal-ish histogram → NOT binarized, and
	// for noise the grayscale JPEG beats PNG, so the mime proves the path.
	photo := image.NewRGBA(image.Rect(0, 0, 400, 400))
	for y := range 400 {
		for x := range 400 {
			photo.Set(x, y, color.RGBA{uint8(rand.IntN(256)), uint8(rand.IntN(256)), uint8(rand.IntN(256)), 255})
		}
	}
	var pBuf bytes.Buffer
	jpeg.Encode(&pBuf, photo, &jpeg.Options{Quality: 90})
	_, mime = optimizeImage(pBuf.Bytes(), "image/jpeg", modeText, 0)
	if mime != "image/jpeg" {
		t.Errorf("photo with text=true should skip binarization and encode as JPEG, got %s", mime)
	}
}

// SVG/XML payloads must be rejected loudly, not passed through for the vision
// model to silently fail on. Real raster bytes must pass.
func TestCheckRaster(t *testing.T) {
	for _, bad := range []struct {
		data []byte
		mime string
	}{
		{[]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "image/png"},
		{[]byte("  \n\t<svg viewBox=\"0 0 1 1\"></svg>"), ""},
		{[]byte(`<?xml version="1.0"?><svg/>`), ""},
		{[]byte("PK..."), "image/svg+xml"}, // declared MIME alone is enough
	} {
		if err := checkRaster(bad.data, bad.mime); err == nil {
			t.Errorf("checkRaster(%q, %q) should error", bad.data[:min(10, len(bad.data))], bad.mime)
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 4, 4)), nil)
	if err := checkRaster(buf.Bytes(), "image/jpeg"); err != nil {
		t.Errorf("real JPEG should pass checkRaster: %v", err)
	}
}

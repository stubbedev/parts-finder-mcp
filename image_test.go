package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
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
	autoBytes, mime := optimizeImage(docBuf.Bytes(), "image/jpeg", modeAuto)
	if mime != "image/png" {
		t.Errorf("text page (auto) should encode as PNG (smaller+lossless), got %s", mime)
	}
	// text mode must be no bigger than auto — binarization is the fewest bytes.
	textBytes, tmime := optimizeImage(docBuf.Bytes(), "image/jpeg", modeText)
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
	_, mime = optimizeImage(pBuf.Bytes(), "image/jpeg", modeColor)
	if mime != "image/jpeg" {
		t.Errorf("noisy photo should encode as JPEG, got %s", mime)
	}
}

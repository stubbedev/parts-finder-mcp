package main

import (
	"bytes"
	"image"
	_ "image/gif" // register GIF decoder
	"image/jpeg"
	_ "image/png" // register PNG decoder
	"math"

	_ "golang.org/x/image/bmp" // register BMP decoder
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff" // register TIFF decoder
	"golang.org/x/image/webp"
)

// Sharp-style pre-processing before an image enters agent context: decode
// (jpeg/png/gif/webp), downscale so the long edge fits Anthropic's optimal
// bound, and re-encode as JPEG. Big product photos otherwise burn tokens and
// upload time for no extra readable detail. Best-effort: on any decode/encode
// problem the original bytes pass through unchanged.
//
// Anthropic vision downscales anything over ~1568px on the long edge OR over
// ~1.15 megapixels, whichever bites first. Matching BOTH here means we ship the
// exact pixels the model will use — no wasted upload, no wasted tokens — and we
// resample with a good filter instead of the server's. A 4:3 photo at 1568px
// long edge is 1.84MP, well over the area cap, so the megapixel bound is what
// actually shrinks most real images further without touching readable detail.
const (
	maxImageEdge   = 1568
	maxImagePixels = 1_150_000
)

// fitScale returns the largest scale ≤1 that keeps the image within both the
// long-edge and megapixel caps.
func fitScale(w, h int) float64 {
	s := 1.0
	if long := max(w, h); long > maxImageEdge {
		s = float64(maxImageEdge) / float64(long)
	}
	if px := float64(w) * float64(h); px > maxImagePixels {
		if ps := math.Sqrt(maxImagePixels / px); ps < s {
			s = ps
		}
	}
	return s
}

func init() {
	// Let image.Decode sniff webp too (stdlib registers jpeg/png/gif on import).
	image.RegisterFormat("webp", "RIFF????WEBPVP", webp.Decode, webp.DecodeConfig)
}

// optimizeImage downscales, optionally desaturates, and re-encodes as JPEG.
// Grayscale is the default: for reading model numbers, socket markings,
// dimensions and datasheet text, colour carries no signal and roughly halves
// the bytes. Pass keepColor=true for the rare case colour matters (connector
// colour-coding, cable ID). Best-effort: on any failure the input passes
// through unchanged.
func optimizeImage(data []byte, mime string, keepColor bool) ([]byte, string) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mime // can't decode (svg, exotic) — pass through
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	scale := fitScale(w, h)

	// Already within caps, already a compact colour raster, and caller wants
	// colour: nothing to gain — leave it.
	if keepColor && scale == 1 && len(data) <= 400*1024 &&
		(mime == "image/jpeg" || mime == "image/png") {
		return data, mime
	}

	nw, nh := int(float64(w)*scale), int(float64(h)*scale)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}

	var dst draw.Image
	if keepColor {
		dst = image.NewRGBA(image.Rect(0, 0, nw, nh))
	} else {
		dst = image.NewGray(image.Rect(0, 0, nw, nh)) // grayscale = smaller, no lost signal
	}
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 82}); err != nil {
		return data, mime
	}
	if buf.Len() >= len(data) && scale == 1 && keepColor {
		return data, mime // re-encode didn't help — keep original
	}
	return buf.Bytes(), "image/jpeg"
}

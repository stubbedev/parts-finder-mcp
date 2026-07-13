package main

import (
	"bytes"
	"image"
	"image/color"
	_ "image/gif" // register GIF decoder
	"image/jpeg"
	"image/png"
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

// Image optimization modes, cheapest-bytes first for their content:
//
//	"text"  — reading glyphs off an image (spec sheets, labels, scans):
//	          grayscale → Otsu threshold → 1-bit black/white PNG. Text needs
//	          no gray or colour to stay legible, and a bilevel PNG is a
//	          fraction of a grayscale JPEG. This is the fewest-bytes path.
//	"auto"  — grayscale, pick the smaller of PNG/JPEG (default).
//	"color" — keep colour, pick the smaller of PNG/JPEG (photos, colour-coding).
const (
	modeText  = "text"
	modeAuto  = "auto"
	modeColor = "color"
)

// optimizeImage downscales to the vision caps and re-encodes as few bytes as
// the mode allows. Best-effort: on any failure the input passes through.
func optimizeImage(data []byte, mime, mode string) ([]byte, string) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mime // can't decode (svg, exotic) — pass through
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	scale := fitScale(w, h)
	nw, nh := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))

	var dst draw.Image
	if mode == modeColor {
		dst = image.NewRGBA(image.Rect(0, 0, nw, nh))
	} else {
		dst = image.NewGray(image.Rect(0, 0, nw, nh)) // no colour signal for our reads
	}
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)

	// Text mode: binarize to 1-bit and PNG it — the fewest bytes for legible
	// text. Guard against a pathological case (e.g. a full-tone photo mislabeled
	// text) by still comparing against the grayscale encoders and keeping the
	// smallest.
	var best []byte
	var bestMIME string
	if mode == modeText {
		if bw := encodePNG(binarize(dst)); bw != nil {
			best, bestMIME = bw, "image/png"
		}
	}
	if j := encodeJPEG(dst); j != nil && (best == nil || len(j) < len(best)) {
		best, bestMIME = j, "image/jpeg"
	}
	if p := encodePNG(dst); p != nil && (best == nil || len(p) < len(best)) {
		best, bestMIME = p, "image/png"
	}
	if best == nil {
		return data, mime
	}
	if scale == 1 && mode == modeColor && len(data) <= len(best) &&
		(mime == "image/jpeg" || mime == "image/png") {
		return data, mime // source already smaller — keep it
	}
	return best, bestMIME
}

// binarize converts an image to 1-bit black/white using an Otsu threshold —
// the classic document-scan compression: legible text, minimal bytes.
func binarize(src image.Image) image.Image {
	b := src.Bounds()
	g, ok := src.(*image.Gray)
	if !ok {
		g = image.NewGray(b)
		draw.Draw(g, b, src, b.Min, draw.Src)
	}
	t := otsu(g.Pix)
	pal := image.NewPaletted(b, color.Palette{color.Gray{Y: 0}, color.Gray{Y: 255}})
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			idx := uint8(0)
			if g.GrayAt(x, y).Y >= t {
				idx = 1
			}
			pal.SetColorIndex(x, y, idx)
		}
	}
	return pal
}

// otsu finds the grayscale threshold maximizing inter-class variance.
func otsu(pix []uint8) uint8 {
	var hist [256]int
	for _, p := range pix {
		hist[p]++
	}
	total := len(pix)
	if total == 0 {
		return 128
	}
	var sum float64
	for i := 0; i < 256; i++ {
		sum += float64(i) * float64(hist[i])
	}
	var sumB, wB float64
	var maxVar float64
	threshold := 128
	for i := 0; i < 256; i++ {
		wB += float64(hist[i])
		if wB == 0 {
			continue
		}
		wF := float64(total) - wB
		if wF == 0 {
			break
		}
		sumB += float64(i) * float64(hist[i])
		mB := sumB / wB
		mF := (sum - sumB) / wF
		between := wB * wF * (mB - mF) * (mB - mF)
		if between > maxVar {
			maxVar = between
			threshold = i
		}
	}
	return uint8(threshold)
}

func encodeJPEG(img image.Image) []byte {
	var buf bytes.Buffer
	if jpeg.Encode(&buf, img, &jpeg.Options{Quality: 82}) != nil {
		return nil
	}
	return buf.Bytes()
}

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if enc.Encode(&buf, img) != nil {
		return nil
	}
	return buf.Bytes()
}

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // register GIF decoder
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"strconv"

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
// Image size caps. Vision cost is paid in TOKENS, which scale with PIXELS (not
// bytes), so we downscale before sending. The defaults are conservative,
// general-purpose values — NOT tied to one model. Different harnesses/models
// tile differently (e.g. some at 512px, some ~1.5k), so every cap is
// env-overridable and there's a per-call max_edge knob. Text caps LARGER in
// area than photo: dense datasheet tables drop below legible around ~1000px
// long edge and the model starts guessing digits — a wrong voltage costs far
// more than the extra tokens. Bytes stay cheap because bimodal text goes out
// as 1-bit PNG anyway.
var (
	maxImageEdge   = envInt("PARTS_IMG_MAX_EDGE", 1568)
	maxImagePixels = float64(envInt("PARTS_IMG_MAX_PIXELS", 1_150_000))
	maxTextEdge    = envInt("PARTS_IMG_TEXT_EDGE", 1600)
	maxTextPixels  = float64(envInt("PARTS_IMG_TEXT_PIXELS", 2_000_000))
)

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// fitScale returns the largest scale ≤1 that keeps the image within both the
// long-edge and pixel caps.
func fitScale(w, h, maxEdge int, maxPixels float64) float64 {
	s := 1.0
	if long := max(w, h); long > maxEdge {
		s = float64(maxEdge) / float64(long)
	}
	if px := float64(w) * float64(h); px > maxPixels {
		if ps := math.Sqrt(maxPixels / px); ps < s {
			s = ps
		}
	}
	return s
}

// capsFor picks the pixel caps for a mode, honouring an explicit long-edge
// override in BOTH directions (0 = mode default): shrink hard for a sparse
// label, or raise past the mode default for a dense table — the tool schema
// promises both, so the override always wins over the default caps.
func capsFor(mode string, override int) (edge int, pixels float64) {
	edge, pixels = maxImageEdge, maxImagePixels
	if mode == modeText {
		edge, pixels = maxTextEdge, maxTextPixels
	}
	if override > 0 {
		edge = override
		pixels = float64(edge) * float64(edge) // allow up to a square at that edge
	}
	return edge, pixels
}

func init() {
	// Let image.Decode sniff webp too (stdlib registers jpeg/png/gif on import).
	image.RegisterFormat("webp", "RIFF????WEBPVP", webp.Decode, webp.DecodeConfig)
}

// checkRaster fails loudly on clearly-non-raster payloads. SVG arrives with an
// image/* Content-Type so it survives the MIME gate, Go's decoders can't touch
// it, and vision models may not read it at all — silently. Other undecodable
// formats still pass through optimizeImage unchanged; this only rejects what
// is provably vector markup by content sniff (leading "<svg" / "<?xml" after
// whitespace/BOM) or declared MIME.
func checkRaster(data []byte, mime string) error {
	s := bytes.TrimLeft(data, " \t\r\n\xef\xbb\xbf")
	switch {
	case mime == "image/svg+xml", bytes.HasPrefix(s, []byte("<svg")):
		return fmt.Errorf("image is SVG (vector markup), not processed — vision cannot reliably read it, fetch a raster (png/jpeg) rendering instead")
	case bytes.HasPrefix(s, []byte("<?xml")):
		return fmt.Errorf("image is XML/vector data, not processed — fetch a raster (png/jpeg) version instead")
	}
	return nil
}

// Image optimization modes, cheapest-bytes first for their content:
//
//	"text"  — reading glyphs off an image (spec sheets, labels, scans):
//	          grayscale → Otsu threshold → 1-bit black/white PNG. Text needs
//	          no gray or colour to stay legible, and a bilevel PNG is a
//	          fraction of a grayscale JPEG. This is the fewest-bytes path.
//	          Only applies when the tonal histogram is genuinely bimodal;
//	          a photo mislabeled text falls through to the auto path.
//	"auto"  — grayscale, pick the smaller of PNG/JPEG (default).
//	"color" — keep colour, pick the smaller of PNG/JPEG (photos, colour-coding).
const (
	modeText  = "text"
	modeAuto  = "auto"
	modeColor = "color"
)

// optimizeImage downscales to the mode's pixel caps (maxEdge>0 overrides the
// long-edge cap) and re-encodes as few bytes as the mode allows. Best-effort:
// on any failure the input passes through.
func optimizeImage(data []byte, mime, mode string, maxEdge int) ([]byte, string) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mime // can't decode (svg, exotic) — pass through
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	edge, pixels := capsFor(mode, maxEdge)
	scale := fitScale(w, h, edge, pixels)
	nw, nh := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))

	var dst draw.Image
	if mode == modeColor {
		dst = image.NewRGBA(image.Rect(0, 0, nw, nh))
	} else {
		dst = image.NewGray(image.Rect(0, 0, nw, nh)) // no colour signal for our reads
	}
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)

	// Text mode: binarize to 1-bit and PNG it — the fewest bytes for legible
	// text. Gate on the image itself, not output byte size: a full-tone photo
	// mislabeled text binarizes into the SMALLEST garbage, so a size race would
	// pick exactly the pathological case. Only binarize when the grayscale
	// histogram is strongly bimodal (genuine scanned text / line art);
	// continuous tone falls through to the normal grayscale encoders.
	var best []byte
	var bestMIME string
	if mode == modeText {
		g := dst.(*image.Gray) // non-color modes always render into Gray
		if _, eta := otsu(g.Pix); eta >= bimodalMin {
			if bw := encodePNG(binarize(g)); bw != nil {
				best, bestMIME = bw, "image/png"
			}
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
	t, _ := otsu(g.Pix)
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

// bimodalMin gates text-mode binarization on Otsu's effectiveness ratio
// (between-class variance / total variance at the chosen threshold). Two
// clean tonal clusters (scanned text, line art) score near 1.0; uniform
// noise scores ~0.75 and real photos lower still.
// ponytail: one global threshold, no per-region analysis — lower it if a
// genuinely dirty scan ever falls back to grayscale.
const bimodalMin = 0.85

// otsu finds the grayscale threshold maximizing inter-class variance, and
// returns that maximum as a fraction of total variance (Otsu's effectiveness
// ratio) — a cheap bimodality score from the same histogram pass.
func otsu(pix []uint8) (uint8, float64) {
	var hist [256]int
	for _, p := range pix {
		hist[p]++
	}
	total := len(pix)
	if total == 0 {
		return 128, 0
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
	// Total variance (×N) for the effectiveness ratio; maxVar above is ×N².
	mean := sum / float64(total)
	var totVar float64
	for i, h := range hist {
		d := float64(i) - mean
		totVar += d * d * float64(h)
	}
	eta := 0.0
	if totVar > 0 {
		eta = maxVar / (float64(total) * totVar)
	}
	return uint8(threshold), eta
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

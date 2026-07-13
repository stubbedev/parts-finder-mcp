package main

import (
	"bytes"
	"image"
	_ "image/gif" // register GIF decoder
	"image/jpeg"
	_ "image/png" // register PNG decoder

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
// 1568px is Anthropic's documented long-edge sweet spot — larger images are
// downscaled server-side anyway, so sending bigger only wastes bandwidth.
const maxImageEdge = 1568

func init() {
	// Let image.Decode sniff webp too (stdlib registers jpeg/png/gif on import).
	image.RegisterFormat("webp", "RIFF????WEBPVP", webp.Decode, webp.DecodeConfig)
}

// optimizeImage downscales + re-encodes; returns the new bytes and MIME, or the
// input untouched if anything fails or it's already small.
func optimizeImage(data []byte, mime string) ([]byte, string) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mime // can't decode (svg, exotic) — pass through
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	long := max(w, h)

	// Already small and already a compact raster format: leave it.
	if long <= maxImageEdge && len(data) <= 400*1024 && (mime == "image/jpeg" || mime == "image/png") {
		return data, mime
	}

	dst := img
	if long > maxImageEdge {
		scale := float64(maxImageEdge) / float64(long)
		nw, nh := int(float64(w)*scale), int(float64(h)*scale)
		rs := image.NewRGBA(image.Rect(0, 0, nw, nh))
		draw.CatmullRom.Scale(rs, rs.Bounds(), img, b, draw.Over, nil)
		dst = rs
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 82}); err != nil {
		return data, mime
	}
	if buf.Len() >= len(data) && long <= maxImageEdge {
		return data, mime // re-encode didn't help — keep original
	}
	return buf.Bytes(), "image/jpeg"
}

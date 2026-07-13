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
	long := max(w, h)

	// Already small colour JPEG/PNG and caller wants colour: leave it.
	if keepColor && long <= maxImageEdge && len(data) <= 400*1024 &&
		(mime == "image/jpeg" || mime == "image/png") {
		return data, mime
	}

	nw, nh := w, h
	if long > maxImageEdge {
		scale := float64(maxImageEdge) / float64(long)
		nw, nh = int(float64(w)*scale), int(float64(h)*scale)
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
	if buf.Len() >= len(data) && long <= maxImageEdge && keepColor {
		return data, mime // re-encode didn't help — keep original
	}
	return buf.Bytes(), "image/jpeg"
}

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
)

// Build an image-only PDF (a scanned datasheet has no text layer) and confirm
// pdfPageImages recovers the page image for vision OCR.
func TestPDFPageImages(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 600, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 600; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var jbuf bytes.Buffer
	if err := jpeg.Encode(&jbuf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}

	var pdf bytes.Buffer
	if err := api.ImportImages(nil, &pdf, []io.Reader{bytes.NewReader(jbuf.Bytes())}, pdfcpu.DefaultImportConfig(), nil); err != nil {
		t.Fatalf("build image pdf: %v", err)
	}

	imgs, total := pdfPageImages(pdf.Bytes(), 5)
	if len(imgs) == 0 {
		t.Fatal("expected at least one page image from a scanned PDF")
	}
	if len(imgs[0].Data) == 0 {
		t.Error("extracted image has no bytes")
	}
	if total < len(imgs) {
		t.Errorf("total %d < returned %d", total, len(imgs))
	}
}

package main

import (
	"bytes"
	"io"
	"sort"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// DocImage is an image extracted from a document (a scanned PDF page), ready to
// hand to vision.
type DocImage struct {
	Data []byte
	MIME string
}

// scannedTextThreshold: below this many non-space chars, a PDF is treated as
// image-only (scanned) and we fall back to page-image extraction for OCR.
const scannedTextThreshold = 120

// pdfPageImages pulls embedded images out of a PDF (pure Go, no external
// binaries) so a scanned datasheet with no text layer can still be read by
// vision. Returns the largest `max` images, optimized. Best-effort: any error
// yields nil and the caller keeps whatever text it had.
func pdfPageImages(data []byte, max int) []DocImage {
	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	pages, err := api.ExtractImagesRaw(bytes.NewReader(data), nil, conf)
	if err != nil {
		return nil
	}
	type cand struct {
		data []byte
		mime string
	}
	var cands []cand
	for _, perPage := range pages {
		for _, img := range perPage {
			if img.IsImgMask {
				continue // masks aren't readable content
			}
			raw, err := io.ReadAll(img)
			// Filter by byte size, not pixels: pdfcpu's raw path often reports
			// Width/Height as 0. <2KB is a decoration/icon, not a page scan.
			if err != nil || len(raw) < 2000 {
				continue
			}
			cands = append(cands, cand{raw, pdfImageMIME(img.FileType)})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return len(cands[i].data) > len(cands[j].data) })
	var out []DocImage
	for _, c := range cands {
		if len(out) >= max {
			break
		}
		d, m := optimizeImage(c.data, c.mime, modeText) // scanned datasheets are text — binarize for fewest bytes
		out = append(out, DocImage{Data: d, MIME: m})
	}
	return out
}

func pdfImageMIME(fileType string) string {
	switch fileType {
	case "png":
		return "image/png"
	case "tif", "tiff":
		return "image/tiff"
	case "jpg", "jpeg":
		return "image/jpeg"
	default:
		return "application/octet-stream" // optimizeImage will sniff/convert if it can
	}
}

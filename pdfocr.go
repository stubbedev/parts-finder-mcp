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
	Page int // 1-based source page — so the reader knows WHICH pages it got
}

// A PDF below EITHER threshold is treated as image-only (scanned) and falls
// back to page-image extraction for OCR: fewer non-space chars than
// scannedTextThreshold, or fewer words than scannedWordThreshold (catches
// extractors that emit a few long junk strings).
const (
	scannedTextThreshold = 120
	scannedWordThreshold = 20
)

// pdfPageImages pulls embedded images out of a PDF (pure Go, no external
// binaries) so a scanned datasheet with no text layer can still be read by
// vision. Returns the largest `max` images, optimized, plus the total number
// of candidate page images so dropped pages are never silent. Best-effort:
// any error yields nil and the caller keeps whatever text it had.
func pdfPageImages(data []byte, max int) ([]DocImage, int) {
	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	pages, err := api.ExtractImagesRaw(bytes.NewReader(data), nil, conf)
	if err != nil {
		return nil, 0
	}
	type cand struct {
		data []byte
		mime string
		page int
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
			cands = append(cands, cand{raw, pdfImageMIME(img.FileType), img.PageNr})
		}
	}
	// Keep the `max` largest (biggest = the actual page scans), then restore
	// document order — page identity matters when reading cross-page tables.
	sort.Slice(cands, func(i, j int) bool { return len(cands[i].data) > len(cands[j].data) })
	total := len(cands)
	if len(cands) > max {
		cands = cands[:max]
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].page < cands[j].page })
	var out []DocImage
	for _, c := range cands {
		d, m := optimizeImage(c.data, c.mime, modeText, 0) // scanned datasheets are text — binarize for fewest bytes
		out = append(out, DocImage{Data: d, MIME: m, Page: c.page})
	}
	return out, total
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

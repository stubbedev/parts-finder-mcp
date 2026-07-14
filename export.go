package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xuri/excelize/v2"
)

// exportLocations are the logical save spots offered via elicitation.
var exportLocations = []string{"Documents", "Home", "Current folder", "Custom folder"}

// resolveExportDir maps a location choice to an absolute directory, working on
// both Linux and macOS. Unknown/empty → current directory.
func resolveExportDir(choice, custom string) string {
	home, _ := os.UserHomeDir()
	switch choice {
	case "Home":
		return home
	case "Documents":
		if d := os.Getenv("XDG_DOCUMENTS_DIR"); d != "" { // Linux XDG override
			return expandHome(d, home)
		}
		return filepath.Join(home, "Documents") // macOS + Linux default
	case "Custom folder":
		if custom != "" {
			return expandHome(custom, home)
		}
	}
	cwd, _ := os.Getwd()
	return cwd
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// elicitExportPath asks the user where to save via MCP form elicitation.
// Returns "" (with nil error) when the client can't elicit or the user
// declines — the caller then falls back to a sensible default.
func elicitExportPath(ctx context.Context, sess *mcp.ServerSession, defaultName string) string {
	if sess == nil {
		return ""
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"location": map[string]any{
				"type": "string", "enum": exportLocations,
				"description": "Where to save the spreadsheet",
			},
			"custom_folder": map[string]any{
				"type": "string", "description": "Absolute folder path (only if you picked 'Custom folder')",
			},
			"filename": map[string]any{
				"type": "string", "description": "File name", "default": defaultName,
			},
		},
		"required": []string{"location"},
	}
	res, err := sess.Elicit(ctx, &mcp.ElicitParams{
		Mode:            "form",
		Message:         "Where should I save the Excel export?",
		RequestedSchema: schema,
	})
	if err != nil || res == nil || res.Action != "accept" {
		return "" // unsupported or declined → caller defaults
	}
	str := func(k string) string { s, _ := res.Content[k].(string); return strings.TrimSpace(s) }
	dir := resolveExportDir(str("location"), str("custom_folder"))
	name := str("filename")
	if name == "" {
		name = defaultName
	}
	if !strings.HasSuffix(strings.ToLower(name), ".xlsx") {
		name += ".xlsx"
	}
	return filepath.Join(dir, filepath.Base(name)) // Base: keep the filename in the chosen dir
}

// exportSpecsXLSX writes one polished workbook: a sheet per spec (title,
// frozen header, bordered/zebra table of parts + live best price + key specs +
// buy links, then a summary block separating what you must BUY from what you
// already OWN) and, for 2+ specs, a Compare sheet — the decision view.
func exportSpecsXLSX(ctx context.Context, specIDs []string, path string, region Region, display string, appendMode bool) (string, error) {
	var f *excelize.File
	fresh := true
	if appendMode {
		existing, err := excelize.OpenFile(path)
		switch {
		case err == nil:
			f, fresh = existing, false // edit the existing workbook in place
		case errors.Is(err, os.ErrNotExist):
			// no workbook yet → start fresh below
		default:
			// corrupt/locked/unreadable: bail rather than silently overwrite
			return "", fmt.Errorf("append to %s: %w", path, err)
		}
	}
	if f == nil {
		f = excelize.NewFile()
	}
	defer f.Close()

	st := newStyles(f, display)
	var summaries []cmpRow
	exportedAt := time.Now() // prices are live-probed below — this IS the fetch time
	asOf := exportedAt.UTC().Format(time.RFC3339)

	for _, sid := range specIDs {
		name, rawIDs, rawOwned, err := store.loadSpec(sid)
		if err != nil {
			return "", fmt.Errorf("spec %s: %w", sid, err)
		}
		partIDs, ownedIDs, err := store.expandSpecIDs(rawIDs, rawOwned)
		if err != nil {
			return "", fmt.Errorf("spec %s: %w", sid, err)
		}
		parts, err := store.getParts(partIDs)
		if err != nil {
			return "", err
		}
		prewarmLiveness(ctx, partIDs)
		spec, err := store.composeIDs(rawIDs)
		if err != nil {
			return "", fmt.Errorf("spec %s: %w", sid, err)
		}

		// Deterministic sheet name so re-exporting a spec REPLACES its sheet
		// (update-in-place) rather than piling up "~2" duplicates.
		sheet := safeSheetName(sid)
		if idx, err := f.GetSheetIndex(sheet); err == nil && idx != -1 {
			f.DeleteSheet(sheet)
		}
		if _, err := f.NewSheet(sheet); err != nil {
			return "", fmt.Errorf("sheet %q: %w", sheet, err)
		}

		// Title.
		title := sid
		if name != "" {
			title += " — " + name
		}
		f.MergeCell(sheet, "A1", "P1")
		f.SetCellValue(sheet, "A1", title)
		f.SetCellStyle(sheet, "A1", "A1", st.title)
		f.SetRowHeight(sheet, 1, 24)

		// Price columns are PER UNIT; "Line total" = qty × unit and feeds the
		// TO BUY total, so a collapsed row never understates the build cost.
		headers := []string{"Category", "Status", "Qty", "Vendor", "Model", "Socket", "Mem", "TDP W", "Key specs", "Unit price", "Cur", "≈ " + display, "ex-VAT " + display, "Line total", "Flags", "Buy link"}
		const hrow = 2
		const nCols = 16
		const cLineTotal, cFlags, cLink = 14, 15, 16
		for i, h := range headers {
			c := cell(i+1, hrow)
			f.SetCellValue(sheet, c, h)
			f.SetCellStyle(sheet, c, c, st.header)
		}
		f.SetPanes(sheet, &excelize.Panes{Freeze: true, YSplit: hrow, TopLeftCell: "A3", ActivePane: "bottomLeft"})

		// Collapse identical part rows: one row per (part id, ownership) with a
		// Qty column. Owned units of a part stay a separate row from its to-buy
		// units (own 3 of 8 = one OWNED row qty 3 + one buy row qty 5).
		demand := toCount(partIDs)
		ownedQty := toCount(ownedIDs)
		partByID := map[string]Part{}
		for _, p := range parts {
			partByID[p.ID] = p
		}
		var rows []exportRow
		for _, id := range uniqueInOrder(partIDs) {
			p := partByID[id]
			qty := demand[id]
			ownedN := min(ownedQty[id], qty)
			if ownedN > 0 {
				rows = append(rows, exportRow{part: p, owned: true, qty: ownedN})
			}
			if buyN := qty - ownedN; buyN > 0 {
				r := exportRow{part: p, qty: buyN}
				ls, err := pricePart(ctx, id, region, display)
				if err != nil {
					return "", err
				}
				if len(ls) > 0 && ls[0].usable() {
					r.best, r.priced = ls[0], true
				}
				rows = append(rows, r)
			}
		}
		// Category grouping: category, then part id, OWNED row before its buy row.
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].part.Category != rows[j].part.Category {
				return rows[i].part.Category < rows[j].part.Category
			}
			if rows[i].part.ID != rows[j].part.ID {
				return rows[i].part.ID < rows[j].part.ID
			}
			return rows[i].owned && !rows[j].owned
		})

		row := hrow + 1
		var total, exTotal float64
		buyCovers, exCovers, buyUnits, ownedUnits := 0, 0, 0, 0
		unconv := 0 // buy rows whose best price couldn't convert — excluded from totals, never silently
		unconvCurs := map[string]bool{}
		catBuyRows, catSubtotal := 0, 0.0
		// flushCat emits a per-category subtotal row (to-buy line totals) when
		// the category holds 2+ buy rows; a lone row IS its own subtotal.
		flushCat := func(cat string) {
			if catBuyRows >= 2 {
				f.SetCellValue(sheet, cell(1, row), "Subtotal — "+cat)
				f.SetCellStyle(sheet, cell(1, row), cell(1, row), st.sumLabel)
				f.SetCellValue(sheet, cell(cLineTotal, row), catSubtotal)
				f.SetCellStyle(sheet, cell(cLineTotal, row), cell(cLineTotal, row), st.moneyBold)
				row++
			}
			catBuyRows, catSubtotal = 0, 0
		}
		for i, r := range rows {
			base := st.cell
			if (row-hrow)%2 == 0 {
				base = st.cellAlt
			}
			p := r.part
			status := "buy"
			if r.owned {
				status = "OWNED"
			}
			vals := []any{p.Category, status, r.qty, p.Vendor, p.Model, p.Socket, p.MemType, nil, keySpecs(p)}
			if p.TDPW > 0 {
				vals[7] = p.TDPW
			}
			for ci, v := range vals {
				c := cell(ci+1, row)
				if v != nil {
					f.SetCellValue(sheet, c, v)
				}
				f.SetCellStyle(sheet, c, c, base)
			}
			// price columns
			if r.owned {
				f.SetCellValue(sheet, cell(10, row), "— already owned —")
				ownedUnits += r.qty
			} else {
				buyUnits += r.qty
				catBuyRows++
				if r.priced {
					best := r.best
					f.SetCellValue(sheet, cell(10, row), best.total())
					f.SetCellValue(sheet, cell(11, row), best.Currency)
					if unit, ok := bestContribution(best, display); ok {
						line := unit * float64(r.qty)
						f.SetCellValue(sheet, cell(12, row), unit)
						f.SetCellValue(sheet, cell(cLineTotal, row), line)
						total += line
						buyCovers += r.qty
						catSubtotal += line
					} else {
						// Row keeps its native price; the flag + summary note make
						// the exclusion from the TO BUY total explicit.
						unconv++
						unconvCurs[best.Currency] = true
					}
					if ex, ok := exVATContribution(best, display); ok {
						f.SetCellValue(sheet, cell(13, row), ex)
						exTotal += ex * float64(r.qty)
						exCovers += r.qty
					}
					if fl := listingFlags(best, exportedAt); fl != "" {
						f.SetCellValue(sheet, cell(cFlags, row), fl)
					}
					if best.URL != "" {
						f.SetCellValue(sheet, cell(cLink, row), "open")
						f.SetCellHyperLink(sheet, cell(cLink, row), best.URL, "External")
						f.SetCellStyle(sheet, cell(cLink, row), cell(cLink, row), st.link)
					}
				} else {
					f.SetCellValue(sheet, cell(10, row), "no live listing")
				}
			}
			for cc := 10; cc <= nCols; cc++ {
				c := cell(cc, row)
				stl := base
				if cc == 10 || cc == 12 || cc == 13 || cc == cLineTotal {
					stl = st.money
					if (row-hrow)%2 == 0 {
						stl = st.moneyAlt
					}
				}
				// don't clobber the hyperlink style
				if v, _ := f.GetCellValue(sheet, c); !(cc == cLink && v == "open") {
					f.SetCellStyle(sheet, c, c, stl)
				}
			}
			if r.owned {
				f.SetCellStyle(sheet, cell(1, row), cell(nCols, row), st.owned)
			}
			row++
			if i+1 == len(rows) || rows[i+1].part.Category != p.Category {
				flushCat(p.Category)
			}
		}

		// Summary block.
		row += 2
		put := func(label, val string, strong bool) {
			lc, vc := cell(1, row), cell(2, row)
			f.SetCellValue(sheet, lc, label)
			f.SetCellValue(sheet, vc, val)
			ls := st.sumLabel
			if strong {
				ls = st.sumStrong
			}
			f.SetCellStyle(sheet, lc, lc, ls)
			row++
		}
		put("Compatible", yesno(spec.Compatible), true)
		if spec.Partial {
			put("Partial build", "missing "+strings.Join(spec.MissingForBuild, ", ")+" (add if this should boot on its own)", false)
		}
		put("Total TDP (W)", fmt.Sprintf("%d", spec.TotalTDPW), false)
		// Rack report lines — simple sums, the compat engine does the real
		// checking. Absent data prints nothing (no fake zeros).
		if uUsed, uCap := rackUnits(parts); uUsed > 0 || uCap > 0 {
			put("Rack units", uFmt(uUsed)+" consumed / "+uFmt(uCap)+" capacity", false)
		}
		if psuW := sumWatts(parts); psuW > 0 && spec.TotalTDPW > 0 {
			put("PSU output", fmt.Sprintf("%dW vs %dW total TDP (%.0f%% headroom)",
				psuW, spec.TotalTDPW, (float64(psuW)/float64(spec.TotalTDPW)-1)*100), false)
		}
		f.SetCellValue(sheet, cell(1, row), "TO BUY total ("+display+")")
		f.SetCellValue(sheet, cell(2, row), total)
		f.SetCellStyle(sheet, cell(1, row), cell(1, row), st.sumStrong)
		f.SetCellStyle(sheet, cell(2, row), cell(2, row), st.moneyBold)
		row++
		if exCovers > 0 {
			f.SetCellValue(sheet, cell(1, row), fmt.Sprintf("TO BUY ex-VAT total (%s, covers %d of %d priced parts)", display, exCovers, buyCovers))
			f.SetCellValue(sheet, cell(2, row), exTotal)
			f.SetCellStyle(sheet, cell(1, row), cell(1, row), st.sumLabel)
			f.SetCellStyle(sheet, cell(2, row), cell(2, row), st.moneyBold)
			row++
		}
		if unconv > 0 {
			curs := make([]string, 0, len(unconvCurs))
			for c := range unconvCurs {
				curs = append(curs, c)
			}
			sort.Strings(curs)
			put("TO BUY total excludes", fmt.Sprintf("%d unconverted listings (%s)", unconv, strings.Join(curs, ", ")), true)
		}
		put("Parts to buy", fmt.Sprintf("%d priced of %d needed", buyCovers, buyUnits), false)
		if ownedUnits > 0 {
			put("Already owned", fmt.Sprintf("%d", ownedUnits), false)
		}
		put("Prices fetched", asOf+" — re-export to refresh", false)
		for _, g := range spec.Gaps {
			put("Gap", g, false)
		}
		for _, n := range spec.Needs {
			put("Also need", fmt.Sprintf("%s ×%d (cable/adapter/slot)", n.Resource, n.Count), false)
		}
		for _, v := range spec.Violations {
			put("INCOMPATIBLE", v.Message, true)
		}

		widths := map[string]float64{"A": 15, "B": 9, "C": 5, "D": 16, "E": 30, "F": 12, "G": 8, "H": 8, "I": 44, "J": 11, "K": 6, "L": 12, "M": 12, "N": 12, "O": 24, "P": 10}
		for col, w := range widths {
			f.SetColWidth(sheet, col, col, w)
		}

		summaries = append(summaries, cmpRow{
			name: title, compatible: spec.Compatible, tdp: spec.TotalTDPW,
			total: total, exTotal: exTotal, exCovers: exCovers,
			buy: buyCovers, owned: ownedUnits, parts: len(parts),
		})
	}

	if len(summaries) > 1 {
		if idx, err := f.GetSheetIndex("Compare"); err == nil && idx != -1 {
			f.DeleteSheet("Compare") // refresh on re-export
		}
		if err := writeCompareSheet(f, st, summaries, display, asOf); err != nil {
			return "", err
		}
	}

	// Drop the default sheet now that real sheets exist (only present on a
	// freshly-created workbook; delete-only-sheet is a no-op so this runs after
	// the first NewSheet).
	if fresh {
		if idx, err := f.GetSheetIndex("Sheet1"); err == nil && idx != -1 {
			f.DeleteSheet("Sheet1")
		}
	}

	// Save via temp + rename so a crash mid-write never truncates a good
	// workbook. The temp name must keep the .xlsx extension — excelize.SaveAs
	// rejects unknown extensions ("unsupported workbook file format").
	tmp := path + ".tmp.xlsx"
	if err := f.SaveAs(tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// --- styling ---

type styles struct {
	title, header, cell, cellAlt, money, moneyAlt, moneyBold, link, owned, sumLabel, sumStrong int
}

func newStyles(f *excelize.File, display string) styles {
	border := []excelize.Border{
		{Type: "left", Color: "D0D0D0", Style: 1}, {Type: "right", Color: "D0D0D0", Style: 1},
		{Type: "top", Color: "D0D0D0", Style: 1}, {Type: "bottom", Color: "D0D0D0", Style: 1},
	}
	mk := func(s *excelize.Style) int { id, _ := f.NewStyle(s); return id }
	numFmt := 4 // #,##0.00
	return styles{
		title:     mk(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 14}}),
		header:    mk(&excelize.Style{Font: &excelize.Font{Bold: true, Color: "FFFFFF"}, Fill: excelize.Fill{Type: "pattern", Color: []string{"2F5496"}, Pattern: 1}, Border: border, Alignment: &excelize.Alignment{Vertical: "center"}}),
		cell:      mk(&excelize.Style{Border: border, Alignment: &excelize.Alignment{Vertical: "center", WrapText: true}}),
		cellAlt:   mk(&excelize.Style{Border: border, Fill: excelize.Fill{Type: "pattern", Color: []string{"F2F5FB"}, Pattern: 1}, Alignment: &excelize.Alignment{Vertical: "center", WrapText: true}}),
		money:     mk(&excelize.Style{Border: border, NumFmt: numFmt}),
		moneyAlt:  mk(&excelize.Style{Border: border, Fill: excelize.Fill{Type: "pattern", Color: []string{"F2F5FB"}, Pattern: 1}, NumFmt: numFmt}),
		moneyBold: mk(&excelize.Style{Font: &excelize.Font{Bold: true}, NumFmt: numFmt}),
		link:      mk(&excelize.Style{Font: &excelize.Font{Color: "0563C1", Underline: "single"}, Border: border}),
		owned:     mk(&excelize.Style{Border: border, Font: &excelize.Font{Italic: true, Color: "808080"}, Fill: excelize.Fill{Type: "pattern", Color: []string{"EDEDED"}, Pattern: 1}, Alignment: &excelize.Alignment{Vertical: "center", WrapText: true}}),
		sumLabel:  mk(&excelize.Style{Font: &excelize.Font{Bold: true}}),
		sumStrong: mk(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 12}}),
	}
}

type cmpRow struct {
	name              string
	compatible        bool
	tdp               int
	total             float64
	exTotal           float64
	exCovers          int
	buy, owned, parts int
}

func writeCompareSheet(f *excelize.File, st styles, rows []cmpRow, display, asOf string) error {
	cmp := "Compare"
	if _, err := f.NewSheet(cmp); err != nil {
		return fmt.Errorf("sheet %q: %w", cmp, err)
	}
	labels := []string{"Spec", "Compatible", "Total TDP (W)", "TO BUY (" + display + ")", "TO BUY ex-VAT (" + display + ")", "Parts to buy", "Already owned", "Prices fetched"}
	for i, l := range labels {
		c := cell(1, i+1)
		f.SetCellValue(cmp, c, l)
		f.SetCellStyle(cmp, c, c, st.header)
	}
	for j, s := range rows {
		col := j + 2
		f.SetCellValue(cmp, cell(col, 1), s.name)
		f.SetCellStyle(cmp, cell(col, 1), cell(col, 1), st.sumStrong)
		f.SetCellValue(cmp, cell(col, 2), yesno(s.compatible))
		f.SetCellValue(cmp, cell(col, 3), s.tdp)
		f.SetCellValue(cmp, cell(col, 4), s.total)
		f.SetCellStyle(cmp, cell(col, 4), cell(col, 4), st.moneyBold)
		if s.exCovers > 0 {
			f.SetCellValue(cmp, cell(col, 5), s.exTotal)
			f.SetCellStyle(cmp, cell(col, 5), cell(col, 5), st.moneyBold)
		} else {
			f.SetCellValue(cmp, cell(col, 5), "no VAT basis known")
		}
		f.SetCellValue(cmp, cell(col, 6), fmt.Sprintf("%d/%d", s.buy, s.parts-s.owned))
		f.SetCellValue(cmp, cell(col, 7), s.owned)
		f.SetCellValue(cmp, cell(col, 8), asOf+" — re-export to refresh")
	}
	f.SetColWidth(cmp, "A", "A", 22)
	f.SetColWidth(cmp, "B", "Z", 28)
	if idx, err := f.GetSheetIndex(cmp); err == nil {
		f.SetActiveSheet(idx)
	}
	return nil
}

func cell(col, row int) string {
	c, _ := excelize.CoordinatesToCellName(col, row)
	return c
}

// exportRow is one collapsed table row: a part × ownership status with its
// unit count and (for buy rows) the chosen best listing.
type exportRow struct {
	part   Part
	owned  bool
	qty    int
	best   Listing
	priced bool // a usable best listing exists
}

// listingFlags renders a listing's price-quality caveats compactly for the
// Flags column ("stale 21d, +shipping?, VAT?"). Empty when clean — the flags
// the engine computes must survive into the shareable artifact.
func listingFlags(l Listing, now time.Time) string {
	var fs []string
	if l.Stale {
		if l.SeenAt.IsZero() {
			fs = append(fs, "stale (no date)")
		} else {
			fs = append(fs, fmt.Sprintf("stale %dd", int(now.Sub(l.SeenAt).Hours()/24)))
		}
	}
	if l.ShippingUnknown {
		fs = append(fs, "+shipping?")
	}
	if l.VATUnknown {
		fs = append(fs, "VAT?")
	}
	if l.Unconverted {
		fs = append(fs, "unconverted")
	}
	if l.Dead {
		fs = append(fs, "dead")
	}
	return strings.Join(fs, ", ")
}

// uTokens sums "u"-kind resource tokens: "u:2"×4 = 8U, bare "u"×4 = 4U.
func uTokens(m map[string]int) float64 {
	var sum float64
	for tok, n := range m {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok != "u" && !strings.HasPrefix(tok, "u:") {
			continue
		}
		h := 1.0
		if v, err := strconv.ParseFloat(strings.TrimPrefix(tok, "u:"), 64); err == nil {
			h = v
		}
		sum += h * float64(n)
	}
	return sum
}

// rackUnits sums rack-space figures across the expanded part list for the
// report line: capacity from "u:" provides or a u_capacity attr, consumption
// from "u:" requires or a height_u attr on non-rack parts (a part providing
// U space never consumes its own units). ponytail: simple sums — the compat
// engine's resource accounting does the real fit checking.
func rackUnits(parts []Part) (used, capacity float64) {
	for _, p := range parts {
		c := uTokens(p.Provides)
		if c == 0 {
			if v, ok := flattenNum(p, "u_capacity"); ok {
				c = v
			}
		}
		if c > 0 {
			capacity += c
			continue
		}
		if u := uTokens(p.Requires); u > 0 {
			used += u
		} else if v, ok := flattenNum(p, "height_u"); ok {
			used += v
		}
	}
	return used, capacity
}

// uFmt renders a rack-unit sum; an absent side reads "?" — never a fake zero.
func uFmt(v float64) string {
	if v == 0 {
		return "?"
	}
	return strconv.FormatFloat(v, 'f', -1, 64) + "U"
}

// sumWatts totals rated PSU output across ALL parts — a server barebones
// carries its PSUs' wattage on itself (same convention as the compat engine).
func sumWatts(parts []Part) int {
	w := 0
	for _, p := range parts {
		w += p.Watts
	}
	return w
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "NO"
}

// keySpecs renders the free-form attrs compactly for the spreadsheet.
func keySpecs(p Part) string {
	if len(p.Attrs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(p.Attrs))
	for k := range p.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%v", k, p.Attrs[k])
	}
	return b.String()
}

// safeSheetName makes a spec id a valid, deterministic Excel sheet name
// (≤31 chars, no []:*?/\). Deterministic so a re-export replaces the same
// sheet. ponytail: two spec ids sharing a 28-char sanitized prefix would
// collide — fine for realistic ids.
func safeSheetName(sid string) string {
	repl := strings.NewReplacer("[", "(", "]", ")", ":", "-", "*", "-", "?", "-", "/", "-", "\\", "-")
	n := repl.Replace(sid)
	if r := []rune(n); len(r) > 28 { // rune-aware: don't split a UTF-8 char
		n = string(r[:28])
	}
	if n == "" {
		n = "spec"
	}
	return n
}

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
		if existing, err := excelize.OpenFile(path); err == nil {
			f, fresh = existing, false // edit the existing workbook in place
		}
	}
	if f == nil {
		f = excelize.NewFile()
	}
	defer f.Close()

	st := newStyles(f, display)
	var summaries []cmpRow

	for _, sid := range specIDs {
		name, partIDs, ownedIDs, err := store.loadSpec(sid)
		if err != nil {
			return "", fmt.Errorf("spec %s: %w", sid, err)
		}
		parts, err := store.getParts(partIDs)
		if err != nil {
			return "", err
		}
		owned := toSet(ownedIDs)
		prewarmLiveness(ctx, partIDs)
		spec := composeSpec(parts)

		// Deterministic sheet name so re-exporting a spec REPLACES its sheet
		// (update-in-place) rather than piling up "~2" duplicates.
		sheet := safeSheetName(sid)
		if idx, err := f.GetSheetIndex(sheet); err == nil && idx != -1 {
			f.DeleteSheet(sheet)
		}
		f.NewSheet(sheet)

		// Title.
		title := sid
		if name != "" {
			title += " — " + name
		}
		f.MergeCell(sheet, "A1", "L1")
		f.SetCellValue(sheet, "A1", title)
		f.SetCellStyle(sheet, "A1", "A1", st.title)
		f.SetRowHeight(sheet, 1, 24)

		headers := []string{"Category", "Status", "Vendor", "Model", "Socket", "Mem", "TDP W", "Key specs", "Best price", "Cur", "≈ " + display, "Buy link"}
		const hrow = 2
		for i, h := range headers {
			c := cell(i+1, hrow)
			f.SetCellValue(sheet, c, h)
			f.SetCellStyle(sheet, c, c, st.header)
		}
		f.SetPanes(sheet, &excelize.Panes{Freeze: true, YSplit: hrow, TopLeftCell: "A3", ActivePane: "bottomLeft"})

		row := hrow + 1
		var total float64
		buy, ownedN := 0, 0
		for _, p := range parts {
			base := st.cell
			if (row-hrow)%2 == 0 {
				base = st.cellAlt
			}
			isOwned := owned[p.ID]
			status := "buy"
			if isOwned {
				status = "OWNED"
				ownedN++
			}
			vals := []any{p.Category, status, p.Vendor, p.Model, p.Socket, p.MemType, nil, keySpecs(p)}
			if p.TDPW > 0 {
				vals[6] = p.TDPW
			}
			for i, v := range vals {
				c := cell(i+1, row)
				if v != nil {
					f.SetCellValue(sheet, c, v)
				}
				f.SetCellStyle(sheet, c, c, base)
			}
			// price columns
			priceCells := []int{9, 10, 11, 12}
			if isOwned {
				f.SetCellValue(sheet, cell(9, row), "— already owned —")
			} else {
				ls, err := pricePart(ctx, p.ID, region, display)
				if err != nil {
					return "", err
				}
				if len(ls) > 0 && ls[0].usable() {
					best := ls[0]
					f.SetCellValue(sheet, cell(9, row), best.total())
					f.SetCellValue(sheet, cell(10, row), best.Currency)
					if best.DisplayTotal > 0 {
						f.SetCellValue(sheet, cell(11, row), best.DisplayTotal)
						total += best.DisplayTotal
						buy++
					} else if best.Currency == display {
						total += best.total()
						buy++
					}
					if best.URL != "" {
						f.SetCellValue(sheet, cell(12, row), "open")
						f.SetCellHyperLink(sheet, cell(12, row), best.URL, "External")
						f.SetCellStyle(sheet, cell(12, row), cell(12, row), st.link)
					}
				} else {
					f.SetCellValue(sheet, cell(9, row), "no live listing")
				}
			}
			for _, cc := range priceCells {
				c := cell(cc, row)
				stl := base
				if cc == 9 || cc == 11 {
					stl = st.money
					if (row-hrow)%2 == 0 {
						stl = st.moneyAlt
					}
				}
				if cc == 12 && !isOwned {
					continue // link style already applied
				}
				// don't clobber the hyperlink style
				if v, _ := f.GetCellValue(sheet, c); !(cc == 12 && v == "open") {
					f.SetCellStyle(sheet, c, c, stl)
				}
			}
			if isOwned {
				f.SetCellStyle(sheet, cell(1, row), cell(12, row), st.owned)
			}
			row++
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
		f.SetCellValue(sheet, cell(1, row), "TO BUY total ("+display+")")
		f.SetCellValue(sheet, cell(2, row), total)
		f.SetCellStyle(sheet, cell(1, row), cell(1, row), st.sumStrong)
		f.SetCellStyle(sheet, cell(2, row), cell(2, row), st.moneyBold)
		row++
		put("Parts to buy", fmt.Sprintf("%d priced of %d needed", buy, len(parts)-ownedN), false)
		if ownedN > 0 {
			put("Already owned", fmt.Sprintf("%d", ownedN), false)
		}
		for _, g := range spec.Gaps {
			put("Gap", g, false)
		}
		for _, n := range spec.Needs {
			put("Also need", fmt.Sprintf("%s ×%d (cable/adapter/slot)", n.Resource, n.Count), false)
		}
		for _, v := range spec.Violations {
			put("INCOMPATIBLE", v.Message, true)
		}

		widths := map[string]float64{"A": 15, "B": 9, "C": 16, "D": 30, "E": 12, "F": 8, "G": 8, "H": 44, "I": 11, "J": 6, "K": 12, "L": 10}
		for col, w := range widths {
			f.SetColWidth(sheet, col, col, w)
		}

		summaries = append(summaries, cmpRow{
			name: title, compatible: spec.Compatible, tdp: spec.TotalTDPW,
			total: total, buy: buy, owned: ownedN, parts: len(parts),
		})
	}

	if len(summaries) > 1 {
		if idx, err := f.GetSheetIndex("Compare"); err == nil && idx != -1 {
			f.DeleteSheet("Compare") // refresh on re-export
		}
		writeCompareSheet(f, st, summaries, display)
	}

	// Drop the default sheet now that real sheets exist (only present on a
	// freshly-created workbook; delete-only-sheet is a no-op so this runs after
	// the first NewSheet).
	if fresh {
		if idx, err := f.GetSheetIndex("Sheet1"); err == nil && idx != -1 {
			f.DeleteSheet("Sheet1")
		}
	}

	if err := f.SaveAs(path); err != nil {
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
	buy, owned, parts int
}

func writeCompareSheet(f *excelize.File, st styles, rows []cmpRow, display string) {
	cmp := "Compare"
	f.NewSheet(cmp)
	labels := []string{"Spec", "Compatible", "Total TDP (W)", "TO BUY (" + display + ")", "Parts to buy", "Already owned"}
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
		f.SetCellValue(cmp, cell(col, 5), fmt.Sprintf("%d/%d", s.buy, s.parts-s.owned))
		f.SetCellValue(cmp, cell(col, 6), s.owned)
	}
	f.SetColWidth(cmp, "A", "A", 18)
	f.SetColWidth(cmp, "B", "Z", 28)
	if idx, err := f.GetSheetIndex(cmp); err == nil {
		f.SetActiveSheet(idx)
	}
}

func cell(col, row int) string {
	c, _ := excelize.CoordinatesToCellName(col, row)
	return c
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
	if len(n) > 28 {
		n = n[:28]
	}
	if n == "" {
		n = "spec"
	}
	return n
}

package main

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

// The flags cell must carry every caveat the engine computed, compactly.
func TestListingFlags(t *testing.T) {
	now := time.Now()
	l := Listing{
		Stale: true, SeenAt: now.AddDate(0, 0, -21),
		ShippingUnknown: true, VATUnknown: true, Unconverted: true, Dead: true,
	}
	want := "stale 21d, +shipping?, VAT?, unconverted, dead"
	if got := listingFlags(l, now); got != want {
		t.Errorf("flags = %q, want %q", got, want)
	}
	if got := listingFlags(Listing{}, now); got != "" {
		t.Errorf("clean listing must render empty flags, got %q", got)
	}
	if got := listingFlags(Listing{Stale: true}, now); got != "stale (no date)" {
		t.Errorf("zero SeenAt must not print garbage days, got %q", got)
	}
}

// exportFixture wires an in-memory store + seeded fx cache (no network) and
// returns the parsed rows of the exported sheet for spec "s1".
func exportFixture(t *testing.T, parts []Part, listings []Listing, partIDs, ownedIDs []string) [][]string {
	t.Helper()
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	old := store
	store = st
	t.Cleanup(func() { store = old })
	// Fresh empty SEK rate table: SEK→DKK fails fast (no rate) without touching
	// the network — the Unconverted path.
	fxMu.Lock()
	fxCache["SEK"] = rateSet{rates: map[string]float64{}, fetched: time.Now()}
	fxMu.Unlock()
	t.Cleanup(func() { fxMu.Lock(); delete(fxCache, "SEK"); fxMu.Unlock() })

	for _, p := range parts {
		if err := st.savePart(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range listings {
		if err := st.saveListing(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.saveSpec("s1", "test build", partIDs, ownedIDs); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "out.xlsx")
	region := Region{Country: "DK", Currency: "DKK"}
	if _, err := exportSpecsXLSX(context.Background(), []string{"s1"}, path, region, "DKK", false); err != nil {
		t.Fatal(err)
	}
	f, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := f.GetRows("s1", excelize.Options{RawCellValue: true})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// findRow returns the first sheet row whose cells contain every needle.
func findRow(rows [][]string, needles ...string) []string {
	for _, r := range rows {
		joined := strings.Join(r, " | ")
		ok := true
		for _, n := range needles {
			if !strings.Contains(joined, n) {
				ok = false
				break
			}
		}
		if ok {
			return r
		}
	}
	return nil
}

func TestExportHonesty(t *testing.T) {
	shipFree := 0.0
	vatIncl, vatExcl := true, false
	now := time.Now()
	parts := []Part{
		{ID: "cpu1", Category: "cpu", Model: "EPYC"},
		{ID: "nic1", Category: "nic", Model: "X710"},
		{ID: "ssd1", Category: "storage", Model: "SSD A"},
		{ID: "ssd2", Category: "storage", Model: "SSD B"},
	}
	listings := []Listing{
		// Clean, converted, VAT-known: 1000 DKK incl 25% VAT → 800 ex-VAT/unit.
		{ID: "l-cpu", PartID: "cpu1", Price: 1000, Shipping: &shipFree, Currency: "DKK",
			VATIncluded: &vatIncl, VATRate: 25, SeenAt: now},
		// Stale + unknown shipping/VAT + unconvertible currency.
		{ID: "l-nic", PartID: "nic1", Price: 500, Currency: "SEK", SeenAt: now.AddDate(0, 0, -21)},
		// Two storage buy rows → category subtotal.
		{ID: "l-ssd1", PartID: "ssd1", Price: 200, Shipping: &shipFree, Currency: "DKK",
			VATIncluded: &vatExcl, SeenAt: now},
		{ID: "l-ssd2", PartID: "ssd2", Price: 300, Shipping: &shipFree, Currency: "DKK",
			VATIncluded: &vatExcl, SeenAt: now},
	}
	// cpu1 ×3, own 1 → OWNED row qty 1 + buy row qty 2.
	partIDs := []string{"cpu1", "cpu1", "cpu1", "nic1", "ssd1", "ssd2"}
	rows := exportFixture(t, parts, listings, partIDs, []string{"cpu1"})

	// Qty collapse: one OWNED row (qty 1) and one buy row (qty 2, per-unit
	// price, line total = 2×1000).
	owned := findRow(rows, "OWNED", "cpu")
	if owned == nil || owned[2] != "1" {
		t.Fatalf("expected OWNED cpu row with qty 1, got %v", owned)
	}
	buy := findRow(rows, "cpu", "buy")
	if buy == nil {
		t.Fatal("no cpu buy row")
	}
	if buy[2] != "2" || buy[9] != "1000" || buy[11] != "1000" || buy[13] != "2000" {
		t.Errorf("qty collapse math wrong: qty=%s unit=%s ≈=%s line=%s", buy[2], buy[9], buy[11], buy[13])
	}
	if buy[12] != "800" {
		t.Errorf("ex-VAT unit = %s, want 800", buy[12])
	}

	// Flags cell of the messy listing.
	nic := findRow(rows, "nic", "buy")
	if nic == nil || len(nic) < 15 {
		t.Fatalf("no nic buy row: %v", nic)
	}
	for _, want := range []string{"stale 21d", "+shipping?", "VAT?", "unconverted"} {
		if !strings.Contains(nic[14], want) {
			t.Errorf("nic flags %q missing %q", nic[14], want)
		}
	}
	// Unconverted row keeps its native price but no converted/line figures.
	if nic[9] != "500" || nic[10] != "SEK" {
		t.Errorf("unconverted row must keep native price: %v", nic[9:11])
	}
	if len(nic) > 13 && (nic[11] != "" || nic[13] != "") {
		t.Errorf("unconverted row must not fake converted totals: %v", nic[11:14])
	}

	// Category subtotal for storage (2 buy rows): 200+300.
	sub := findRow(rows, "Subtotal — storage")
	if sub == nil || sub[13] != "500" {
		t.Fatalf("storage subtotal row wrong: %v", sub)
	}

	// TO BUY total: 2×1000 + 200 + 300 = 2500 — nic excluded, but EXPLICITLY.
	tot := findRow(rows, "TO BUY total (DKK)")
	if tot == nil || tot[1] != "2500" {
		t.Fatalf("TO BUY total wrong: %v", tot)
	}
	excl := findRow(rows, "TO BUY total excludes")
	if excl == nil || !strings.Contains(strings.Join(excl, " "), "1 unconverted listings (SEK)") {
		t.Fatalf("missing/wrong unconverted exclusion note: %v", excl)
	}

	// Ex-VAT summary: 2×800 + 200 + 300 = 2100 covering 4 of 4 priced units.
	ex := findRow(rows, "TO BUY ex-VAT total")
	if ex == nil {
		t.Fatal("missing ex-VAT summary line")
	}
	if !strings.Contains(ex[0], "covers 4 of 4 priced") {
		t.Errorf("ex-VAT coverage label wrong: %q", ex[0])
	}
	if v, _ := strconv.ParseFloat(ex[1], 64); v < 2099.99 || v > 2100.01 {
		t.Errorf("ex-VAT total = %s, want 2100", ex[1])
	}

	// Priced count excludes the unconverted unit — 4 of 5 needed.
	pc := findRow(rows, "Parts to buy")
	if pc == nil || pc[1] != "4 priced of 5 needed" {
		t.Fatalf("priced count wrong: %v", pc)
	}

	// As-of stamp.
	if findRow(rows, "Prices fetched", "re-export to refresh") == nil {
		t.Error("missing as-of stamp")
	}
}

// The rack report lines appear only when the data exists — no fake zeros.
func TestExportRackLines(t *testing.T) {
	parts := []Part{
		{ID: "rack1", Category: "case", Provides: map[string]int{"u:1": 42}},
		{ID: "srv1", Category: "server", Attrs: map[string]any{"height_u": 2}, Watts: 800, TDPW: 300},
	}
	rows := exportFixture(t, parts, nil, []string{"rack1", "srv1", "srv1"}, nil)
	u := findRow(rows, "Rack units")
	if u == nil || u[1] != "4U consumed / 42U capacity" {
		t.Fatalf("rack units line wrong: %v", u)
	}
	psu := findRow(rows, "PSU output")
	if psu == nil || !strings.Contains(psu[1], "1600W vs 600W") {
		t.Fatalf("psu line wrong: %v", psu)
	}
	// No U/watt data → no lines.
	rows = exportFixture(t, []Part{{ID: "p1", Category: "cpu"}}, nil, []string{"p1"}, nil)
	if findRow(rows, "Rack units") != nil || findRow(rows, "PSU output") != nil {
		t.Error("rack/psu lines must be absent without data")
	}
}

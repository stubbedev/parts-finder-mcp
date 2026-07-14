package main

import (
	"context"
	"testing"
	"time"
)

func fptr(v float64) *float64 { return &v }

func TestListingsSortAndStale(t *testing.T) {
	now := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	ls := []Listing{
		{ID: "a", Price: 100, Shipping: fptr(20), SeenAt: now},                   // total 120, fresh
		{ID: "b", Price: 90, Shipping: fptr(10), SeenAt: now.AddDate(0, 0, -30)}, // total 100, stale
		{ID: "c", Price: 100, Shipping: fptr(0), SeenAt: now},                    // total 100, fresh
	}
	markStale(ls, now, 0)
	sortListings(ls)
	// cheapest total first; tie (100) broken by freshness => c before b
	if ls[0].ID != "c" || ls[1].ID != "b" || ls[2].ID != "a" {
		t.Fatalf("bad order: %s %s %s", ls[0].ID, ls[1].ID, ls[2].ID)
	}
	if !ls[1].Stale {
		t.Errorf("30-day-old listing should be stale")
	}
	if ls[0].Stale {
		t.Errorf("fresh listing should not be stale")
	}
}

// Dead/unshippable listings sink but are NEVER removed.
func TestFlaggedListingsSinkNotDropped(t *testing.T) {
	ls := []Listing{
		{ID: "dead-cheap", Price: 10, Dead: true},
		{ID: "unship", Price: 20, Unshippable: true},
		{ID: "ok", Price: 100},
	}
	sortListings(ls)
	if len(ls) != 3 {
		t.Fatalf("nothing may be dropped, got %d", len(ls))
	}
	if ls[0].ID != "ok" {
		t.Errorf("usable must sort first, got %s", ls[0].ID)
	}
	if ls[1].ID != "dead-cheap" || ls[2].ID != "unship" {
		t.Errorf("flagged sorted by price after usable: %s, %s", ls[1].ID, ls[2].ID)
	}
}

func TestCheapestConverted(t *testing.T) {
	// Same-currency comparison needs no network (no convert() call).
	ls := []Listing{
		{ID: "a", Price: 50, Shipping: fptr(10), Currency: "USD"}, // total 60
		{ID: "b", Price: 55, Currency: "USD"},                     // total 55 <- cheapest
		{ID: "c", Price: 90, Currency: "USD"},
	}
	best, total, ok := cheapestConverted(context.Background(), ls, "USD")
	if !ok || best.ID != "b" || total != 55 {
		t.Fatalf("want b/55, got ok=%v id=%v total=%v", ok, best.ID, total)
	}
	// currency=="" compares native totals as-is, unknown-currency listings included.
	best, total, ok = cheapestConverted(context.Background(), ls, "")
	if !ok || best.ID != "b" || total != 55 {
		t.Fatalf("native: want b/55, got ok=%v id=%v total=%v", ok, best.ID, total)
	}
}

// VAT basis: ex-VAT is the comparison figure when known; unknown compares by
// gross (overestimate, flagged) and never fails.
func TestVATBasis(t *testing.T) {
	tr, fa := true, false
	incl := Listing{Price: 1250, VATIncluded: &tr, VATRate: 25}
	if ex, ok := incl.exVATTotal(); !ok || ex != 1000 {
		t.Errorf("incl 1250 @25%% should be ex 1000, got %v %v", ex, ok)
	}
	excl := Listing{Price: 1100, VATIncluded: &fa}
	if ex, ok := excl.exVATTotal(); !ok || ex != 1100 {
		t.Errorf("ex-VAT price passes through, got %v %v", ex, ok)
	}
	unknown := Listing{Price: 1050}
	if _, ok := unknown.exVATTotal(); ok {
		t.Errorf("unknown VAT basis must not be computable")
	}
	inclNoRate := Listing{Price: 1250, VATIncluded: &tr}
	if _, ok := inclNoRate.exVATTotal(); ok {
		t.Errorf("incl VAT with unknown rate must not be computable")
	}
	// Sorting: incl-VAT 1250 (ex 1000) must beat ex-VAT 1100 once ex totals are
	// annotated — the whole point of the VAT basis.
	ls := []Listing{
		{ID: "b2b", Price: 1100, VATIncluded: &fa, DisplayExVAT: 1100, DisplayTotal: 1100},
		{ID: "ebay", Price: 1250, VATIncluded: &tr, VATRate: 25, DisplayExVAT: 1000, DisplayTotal: 1250},
	}
	sortListings(ls)
	if ls[0].ID != "ebay" {
		t.Errorf("ex-VAT comparison should rank incl-VAT 1250 (ex 1000) first, got %s", ls[0].ID)
	}
}

// Explicitly out-of-stock listings sink like dead ones; unknown stock stays usable.
func TestOutOfStockSinks(t *testing.T) {
	fa := false
	ls := []Listing{
		{ID: "oos", Price: 10, InStock: &fa},
		{ID: "ok", Price: 100},
	}
	sortListings(ls)
	if len(ls) != 2 || ls[0].ID != "ok" {
		t.Fatalf("out-of-stock must sink, never drop: %+v", ls)
	}
	if ls[1].usable() {
		t.Errorf("in_stock=false must not be usable")
	}
}

func TestShipsTo(t *testing.T) {
	cases := []struct {
		tokens  []string
		country string
		want    bool
	}{
		{nil, "DK", true},                   // unknown => don't exclude
		{[]string{"DK"}, "DK", true},        // exact
		{[]string{"EU"}, "DK", true},        // EU member
		{[]string{"EU"}, "US", false},       // non-member
		{[]string{"WORLD"}, "DK", true},     // worldwide
		{[]string{"US", "CA"}, "DK", false}, // not listed
	}
	for _, c := range cases {
		if got := shipsTo(c.tokens, c.country); got != c.want {
			t.Errorf("shipsTo(%v,%q)=%v want %v", c.tokens, c.country, got, c.want)
		}
	}
}

func TestRankHits(t *testing.T) {
	dk := Region{Country: "DK"}
	// A vendor learned from stored listings (no hardcoded list).
	known := map[string]bool{"komplett.dk": true}
	hits := []SearchHit{
		{URL: "https://shop.jp/x"},         // foreign ccTLD => demote
		{URL: "https://www.newegg.com/y"},  // generic .com => neutral
		{URL: "https://proshop.dk/z"},      // local ccTLD => boost
		{URL: "https://server-parts.eu/w"}, // .eu + DK is EU => boost
	}
	rankHits(hits, dk, known)
	top := map[string]bool{hostOf(hits[0].URL): true, hostOf(hits[1].URL): true}
	if !top["proshop.dk"] || !top["server-parts.eu"] {
		t.Errorf("local/EU vendors should rank first, got %s, %s", hits[0].URL, hits[1].URL)
	}
	if hostOf(hits[3].URL) != "shop.jp" {
		t.Errorf("foreign ccTLD should sort last, got %s", hits[3].URL)
	}
	// Learned vendor scores as boost even though it's just a .dk here; verify
	// the known-set path directly.
	if rankScore("https://komplett.dk/a", dk, known) != 0 {
		t.Errorf("known vendor should score 0")
	}
	if rankScore("https://randomshop.com/a", dk, known) != 1 {
		t.Errorf("unknown generic .com should be neutral")
	}
}

func TestSubstituteMatch(t *testing.T) {
	orig := Part{ID: "cpu1", Category: "cpu", Socket: "SP5"}
	cases := []struct {
		cand Part
		want bool
	}{
		{Part{ID: "cpu2", Category: "cpu", Socket: "SP5"}, true},      // same slot
		{Part{ID: "cpu3", Category: "cpu", Socket: "LGA4677"}, false}, // wrong socket
		{Part{ID: "cpu4", Category: "cpu"}, true},                     // unknown socket => allowed
		{Part{ID: "gpu1", Category: "gpu", Socket: "SP5"}, false},     // wrong category
		{Part{ID: "cpu1", Category: "cpu", Socket: "SP5"}, false},     // itself
	}
	for _, c := range cases {
		if got := substituteMatch(orig, c.cand); got != c.want {
			t.Errorf("substituteMatch(%s)=%v want %v", c.cand.ID, got, c.want)
		}
	}
}

// A listing that couldn't convert to the display currency must sink below
// converted peers — a native SEK total compared raw against DKK misranks.
func TestUnconvertedSinks(t *testing.T) {
	ls := []Listing{
		{ID: "no-currency", Price: 90},                 // saved before currency was required
		{ID: "converted", Price: 100, Currency: "DKK"}, // display==currency → converts offline
	}
	annotateDisplay(context.Background(), ls, "DKK")
	if !ls[0].Unconverted {
		t.Fatalf("currency-less listing must be flagged unconverted")
	}
	if ls[1].Unconverted || ls[1].DisplayTotal != 100 {
		t.Fatalf("same-currency listing must convert: %+v", ls[1])
	}
	sortListings(ls)
	if ls[0].ID != "converted" {
		t.Errorf("unconverted must sink below converted peers, got %s first", ls[0].ID)
	}
}

// Shipping: nil = unknown (flagged), 0 = explicitly free.
func TestShippingUnknownFlag(t *testing.T) {
	ls := []Listing{
		{ID: "unknown", Price: 100, Currency: "DKK"},
		{ID: "free", Price: 100, Shipping: fptr(0), Currency: "DKK"},
	}
	annotateDisplay(context.Background(), ls, "DKK")
	if !ls[0].ShippingUnknown || ls[1].ShippingUnknown {
		t.Errorf("nil shipping flagged, explicit 0 not: %+v %+v", ls[0], ls[1])
	}
}

func TestShipsToAliases(t *testing.T) {
	if !shipsTo([]string{"UK"}, "GB") {
		t.Errorf("UK must match GB")
	}
	if !shipsTo([]string{"Europe"}, "DK") {
		t.Errorf("Europe must cover an EU member")
	}
	if shipsTo([]string{"Europe"}, "US") {
		t.Errorf("Europe must not cover US")
	}
}

// RDIMM in a UDIMM slot won't boot — substitutes must respect mem_module.
func TestSubstituteMatchMemModule(t *testing.T) {
	orig := Part{ID: "r1", Category: "ram", Attrs: map[string]any{"mem_module": "RDIMM"}}
	udimm := Part{ID: "r2", Category: "ram", Attrs: map[string]any{"mem_module": "UDIMM"}}
	rdimm := Part{ID: "r3", Category: "ram", Attrs: map[string]any{"mem_module": "rdimm"}}
	unknown := Part{ID: "r4", Category: "ram"}
	if substituteMatch(orig, udimm) {
		t.Errorf("UDIMM must not substitute RDIMM")
	}
	if !substituteMatch(orig, rdimm) {
		t.Errorf("same module type (case-insensitive) must match")
	}
	if !substituteMatch(orig, unknown) {
		t.Errorf("unknown module type stays allowed (unknown ≠ violation)")
	}
}

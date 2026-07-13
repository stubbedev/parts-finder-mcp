package main

import (
	"context"
	"testing"
	"time"
)

func TestListingsSortAndStale(t *testing.T) {
	now := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	ls := []Listing{
		{ID: "a", Price: 100, Shipping: 20, SeenAt: now},                    // total 120, fresh
		{ID: "b", Price: 90, Shipping: 10, SeenAt: now.AddDate(0, 0, -30)},  // total 100, stale
		{ID: "c", Price: 100, Shipping: 0, SeenAt: now},                     // total 100, fresh
	}
	markStale(ls, now)
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

func TestCheapestConverted(t *testing.T) {
	// Same-currency comparison needs no network (no convert() call).
	ls := []Listing{
		{ID: "a", Price: 50, Shipping: 10, Currency: "USD"}, // total 60
		{ID: "b", Price: 55, Currency: "USD"},               // total 55 <- cheapest
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

func TestShipsTo(t *testing.T) {
	cases := []struct {
		tokens  []string
		country string
		want    bool
	}{
		{nil, "DK", true},                    // unknown => don't exclude
		{[]string{"DK"}, "DK", true},         // exact
		{[]string{"EU"}, "DK", true},         // EU member
		{[]string{"EU"}, "US", false},        // non-member
		{[]string{"WORLD"}, "DK", true},      // worldwide
		{[]string{"US", "CA"}, "DK", false},  // not listed
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
		{URL: "https://shop.jp/x"},          // foreign ccTLD => demote
		{URL: "https://www.newegg.com/y"},   // generic .com => neutral
		{URL: "https://proshop.dk/z"},       // local ccTLD => boost
		{URL: "https://server-parts.eu/w"},  // .eu + DK is EU => boost
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
		{Part{ID: "cpu2", Category: "cpu", Socket: "SP5"}, true},        // same slot
		{Part{ID: "cpu3", Category: "cpu", Socket: "LGA4677"}, false},   // wrong socket
		{Part{ID: "cpu4", Category: "cpu"}, true},                       // unknown socket => allowed
		{Part{ID: "gpu1", Category: "gpu", Socket: "SP5"}, false},       // wrong category
		{Part{ID: "cpu1", Category: "cpu", Socket: "SP5"}, false},       // itself
	}
	for _, c := range cases {
		if got := substituteMatch(orig, c.cand); got != c.want {
			t.Errorf("substituteMatch(%s)=%v want %v", c.cand.ID, got, c.want)
		}
	}
}

package main

import (
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

func TestCheapestCurrencyFilter(t *testing.T) {
	ls := []Listing{
		{Price: 50, Currency: "USD"},
		{Price: 30, Currency: "EUR"}, // cheaper but wrong currency
		{Price: 60, Currency: "USD"},
	}
	best, ok := cheapest(ls, "USD")
	if !ok || best.Price != 50 {
		t.Fatalf("want cheapest USD 50, got ok=%v price=%v", ok, best.Price)
	}
	if _, ok := cheapest(ls, "GBP"); ok {
		t.Errorf("no GBP listings, should not match")
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

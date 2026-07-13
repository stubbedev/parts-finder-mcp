package main

import (
	"testing"
	"time"
)

// spec:<id> references expand recursively (rack = 12x node), owned carries up,
// and cycles error instead of hanging.
func TestExpandSpecIDs(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.saveSpec("node", "", []string{"cpu", "ram", "ram"}, []string{"ram"}); err != nil {
		t.Fatal(err)
	}
	if err := st.saveSpec("rack", "", []string{"spec:node", "spec:node", "switch"}, nil); err != nil {
		t.Fatal(err)
	}
	parts, owned, err := st.expandSpecIDs([]string{"spec:rack", "pdu"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := toCount(parts)
	if c["cpu"] != 2 || c["ram"] != 4 || c["switch"] != 1 || c["pdu"] != 1 {
		t.Fatalf("bad expansion: %v", parts)
	}
	// node owns 1 ram; two nodes => 2 owned ram carried up.
	if oc := toCount(owned); oc["ram"] != 2 {
		t.Fatalf("sub-spec owned must carry up per instance, got %v", owned)
	}
	// Cycle: a spec referencing itself must error, not recurse forever.
	if err := st.saveSpec("loop", "", []string{"spec:loop"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.expandSpecIDs([]string{"spec:loop"}, nil); err == nil {
		t.Fatal("cycle must error")
	}
}

// Re-saving a listing must never erase the previous price: history keeps every
// change (and skips no-change repeats).
func TestPriceHistory(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	l := Listing{ID: "l1", PartID: "p1", Price: 100, Currency: "DKK",
		SeenAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	if err := st.saveListing(l); err != nil {
		t.Fatal(err)
	}
	l.SeenAt = l.SeenAt.AddDate(0, 0, 1)
	if err := st.saveListing(l); err != nil { // same price — no new history row
		t.Fatal(err)
	}
	l.Price, l.SeenAt = 90, l.SeenAt.AddDate(0, 0, 6)
	if err := st.saveListing(l); err != nil {
		t.Fatal(err)
	}
	obs, err := st.priceHistory("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 2 || obs[0].Price != 100 || obs[1].Price != 90 {
		t.Fatalf("want [100, 90], got %+v", obs)
	}
	// The listing row itself holds the latest price.
	ls, err := st.listingsFor("p1")
	if err != nil || len(ls) != 1 || ls[0].Price != 90 {
		t.Fatalf("latest listing should be 90: %+v (%v)", ls, err)
	}
}

// VAT/stock/qty listing fields round-trip through SQLite (nullable columns).
func TestListingFieldsRoundTrip(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	tr := true
	in := Listing{ID: "l1", PartID: "p1", Price: 100, VATIncluded: &tr, VATRate: 25,
		QtyAvailable: 3, LeadDays: 5, SeenAt: time.Now()}
	if err := st.saveListing(in); err != nil {
		t.Fatal(err)
	}
	ls, err := st.listingsFor("p1")
	if err != nil || len(ls) != 1 {
		t.Fatal(err)
	}
	got := ls[0]
	if got.VATIncluded == nil || !*got.VATIncluded || got.VATRate != 25 {
		t.Errorf("VAT fields lost: %+v", got)
	}
	if got.QtyAvailable != 3 || got.LeadDays != 5 {
		t.Errorf("qty/lead lost: %+v", got)
	}
	if got.InStock != nil {
		t.Errorf("unset in_stock must stay nil (unknown), got %v", *got.InStock)
	}
}

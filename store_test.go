package main

import (
	"strings"
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

// Nested specs compose hierarchically: rules see one node at a time, child
// violations surface prefixed, and unmet child needs bubble to the parent
// where its parts (switch ports) can satisfy them.
func TestComposeHierarchical(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	parts := []Part{
		{ID: "mb", Category: "motherboard", Socket: "SP5", MemType: "DDR5", Attrs: map[string]any{"mem_max_gb": 512}},
		{ID: "cpu", Category: "cpu", Socket: "SP5", TDPW: 200},
		{ID: "ram", Category: "ram", MemType: "DDR5", Attrs: map[string]any{"capacity_gb": 256}},
		{ID: "psu", Category: "psu", Watts: 800},
		{ID: "nic", Category: "nic", Requires: map[string]int{"port:10g": 1}},
		{ID: "sw", Category: "switch", Provides: map[string]int{"port:10g": 48}},
	}
	for _, p := range parts {
		if err := st.savePart(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.saveSpec("node", "", []string{"mb", "cpu", "ram", "ram", "psu", "nic"}, nil); err != nil {
		t.Fatal(err)
	}
	// fat: 3x256=768 > 512 board max; lean: 1x256. POOLED limits would pass
	// (1024 vs 1024) — only per-node checking catches the overload.
	if err := st.saveSpec("fat", "", []string{"mb", "cpu", "ram", "ram", "ram", "psu", "nic"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.saveSpec("lean", "", []string{"mb", "cpu", "ram", "psu", "nic"}, nil); err != nil {
		t.Fatal(err)
	}

	// Healthy rack: nodes clean, bubbled port needs met by the switch.
	spec, err := st.composeIDs([]string{"spec:node", "spec:node", "sw"})
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Compatible {
		t.Fatalf("healthy rack must be compatible, violations: %+v", spec.Violations)
	}
	if len(spec.Needs) != 0 {
		t.Fatalf("switch must satisfy bubbled port needs, got %+v", spec.Needs)
	}
	if spec.TotalTDPW != 400 {
		t.Errorf("2 nodes x 200W = 400, got %d", spec.TotalTDPW)
	}

	// No switch: the nodes' port:10g needs surface at rack level.
	spec, err = st.composeIDs([]string{"spec:node", "spec:node"})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Needs) != 1 || spec.Needs[0].Resource != "port:10g" || spec.Needs[0].Count != 2 {
		t.Fatalf("want bubbled need port:10g x2, got %+v", spec.Needs)
	}

	// Per-node overload fires, prefixed with the node's spec id.
	spec, err = st.composeIDs([]string{"spec:fat", "spec:lean", "sw"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range spec.Violations {
		if v.Rule == "mem_capacity" && strings.Contains(v.Message, "spec:fat") {
			found = true
		}
	}
	if !found {
		t.Fatalf("per-node mem overload must fire prefixed, got %+v", spec.Violations)
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

// Opening a store rewrites pre-UTC local-offset timestamps to UTC so string
// ordering stays correct; unparsable values survive untouched.
func TestTimestampMigration(t *testing.T) {
	path := t.TempDir() + "/parts.db"
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate rows written before UTC normalization.
	if _, err := st.db.Exec(`INSERT INTO listing_history
	  (listing_id,part_id,vendor,price,shipping,currency,seen_at)
	  VALUES ('l1','p1','v',100,0,'DKK','2026-07-14T10:00:00+02:00'),
	         ('l1','p1','v',90,0,'DKK','not-a-time')`); err != nil {
		t.Fatal(err)
	}
	st.db.Close()
	st, err = openStore(path) // migration runs on open
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.db.Query(`SELECT seen_at FROM listing_history ORDER BY price DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if len(got) != 2 || got[0] != "2026-07-14T08:00:00Z" || got[1] != "not-a-time" {
		t.Fatalf("want [2026-07-14T08:00:00Z not-a-time], got %v", got)
	}
}

// Compat rules persist and round-trip (the overlay source).
func TestRulesRoundTrip(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	in := CompatRule{Name: "x", Kind: "match", CatA: "a", AttrA: "p",
		CatB: "b", AttrB: "q", Mode: "eq", Note: "why", SourceURL: "https://x"}
	if err := st.saveRule(in); err != nil {
		t.Fatal(err)
	}
	in.Disabled = true
	if err := st.saveRule(in); err != nil { // upsert
		t.Fatal(err)
	}
	rs, err := st.loadRules()
	if err != nil || len(rs) != 1 {
		t.Fatalf("want 1 rule, got %v (%v)", rs, err)
	}
	if !rs[0].Disabled || rs[0].SourceURL != "https://x" || rs[0].Mode != "eq" {
		t.Errorf("round-trip lost fields: %+v", rs[0])
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

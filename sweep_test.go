package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Rack physics are data rules over free-form attrs — no code per constraint.
func TestRackRules(t *testing.T) {
	rack := Part{ID: "rack", Category: "rack", Attrs: map[string]any{"u_capacity": 4, "weight_capacity_kg": 100}}
	srv1 := Part{ID: "srv1", Category: "server", Attrs: map[string]any{"height_u": 3, "weight_kg": 60}}
	srv2 := Part{ID: "srv2", Category: "server", Attrs: map[string]any{"height_u": 2, "weight_kg": 60}}
	fired := map[string]bool{}
	for _, v := range checkCompat([]Part{rack, srv1, srv2}) {
		fired[v.Rule] = true
	}
	if !fired["rack_u"] {
		t.Error("5U into a 4U cabinet must violate rack_u")
	}
	if !fired["rack_weight"] {
		t.Error("120kg on a 100kg rating must violate rack_weight")
	}
	for _, v := range checkCompat([]Part{rack, srv1}) {
		if v.Rule == "rack_u" || v.Rule == "rack_weight" {
			t.Errorf("fitting rack must not violate: %+v", v)
		}
	}
}

func TestPDUPowerRule(t *testing.T) {
	pdu := Part{ID: "pdu", Category: "pdu", Attrs: map[string]any{"power_capacity_w": 500}}
	cpu := Part{ID: "cpu", Category: "cpu", TDPW: 400}
	gpu := Part{ID: "gpu", Category: "gpu", TDPW: 300}
	fired := false
	for _, v := range checkCompat([]Part{pdu, cpu, gpu}) {
		if v.Rule == "pdu_power" {
			fired = true
		}
	}
	if !fired {
		t.Error("700W of draw on a 500W PDU must violate pdu_power")
	}
}

// The flip side of unknowns-never-violate: a rule whose counterpart is present
// reports the missing data instead of silently not biting.
func TestRuleGaps(t *testing.T) {
	spec := composeSpec([]Part{
		{ID: "cpu1", Category: "cpu", TDPW: 100}, // socket unknown
		{ID: "mb1", Category: "motherboard", Socket: "SP5"},
	})
	if !containsSub(spec.Gaps, "cpu1: socket unknown") {
		t.Errorf("cpu without socket next to a board must gap, got %v", spec.Gaps)
	}
	// Wildcard rule: rack present, nothing declares height_u → ONE aggregate
	// gap, not a flag on every part in the build.
	spec = composeSpec([]Part{
		{ID: "rack", Category: "rack", Attrs: map[string]any{"u_capacity": 42}},
		{ID: "srv", Category: "server"},
	})
	if !containsSub(spec.Gaps, "rule rack_u") {
		t.Errorf("rack with no U-height consumers must gap, got %v", spec.Gaps)
	}
	n := 0
	for _, g := range spec.Gaps {
		if strings.Contains(g, "height_u") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("wildcard gap must fire once, got %d in %v", n, spec.Gaps)
	}
}

func TestAggregateOnlyFlag(t *testing.T) {
	spec := composeSpec([]Part{
		{ID: "mb1", Category: "motherboard", Socket: "SP5"},
		{ID: "mb2", Category: "motherboard", Socket: "SP5"},
	})
	if !spec.AggregateOnly {
		t.Error("flat multi-motherboard build must set aggregate_only")
	}
}

// Nodes bubble their U-height/weight up through spec:<id> composition, so the
// rack rule sees them on the synthetic aggregate.
func TestRackBubbleThroughSpecs(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Part{
		{ID: "chassis", Category: "server", Watts: 800, Attrs: map[string]any{"height_u": 2}},
		{ID: "cpu", Category: "cpu", Socket: "SP5", TDPW: 200},
		{ID: "rack", Category: "rack", Attrs: map[string]any{"u_capacity": 4}},
	} {
		if err := st.savePart(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.saveSpec("node", "", []string{"chassis", "cpu"}, nil); err != nil {
		t.Fatal(err)
	}
	spec, err := st.composeIDs([]string{"spec:node", "spec:node", "spec:node", "rack"})
	if err != nil {
		t.Fatal(err)
	}
	fired := false
	for _, v := range spec.Violations {
		if v.Rule == "rack_u" {
			fired = true
		}
	}
	if !fired {
		t.Errorf("3 x 2U nodes must violate a 4U cabinet, got %+v", spec.Violations)
	}
	spec, err = st.composeIDs([]string{"spec:node", "spec:node", "rack"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range spec.Violations {
		if v.Rule == "rack_u" {
			t.Errorf("2 x 2U nodes fit a 4U cabinet: %+v", v)
		}
	}
}

// A thin re-save (id + category to hang a listing on) must not wipe enrichment.
func TestSavePartMerge(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	full := Part{ID: "x", Category: "cpu", Socket: "SP5", TDPW: 280,
		Attrs: map[string]any{"cores": 32}, Provides: map[string]int{"pcie:lane": 128}}
	if err := st.savePart(full); err != nil {
		t.Fatal(err)
	}
	if err := st.savePart(Part{ID: "x", Category: "cpu", Vendor: "AMD"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.getParts([]string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	p := got[0]
	if p.Socket != "SP5" || p.TDPW != 280 {
		t.Errorf("thin re-save wiped scalars: %+v", p)
	}
	if p.Vendor != "AMD" {
		t.Error("incoming known value must overwrite")
	}
	if p.Attrs["cores"] == nil {
		t.Error("thin re-save wiped attrs")
	}
	if p.Provides["pcie:lane"] != 128 {
		t.Error("thin re-save wiped provides")
	}
}

func TestCollapseWS(t *testing.T) {
	in := "  a   b\t\tc\n\n\n\n   d  \n"
	if got, want := collapseWS(in), "a b c\n\nd"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestConsentWallDetection(t *testing.T) {
	banner := "Wir verwenden Cookies und andere Technologien. Wenn Sie zustimmen und Alle akzeptieren auswählen, können auch 24 Drittunternehmen, die als unsere Partner agieren, personenbezogene Daten erfassen."
	if !isConsentWall(banner) {
		t.Error("cookie banner text must be detected")
	}
	if isConsentWall("Dell PowerEdge R740 2U server, 8x 3.5 bays, price 5230 kr.") {
		t.Error("real listing text must not be flagged")
	}
	if isConsentWall(strings.Repeat("we use cookies and consent partners. ", 100)) {
		t.Error("long real content mentioning cookies must not be flagged")
	}
}

func TestSubstituteCautions(t *testing.T) {
	orig := Part{ID: "g1", Category: "gpu", LengthMM: 250, TDPW: 200, Requires: map[string]int{"pin:8": 2}}
	cand := Part{ID: "g2", Category: "gpu", LengthMM: 300, TDPW: 300, Requires: map[string]int{"pin:8": 3}}
	cs := substituteCautions(orig, cand)
	if len(cs) != 3 {
		t.Errorf("want cautions for requires/length/draw, got %v", cs)
	}
	if cs := substituteCautions(orig, orig); len(cs) != 0 {
		t.Errorf("identical part must produce no cautions, got %v", cs)
	}
}

func TestListingAgeDays(t *testing.T) {
	now := time.Now()
	ls := []Listing{{ID: "l", SeenAt: now.AddDate(0, 0, -5)}}
	markStale(ls, now, 0)
	if ls[0].AgeDays != 5 || ls[0].Stale {
		t.Errorf("5-day listing: age=%d stale=%v, want 5/false", ls[0].AgeDays, ls[0].Stale)
	}
	markStale(ls, now, 3)
	if !ls[0].Stale {
		t.Error("max_age_days=3 must flag a 5-day listing stale")
	}
}

// Fan-out merges engines and dedupes; a fully throttled wave falls through to
// the next one.
func TestSearchChainFanout(t *testing.T) {
	mk := func(name string, hits []SearchHit, err error) searchEngine {
		return searchEngine{name, func(context.Context, string, int, Region) ([]SearchHit, error) {
			return hits, err
		}}
	}
	hits, err := searchChain(context.Background(), []searchEngine{
		mk("t-a", []SearchHit{{URL: "http://a"}, {URL: "http://b"}}, nil),
		mk("t-b", []SearchHit{{URL: "http://b"}, {URL: "http://c"}}, nil),
	}, "q", 10, Region{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Errorf("merged fan-out must dedupe to 3 hits, got %v", hits)
	}
	hits, err = searchChain(context.Background(), []searchEngine{
		mk("t-c", nil, errRateLimited),
		mk("t-d", nil, errRateLimited),
		mk("t-e", nil, errRateLimited),
		mk("t-f", []SearchHit{{URL: "http://z"}}, nil),
	}, "q", 10, Region{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].URL != "http://z" {
		t.Errorf("throttled wave must fall through to the next, got %v", hits)
	}
}

func containsSub(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

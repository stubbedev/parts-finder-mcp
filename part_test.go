package main

import "testing"

// Self-test: known-good build has no violations; known-bad build fires the
// expected rules. Guards the compat engine against silent breakage.
func TestCompat(t *testing.T) {
	good := []Part{
		{ID: "cpu1", Category: "cpu", Socket: "SP5", TDPW: 300},
		{ID: "mb1", Category: "motherboard", Socket: "SP5", MemType: "DDR5", FormFactor: "ATX"},
		{ID: "ram1", Category: "ram", MemType: "DDR5"},
		{ID: "psu1", Category: "psu", Watts: 800, PowerConnectors: []string{"24-pin", "8-pin", "8-pin"}},
		{ID: "case1", Category: "case", LengthMM: 400, FormFactor: "E-ATX"},
		{ID: "gpu1", Category: "gpu", LengthMM: 300, TDPW: 250, PowerConnectors: []string{"8-pin"}},
	}
	if vs := checkCompat(good); len(vs) != 0 {
		t.Fatalf("good build should be compatible, got: %+v", vs)
	}

	bad := []Part{
		{ID: "cpu1", Category: "cpu", Socket: "LGA4677", TDPW: 350},
		{ID: "mb1", Category: "motherboard", Socket: "SP5", MemType: "DDR5", FormFactor: "E-ATX"},
		{ID: "ram1", Category: "ram", MemType: "DDR4"},
		{ID: "psu1", Category: "psu", Watts: 300, PowerConnectors: []string{"24-pin"}},
		{ID: "case1", Category: "case", LengthMM: 250, FormFactor: "Micro-ATX"},
		{ID: "gpu1", Category: "gpu", LengthMM: 320, TDPW: 250, PowerConnectors: []string{"12VHPWR"}},
	}
	got := map[string]bool{}
	for _, v := range checkCompat(bad) {
		got[v.Rule] = true
	}
	for _, want := range []string{"cpu_socket", "ram_mem_type", "psu_headroom", "gpu_length", "form_factor_fit", "power_connector"} {
		if !got[want] {
			t.Errorf("bad build should fire rule %q, violations: %+v", want, checkCompat(bad))
		}
	}
}

// Generic resource accounting: any part type participates via provides/requires.
func TestResourceAccounting(t *testing.T) {
	mobo := Part{ID: "mb", Category: "motherboard",
		Provides: map[string]int{"dimm:ddr5": 4, "pcie:x16": 1, "pcie:x8": 1}}
	ram := Part{ID: "ram", Category: "ram", Requires: map[string]int{"dimm:ddr5": 4}}
	hba := Part{ID: "hba", Category: "hba", Requires: map[string]int{"pcie:x8": 1}}
	nic := Part{ID: "nic", Category: "nic", Requires: map[string]int{"pcie:x8": 1}}
	cse := Part{ID: "case", Category: "case", Provides: map[string]int{"bay:3.5": 2}}
	hdd := Part{ID: "hdd", Category: "storage", Requires: map[string]int{"bay:3.5": 1}}

	// Good: nic's x8 fits the spare x16 slot (width flexibility).
	if vs := resourceViolations([]Part{mobo, ram, hba, nic, cse, hdd}); len(vs) != 0 {
		t.Fatalf("should fit (x8 into x16), got: %+v", vs)
	}
	// Bad: 3 drives into 2 bays.
	hdd2 := Part{ID: "hdd2", Category: "storage", Requires: map[string]int{"bay:3.5": 1}}
	hdd3 := Part{ID: "hdd3", Category: "storage", Requires: map[string]int{"bay:3.5": 1}}
	vs := resourceViolations([]Part{cse, hdd, hdd2, hdd3})
	if len(vs) != 1 || vs[0].Rule != "resource" {
		t.Fatalf("want 1 resource violation for bays, got: %+v", vs)
	}
	// Bad: x16 GPU can't go into an x8 slot (narrower never satisfies wider).
	gpu := Part{ID: "gpu", Category: "gpu", Requires: map[string]int{"pcie:x16": 1}}
	onlyX8 := Part{ID: "mb2", Category: "motherboard", Provides: map[string]int{"pcie:x8": 2}}
	if vs := resourceViolations([]Part{onlyX8, gpu}); len(vs) != 1 {
		t.Fatalf("x16 into x8 must violate, got: %+v", vs)
	}
	// Parts with no maps don't constrain anything.
	if vs := resourceViolations([]Part{{ID: "x", Category: "cpu"}}); len(vs) != 0 {
		t.Fatalf("no maps => no violations, got: %+v", vs)
	}
}

// Repeating a part id in a spec = quantity: each instance consumes resources
// and counts toward TDP; deficits surface as structured Needs.
func TestQuantityAndNeeds(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	psu := Part{ID: "psu", Category: "psu", Watts: 1000,
		Provides: map[string]int{"cable:8pin-eps": 1}}
	cpu := Part{ID: "cpu", Category: "cpu", TDPW: 200,
		Requires: map[string]int{"cable:8pin-eps": 1}}
	for _, p := range []Part{psu, cpu} {
		if err := st.savePart(p); err != nil {
			t.Fatal(err)
		}
	}
	// Dual-CPU build: cpu twice.
	parts, err := st.getParts([]string{"psu", "cpu", "cpu"})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("duplicates must be preserved, got %d parts", len(parts))
	}
	spec := composeSpec(parts)
	if spec.TotalTDPW != 400 {
		t.Errorf("2x cpu TDP should be 400, got %d", spec.TotalTDPW)
	}
	// 2 CPUs need 2 EPS cables, PSU provides 1 -> Need{cable:8pin-eps, 1}.
	if len(spec.Needs) != 1 || spec.Needs[0].Resource != "cable:8pin-eps" || spec.Needs[0].Count != 1 {
		t.Fatalf("want need cable:8pin-eps x1, got %+v", spec.Needs)
	}
}

// Attribute queries: numeric + string ops over scalar fields AND free attrs.
func TestMatchWhere(t *testing.T) {
	gpu := Part{ID: "gpu", Category: "gpu", TDPW: 140,
		Attrs: map[string]any{"cuda_compute": 8.9, "vram_gb": 20, "interface": "PCIe 4.0 x16"}}
	cpu := Part{ID: "cpu", Category: "cpu",
		Attrs: map[string]any{"l3_cache_mb": 384, "cores": 32}}
	cases := []struct {
		p    Part
		w    Where
		want bool
	}{
		{gpu, Where{"cuda_compute", "gte", 8.9}, true},
		{gpu, Where{"cuda_compute", "gt", 9.0}, false},
		{gpu, Where{"vram_gb", "gte", 16}, true},
		{gpu, Where{"tdp_w", "lte", 150.0}, true},          // scalar field via same path
		{gpu, Where{"interface", "contains", "pcie 4"}, true}, // case-folded contains
		{gpu, Where{"l3_cache_mb", "gte", 1}, false},       // absent attr never matches
		{cpu, Where{"l3_cache_mb", "gte", 256.0}, true},
		{cpu, Where{"l3_cache_mb", "exists", nil}, true},
		{gpu, Where{"l3_cache_mb", "exists", nil}, false},
		{cpu, Where{"cores", "eq", "32"}, true},            // string "32" vs number coerces numeric
	}
	for _, c := range cases {
		if got := matchWhere(c.p, c.w); got != c.want {
			t.Errorf("matchWhere(%s, %+v)=%v want %v", c.p.ID, c.w, got, c.want)
		}
	}
}

// Attrs must survive a store round-trip (JSON column).
func TestAttrsRoundTrip(t *testing.T) {
	st, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	in := Part{ID: "g", Category: "gpu", Attrs: map[string]any{"cuda_compute": 8.9, "name": "ada"}}
	if err := st.savePart(in); err != nil {
		t.Fatal(err)
	}
	got, err := st.getParts([]string{"g"})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := toFloat(got[0].Attrs["cuda_compute"]); v != 8.9 {
		t.Errorf("cuda_compute lost: %+v", got[0].Attrs)
	}
	if got[0].Attrs["name"] != "ada" {
		t.Errorf("string attr lost: %+v", got[0].Attrs)
	}
}

// Unknown attributes must be gaps, never violations.
func TestUnknownNoFalseViolation(t *testing.T) {
	parts := []Part{
		{ID: "cpu1", Category: "cpu"},         // no socket/TDP
		{ID: "mb1", Category: "motherboard"},  // no socket/memtype
		{ID: "ram1", Category: "ram"},
		{ID: "psu1", Category: "psu"},
	}
	if vs := checkCompat(parts); len(vs) != 0 {
		t.Fatalf("unknown attrs must not violate, got: %+v", vs)
	}
	spec := composeSpec(parts)
	if spec.Compatible != true {
		t.Errorf("no violations => compatible")
	}
	if len(spec.Gaps) == 0 {
		t.Errorf("missing TDP/attrs should surface as gaps")
	}
}

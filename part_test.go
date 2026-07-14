package main

import (
	"strings"
	"testing"
)

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

// Data-driven combo rules: module type, cpu mem gen, capacity caps, cooler
// socket lists, superset port tokens. Unknowns never violate.
func TestExtendedCombos(t *testing.T) {
	mb := Part{ID: "mb", Category: "motherboard", Socket: "SP5", MemType: "DDR5",
		Attrs: map[string]any{"mem_module": "RDIMM", "mem_max_gb": 512, "dimm_max_gb": 64}}
	fire := func(parts []Part, rule string) bool {
		for _, v := range checkCompat(parts) {
			if v.Rule == rule {
				return true
			}
		}
		return false
	}
	// RDIMM board + UDIMM stick = no boot.
	udimm := Part{ID: "u1", Category: "ram", MemType: "DDR5", Attrs: map[string]any{"mem_module": "UDIMM"}}
	if !fire([]Part{mb, udimm}, "ram_module_type") {
		t.Errorf("UDIMM on RDIMM board must violate")
	}
	// Matching module type, normalized case: fine.
	rdimm := Part{ID: "r1", Category: "ram", MemType: "DDR5",
		Attrs: map[string]any{"mem_module": "rdimm", "capacity_gb": 64}}
	if fire([]Part{mb, rdimm}, "ram_module_type") {
		t.Errorf("rdimm == RDIMM after normalization")
	}
	// Unknown module type on the stick: gap territory, no violation.
	plain := Part{ID: "p1", Category: "ram", MemType: "DDR5"}
	if fire([]Part{mb, plain}, "ram_module_type") {
		t.Errorf("unknown module type must not violate")
	}
	// CPU with a DDR4 controller on a DDR5 board.
	cpu4 := Part{ID: "c4", Category: "cpu", Socket: "SP5", MemType: "DDR4"}
	if !fire([]Part{mb, cpu4}, "cpu_mem_type") {
		t.Errorf("DDR4 cpu on DDR5 board must violate")
	}
	// 9x64GB = 576GB > 512GB board max (sum); a 128GB stick > 64GB/DIMM (each).
	nine := []Part{mb}
	for i := 0; i < 9; i++ {
		nine = append(nine, rdimm)
	}
	if !fire(nine, "mem_capacity") {
		t.Errorf("576GB total on 512GB board must violate")
	}
	big := Part{ID: "big", Category: "ram", MemType: "DDR5",
		Attrs: map[string]any{"mem_module": "RDIMM", "capacity_gb": 128}}
	if !fire([]Part{mb, big}, "dimm_capacity") {
		t.Errorf("128GB stick vs 64GB/DIMM cap must violate")
	}
	// Cooler socket list: "SP5, SP6" fits SP5 board; "AM45" must not match "AM4".
	cooler := Part{ID: "cool", Category: "cooler", Attrs: map[string]any{"sockets": "SP5, SP6"}}
	if fire([]Part{mb, cooler}, "cooler_socket") {
		t.Errorf("SP5 in [SP5, SP6] should fit")
	}
	wrong := Part{ID: "wrongcool", Category: "cooler", Attrs: map[string]any{"sockets": "LGA4677"}}
	if !fire([]Part{mb, wrong}, "cooler_socket") {
		t.Errorf("LGA4677-only cooler on SP5 board must violate")
	}
	if tokensContain("AM45", "AM4") {
		t.Errorf("token match must be exact, not substring")
	}
	// SAS ports satisfy SATA drives; never the reverse.
	hba := Part{ID: "hba", Category: "hba", Provides: map[string]int{"port:sas": 8}}
	sata := Part{ID: "ssd", Category: "storage", Requires: map[string]int{"port:sata": 1}}
	if vs := resourceViolations([]Part{hba, sata}); len(vs) != 0 {
		t.Errorf("sata drive on sas port should fit, got %+v", vs)
	}
	sataCtl := Part{ID: "ctl", Category: "hba", Provides: map[string]int{"port:sata": 8}}
	sasDrive := Part{ID: "sas1", Category: "storage", Requires: map[string]int{"port:sas": 1}}
	if vs := resourceViolations([]Part{sataCtl, sasDrive}); len(vs) != 1 {
		t.Errorf("sas drive on sata-only controller must violate, got %+v", vs)
	}
	// RAM faster than board max: gap (downclock), never a violation.
	fastRAM := Part{ID: "fast", Category: "ram", MemType: "DDR5", MemSpeed: 6400}
	slowMB := Part{ID: "smb", Category: "motherboard", MemType: "DDR5", MemSpeed: 4800}
	if fire([]Part{slowMB, fastRAM}, "mem_speed") {
		t.Errorf("faster RAM must not violate")
	}
	spec := composeSpec([]Part{slowMB, fastRAM})
	found := false
	for _, g := range spec.Gaps {
		if strings.Contains(g, "downclock") {
			found = true
		}
	}
	if !found {
		t.Errorf("downclock should surface as gap, gaps: %v", spec.Gaps)
	}
}

// Multi-node (rack) semantics: violations need proof — matches accept any
// counterpart, capacity limits pool across nodes — and the aggregate check is
// announced as a gap, never silent.
func TestMultiNodeAggregate(t *testing.T) {
	mb := Part{ID: "mb", Category: "motherboard", Socket: "SP5", MemType: "DDR5",
		Attrs: map[string]any{"mem_max_gb": 512}}
	cpu := Part{ID: "cpu", Category: "cpu", Socket: "SP5"}
	ram := Part{ID: "ram", Category: "ram", MemType: "DDR5", Attrs: map[string]any{"capacity_gb": 256}}
	// 2 nodes flattened: 4x256GB vs 2x512GB pooled limit — no cross-node
	// false violation (the old first()-limit check would have fired here).
	rack := []Part{mb, mb, cpu, cpu, ram, ram, ram, ram}
	if vs := checkCompat(rack); len(vs) != 0 {
		t.Fatalf("homogeneous rack must not cross-pair nodes, got: %+v", vs)
	}
	spec := composeSpec(rack)
	found := false
	for _, g := range spec.Gaps {
		if strings.Contains(g, "multi-node") {
			found = true
		}
	}
	if !found {
		t.Errorf("multi-node build must gap on aggregate checking, gaps: %v", spec.Gaps)
	}
	// A 5th stick blows the pooled limit (1280 > 1024).
	if vs := checkCompat(append(rack, ram)); len(vs) == 0 {
		t.Errorf("pooled capacity exceeded must violate")
	}
	// Reverse match direction: a board no CPU fits is a proven misfit even
	// when every CPU found a home elsewhere.
	lga := Part{ID: "mb2", Category: "motherboard", Socket: "LGA4677", MemType: "DDR5"}
	got := map[string]bool{}
	for _, v := range checkCompat([]Part{mb, lga, cpu, cpu}) {
		got[v.Rule] = true
	}
	if !got["cpu_socket"] {
		t.Errorf("board matched by no cpu must fire cpu_socket")
	}
}

// Rules are data: the store overlay can disable a builtin, override it, and
// add new rules (including supersets) that apply immediately.
func TestRulesOverlay(t *testing.T) {
	defer setRuleOverlay(nil) // never leak overlay into other tests

	cpu := Part{ID: "cpu", Category: "cpu", Socket: "SP5"}
	mb := Part{ID: "mb", Category: "motherboard", Socket: "LGA4677"}
	fire := func(rule string) bool {
		for _, v := range checkCompat([]Part{cpu, mb}) {
			if v.Rule == rule {
				return true
			}
		}
		return false
	}
	if !fire("cpu_socket") {
		t.Fatal("builtin must fire before overlay")
	}
	// Disable the builtin.
	setRuleOverlay([]CompatRule{{Name: "cpu_socket", Disabled: true}})
	if fire("cpu_socket") {
		t.Errorf("disabled builtin must not fire")
	}
	// Store-added rule: psu efficiency must match case airflow class (nonsense
	// pair, but proves arbitrary attrs work).
	setRuleOverlay([]CompatRule{{
		Name: "custom_pair", Kind: "match",
		CatA: "cpu", AttrA: "socket", CatB: "motherboard", AttrB: "socket", Mode: "eq",
	}})
	if !fire("custom_pair") {
		t.Errorf("store-added rule must fire")
	}
	// Store-added superset: nvme port satisfies sata request.
	setRuleOverlay([]CompatRule{{Name: "u2_sata", Kind: "superset", AttrA: "port:u.2", AttrB: "port:sata"}})
	ctl := Part{ID: "bp", Category: "backplane", Provides: map[string]int{"port:u.2": 4}}
	ssd := Part{ID: "ssd", Category: "storage", Requires: map[string]int{"port:sata": 1}}
	if vs := resourceViolations([]Part{ctl, ssd}); len(vs) != 0 {
		t.Errorf("store-taught superset must satisfy, got %+v", vs)
	}
}

// The deep-drill checklist is generated from the active rules: adding a rule
// starts asking for its attributes; no hand-maintained list.
func TestEmptyFieldsRuleDriven(t *testing.T) {
	defer setRuleOverlay(nil)
	ram := Part{ID: "r", Category: "ram", MemType: "DDR5"}
	has := func(fields []string, want string) bool {
		for _, f := range fields {
			if f == want {
				return true
			}
		}
		return false
	}
	f := emptyFields(ram)
	if !has(f, "mem_module") || !has(f, "capacity_gb") {
		t.Fatalf("builtin rule attrs must be requested for ram, got %v", f)
	}
	// New rule on a new category => its attr shows up in that category's list.
	setRuleOverlay([]CompatRule{{
		Name: "nic_port_speed", Kind: "match",
		CatA: "nic", AttrA: "port_speed", CatB: "switch", AttrB: "port_speed", Mode: "eq",
	}})
	nic := Part{ID: "n", Category: "nic"}
	if f := emptyFields(nic); !has(f, "port_speed") {
		t.Errorf("rule-added attr must appear in checklist, got %v", f)
	}
	// A filled attr disappears from the checklist.
	ram.Attrs = map[string]any{"mem_module": "RDIMM"}
	if f := emptyFields(ram); has(f, "mem_module") {
		t.Errorf("known attr must not be listed, got %v", f)
	}
}

// Dual-PSU: capacity is the SUM of PSU outputs; losing N+1 (largest PSU alone
// can't carry the load) is a gap, not a violation.
func TestMultiPSU(t *testing.T) {
	cpu := Part{ID: "cpu", Category: "cpu", TDPW: 600}
	psu := Part{ID: "psu", Category: "psu", Watts: 500}
	// Two 500W PSUs cover 600*1.3=780W combined — no violation.
	if vs := checkCompat([]Part{cpu, psu, psu}); len(vs) != 0 {
		t.Fatalf("dual 500W covers 780W need, got: %+v", vs)
	}
	// One 500W does not.
	got := map[string]bool{}
	for _, v := range checkCompat([]Part{cpu, psu}) {
		got[v.Rule] = true
	}
	if !got["psu_headroom"] {
		t.Errorf("single 500W vs 780W need must violate")
	}
	// Combined-OK but single-PSU-short => redundancy gap.
	spec := composeSpec([]Part{cpu, psu, psu})
	found := false
	for _, g := range spec.Gaps {
		if strings.Contains(g, "N+1") {
			found = true
		}
	}
	if !found {
		t.Errorf("dual PSU without single-PSU headroom should gap on N+1, gaps: %v", spec.Gaps)
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
		{gpu, Where{"tdp_w", "lte", 150.0}, true},             // scalar field via same path
		{gpu, Where{"interface", "contains", "pcie 4"}, true}, // case-folded contains
		{gpu, Where{"l3_cache_mb", "gte", 1}, false},          // absent attr never matches
		{cpu, Where{"l3_cache_mb", "gte", 256.0}, true},
		{cpu, Where{"l3_cache_mb", "exists", nil}, true},
		{gpu, Where{"l3_cache_mb", "exists", nil}, false},
		{cpu, Where{"cores", "eq", "32"}, true}, // string "32" vs number coerces numeric
		// Zero-valued attrs are unknown regardless of concrete type — JSON
		// attrs decode float64(0), which `v == 0` (int) used to miss.
		{Part{ID: "z", Category: "gpu", Attrs: map[string]any{"vram_gb": 0.0}},
			Where{"vram_gb", "exists", nil}, false},
		{Part{ID: "z", Category: "gpu", Attrs: map[string]any{"vram_gb": 0.0}},
			Where{"vram_gb", "lte", 8}, false},
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
		{ID: "cpu1", Category: "cpu"},        // no socket/TDP
		{ID: "mb1", Category: "motherboard"}, // no socket/memtype
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

// x4 + x16 requirements against x8 + x16 slots must ALWAYS fit (x4→x8,
// x16→x16). Before best-fit ordering this failed on ~half of runs when the x4
// requirement randomly grabbed the x16 slot.
func TestPCIeWiderSlotBestFit(t *testing.T) {
	for i := 0; i < 100; i++ {
		parts := []Part{
			{ID: "board", Category: "motherboard", Provides: map[string]int{"pcie:x16": 1, "pcie:x8": 1}},
			{ID: "nic", Category: "nic", Requires: map[string]int{"pcie:x4": 1}},
			{ID: "gpu", Category: "gpu", Requires: map[string]int{"pcie:x16": 1}},
		}
		deficits, _ := resourceDeficits(parts)
		if len(deficits) != 0 {
			t.Fatalf("run %d: false deficit %v — wider-slot fill must be best-fit", i, deficits)
		}
	}
}

// A server barebones (chassis + board + PSUs in one part) must cover the
// motherboard and psu functions — otherwise every barebones build reads
// "partial" forever.
func TestBarebonesCoversBoardAndPSU(t *testing.T) {
	spec := composeSpec([]Part{
		{ID: "r730", Category: "barebones", Watts: 1100,
			Provides: map[string]int{"dimm:ddr4": 24, "pcie:x16": 2}},
		{ID: "cpu1", Category: "cpu", Socket: "LGA2011-3"},
		{ID: "ram1", Category: "ram", Requires: map[string]int{"dimm:ddr4": 1}},
	})
	for _, m := range spec.MissingForBuild {
		if m == "motherboard" || m == "psu" {
			t.Errorf("barebones with dimm slots + watts must cover %s", m)
		}
	}
}

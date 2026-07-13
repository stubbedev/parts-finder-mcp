package main

import "testing"

// Self-test: known-good build has no violations; known-bad build fires the
// expected rules. Guards the compat engine against silent breakage.
func TestCompat(t *testing.T) {
	good := []Part{
		{ID: "cpu1", Category: "cpu", Socket: "SP5", TDPW: 300},
		{ID: "mb1", Category: "motherboard", Socket: "SP5", MemType: "DDR5"},
		{ID: "ram1", Category: "ram", MemType: "DDR5"},
		{ID: "psu1", Category: "psu", Watts: 800},
		{ID: "case1", Category: "case", LengthMM: 400},
		{ID: "gpu1", Category: "gpu", LengthMM: 300, TDPW: 250},
	}
	if vs := checkCompat(good); len(vs) != 0 {
		t.Fatalf("good build should be compatible, got: %+v", vs)
	}

	bad := []Part{
		{ID: "cpu1", Category: "cpu", Socket: "LGA4677", TDPW: 350},
		{ID: "mb1", Category: "motherboard", Socket: "SP5", MemType: "DDR5"},
		{ID: "ram1", Category: "ram", MemType: "DDR4"},
		{ID: "psu1", Category: "psu", Watts: 300},
		{ID: "case1", Category: "case", LengthMM: 250},
		{ID: "gpu1", Category: "gpu", LengthMM: 320, TDPW: 250},
	}
	got := map[string]bool{}
	for _, v := range checkCompat(bad) {
		got[v.Rule] = true
	}
	for _, want := range []string{"cpu_socket", "ram_mem_type", "psu_headroom", "gpu_length"} {
		if !got[want] {
			t.Errorf("bad build should fire rule %q, violations: %+v", want, checkCompat(bad))
		}
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

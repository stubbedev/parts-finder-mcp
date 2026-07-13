package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Part is one piece of hardware. Attributes are stored once; compatibility is
// derived from predicates over these fields (no N^2 A-fits-B pair table).
// Zero-valued attributes mean "unknown" — rules treat unknowns as gaps, never
// as violations, so missing scrape data can't produce a false incompatibility.
type Part struct {
	ID              string    `json:"id,omitempty"` // omitempty so save_part can derive it; always set on output
	Category        string    `json:"category"`     // cpu, motherboard, ram, psu, gpu, case, storage, ...
	Vendor          string    `json:"vendor,omitempty"`
	Model           string    `json:"model,omitempty"`
	Socket          string    `json:"socket,omitempty"`     // cpu, motherboard
	MemType         string    `json:"mem_type,omitempty"`   // DDR4, DDR5 — ram, motherboard
	MemSpeed        int       `json:"mem_speed,omitempty"`  // MT/s
	FormFactor      string    `json:"form_factor,omitempty"`// ATX, EATX, ... — motherboard, case
	TDPW            int       `json:"tdp_w,omitempty"`      // watts drawn
	PCIeGen         int       `json:"pcie_gen,omitempty"`
	PCIeLanes       int       `json:"pcie_lanes,omitempty"`
	PowerConnectors []string  `json:"power_connectors,omitempty"`
	LengthMM        int       `json:"length_mm,omitempty"` // gpu = card length; case = max gpu clearance
	Watts           int       `json:"watts,omitempty"`     // psu rated output
	RawSpecs        string    `json:"raw_specs,omitempty"` // original scraped blob (json/text)
	SourceURL       string    `json:"source_url,omitempty"`
	FetchedAt       time.Time `json:"fetched_at,omitempty"`
}

// Violation is a compat rule that a set of parts fails.
type Violation struct {
	Rule    string   `json:"rule"`
	Parts   []string `json:"parts"` // part IDs involved
	Message string   `json:"message"`
}

// rule inspects a build (parts grouped by category) and returns any violations.
// Rules must return nothing when a needed attribute is zero/unknown.
type rule func(byCat map[string][]Part) []Violation

func first(ps []Part) (Part, bool) {
	if len(ps) == 0 {
		return Part{}, false
	}
	return ps[0], true
}

// rules seeds the core server-build predicates. ~15 rules eventually cover 90%
// of server builds; M1 seeds the high-value ones that use unambiguous fields.
var rules = []rule{
	// CPU socket must match motherboard socket.
	func(c map[string][]Part) []Violation {
		cpu, ok1 := first(c["cpu"])
		mb, ok2 := first(c["motherboard"])
		if !ok1 || !ok2 || cpu.Socket == "" || mb.Socket == "" {
			return nil
		}
		if cpu.Socket != mb.Socket {
			return []Violation{{"cpu_socket", []string{cpu.ID, mb.ID},
				fmt.Sprintf("CPU socket %s != motherboard socket %s", cpu.Socket, mb.Socket)}}
		}
		return nil
	},
	// RAM memory type must match motherboard memory type.
	func(c map[string][]Part) []Violation {
		var vs []Violation
		mb, ok := first(c["motherboard"])
		if !ok || mb.MemType == "" {
			return nil
		}
		for _, ram := range c["ram"] {
			if ram.MemType != "" && ram.MemType != mb.MemType {
				vs = append(vs, Violation{"ram_mem_type", []string{ram.ID, mb.ID},
					fmt.Sprintf("RAM %s is %s, motherboard needs %s", ram.ID, ram.MemType, mb.MemType)})
			}
		}
		return vs
	},
	// PSU must supply total TDP with 30% headroom.
	func(c map[string][]Part) []Violation {
		psu, ok := first(c["psu"])
		if !ok || psu.Watts == 0 {
			return nil
		}
		total := totalTDP(c)
		if total == 0 {
			return nil
		}
		need := int(float64(total) * 1.3)
		if psu.Watts < need {
			return []Violation{{"psu_headroom", []string{psu.ID},
				fmt.Sprintf("PSU %dW < %dW needed (total TDP %dW * 1.3)", psu.Watts, need, total)}}
		}
		return nil
	},
	// GPU must physically fit the case.
	func(c map[string][]Part) []Violation {
		var vs []Violation
		cs, ok := first(c["case"])
		if !ok || cs.LengthMM == 0 {
			return nil
		}
		for _, gpu := range c["gpu"] {
			if gpu.LengthMM != 0 && gpu.LengthMM > cs.LengthMM {
				vs = append(vs, Violation{"gpu_length", []string{gpu.ID, cs.ID},
					fmt.Sprintf("GPU %s is %dmm, case fits %dmm max", gpu.ID, gpu.LengthMM, cs.LengthMM)})
			}
		}
		return vs
	},
	// Motherboard form factor must fit the case. A case's form factor is the
	// largest board it accepts (an ATX case also fits mATX/mITX).
	func(c map[string][]Part) []Violation {
		mb, ok1 := first(c["motherboard"])
		cs, ok2 := first(c["case"])
		if !ok1 || !ok2 {
			return nil
		}
		mbSize, ok3 := formFactorSize[normFF(mb.FormFactor)]
		csSize, ok4 := formFactorSize[normFF(cs.FormFactor)]
		if !ok3 || !ok4 {
			return nil // unknown or unrecognized form factor => gap, not violation
		}
		if mbSize > csSize {
			return []Violation{{"form_factor_fit", []string{mb.ID, cs.ID},
				fmt.Sprintf("motherboard %s (%s) too large for case %s (%s)",
					mb.ID, mb.FormFactor, cs.ID, cs.FormFactor)}}
		}
		return nil
	},
	// PSU must provide every power connector each GPU requires (by type).
	func(c map[string][]Part) []Violation {
		psu, ok := first(c["psu"])
		if !ok || len(psu.PowerConnectors) == 0 {
			return nil
		}
		have := map[string]bool{}
		for _, pc := range psu.PowerConnectors {
			have[normFF(pc)] = true
		}
		var vs []Violation
		for _, gpu := range c["gpu"] {
			for _, need := range gpu.PowerConnectors {
				if !have[normFF(need)] {
					vs = append(vs, Violation{"power_connector", []string{gpu.ID, psu.ID},
						fmt.Sprintf("GPU %s needs %s connector, PSU %s doesn't provide it", gpu.ID, need, psu.ID)})
				}
			}
		}
		return vs
	},
}

// formFactorSize ranks board/case sizes; a case fits any board of equal or
// smaller size. ponytail: covers the common server/desktop set; add entries
// when a new form factor shows up.
var formFactorSize = map[string]int{
	"miniitx": 1,
	"microatx": 2, "matx": 2, "uatx": 2,
	"atx":   3,
	"eatx":  4,
	"ssiceb": 4,
	"ssieeb": 5, "eeb": 5,
}

var ffStrip = regexp.MustCompile(`[^a-z0-9]+`)

func normFF(s string) string {
	return ffStrip.ReplaceAllString(strings.ToLower(s), "")
}

func groupByCategory(parts []Part) map[string][]Part {
	m := map[string][]Part{}
	for _, p := range parts {
		m[p.Category] = append(m[p.Category], p)
	}
	return m
}

func totalTDP(byCat map[string][]Part) int {
	t := 0
	for _, ps := range byCat {
		for _, p := range ps {
			t += p.TDPW
		}
	}
	return t
}

// checkCompat runs every rule over the parts and returns all violations.
func checkCompat(parts []Part) []Violation {
	byCat := groupByCategory(parts)
	var vs []Violation
	for _, r := range rules {
		vs = append(vs, r(byCat)...)
	}
	return vs
}

// Spec is a composed build: the parts plus derived compat + gaps.
type Spec struct {
	Parts      []Part      `json:"parts"`
	Compatible bool        `json:"compatible"`
	Violations []Violation `json:"violations"`
	Gaps       []string    `json:"gaps"`
	TotalTDPW  int         `json:"total_tdp_w"`
}

// requiredCategories is the minimum for a bootable server build.
var requiredCategories = []string{"cpu", "motherboard", "ram", "psu"}

func composeSpec(parts []Part) Spec {
	byCat := groupByCategory(parts)
	vs := checkCompat(parts)
	var gaps []string
	for _, cat := range requiredCategories {
		if len(byCat[cat]) == 0 {
			gaps = append(gaps, "missing "+cat)
		}
	}
	// Flag parts whose category has no wattage — undercuts PSU sizing.
	for _, p := range parts {
		if (p.Category == "cpu" || p.Category == "gpu") && p.TDPW == 0 {
			gaps = append(gaps, "unknown TDP for "+p.ID)
		}
	}
	return Spec{
		Parts:      parts,
		Compatible: len(vs) == 0,
		Violations: vs,
		Gaps:       gaps,
		TotalTDPW:  totalTDP(byCat),
	}
}

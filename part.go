package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Part is one piece of hardware. Attributes are stored once; compatibility is
// derived from predicates over these fields (no N^2 A-fits-B pair table).
// Zero-valued attributes mean "unknown" — rules treat unknowns as gaps, never
// as violations, so missing scrape data can't produce a false incompatibility.
type Part struct {
	ID              string   `json:"id,omitempty"` // omitempty so save_part can derive it; always set on output
	Category        string   `json:"category"`     // cpu, motherboard, ram, psu, gpu, case, storage, ...
	Vendor          string   `json:"vendor,omitempty"`
	Model           string   `json:"model,omitempty"`
	Socket          string   `json:"socket,omitempty"`      // cpu, motherboard
	MemType         string   `json:"mem_type,omitempty"`    // DDR4, DDR5 — ram, motherboard
	MemSpeed        int      `json:"mem_speed,omitempty"`   // MT/s
	FormFactor      string   `json:"form_factor,omitempty"` // ATX, EATX, ... — motherboard, case
	TDPW            int      `json:"tdp_w,omitempty"`       // watts drawn
	PCIeGen         int      `json:"pcie_gen,omitempty"`
	PCIeLanes       int      `json:"pcie_lanes,omitempty"`
	PowerConnectors []string `json:"power_connectors,omitempty"`
	LengthMM        int      `json:"length_mm,omitempty"` // gpu = card length; case = max gpu clearance
	Watts           int      `json:"watts,omitempty"`     // psu rated output

	// Generic resource accounting — covers every part type without a rule per
	// pair. A part PROVIDES resources (motherboard: "dimm:ddr5":12,
	// "pcie:x16":3; case: "bay:3.5":8) and REQUIRES them (RAM stick:
	// "dimm:ddr5":1; HBA: "pcie:x8":1; drive: "bay:3.5":1). One engine rule
	// checks that, per resource, sum(requires) <= sum(provides). Tokens are
	// free-form "kind:variant" strings the client normalizes at save time.
	// Larger PCIe slots satisfy smaller cards via pcieWidth.
	Provides map[string]int `json:"provides,omitempty"`
	Requires map[string]int `json:"requires,omitempty"`

	// Attrs holds any further typed attribute worth querying on — cache sizes,
	// CUDA compute capability, core counts, rotational speed, whatever the
	// spec sheet yields ("l3_cache_mb": 384, "cuda_compute": 8.9,
	// "cores": 32). query_parts filters over these plus the scalar fields.
	Attrs map[string]any `json:"attrs,omitempty"`

	RawSpecs  string    `json:"raw_specs,omitempty"` // original scraped blob (json/text)
	SourceURL string    `json:"source_url,omitempty"`
	FetchedAt time.Time `json:"fetched_at,omitempty"`
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

// CompatRule is one data-driven compatibility predicate. Rules are DATA, not
// code: the builtin set below seeds coverage, and the store can add, override
// (same name), or disable any rule via save_rule — e.g. after a datasheet or
// vendor QVL page reveals a constraint the engine lacks. Kinds:
//
//	match    — attr_a on cat_a must equal (mode "eq") or appear among the
//	           comma/space-separated tokens of (mode "in") attr_b on cat_b
//	capacity — attr_b usage on cat_b must fit the attr_a limit on cat_a;
//	           mode "sum" checks all instances together (memory capacity),
//	           mode "each" checks every instance alone (physical clearance)
//	superset — resource token attr_a satisfies requests for token attr_b
//	           (standards facts: "port:sas" satisfies "port:sata")
//
// Attributes resolve through flatten — scalar Part fields and free-form attrs
// alike. Unknown values on either side = gap territory, never a violation.
type CompatRule struct {
	Name      string `json:"name" jsonschema:"unique rule id; reusing a builtin name overrides it"`
	Kind      string `json:"kind" jsonschema:"match, capacity, or superset"`
	CatA      string `json:"cat_a,omitempty" jsonschema:"category holding attr_a (the limit holder for capacity rules)"`
	AttrA     string `json:"attr_a,omitempty" jsonschema:"attribute on cat_a; for superset rules the WIDER resource token"`
	CatB      string `json:"cat_b,omitempty" jsonschema:"category holding attr_b (the consumer for capacity rules)"`
	AttrB     string `json:"attr_b,omitempty" jsonschema:"attribute on cat_b; for superset rules the NARROWER resource token"`
	Mode      string `json:"mode,omitempty" jsonschema:"match: eq (default) or in; capacity: sum or each"`
	Note      string `json:"note,omitempty" jsonschema:"why this rule exists — cite what the source says"`
	SourceURL string `json:"source_url,omitempty" jsonschema:"page that documents the constraint (datasheet, QVL, manual)"`
	Disabled  bool   `json:"disabled,omitempty" jsonschema:"true switches the rule off (works on builtins too)"`
	Builtin   bool   `json:"builtin,omitempty"` // seeded rule (output only)
}

// builtinRules seeds the engine. Everything here is a STANDARDS fact (socket
// equality, DDR generations, size ladders, SAS⊃SATA) — stable across hardware
// releases, since rules compare values scraped live from the web. Per-model
// quirks belong in store rules with a source_url, not here.
var builtinRules = []CompatRule{
	{Name: "cpu_socket", Kind: "match", CatA: "cpu", AttrA: "socket", CatB: "motherboard", AttrB: "socket", Mode: "eq"},
	{Name: "ram_mem_type", Kind: "match", CatA: "ram", AttrA: "mem_type", CatB: "motherboard", AttrB: "mem_type", Mode: "eq"},
	{Name: "cpu_mem_type", Kind: "match", CatA: "cpu", AttrA: "mem_type", CatB: "motherboard", AttrB: "mem_type", Mode: "eq"},
	// RDIMM/UDIMM/LRDIMM: registered DIMMs on an unbuffered board (or vice
	// versa) simply won't boot — the classic used-server-RAM trap.
	{Name: "ram_module_type", Kind: "match", CatA: "ram", AttrA: "mem_module", CatB: "motherboard", AttrB: "mem_module", Mode: "eq"},
	{Name: "cooler_socket", Kind: "match", CatA: "motherboard", AttrA: "socket", CatB: "cooler", AttrB: "sockets", Mode: "in"},
	{Name: "mem_capacity", Kind: "capacity", CatA: "motherboard", AttrA: "mem_max_gb", CatB: "ram", AttrB: "capacity_gb", Mode: "sum"},
	{Name: "dimm_capacity", Kind: "capacity", CatA: "motherboard", AttrA: "dimm_max_gb", CatB: "ram", AttrB: "capacity_gb", Mode: "each"},
	{Name: "gpu_length", Kind: "capacity", CatA: "case", AttrA: "length_mm", CatB: "gpu", AttrB: "length_mm", Mode: "each"},
	{Name: "cooler_height", Kind: "capacity", CatA: "case", AttrA: "cooler_max_mm", CatB: "cooler", AttrB: "height_mm", Mode: "each"},
	{Name: "psu_length", Kind: "capacity", CatA: "case", AttrA: "psu_max_mm", CatB: "psu", AttrB: "length_mm", Mode: "each"},
	// NOT bay:2.5 <- bay:3.5: that needs an adapter tray — hiding it would
	// silently swallow a real purchase; let it surface as a need instead.
	{Name: "sas_satisfies_sata", Kind: "superset", AttrA: "port:sas", AttrB: "port:sata"},
}

// ruleOverlay holds store-persisted rules (added, overridden, or disabled),
// keyed by name. Guarded because tool handlers run concurrently.
var (
	rulesMu     sync.RWMutex
	ruleOverlay = map[string]CompatRule{}
)

func setRuleOverlay(rs []CompatRule) {
	m := make(map[string]CompatRule, len(rs))
	for _, r := range rs {
		m[r.Name] = r
	}
	rulesMu.Lock()
	ruleOverlay = m
	rulesMu.Unlock()
}

// currentRules is the effective rule set: builtins, overridden by any store
// rule with the same name, plus store-only rules; disabled rules dropped.
func currentRules() []CompatRule {
	rulesMu.RLock()
	defer rulesMu.RUnlock()
	var out []CompatRule
	seen := map[string]bool{}
	for _, b := range builtinRules {
		b.Builtin = true
		seen[b.Name] = true
		if o, ok := ruleOverlay[b.Name]; ok {
			if o.Disabled {
				continue
			}
			o.Builtin = true // overridden builtin keeps the badge
			out = append(out, o)
			continue
		}
		out = append(out, b)
	}
	for name, o := range ruleOverlay {
		if !seen[name] && !o.Disabled {
			out = append(out, o)
		}
	}
	return out
}

// currentSupersets derives the token-satisfaction map from superset rules.
func currentSupersets() map[string][]string {
	m := map[string][]string{}
	for _, r := range currentRules() {
		if r.Kind == "superset" && r.AttrA != "" && r.AttrB != "" {
			m[strings.ToLower(r.AttrB)] = append(m[strings.ToLower(r.AttrB)], strings.ToLower(r.AttrA))
		}
	}
	return m
}

// attrRuleViolations evaluates every match/capacity rule over a build.
//
// Multi-instance semantics (a rack spec expands to 12 motherboards + 96 DIMMs
// in ONE part list, so single-chassis pairwise checks would cross-pair nodes
// and report garbage): a violation needs PROOF, consistent with unknowns-
// never-violate. match: a part violates only when it fits NO counterpart.
// capacity sum: usage is checked against the POOLED limit of every limit
// holder. capacity each: a part violates only when it exceeds the LARGEST
// limit holder. Single-chassis builds (one counterpart, one limit holder)
// behave exactly as before; multi-node builds get an aggregate-check gap
// from composeSpec so the coarser check is never silent.
func attrRuleViolations(byCat map[string][]Part) []Violation {
	var vs []Violation
	for _, r := range currentRules() {
		switch r.Kind {
		case "match":
			type known struct {
				p Part
				v string
			}
			var as, bs []known
			for _, a := range byCat[r.CatA] {
				if v, ok := flattenStr(a, r.AttrA); ok {
					as = append(as, known{a, v})
				}
			}
			for _, b := range byCat[r.CatB] {
				if v, ok := flattenStr(b, r.AttrB); ok {
					bs = append(bs, known{b, v})
				}
			}
			if len(as) == 0 || len(bs) == 0 {
				continue
			}
			match := func(av, bv string) bool {
				if r.Mode == "in" {
					return tokensContain(bv, av)
				}
				return normFF(av) == normFF(bv)
			}
			misfit := func(x known, others []known, attr, otherCat, otherAttr string) Violation {
				ids := []string{x.p.ID}
				var vals []string
				for _, o := range others {
					ids = append(ids, o.p.ID)
					vals = append(vals, o.v)
				}
				return Violation{r.Name, ids,
					fmt.Sprintf("%s %s=%q matches no %s (%s: %s)", x.p.ID, attr, x.v, otherCat, otherAttr, strings.Join(vals, ", "))}
			}
			anyAMatched := false
			for _, a := range as {
				ok := false
				for _, b := range bs {
					if match(a.v, b.v) {
						ok = true
						break
					}
				}
				if ok {
					anyAMatched = true
					continue
				}
				vs = append(vs, misfit(a, bs, r.AttrA, r.CatB, r.AttrB))
			}
			// The reverse direction (a counterpart no part fits) only adds
			// information when some pairs DID match — otherwise the forward
			// pass already reported the total mismatch.
			if anyAMatched {
				for _, b := range bs {
					ok := false
					for _, a := range as {
						if match(a.v, b.v) {
							ok = true
							break
						}
					}
					if !ok {
						vs = append(vs, misfit(b, as, r.AttrB, r.CatA, r.AttrA))
					}
				}
			}
		case "capacity":
			var limSum, limMax float64
			var limIDs []string
			limMaxID := ""
			for _, l := range byCat[r.CatA] {
				if v, ok := flattenNum(l, r.AttrA); ok {
					limSum += v
					limIDs = append(limIDs, l.ID)
					if v > limMax {
						limMax, limMaxID = v, l.ID
					}
				}
			}
			if len(limIDs) == 0 {
				continue
			}
			var sum float64
			var users []string
			for _, u := range byCat[r.CatB] {
				uv, uok := flattenNum(u, r.AttrB)
				if !uok {
					continue
				}
				if r.Mode == "each" && uv > limMax {
					vs = append(vs, Violation{r.Name, []string{u.ID, limMaxID},
						fmt.Sprintf("%s %s=%g exceeds %s %s=%g", u.ID, r.AttrB, uv, limMaxID, r.AttrA, limMax)})
				}
				sum += uv
				users = append(users, u.ID)
			}
			if r.Mode == "sum" && sum > limSum {
				vs = append(vs, Violation{r.Name, append(users, limIDs...),
					fmt.Sprintf("total %s %g exceeds %s %s=%g", r.AttrB, sum, strings.Join(limIDs, "+"), r.AttrA, limSum)})
			}
		}
	}
	return vs
}

// ruleAttrsFor lists the attributes the active rules read for a category —
// the extraction checklist deep_specs surfaces. Adding a rule automatically
// starts asking for its attributes; no tool description to maintain.
func ruleAttrsFor(category string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, r := range currentRules() {
		if r.Kind == "superset" {
			continue
		}
		if r.CatA == category {
			add(r.AttrA)
		}
		if r.CatB == category {
			add(r.AttrB)
		}
	}
	sort.Strings(out)
	return out
}

// flattenStr reads a part attribute as a non-empty string ("" = unknown).
func flattenStr(p Part, attr string) (string, bool) {
	v, ok := flatten(p)[attr]
	if !ok || v == nil {
		return "", false
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	if s == "" || s == "0" {
		return "", false
	}
	return s, true
}

// flattenNum reads a part attribute as a positive number (0 = unknown).
func flattenNum(p Part, attr string) (float64, bool) {
	v, ok := toFloat(flatten(p)[attr])
	if !ok || v <= 0 {
		return 0, false
	}
	return v, true
}

// tokensContain reports whether needle equals one of the comma/slash/space
// separated tokens in list, normalized — so "SP5" matches "SP5, SP6" but a
// substring like "AM4" never matches "AM45".
func tokensContain(list, needle string) bool {
	n := normFF(needle)
	for _, t := range strings.FieldsFunc(list, func(r rune) bool {
		return r == ',' || r == '/' || r == ' ' || r == ';'
	}) {
		if normFF(t) == n {
			return true
		}
	}
	return false
}

// psuHeadroom: combined PSU output must exceed total TDP by this factor.
// Shared by the psu_headroom violation and the N+1 redundancy gap so the two
// checks can never disagree.
const psuHeadroom = 1.3

// rules seeds the core server-build predicates that need bespoke logic; the
// simple equality/limit pairs live in matchRules/capacityRules as data.
var rules = []rule{
	attrRuleViolations,
	// Combined PSU output must supply total TDP with headroom. Summing
	// covers dual/quad-PSU servers; whether the pair also gives N+1 redundancy
	// is reported as a gap in composeSpec, not a violation.
	func(c map[string][]Part) []Violation {
		var sum int
		var ids []string
		for _, psu := range c["psu"] {
			if psu.Watts > 0 {
				sum += psu.Watts
				ids = append(ids, psu.ID)
			}
		}
		if sum == 0 {
			return nil
		}
		total := totalTDP(c)
		if total == 0 {
			return nil
		}
		need := int(float64(total) * psuHeadroom)
		if sum < need {
			return []Violation{{"psu_headroom", ids,
				fmt.Sprintf("PSU output %dW < %dW needed (total TDP %dW * %g)", sum, need, total, psuHeadroom)}}
		}
		return nil
	},
	// Motherboard form factor must fit the case. A case's form factor is the
	// largest board it accepts (an ATX case also fits mATX/mITX). With several
	// cases (multi-node), a board only provably misfits when even the largest
	// case can't take it — proof-based, same as the data-driven rules.
	func(c map[string][]Part) []Violation {
		csSize := 0
		var csPart Part
		for _, cs := range c["case"] {
			if s, ok := formFactorSize[normFF(cs.FormFactor)]; ok && s > csSize {
				csSize, csPart = s, cs
			}
		}
		if csSize == 0 {
			return nil // no case with a recognized form factor => gap, not violation
		}
		var vs []Violation
		for _, mb := range c["motherboard"] {
			mbSize, ok := formFactorSize[normFF(mb.FormFactor)]
			if !ok {
				continue
			}
			if mbSize > csSize {
				vs = append(vs, Violation{"form_factor_fit", []string{mb.ID, csPart.ID},
					fmt.Sprintf("motherboard %s (%s) too large for case %s (%s)",
						mb.ID, mb.FormFactor, csPart.ID, csPart.FormFactor)})
			}
		}
		return vs
	},
	// PSUs together must provide every power connector type each GPU requires.
	// Union across PSUs: in a multi-PSU build any PSU may feed the card.
	func(c map[string][]Part) []Violation {
		have := map[string]bool{}
		var psuIDs []string
		for _, psu := range c["psu"] {
			if len(psu.PowerConnectors) == 0 {
				continue
			}
			psuIDs = append(psuIDs, psu.ID)
			for _, pc := range psu.PowerConnectors {
				have[normFF(pc)] = true
			}
		}
		if len(have) == 0 {
			return nil
		}
		var vs []Violation
		for _, gpu := range c["gpu"] {
			for _, need := range gpu.PowerConnectors {
				if !have[normFF(need)] {
					vs = append(vs, Violation{"power_connector", append([]string{gpu.ID}, psuIDs...),
						fmt.Sprintf("GPU %s needs %s connector, no PSU provides it", gpu.ID, need)})
				}
			}
		}
		return vs
	},
}

// resourceViolations does generic provider/consumer accounting: per resource
// token, total required must not exceed total provided. PCIe is width-aware —
// an x16 slot satisfies an x8 card. Parts with no Requires/Provides simply
// don't participate (unknown = no constraint, consistent with the other rules).
func resourceViolations(parts []Part) []Violation {
	deficits, users := resourceDeficits(parts)
	var vs []Violation
	for tok, d := range deficits {
		vs = append(vs, Violation{"resource", users[tok],
			fmt.Sprintf("resource %q short by %d — add a part providing it (cable, adapter, slot, bay, ...)", tok, d)})
	}
	return vs
}

// resourceDeficits returns, per resource token, how many units the build is
// short — the structured shopping list for the small stuff (extra cables,
// adapters, rails). Also returns which part IDs touch each token.
func resourceDeficits(parts []Part) (map[string]int, map[string][]string) {
	supersets := currentSupersets()
	provides := map[string]int{}
	requires := map[string]int{}
	users := map[string][]string{} // token -> part IDs involved (for the report)
	for _, p := range parts {
		for tok, n := range p.Provides {
			tok = strings.ToLower(strings.TrimSpace(tok))
			provides[tok] += n
			users[tok] = append(users[tok], p.ID)
		}
		for tok, n := range p.Requires {
			tok = strings.ToLower(strings.TrimSpace(tok))
			requires[tok] += n
			users[tok] = append(users[tok], p.ID)
		}
	}
	deficits := map[string]int{}
	for tok, need := range requires {
		have := provides[tok]
		if have >= need {
			provides[tok] -= need
			continue
		}
		deficit := need - have
		provides[tok] = 0
		// PCIe width flexibility: fill remaining deficit from wider slots.
		if w := pcieWidth(tok); w > 0 {
			for wider, avail := range provides {
				if avail > 0 && pcieWidth(wider) > w {
					take := min(avail, deficit)
					provides[wider] -= take
					deficit -= take
					if deficit == 0 {
						break
					}
				}
			}
		}
		// Superset tokens: fill remaining deficit from tokens that satisfy
		// this one by standard (SAS ports run SATA drives). Derived from
		// superset rules, so the store can teach new equivalences.
		for _, super := range supersets[tok] {
			if deficit == 0 {
				break
			}
			if avail := provides[super]; avail > 0 {
				take := min(avail, deficit)
				provides[super] -= take
				deficit -= take
			}
		}
		if deficit > 0 {
			deficits[tok] = deficit
		}
	}
	return deficits, users
}

// pcieWidth parses "pcie:x8" -> 8; 0 for non-pcie tokens.
func pcieWidth(tok string) int {
	var w int
	if _, err := fmt.Sscanf(tok, "pcie:x%d", &w); err != nil {
		return 0
	}
	return w
}

// formFactorSize ranks board/case sizes; a case fits any board of equal or
// smaller size. ponytail: covers the common server/desktop set; add entries
// when a new form factor shows up.
var formFactorSize = map[string]int{
	"miniitx":  1,
	"microatx": 2, "matx": 2, "uatx": 2,
	"atx":    3,
	"eatx":   4,
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

// flatten exposes a part's scalar fields and free-form attrs as one queryable
// key space, so "socket" and "l3_cache_mb" filter through the same path.
func flatten(p Part) map[string]any {
	m := map[string]any{
		"id": p.ID, "category": p.Category, "vendor": p.Vendor, "model": p.Model,
		"socket": p.Socket, "mem_type": p.MemType, "mem_speed": p.MemSpeed,
		"form_factor": p.FormFactor, "tdp_w": p.TDPW, "pcie_gen": p.PCIeGen,
		"pcie_lanes": p.PCIeLanes, "length_mm": p.LengthMM, "watts": p.Watts,
	}
	for k, v := range p.Attrs {
		m[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return m
}

// Where is one query clause: attr op value.
type Where struct {
	Attr  string `json:"attr"`
	Op    string `json:"op" jsonschema:"one of: eq, ne, gt, gte, lt, lte, contains, exists"`
	Value any    `json:"value,omitempty" jsonschema:"number or string; omit for exists"`
}

// matchWhere evaluates one clause against a part. Unknown attribute => no
// match for every op except a failed exists — consistent with "absent means
// unknown", and queries are for FINDING things, so unknowns don't qualify.
func matchWhere(p Part, w Where) bool {
	v, ok := flatten(p)[strings.ToLower(strings.TrimSpace(w.Attr))]
	if !ok || v == nil || v == "" {
		return false
	}
	// Numeric zero = unknown, whatever the concrete type — JSON attrs decode as
	// float64, scalar fields are int; `v == 0` would miss float64(0).
	if f, isNum := toFloat(v); isNum && f == 0 {
		return false
	}
	if w.Op == "exists" {
		return true
	}
	an, aok := toFloat(v)
	bn, bok := toFloat(w.Value)
	if aok && bok { // numeric comparison ("cuda_compute" >= 8.9)
		switch w.Op {
		case "eq":
			return an == bn
		case "ne":
			return an != bn
		case "gt":
			return an > bn
		case "gte":
			return an >= bn
		case "lt":
			return an < bn
		case "lte":
			return an <= bn
		}
		return false
	}
	as := strings.ToLower(fmt.Sprint(v))
	bs := strings.ToLower(fmt.Sprint(w.Value))
	switch w.Op {
	case "eq":
		return as == bs
	case "ne":
		return as != bs
	case "contains":
		return strings.Contains(as, bs)
	case "gt":
		return as > bs
	case "gte":
		return as >= bs
	case "lt":
		return as < bs
	case "lte":
		return as <= bs
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		// Full-string parse: Sscanf %g would read "4x8GB" as 4 and silently
		// match garbage in numeric queries.
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// checkCompat runs every rule over the parts and returns all violations.
func checkCompat(parts []Part) []Violation {
	byCat := groupByCategory(parts)
	var vs []Violation
	for _, r := range rules {
		vs = append(vs, r(byCat)...)
	}
	vs = append(vs, resourceViolations(parts)...)
	return vs
}

// Need is a concrete shortage the build must fill — the actionable shopping
// list for small stuff (cables, adapters, rails, spare slots).
type Need struct {
	Resource string   `json:"resource"`
	Count    int      `json:"count"`
	Parts    []string `json:"parts,omitempty"` // parts that create/touch the shortage
}

// Spec is a composed build: the parts plus derived compat + gaps + needs.
type Spec struct {
	Parts           []Part      `json:"parts"`
	Compatible      bool        `json:"compatible"`
	Violations      []Violation `json:"violations"`
	Gaps            []string    `json:"gaps"`                             // data-quality problems: unverified/stale/unknown attributes
	MissingForBuild []string    `json:"missing_for_full_build,omitempty"` // core categories absent — informational; a partial/upgrade spec may omit these on purpose
	Partial         bool        `json:"partial,omitempty"`                // not a complete bootable build (missing core categories)
	Needs           []Need      `json:"needs,omitempty"`                  // resource shortages to shop for
	Owned           []string    `json:"owned,omitempty"`                  // part ids already owned (not purchased)
	TotalTDPW       int         `json:"total_tdp_w"`
}

// requiredCategories is the minimum for a bootable server build.
var requiredCategories = []string{"cpu", "motherboard", "ram", "psu"}

// partDataMaxAge: part data older than this gets a freshness gap.
// ponytail: fixed 30d; make it a tool arg if it ever needs tuning.
const partDataMaxAge = 30 * 24 * time.Hour

func composeSpec(parts []Part) Spec {
	byCat := groupByCategory(parts)
	vs := checkCompat(parts)
	var gaps []string
	// Core categories absent = a partial spec (upgrade, parts-list, single
	// component). That's a normal way to work here, NOT a data problem — keep
	// it out of gaps so real data issues stay visible.
	var missing []string
	for _, cat := range requiredCategories {
		if len(byCat[cat]) == 0 {
			missing = append(missing, cat)
		}
	}
	// Multiple motherboards = a flattened multi-node build (rack of specs).
	// Rules then check in aggregate (matches accept any counterpart, capacity
	// limits pool across nodes), which can miss a per-node overload — say so
	// loudly instead of hiding behind a green checkmark.
	if n := len(byCat["motherboard"]); n > 1 {
		gaps = append(gaps, fmt.Sprintf(
			"multi-node build (%d motherboards): compatibility checked in aggregate — verify one node alone with compose_spec on its sub-spec", n))
	}
	// Flag parts whose category has no wattage — undercuts PSU sizing.
	for _, p := range parts {
		if (p.Category == "cpu" || p.Category == "gpu") && p.TDPW == 0 {
			gaps = append(gaps, "unknown TDP for "+p.ID)
		}
	}
	// RAM faster than the board's max isn't a failure — it downclocks — but
	// it IS paid-for headroom you don't get. Surface it; don't violate.
	if mb, ok := first(byCat["motherboard"]); ok && mb.MemSpeed > 0 {
		for _, ram := range byCat["ram"] {
			if ram.MemSpeed > mb.MemSpeed {
				gaps = append(gaps, fmt.Sprintf("%s: %d MT/s will downclock to %s's %d MT/s max — paying for unused speed",
					ram.ID, ram.MemSpeed, mb.ID, mb.MemSpeed))
			}
		}
	}
	// Multi-PSU: capacity passes on the SUM, but redundancy needs the largest
	// single PSU to carry the load alone. Loss of N+1 is a gap (often the whole
	// point of dual PSUs), not a violation (combined-only can be intentional).
	if psus := byCat["psu"]; len(psus) > 1 {
		maxW := 0
		for _, p := range psus {
			if p.Watts > maxW {
				maxW = p.Watts
			}
		}
		if need := int(float64(totalTDP(byCat)) * psuHeadroom); maxW > 0 && need > 0 && maxW < need {
			gaps = append(gaps, fmt.Sprintf(
				"no N+1 redundancy: largest PSU %dW < %dW needed — build only survives with all PSUs running", maxW, need))
		}
	}
	// Freshness: stale part data is as dangerous as missing data — models and
	// revisions change. Flag anything not refreshed recently.
	for _, p := range parts {
		if age := time.Since(p.FetchedAt); !p.FetchedAt.IsZero() && age > partDataMaxAge {
			gaps = append(gaps, fmt.Sprintf("%s: part data %d days old — refresh with deep_specs", p.ID, int(age.Hours()/24)))
		}
	}
	// "Compatible" only covers KNOWN data. Anything unverifiable gets a loud
	// gap so a missing length or undeclared power cable can't hide behind a
	// green checkmark. Generic: no category list — driven by what's absent.
	cs, hasCase := first(byCat["case"])
	for _, p := range parts {
		if len(p.Provides) == 0 && len(p.Requires) == 0 {
			gaps = append(gaps, p.ID+": no provides/requires declared — power cables, slots, bays unverified (run deep_specs)")
		}
		if p.Category == "gpu" && hasCase && (p.LengthMM == 0 || cs.LengthMM == 0) {
			gaps = append(gaps, p.ID+": physical fit vs "+cs.ID+" unverified — card length or case clearance unknown")
		}
	}
	deficits, dusers := resourceDeficits(parts)
	var needs []Need
	for tok, d := range deficits {
		needs = append(needs, Need{Resource: tok, Count: d, Parts: dusers[tok]})
	}
	sort.Slice(needs, func(i, j int) bool { return needs[i].Resource < needs[j].Resource })
	return Spec{
		Parts:           parts,
		Compatible:      len(vs) == 0,
		Violations:      vs,
		Gaps:            gaps,
		MissingForBuild: missing,
		Partial:         len(missing) > 0,
		Needs:           needs,
		TotalTDPW:       totalTDP(byCat),
	}
}

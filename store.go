package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the local SQLite cache: parts, saved specs, fetched content.
type Store struct{ db *sql.DB }

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	schema := `
CREATE TABLE IF NOT EXISTS parts (
  id TEXT PRIMARY KEY, category TEXT, vendor TEXT, model TEXT,
  socket TEXT, mem_type TEXT, mem_speed INT, form_factor TEXT,
  tdp_w INT, pcie_gen INT, pcie_lanes INT, power_connectors TEXT,
  length_mm INT, watts INT, provides TEXT, requires TEXT,
  raw_specs TEXT, source_url TEXT, fetched_at TEXT
);
CREATE TABLE IF NOT EXISTS specs (
  id TEXT PRIMARY KEY, name TEXT, part_ids TEXT, created_at TEXT
);
CREATE TABLE IF NOT EXISTS content_cache (
  url TEXT PRIMARY KEY, title TEXT, content TEXT, fetched_at TEXT
);
CREATE TABLE IF NOT EXISTS listings (
  id TEXT PRIMARY KEY, part_id TEXT, vendor TEXT, price REAL, shipping REAL,
  currency TEXT, condition TEXT, url TEXT, ships_to TEXT, seen_at TEXT
);
CREATE TABLE IF NOT EXISTS compat_rules (
  name TEXT PRIMARY KEY, kind TEXT, cat_a TEXT, attr_a TEXT, cat_b TEXT,
  attr_b TEXT, mode TEXT, note TEXT, source_url TEXT, disabled INT
);
CREATE TABLE IF NOT EXISTS listing_history (
  listing_id TEXT, part_id TEXT, vendor TEXT, price REAL, shipping REAL,
  currency TEXT, seen_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_history_part ON listing_history(part_id);
CREATE INDEX IF NOT EXISTS idx_listings_part ON listings(part_id);
CREATE INDEX IF NOT EXISTS idx_parts_category ON parts(category);`
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// Migrate pre-existing DBs; "duplicate column" errors are expected noise.
	db.Exec(`ALTER TABLE parts ADD COLUMN provides TEXT`)
	db.Exec(`ALTER TABLE parts ADD COLUMN requires TEXT`)
	db.Exec(`ALTER TABLE parts ADD COLUMN attrs TEXT`)
	db.Exec(`ALTER TABLE content_cache ADD COLUMN etag TEXT`)
	db.Exec(`ALTER TABLE content_cache ADD COLUMN last_modified TEXT`)
	db.Exec(`ALTER TABLE content_cache ADD COLUMN kind TEXT`)
	db.Exec(`ALTER TABLE specs ADD COLUMN owned_ids TEXT`)
	db.Exec(`ALTER TABLE listings ADD COLUMN vat_included INT`)
	db.Exec(`ALTER TABLE listings ADD COLUMN vat_rate REAL`)
	db.Exec(`ALTER TABLE listings ADD COLUMN qty_available INT`)
	db.Exec(`ALTER TABLE listings ADD COLUMN in_stock INT`)
	db.Exec(`ALTER TABLE listings ADD COLUMN lead_days INT`)
	// Timestamps are compared and ORDERed as strings, so every stored value
	// must be UTC ("...Z") — rewrite pre-UTC local-offset rows once. COALESCE
	// keeps anything strftime can't parse; the NOT LIKE guard makes re-runs
	// no-ops.
	for _, tc := range []struct{ table, col string }{
		{"parts", "fetched_at"}, {"listings", "seen_at"},
		{"listing_history", "seen_at"}, {"specs", "created_at"},
		{"content_cache", "fetched_at"},
	} {
		db.Exec(fmt.Sprintf(
			`UPDATE %s SET %s = COALESCE(strftime('%%Y-%%m-%%dT%%H:%%M:%%SZ', %s), %s) WHERE %s NOT LIKE '%%Z'`,
			tc.table, tc.col, tc.col, tc.col, tc.col))
	}
	return &Store{db}, nil
}

func (s *Store) savePart(p Part) error {
	conns, _ := json.Marshal(p.PowerConnectors)
	prov, _ := json.Marshal(p.Provides)
	req, _ := json.Marshal(p.Requires)
	attrs, _ := json.Marshal(p.Attrs)
	_, err := s.db.Exec(`
INSERT INTO parts (id,category,vendor,model,socket,mem_type,mem_speed,form_factor,
  tdp_w,pcie_gen,pcie_lanes,power_connectors,length_mm,watts,provides,requires,
  attrs,raw_specs,source_url,fetched_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET category=excluded.category,vendor=excluded.vendor,
  model=excluded.model,socket=excluded.socket,mem_type=excluded.mem_type,
  mem_speed=excluded.mem_speed,form_factor=excluded.form_factor,tdp_w=excluded.tdp_w,
  pcie_gen=excluded.pcie_gen,pcie_lanes=excluded.pcie_lanes,
  power_connectors=excluded.power_connectors,length_mm=excluded.length_mm,
  watts=excluded.watts,provides=excluded.provides,requires=excluded.requires,
  attrs=excluded.attrs,raw_specs=excluded.raw_specs,source_url=excluded.source_url,
  fetched_at=excluded.fetched_at`,
		p.ID, p.Category, p.Vendor, p.Model, p.Socket, p.MemType, p.MemSpeed,
		p.FormFactor, p.TDPW, p.PCIeGen, p.PCIeLanes, string(conns), p.LengthMM,
		p.Watts, string(prov), string(req), string(attrs), p.RawSpecs, p.SourceURL,
		utcRFC3339(p.FetchedAt))
	return err
}

const partCols = `id,category,vendor,model,socket,mem_type,mem_speed,form_factor,
  tdp_w,pcie_gen,pcie_lanes,power_connectors,length_mm,watts,provides,requires,
  attrs,raw_specs,source_url,fetched_at`

func scanPart(rows *sql.Rows) (Part, error) {
	var p Part
	var conns, prov, req, attrs, fetched sql.NullString
	if err := rows.Scan(&p.ID, &p.Category, &p.Vendor, &p.Model, &p.Socket,
		&p.MemType, &p.MemSpeed, &p.FormFactor, &p.TDPW, &p.PCIeGen, &p.PCIeLanes,
		&conns, &p.LengthMM, &p.Watts, &prov, &req, &attrs, &p.RawSpecs,
		&p.SourceURL, &fetched); err != nil {
		return Part{}, err
	}
	json.Unmarshal([]byte(conns.String), &p.PowerConnectors)
	json.Unmarshal([]byte(prov.String), &p.Provides)
	json.Unmarshal([]byte(req.String), &p.Requires)
	json.Unmarshal([]byte(attrs.String), &p.Attrs)
	p.FetchedAt, _ = time.Parse(time.RFC3339, fetched.String)
	return p, nil
}

func (s *Store) getParts(ids []string) ([]Part, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// Query unique ids in bounded chunks: repeats (quantity) don't need their
	// own placeholder, and one flat IN(...) would hit SQLite's variable limit
	// on a large expanded spec.
	const chunkSize = 500
	unique := uniqueInOrder(ids)
	found := map[string]Part{}
	for start := 0; start < len(unique); start += chunkSize {
		chunk := unique[start:min(start+chunkSize, len(unique))]
		ph := strings.Repeat("?,", len(chunk))
		ph = ph[:len(ph)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.Query(`SELECT `+partCols+` FROM parts WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			p, err := scanPart(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			found[p.ID] = p
		}
		rows.Close()
	}
	// Preserve request order; error on any missing id so callers see the gap.
	var out []Part
	var missing []string
	for _, id := range ids {
		if p, ok := found[id]; ok {
			out = append(out, p)
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return out, fmt.Errorf("unknown part ids: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// partsByCategory returns parts in a category; empty cat returns everything.
func (s *Store) partsByCategory(cat string) ([]Part, error) {
	q, args := `SELECT `+partCols+` FROM parts`, []any{}
	if cat != "" {
		q, args = q+` WHERE category=?`, []any{cat}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Part
	for rows.Next() {
		p, err := scanPart(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// nullBool maps *bool <-> a nullable INT column (NULL = unknown).
func nullBool(b *bool) any {
	if b == nil {
		return nil
	}
	if *b {
		return 1
	}
	return 0
}

func fromNullBool(n sql.NullInt64) *bool {
	if !n.Valid {
		return nil
	}
	b := n.Int64 == 1
	return &b
}

func (s *Store) saveListing(l Listing) error {
	ships, _ := json.Marshal(l.ShipsTo)
	_, err := s.db.Exec(`INSERT INTO listings
  (id,part_id,vendor,price,shipping,currency,condition,url,ships_to,seen_at,
   vat_included,vat_rate,qty_available,in_stock,lead_days)
  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET part_id=excluded.part_id,vendor=excluded.vendor,
  price=excluded.price,shipping=excluded.shipping,currency=excluded.currency,
  condition=excluded.condition,url=excluded.url,ships_to=excluded.ships_to,
  seen_at=excluded.seen_at,vat_included=excluded.vat_included,
  vat_rate=excluded.vat_rate,qty_available=excluded.qty_available,
  in_stock=excluded.in_stock,lead_days=excluded.lead_days`,
		l.ID, l.PartID, l.Vendor, l.Price, l.Shipping, l.Currency, l.Condition,
		l.URL, string(ships), utcRFC3339(l.SeenAt),
		nullBool(l.VATIncluded), l.VATRate, l.QtyAvailable, nullBool(l.InStock), l.LeadDays)
	if err != nil {
		return err
	}
	s.recordHistory(l)
	return nil
}

// recordHistory appends a price observation so re-saving a listing (same
// part+vendor+condition id) never silently erases the previous price. Only
// price movements are recorded — a repeat save at the same price is noise.
// Best-effort: history must never fail a save.
func (s *Store) recordHistory(l Listing) {
	var price, shipping float64
	err := s.db.QueryRow(`SELECT price, shipping FROM listing_history
  WHERE listing_id=? ORDER BY seen_at DESC LIMIT 1`, l.ID).Scan(&price, &shipping)
	if err == nil && price == l.Price && shipping == l.Shipping {
		return
	}
	s.db.Exec(`INSERT INTO listing_history
  (listing_id,part_id,vendor,price,shipping,currency,seen_at)
  VALUES (?,?,?,?,?,?,?)`,
		l.ID, l.PartID, l.Vendor, l.Price, l.Shipping, l.Currency,
		utcRFC3339(l.SeenAt))
}

// utcRFC3339 renders a timestamp for storage. Always UTC: timestamps are
// compared and ORDERed as strings, and mixed zone offsets ("Z" vs "+02:00")
// break lexicographic time order.
func utcRFC3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// PriceObs is one historical price observation for a listing.
type PriceObs struct {
	ListingID string    `json:"listing_id"`
	Vendor    string    `json:"vendor,omitempty"`
	Price     float64   `json:"price"`
	Shipping  float64   `json:"shipping,omitempty"`
	Currency  string    `json:"currency,omitempty"`
	SeenAt    time.Time `json:"seen_at"`
}

func (s *Store) priceHistory(partID string) ([]PriceObs, error) {
	rows, err := s.db.Query(`SELECT listing_id,vendor,price,shipping,currency,seen_at
  FROM listing_history WHERE part_id=? ORDER BY seen_at`, partID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PriceObs
	for rows.Next() {
		var o PriceObs
		var seen string
		if err := rows.Scan(&o.ListingID, &o.Vendor, &o.Price, &o.Shipping, &o.Currency, &seen); err != nil {
			return nil, err
		}
		o.SeenAt, _ = time.Parse(time.RFC3339, seen)
		out = append(out, o)
	}
	return out, nil
}

func (s *Store) listingsFor(partID string) ([]Listing, error) {
	rows, err := s.db.Query(`SELECT id,part_id,vendor,price,shipping,currency,
  condition,url,ships_to,seen_at,vat_included,vat_rate,qty_available,in_stock,
  lead_days FROM listings WHERE part_id=?`, partID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Listing
	for rows.Next() {
		var l Listing
		var ships, seen string
		var vatInc, inStock sql.NullInt64
		var vatRate sql.NullFloat64
		var qty, lead sql.NullInt64
		if err := rows.Scan(&l.ID, &l.PartID, &l.Vendor, &l.Price, &l.Shipping,
			&l.Currency, &l.Condition, &l.URL, &ships, &seen,
			&vatInc, &vatRate, &qty, &inStock, &lead); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(ships), &l.ShipsTo)
		l.SeenAt, _ = time.Parse(time.RFC3339, seen)
		l.VATIncluded, l.InStock = fromNullBool(vatInc), fromNullBool(inStock)
		l.VATRate = vatRate.Float64
		l.QtyAvailable, l.LeadDays = int(qty.Int64), int(lead.Int64)
		out = append(out, l)
	}
	return out, nil
}

// knownVendors returns the set of vendor domains (host of listing URL) we've
// stored a listing from that ships to the given country. This is the learned,
// data-driven signal for ranking search results — it replaces any hardcoded
// vendor list and improves as more listings are saved.
func (s *Store) knownVendors(country string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT url, ships_to FROM listings WHERE url != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var u, ships string
		if err := rows.Scan(&u, &ships); err != nil {
			return nil, err
		}
		var tokens []string
		json.Unmarshal([]byte(ships), &tokens)
		if !shipsTo(tokens, country) {
			continue
		}
		if h := hostOf(u); h != "" {
			out[h] = true
		}
	}
	return out, nil
}

func (s *Store) saveSpec(id, name string, partIDs, ownedIDs []string) error {
	ids, _ := json.Marshal(partIDs)
	owned, _ := json.Marshal(ownedIDs)
	_, err := s.db.Exec(`INSERT INTO specs (id,name,part_ids,owned_ids,created_at) VALUES (?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,part_ids=excluded.part_ids,
  owned_ids=excluded.owned_ids`,
		id, name, string(ids), string(owned), utcRFC3339(time.Now()))
	return err
}

// SpecInfo is a saved spec's identity for listing.
type SpecInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	PartIDs  []string `json:"part_ids"`
	OwnedIDs []string `json:"owned_ids,omitempty"`
}

func (s *Store) listSpecs() ([]SpecInfo, error) {
	rows, err := s.db.Query(`SELECT id,name,part_ids,owned_ids FROM specs ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpecInfo
	for rows.Next() {
		var si SpecInfo
		var ids string
		var owned sql.NullString
		if err := rows.Scan(&si.ID, &si.Name, &ids, &owned); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(ids), &si.PartIDs)
		json.Unmarshal([]byte(owned.String), &si.OwnedIDs)
		out = append(out, si)
	}
	return out, nil
}

func (s *Store) loadSpec(id string) (name string, partIDs, ownedIDs []string, err error) {
	var ids string
	var owned sql.NullString
	err = s.db.QueryRow(`SELECT name,part_ids,owned_ids FROM specs WHERE id=?`, id).Scan(&name, &ids, &owned)
	if err != nil {
		return "", nil, nil, err
	}
	json.Unmarshal([]byte(owned.String), &ownedIDs)
	json.Unmarshal([]byte(ids), &partIDs)
	return name, partIDs, ownedIDs, nil
}

// expandSpecIDs flattens "spec:<id>" references in a part-id list into the
// referenced spec's part ids, recursively — a rack spec can be 12x "spec:node"
// plus switches and PDUs (repeat the ref for quantity). Sub-spec owned_ids
// carry up, so an upgrade node reused in a rack keeps its owned parts. A
// "spec:<id>" in the OWNED list means you own that whole sub-build. Cycle-safe.
func (s *Store) expandSpecIDs(partIDs, ownedIDs []string) (parts, owned []string, err error) {
	parts, owned, err = s.expand(partIDs, map[string]bool{})
	if err != nil {
		return nil, nil, err
	}
	op, oo, err := s.expand(ownedIDs, map[string]bool{})
	if err != nil {
		return nil, nil, err
	}
	owned = append(owned, op...)
	owned = append(owned, oo...)
	return parts, owned, nil
}

func (s *Store) expand(ids []string, visiting map[string]bool) (parts, owned []string, err error) {
	for _, id := range ids {
		sub, ok := strings.CutPrefix(id, "spec:")
		if !ok {
			parts = append(parts, id)
			continue
		}
		if visiting[sub] {
			return nil, nil, fmt.Errorf("spec cycle at %q", sub)
		}
		visiting[sub] = true
		_, subParts, subOwned, err := s.loadSpec(sub)
		if err != nil {
			return nil, nil, fmt.Errorf("sub-spec %s: %w", sub, err)
		}
		p, o, err := s.expand(subParts, visiting)
		if err != nil {
			return nil, nil, err
		}
		so, soo, err := s.expand(subOwned, visiting)
		if err != nil {
			return nil, nil, err
		}
		delete(visiting, sub)
		parts = append(parts, p...)
		owned = append(owned, o...)
		owned = append(owned, so...)
		owned = append(owned, soo...)
	}
	return parts, owned, nil
}

// composeIDs composes a build from part ids that may contain "spec:<id>"
// references — HIERARCHICALLY. Each sub-spec composes on its own, so rules
// see one node at a time and a rack can never cross-pair nodes (or hide a
// per-node overload inside a pooled limit). Child violations and gaps bubble
// up prefixed with their spec; child UNMET needs become requirements at the
// parent level, where a rack's switch/PDU/cabinet can satisfy them — a node's
// internal surplus (spare DIMM slots) deliberately does NOT leak to siblings.
// Distinct sub-specs compose once and multiply by their count. A flat id list
// (no refs) behaves exactly like composeSpec.
func (s *Store) composeIDs(ids []string) (Spec, error) {
	return s.composeTree(ids, map[string]bool{})
}

func (s *Store) composeTree(ids []string, visiting map[string]bool) (Spec, error) {
	var direct []string
	counts := map[string]int{}
	var refs []string
	for _, id := range ids {
		if sub, ok := strings.CutPrefix(id, "spec:"); ok {
			if counts[sub] == 0 {
				refs = append(refs, sub)
			}
			counts[sub]++
		} else {
			direct = append(direct, id)
		}
	}
	parts, err := s.getParts(direct)
	if err != nil {
		return Spec{}, err
	}
	level := append([]Part(nil), parts...)    // direct parts + synthetic child aggregates
	allParts := append([]Part(nil), parts...) // flattened output (children repeated per count)
	var childViolations []Violation
	var childGaps []string
	anyChildPartial := false
	for _, sub := range refs {
		n := counts[sub]
		if visiting[sub] {
			return Spec{}, fmt.Errorf("spec cycle at %q", sub)
		}
		visiting[sub] = true
		_, subIDs, _, err := s.loadSpec(sub)
		if err != nil {
			return Spec{}, fmt.Errorf("sub-spec %s: %w", sub, err)
		}
		child, err := s.composeTree(subIDs, visiting)
		if err != nil {
			return Spec{}, err
		}
		delete(visiting, sub)
		tag := "spec:" + sub
		if n > 1 {
			tag = fmt.Sprintf("spec:%s (x%d)", sub, n)
		}
		for _, v := range child.Violations {
			if v.Rule == "resource" {
				// A child's resource deficit bubbles up as a need the parent
				// may satisfy (switch ports, PDU outlets); the parent's own
				// resource check re-raises whatever stays unmet.
				continue
			}
			childViolations = append(childViolations, Violation{v.Rule, v.Parts, tag + ": " + v.Message})
		}
		for _, g := range child.Gaps {
			childGaps = append(childGaps, tag+": "+g)
		}
		if child.Partial {
			anyChildPartial = true
			childGaps = append(childGaps, tag+": partial build (missing "+strings.Join(child.MissingForBuild, ", ")+")")
		}
		req := map[string]int{}
		for _, need := range child.Needs {
			req[need.Resource] = need.Count * n
		}
		level = append(level, Part{ID: "spec:" + sub, Category: subSpecCategory,
			TDPW: child.TotalTDPW * n, Requires: req})
		for range n {
			allParts = append(allParts, child.Parts...)
		}
	}
	spec := composeSpec(level)
	spec.Parts = allParts
	spec.Violations = append(spec.Violations, childViolations...)
	spec.Compatible = len(spec.Violations) == 0
	spec.Gaps = append(spec.Gaps, childGaps...)
	if len(refs) > 0 {
		// A parent of sub-builds isn't itself a bootable machine; each
		// child's completeness was checked (and gapped) per child above.
		spec.MissingForBuild = nil
		spec.Partial = anyChildPartial
	}
	return spec, nil
}

// saveRule upserts a store compat rule (added, overridden, or disabled).
func (s *Store) saveRule(r CompatRule) error {
	_, err := s.db.Exec(`INSERT INTO compat_rules
  (name,kind,cat_a,attr_a,cat_b,attr_b,mode,note,source_url,disabled)
  VALUES (?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(name) DO UPDATE SET kind=excluded.kind,cat_a=excluded.cat_a,
  attr_a=excluded.attr_a,cat_b=excluded.cat_b,attr_b=excluded.attr_b,
  mode=excluded.mode,note=excluded.note,source_url=excluded.source_url,
  disabled=excluded.disabled`,
		r.Name, r.Kind, r.CatA, r.AttrA, r.CatB, r.AttrB, r.Mode, r.Note,
		r.SourceURL, boolInt(r.Disabled))
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) loadRules() ([]CompatRule, error) {
	rows, err := s.db.Query(`SELECT name,kind,cat_a,attr_a,cat_b,attr_b,mode,
  note,source_url,disabled FROM compat_rules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CompatRule
	for rows.Next() {
		var r CompatRule
		var dis int
		if err := rows.Scan(&r.Name, &r.Kind, &r.CatA, &r.AttrA, &r.CatB,
			&r.AttrB, &r.Mode, &r.Note, &r.SourceURL, &dis); err != nil {
			return nil, err
		}
		r.Disabled = dis == 1
		out = append(out, r)
	}
	return out, nil
}

// knownTokens returns every provides/requires resource token in the store —
// the LEARNED vocabulary, so new parts reuse token names consistently instead
// of relying on examples in a tool description.
func (s *Store) knownTokens() ([]string, error) {
	parts, err := s.partsByCategory("")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range parts {
		for tok := range p.Provides {
			seen[strings.ToLower(strings.TrimSpace(tok))] = true
		}
		for tok := range p.Requires {
			seen[strings.ToLower(strings.TrimSpace(tok))] = true
		}
	}
	out := make([]string, 0, len(seen))
	for tok := range seen {
		out = append(out, tok)
	}
	sort.Strings(out)
	return out, nil
}

// cacheRec is a persisted fetch with its HTTP validators and age.
type cacheRec struct {
	Title, Text        string
	ETag, LastModified string
	Kind               string
	FetchedAt          time.Time
}

func (s *Store) getCache(url string) (cacheRec, bool) {
	var r cacheRec
	var etag, lastMod, kind, fetched sql.NullString
	err := s.db.QueryRow(`SELECT title,content,etag,last_modified,kind,fetched_at
  FROM content_cache WHERE url=?`, url).
		Scan(&r.Title, &r.Text, &etag, &lastMod, &kind, &fetched)
	if err != nil {
		return cacheRec{}, false
	}
	r.ETag, r.LastModified, r.Kind = etag.String, lastMod.String, kind.String
	r.FetchedAt, _ = time.Parse(time.RFC3339, fetched.String)
	return r, true
}

func (s *Store) putCache(url, title, content, etag, lastMod, kind string) {
	s.db.Exec(`INSERT INTO content_cache (url,title,content,etag,last_modified,kind,fetched_at)
  VALUES (?,?,?,?,?,?,?)
ON CONFLICT(url) DO UPDATE SET title=excluded.title,content=excluded.content,
  etag=excluded.etag,last_modified=excluded.last_modified,kind=excluded.kind,
  fetched_at=excluded.fetched_at`,
		url, title, content, etag, lastMod, kind, utcRFC3339(time.Now()))
}

// touchCache bumps fetched_at after a 304 — the content is still current, so
// reset its TTL without re-storing the body.
func (s *Store) touchCache(url string) {
	s.db.Exec(`UPDATE content_cache SET fetched_at=? WHERE url=?`,
		utcRFC3339(time.Now()), url)
}

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the local SQLite cache: parts, saved specs, fetched content.
type Store struct{ db *sql.DB }

func openStore(path string) (*Store, error) {
	// Two MCP sessions share this DB; without a busy timeout the second writer
	// gets an instant SQLITE_BUSY. WAL lets a reader coexist with the other
	// session's writer, and one conn serializes this process's own writers.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// One connection serializes this process's writers (no intra-process
	// SQLITE_BUSY) and, crucially, keeps ":memory:" test stores coherent — a
	// pool would give each conn its own in-memory DB. Reads never hold the conn
	// across network (handlers fetch AFTER the store call returns), so a write
	// waits at most one fast query; storeWriteTimeout bounds even that.
	db.SetMaxOpenConns(1)
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
	// Migrate pre-existing DBs; "duplicate column" errors are expected noise,
	// anything else is a real failure.
	for _, stmt := range []string{
		`ALTER TABLE parts ADD COLUMN provides TEXT`,
		`ALTER TABLE parts ADD COLUMN requires TEXT`,
		`ALTER TABLE parts ADD COLUMN attrs TEXT`,
		`ALTER TABLE content_cache ADD COLUMN etag TEXT`,
		`ALTER TABLE content_cache ADD COLUMN last_modified TEXT`,
		`ALTER TABLE content_cache ADD COLUMN kind TEXT`,
		`ALTER TABLE specs ADD COLUMN owned_ids TEXT`,
		`ALTER TABLE listings ADD COLUMN vat_included INT`,
		`ALTER TABLE listings ADD COLUMN vat_rate REAL`,
		`ALTER TABLE listings ADD COLUMN qty_available INT`,
		`ALTER TABLE listings ADD COLUMN in_stock INT`,
		`ALTER TABLE listings ADD COLUMN lead_days INT`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	// Timestamps are compared and ORDERed as strings, so every stored value
	// must be UTC ("...Z") — rewrite pre-UTC local-offset rows once. COALESCE
	// keeps anything strftime can't parse; the NOT LIKE guard makes re-runs
	// no-ops.
	for _, tc := range []struct{ table, col string }{
		{"parts", "fetched_at"}, {"listings", "seen_at"},
		{"listing_history", "seen_at"}, {"specs", "created_at"},
		{"content_cache", "fetched_at"},
	} {
		if _, err := db.Exec(fmt.Sprintf(
			`UPDATE %s SET %s = COALESCE(strftime('%%Y-%%m-%%dT%%H:%%M:%%SZ', %s), %s) WHERE %s NOT LIKE '%%Z'`,
			tc.table, tc.col, tc.col, tc.col, tc.col)); err != nil {
			return nil, err
		}
	}
	return &Store{db}, nil
}

// storeWriteTimeout bounds every write. A save is sub-second; if it can't
// finish in this window the DB is wedged or contended, and a FAST clean error
// the agent can retry beats minutes of dead air — a client reads a silent hang
// as a broken tool. Independent of the request deadline so a write stays
// cancellable even when a handler forwards no context (the save handlers don't).
const storeWriteTimeout = 15 * time.Second

func writeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), storeWriteTimeout)
}

func (s *Store) savePart(p Part) error {
	conns, _ := json.Marshal(p.PowerConnectors)
	prov, _ := json.Marshal(p.Provides)
	req, _ := json.Marshal(p.Requires)
	attrs, _ := json.Marshal(p.Attrs)
	ctx, cancel := writeCtx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
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
	unmarshalLogged("part "+p.ID, "power_connectors", conns.String, &p.PowerConnectors)
	unmarshalLogged("part "+p.ID, "provides", prov.String, &p.Provides)
	unmarshalLogged("part "+p.ID, "requires", req.String, &p.Requires)
	unmarshalLogged("part "+p.ID, "attrs", attrs.String, &p.Attrs)
	if fetched.String != "" {
		var err error
		if p.FetchedAt, err = time.Parse(time.RFC3339, fetched.String); err != nil {
			log.Printf("part %s: bad fetched_at %q: %v", p.ID, fetched.String, err)
		}
	}
	return p, nil
}

// unmarshalLogged decodes a JSON column, logging (not failing) a corrupt blob
// so it's distinguishable from a never-extracted NULL/empty one. Logs go to
// stderr to keep the stdio protocol clean.
func unmarshalLogged(rowID, col, blob string, dst any) {
	if blob == "" {
		return
	}
	if err := json.Unmarshal([]byte(blob), dst); err != nil {
		log.Printf("%s: corrupt %s: %v", rowID, col, err)
	}
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

// partExists reports whether a part id is stored — the guard that keeps a
// typo'd part_id from persisting an ORPHAN listing (a saved price find_deals
// can never reach, indistinguishable from "no deal recorded").
func (s *Store) partExists(id string) bool {
	var one int
	return s.db.QueryRow(`SELECT 1 FROM parts WHERE id=? LIMIT 1`, id).Scan(&one) == nil
}

func (s *Store) saveListing(l Listing) error {
	if !s.partExists(l.PartID) {
		return fmt.Errorf("unknown part id %q — save_part before save_listing", l.PartID)
	}
	ships, _ := json.Marshal(l.ShipsTo)
	ctx, cancel := writeCtx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO listings
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
	return s.recordHistory(l)
}

// recordHistory appends a price observation so re-saving a listing (same
// part+vendor+condition id) never silently erases the previous price. Only
// price movements are recorded — a repeat save at the same price is noise.
// The tool promises every price change is kept, so a lost insert must fail
// the save, not vanish silently.
func (s *Store) recordHistory(l Listing) error {
	ctx, cancel := writeCtx()
	defer cancel()
	var price float64
	var shipping sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `SELECT price, shipping FROM listing_history
  WHERE listing_id=? ORDER BY seen_at DESC, rowid DESC LIMIT 1`, l.ID).Scan(&price, &shipping)
	sameShipping := (l.Shipping == nil && !shipping.Valid) ||
		(l.Shipping != nil && shipping.Valid && *l.Shipping == shipping.Float64)
	if err == nil && price == l.Price && sameShipping {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO listing_history
  (listing_id,part_id,vendor,price,shipping,currency,seen_at)
  VALUES (?,?,?,?,?,?,?)`,
		l.ID, l.PartID, l.Vendor, l.Price, l.Shipping, l.Currency,
		utcRFC3339(l.SeenAt))
	return err
}

// utcRFC3339 renders a timestamp for storage. Always UTC: timestamps are
// compared and ORDERed as strings, and mixed zone offsets ("Z" vs "+02:00")
// break lexicographic time order. FIXED-WIDTH nanoseconds (not RFC3339Nano,
// which drops trailing zeros): "…00Z" would otherwise sort AFTER "…00.5Z"
// (because 'Z' > '.'), mis-ordering same-second rows written at whole vs
// fractional seconds. Padding to 9 digits keeps string order == time order.
func utcRFC3339(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

// PriceObs is one historical price observation for a listing.
type PriceObs struct {
	ListingID string    `json:"listing_id"`
	Vendor    string    `json:"vendor,omitempty"`
	Price     float64   `json:"price"`
	Shipping  *float64  `json:"shipping,omitempty"` // nil = shipping wasn't recorded
	Currency  string    `json:"currency,omitempty"`
	SeenAt    time.Time `json:"seen_at"`
}

func (s *Store) priceHistory(partID string) ([]PriceObs, error) {
	rows, err := s.db.Query(`SELECT listing_id,vendor,price,shipping,currency,seen_at
  FROM listing_history WHERE part_id=? ORDER BY seen_at, rowid`, partID)
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

// validateSpecRefs rejects a spec save that points at parts or sub-specs that
// don't exist (a typo'd id), forms a cycle, or lists an owned unit not in the
// build. Without it a dangling spec persists silently and only breaks later at
// load/compose — exactly the kind of footgun a save should catch at the door.
func (s *Store) validateSpecRefs(partIDs, ownedIDs []string) error {
	if len(partIDs) == 0 {
		return fmt.Errorf("part_ids is empty — a spec with no parts is almost always a mistake")
	}
	// expandSpecIDs resolves "spec:<id>" refs, surfacing missing sub-specs and
	// cycles; getParts then errors on any unknown concrete part id.
	parts, owned, err := s.expandSpecIDs(partIDs, ownedIDs)
	if err != nil {
		return err
	}
	if _, err := s.getParts(parts); err != nil {
		return fmt.Errorf("%w — save the parts (save_part) before the spec", err)
	}
	have := map[string]int{}
	for _, id := range parts {
		have[id]++
	}
	for _, id := range owned {
		if have[id] == 0 {
			return fmt.Errorf("owned id %q is not in the build's parts — you can't own what isn't in the spec", id)
		}
		have[id]--
	}
	return nil
}

func (s *Store) saveSpec(id, name string, partIDs, ownedIDs []string) error {
	if err := s.validateSpecRefs(partIDs, ownedIDs); err != nil {
		return err
	}
	ids, _ := json.Marshal(partIDs)
	owned, _ := json.Marshal(ownedIDs)
	ctx, cancel := writeCtx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO specs (id,name,part_ids,owned_ids,created_at) VALUES (?,?,?,?,?)
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
		unmarshalLogged("spec "+si.ID, "part_ids", ids, &si.PartIDs)
		unmarshalLogged("spec "+si.ID, "owned_ids", owned.String, &si.OwnedIDs)
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
	unmarshalLogged("spec "+id, "owned_ids", owned.String, &ownedIDs)
	unmarshalLogged("spec "+id, "part_ids", ids, &partIDs)
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
	ctx, cancel := writeCtx()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO compat_rules
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

// putCache is best-effort — a failed cache write must not fail the fetch, but
// it is logged (stderr, so the stdio protocol stays clean).
func (s *Store) putCache(url, title, content, etag, lastMod, kind string) {
	ctx, cancel := writeCtx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO content_cache (url,title,content,etag,last_modified,kind,fetched_at)
  VALUES (?,?,?,?,?,?,?)
ON CONFLICT(url) DO UPDATE SET title=excluded.title,content=excluded.content,
  etag=excluded.etag,last_modified=excluded.last_modified,kind=excluded.kind,
  fetched_at=excluded.fetched_at`,
		url, title, content, etag, lastMod, kind, utcRFC3339(time.Now())); err != nil {
		log.Printf("cache write failed for %s: %v", url, err)
	}
}

// touchCache bumps fetched_at after a 304 — the content is still current, so
// reset its TTL without re-storing the body. Best-effort like putCache.
func (s *Store) touchCache(url string) {
	ctx, cancel := writeCtx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `UPDATE content_cache SET fetched_at=? WHERE url=?`,
		utcRFC3339(time.Now()), url); err != nil {
		log.Printf("cache touch failed for %s: %v", url, err)
	}
}

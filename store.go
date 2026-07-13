package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
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
  length_mm INT, watts INT, raw_specs TEXT, source_url TEXT, fetched_at TEXT
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
CREATE INDEX IF NOT EXISTS idx_listings_part ON listings(part_id);
CREATE INDEX IF NOT EXISTS idx_parts_category ON parts(category);`
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db}, nil
}

func (s *Store) savePart(p Part) error {
	conns, _ := json.Marshal(p.PowerConnectors)
	_, err := s.db.Exec(`
INSERT INTO parts (id,category,vendor,model,socket,mem_type,mem_speed,form_factor,
  tdp_w,pcie_gen,pcie_lanes,power_connectors,length_mm,watts,raw_specs,source_url,fetched_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET category=excluded.category,vendor=excluded.vendor,
  model=excluded.model,socket=excluded.socket,mem_type=excluded.mem_type,
  mem_speed=excluded.mem_speed,form_factor=excluded.form_factor,tdp_w=excluded.tdp_w,
  pcie_gen=excluded.pcie_gen,pcie_lanes=excluded.pcie_lanes,
  power_connectors=excluded.power_connectors,length_mm=excluded.length_mm,
  watts=excluded.watts,raw_specs=excluded.raw_specs,source_url=excluded.source_url,
  fetched_at=excluded.fetched_at`,
		p.ID, p.Category, p.Vendor, p.Model, p.Socket, p.MemType, p.MemSpeed,
		p.FormFactor, p.TDPW, p.PCIeGen, p.PCIeLanes, string(conns), p.LengthMM,
		p.Watts, p.RawSpecs, p.SourceURL, p.FetchedAt.Format(time.RFC3339))
	return err
}

func (s *Store) getParts(ids []string) ([]Part, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := strings.Repeat("?,", len(ids))
	ph = ph[:len(ph)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.Query(`SELECT id,category,vendor,model,socket,mem_type,mem_speed,
  form_factor,tdp_w,pcie_gen,pcie_lanes,power_connectors,length_mm,watts,raw_specs,
  source_url,fetched_at FROM parts WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]Part{}
	for rows.Next() {
		var p Part
		var conns, fetched string
		if err := rows.Scan(&p.ID, &p.Category, &p.Vendor, &p.Model, &p.Socket,
			&p.MemType, &p.MemSpeed, &p.FormFactor, &p.TDPW, &p.PCIeGen, &p.PCIeLanes,
			&conns, &p.LengthMM, &p.Watts, &p.RawSpecs, &p.SourceURL, &fetched); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(conns), &p.PowerConnectors)
		p.FetchedAt, _ = time.Parse(time.RFC3339, fetched)
		found[p.ID] = p
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

func (s *Store) partsByCategory(cat string) ([]Part, error) {
	rows, err := s.db.Query(`SELECT id,category,vendor,model,socket,mem_type,mem_speed,
  form_factor,tdp_w,pcie_gen,pcie_lanes,power_connectors,length_mm,watts,raw_specs,
  source_url,fetched_at FROM parts WHERE category=?`, cat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Part
	for rows.Next() {
		var p Part
		var conns, fetched string
		if err := rows.Scan(&p.ID, &p.Category, &p.Vendor, &p.Model, &p.Socket,
			&p.MemType, &p.MemSpeed, &p.FormFactor, &p.TDPW, &p.PCIeGen, &p.PCIeLanes,
			&conns, &p.LengthMM, &p.Watts, &p.RawSpecs, &p.SourceURL, &fetched); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(conns), &p.PowerConnectors)
		p.FetchedAt, _ = time.Parse(time.RFC3339, fetched)
		out = append(out, p)
	}
	return out, nil
}

func (s *Store) saveListing(l Listing) error {
	ships, _ := json.Marshal(l.ShipsTo)
	_, err := s.db.Exec(`INSERT INTO listings
  (id,part_id,vendor,price,shipping,currency,condition,url,ships_to,seen_at)
  VALUES (?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET part_id=excluded.part_id,vendor=excluded.vendor,
  price=excluded.price,shipping=excluded.shipping,currency=excluded.currency,
  condition=excluded.condition,url=excluded.url,ships_to=excluded.ships_to,
  seen_at=excluded.seen_at`,
		l.ID, l.PartID, l.Vendor, l.Price, l.Shipping, l.Currency, l.Condition,
		l.URL, string(ships), l.SeenAt.Format(time.RFC3339))
	return err
}

func (s *Store) listingsFor(partID string) ([]Listing, error) {
	rows, err := s.db.Query(`SELECT id,part_id,vendor,price,shipping,currency,
  condition,url,ships_to,seen_at FROM listings WHERE part_id=?`, partID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Listing
	for rows.Next() {
		var l Listing
		var ships, seen string
		if err := rows.Scan(&l.ID, &l.PartID, &l.Vendor, &l.Price, &l.Shipping,
			&l.Currency, &l.Condition, &l.URL, &ships, &seen); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(ships), &l.ShipsTo)
		l.SeenAt, _ = time.Parse(time.RFC3339, seen)
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

func (s *Store) saveSpec(id, name string, partIDs []string) error {
	ids, _ := json.Marshal(partIDs)
	_, err := s.db.Exec(`INSERT INTO specs (id,name,part_ids,created_at) VALUES (?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,part_ids=excluded.part_ids`,
		id, name, string(ids), time.Now().Format(time.RFC3339))
	return err
}

func (s *Store) loadSpec(id string) (name string, partIDs []string, err error) {
	var ids string
	err = s.db.QueryRow(`SELECT name,part_ids FROM specs WHERE id=?`, id).Scan(&name, &ids)
	if err != nil {
		return "", nil, err
	}
	json.Unmarshal([]byte(ids), &partIDs)
	return name, partIDs, nil
}

func (s *Store) getCached(url string) (title, content string, ok bool) {
	err := s.db.QueryRow(`SELECT title,content FROM content_cache WHERE url=?`, url).
		Scan(&title, &content)
	return title, content, err == nil
}

func (s *Store) putCached(url, title, content string) {
	s.db.Exec(`INSERT INTO content_cache (url,title,content,fetched_at) VALUES (?,?,?,?)
ON CONFLICT(url) DO UPDATE SET title=excluded.title,content=excluded.content,
  fetched_at=excluded.fetched_at`,
		url, title, content, time.Now().Format(time.RFC3339))
}

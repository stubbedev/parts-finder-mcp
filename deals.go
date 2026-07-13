package main

import (
	"sort"
	"time"
)

// Listing is a price observation for a part from one vendor/reseller. Prices
// die fast: SeenAt drives the staleness flag; there is no live-price guarantee.
type Listing struct {
	ID        string    `json:"id,omitempty"` // derived from part+vendor+condition if omitted
	PartID    string    `json:"part_id"`
	Vendor    string    `json:"vendor,omitempty"`   // seller, e.g. "ebay:seller123", "lenovo"
	Price     float64   `json:"price"`
	Shipping  float64   `json:"shipping,omitempty"`
	Currency  string    `json:"currency,omitempty"` // ISO code; not converted across currencies
	Condition string    `json:"condition,omitempty"`// new, used, refurbished
	URL       string    `json:"url,omitempty"`
	SeenAt    time.Time `json:"seen_at,omitempty"`
	Stale     bool      `json:"stale,omitempty"` // derived on read, not stored
}

func (l Listing) total() float64 { return l.Price + l.Shipping }

// stalenessDays: a listing older than this is flagged stale.
// ponytail: fixed 14d threshold; make it a tool arg if it ever needs tuning.
const stalenessDays = 14

func markStale(ls []Listing, now time.Time) {
	cut := now.AddDate(0, 0, -stalenessDays)
	for i := range ls {
		ls[i].Stale = ls[i].SeenAt.Before(cut)
	}
}

// sortListings orders cheapest total first; ties broken by most recent.
func sortListings(ls []Listing) {
	sort.SliceStable(ls, func(i, j int) bool {
		if ls[i].total() != ls[j].total() {
			return ls[i].total() < ls[j].total()
		}
		return ls[i].SeenAt.After(ls[j].SeenAt)
	})
}

// cheapest returns the lowest-total listing in the given currency, if any.
func cheapest(ls []Listing, currency string) (Listing, bool) {
	var best Listing
	found := false
	for _, l := range ls {
		if currency != "" && l.Currency != currency {
			continue
		}
		if !found || l.total() < best.total() {
			best, found = l, true
		}
	}
	return best, found
}

// Substitute is a candidate replacement part with its cheapest qualifying listing.
type Substitute struct {
	Part    Part    `json:"part"`
	Listing Listing `json:"listing"`
}

// substituteMatch reports whether candidate can drop into the same build slot as
// orig: same category, and any constraining attribute the original fixes must
// match. ponytail: "similar performance" is approximated by same category +
// attribute compatibility — there are no benchmark scores in the store. Add a
// perf field + score comparison when substitution needs true perf parity.
func substituteMatch(orig, cand Part) bool {
	if cand.Category != orig.Category || cand.ID == orig.ID {
		return false
	}
	if orig.Socket != "" && cand.Socket != "" && orig.Socket != cand.Socket {
		return false
	}
	if orig.MemType != "" && cand.MemType != "" && orig.MemType != cand.MemType {
		return false
	}
	if orig.FormFactor != "" && cand.FormFactor != "" && orig.FormFactor != cand.FormFactor {
		return false
	}
	return true
}

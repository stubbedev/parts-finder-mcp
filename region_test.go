package main

import "testing"

func TestCLDRDerivedLocale(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"DK", "DKK"}, {"DE", "EUR"}, {"US", "USD"}, {"GB", "GBP"}, {"TH", "THB"}, {"XX", "USD"}, {"", "USD"},
	} {
		if got := currencyOf(tc.in); got != tc.want {
			t.Errorf("currencyOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"DK", "dk-da"}, {"SE", "se-sv"}, {"NO", "no-no"}, {"DE", "de-de"}, {"GB", "uk-en"}, {"US", "us-en"}, {"AT", "at-de"}, {"", ""},
	} {
		if got := ddgRegion(tc.in); got != tc.want {
			t.Errorf("ddgRegion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

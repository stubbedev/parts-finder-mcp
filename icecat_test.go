package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const icecatFixture = `{
  "msg": "OK",
  "data": {
    "GeneralInfo": {"Title": "HPE ProLiant DL380 Gen11"},
    "Multimedia": [
      {"URL": "https://icecat.example/ds.pdf", "Type": "product_pdf", "Description": "Datasheet"},
      {"URL": "https://icecat.example/video.mp4", "Type": "video", "Description": ""}
    ],
    "FeaturesGroups": [
      {"FeatureGroup": {"Name": {"Value": "Processor"}},
       "Features": [
         {"Feature": {"Name": {"Value": "Processor socket"}}, "PresentationValue": "LGA 4677"},
         {"Feature": {"Name": {"Value": "TDP"}}, "PresentationValue": "270 W"}
       ]}
    ]
  }
}`

func TestRenderIcecat(t *testing.T) {
	var r icecatResp
	if err := json.Unmarshal([]byte(icecatFixture), &r); err != nil {
		t.Fatal(err)
	}
	src, ok := renderIcecat("https://api.example", r)
	if !ok {
		t.Fatal("fixture must render")
	}
	for _, want := range []string{"| Processor socket | LGA 4677 |", "| TDP | 270 W |", "ds.pdf"} {
		if !strings.Contains(src.Text, want) {
			t.Errorf("missing %q in:\n%s", want, src.Text)
		}
	}
	if strings.Contains(src.Text, "video.mp4") {
		t.Errorf("non-PDF multimedia must be excluded")
	}
	// Error message => no source.
	if _, ok := renderIcecat("u", icecatResp{Msg: "The requested XML data-sheet is not present"}); ok {
		t.Errorf("non-OK msg must not render")
	}
}

func TestIcecatQuery(t *testing.T) {
	t.Setenv("ICECAT_USER", "tester")
	// GTIN attr wins over brand+model.
	p := Part{Vendor: "HPE", Model: "DL380", Attrs: map[string]any{"gtin": "0190017289571"}}
	if q := icecatQuery(p); !strings.Contains(q, "GTIN=0190017289571") {
		t.Errorf("gtin lookup expected, got %s", q)
	}
	// mpn attr beats display model name.
	p = Part{Vendor: "HPE", Model: "ProLiant DL380 Gen11", Attrs: map[string]any{"mpn": "P52560-B21"}}
	q := icecatQuery(p)
	if !strings.Contains(q, "ProductCode=P52560-B21") || !strings.Contains(q, "Brand=HPE") {
		t.Errorf("brand+mpn lookup expected, got %s", q)
	}
	// Nothing to look up by.
	if q := icecatQuery(Part{Category: "ram"}); q != "" {
		t.Errorf("no identifiers => no query, got %s", q)
	}
	// Zero config: no env => the public open-content user.
	t.Setenv("ICECAT_USER", "")
	if q := icecatQuery(p); !strings.Contains(q, "UserName="+icecatPublicUser) {
		t.Errorf("no ICECAT_USER must default to public user, got %s", q)
	}
}

package apt

import (
	"strings"
	"testing"
)

func TestParsePackagesCatalogsArtifactRecords(t *testing.T) {
	records, err := parsePackages(strings.NewReader(`Package: demo
Version: 2:1.10-3
Filename: pool/main/d/demo/demo_1.10-3_amd64.deb
Description: a package
 continued description

Package: source-only
Version: 1.0

Package: second
Version: 3.0
Filename: pool/main/s/second/second_3.0_all.deb
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %#v", records)
	}
	if records[0].Package != "demo" || records[0].Version != "2:1.10-3" || records[1].ArtifactPath != "pool/main/s/second/second_3.0_all.deb" {
		t.Fatalf("records = %#v", records)
	}
}

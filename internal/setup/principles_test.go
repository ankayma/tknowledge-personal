package setup

import (
	"slices"
	"strings"
	"testing"
)

// The owner's own list is the canonical shape: a table, a gloss in `[A]`, and
// gaps in the numbering that must survive (the list says so itself).
func TestPrinciples_ParseRenderRoundTrip(t *testing.T) {
	js, _ := MustLoad(Embedded).Get("job-search")
	ps := ParsePrinciples(js.Principles.Body)
	codes := []string{}
	for _, p := range ps {
		codes = append(codes, p.Code)
	}
	if !slices.Equal(codes, []string{"P.1", "P.2", "P.3", "P.4", "P.5", "P.8", "P.9"}) {
		t.Fatalf("codes (P.6, P.7 are absent on purpose): %v", codes)
	}
	if !strings.HasPrefix(ps[0].Statement, "*Strong architecture absorbs weak execution.*") ||
		ps[0].Note != "Depending on specific individuals instead of structure is a violation." ||
		ps[2].Note != "" {
		t.Fatalf("first principle: %+v", ps[0])
	}
	if again := ParsePrinciples(RenderPrinciples(ps)); !slices.Equal(again, ps) {
		t.Fatalf("round trip changed the list:\n%+v\n%+v", again, ps)
	}
}

func TestPrinciples_NumberedListAndCodes(t *testing.T) {
	ps := ParsePrinciples("# Principle List\n\n1. Strong architecture absorbs weak execution.\n2. Honest gaps over false promises.\n")
	if len(ps) != 2 || ps[1].Code != "P.2" || ps[1].Statement != "Honest gaps over false promises." {
		t.Fatalf("numbered list: %+v", ps)
	}
	for raw, want := range map[string]string{"P.3": "P.3", "p3": "P.3", "3": "P.3", "P.10": "P.10"} {
		if got, ok := NormalizeCode(raw); !ok || got != want {
			t.Errorf("NormalizeCode(%q) = %q %v", raw, got, ok)
		}
	}
	for _, bad := range []string{"", "P", "P.x", "principle", "P.1234"} {
		if _, ok := NormalizeCode(bad); ok {
			t.Errorf("NormalizeCode(%q) should be refused", bad)
		}
	}
	if CodeKey("P.3") != "p3" || CodeFromKey("p3") != "P.3" || !CodeLess("P.9", "P.10") {
		t.Fatal("code keys")
	}
}

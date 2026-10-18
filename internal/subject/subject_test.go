package subject

import (
	"errors"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	good := map[string]string{
		"tracker:D4.band":                  "tracker:d4.band",
		" Tracker : Đà Nẵng Office.Status": "tracker:da-nang-office.status", // diacritics and đ stripped, inner spaces → hyphens
		"person:Arjun Mehta.tier":          "person:arjun-mehta.tier",
		"thread:thr_01.last-touch":         "thread:thr_01.last-touch",
		"grant:kalshi.builders.status":     "grant:kalshi.builders.status", // attribute after the last dot
	}
	for in, want := range good {
		s, err := Parse(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if s.String() != want {
			t.Errorf("%q → %q, want %q", in, s.String(), want)
		}
	}
	for _, bad := range []string{"", "tracker", "tracker:d4", ":d4.band", "tracker:.band", "tracker:d4."} {
		if _, err := Parse(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q should be invalid, got %v", bad, err)
		}
	}
}

func TestNormalizeStatement(t *testing.T) {
	a := NormalizeStatement("  Band cho D4 = 50–60k   AED/tháng. ")
	b := NormalizeStatement("band cho d4 = 50–60k aed/tháng")
	if a != b {
		t.Fatalf("%q != %q", a, b)
	}
	if NormalizeStatement("50k") == NormalizeStatement("60k") {
		t.Fatal("different statements must differ")
	}
}

func TestLabels(t *testing.T) {
	for in, want := range map[string][3]any{"T": {"T", "T", false}, "a": {"A", "A", false}, "A-b": {"A-b", "A", true}, "a-B": {"A-b", "A", true}} {
		l, m, ab, err := ParseLabel(in)
		if err != nil || l != want[0] || m != want[1] || ab != want[2] {
			t.Errorf("%q → %s %s %v %v", in, l, m, ab, err)
		}
	}
	if _, _, _, err := ParseLabel("B"); !errors.Is(err, ErrLabel) {
		t.Error("B must be refused")
	}
	if Label("T", true) != "T" || Label("A", true) != "A-b" || Label("A", false) != "A" {
		t.Error("Label mapping")
	}
}

func TestIDs(t *testing.T) {
	if FormatID(42) != "clm_42" {
		t.Fatal(FormatID(42))
	}
	for in, want := range map[string]int64{"clm_42": 42, "42": 42} {
		if id, err := ParseID(in); err != nil || id != want {
			t.Errorf("%q → %d %v", in, id, err)
		}
	}
	for _, bad := range []string{"", "clm_", "clm_x", "clm_0", "doc_1"} {
		if _, err := ParseID(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestBasis(t *testing.T) {
	at := time.Date(2026, 9, 14, 23, 0, 0, 0, time.UTC)
	for _, ok := range []string{"doc_01J#g3.5", "evt_9", "chat:46763f35#12", "template:job-search@1", "https://linkedin.com/posts/x"} {
		b, err := Basis(ok, at)
		if err != nil || b.Kind != "fact" || b.Note != ok+" @2026-09-14" {
			t.Errorf("%q → %+v %v", ok, b, err)
			continue
		}
		if BasisRef(&b) != ok {
			t.Errorf("round trip %q → %q", ok, BasisRef(&b))
		}
	}
	if _, err := Basis("  ", at); !errors.Is(err, ErrNoBasis) {
		t.Errorf("empty basis: %v", err)
	}
	for _, bad := range []string{"because I said so", "doc_", "ftp://x", "doc_1 and more"} {
		if _, err := Basis(bad, at); !errors.Is(err, ErrBadBasis) {
			t.Errorf("%q should be a bad basis, got %v", bad, err)
		}
	}
	if !Concluded("resolve") || !Concluded("refute") || Concluded("demote") || Concluded("propose") {
		t.Error("Concluded")
	}
}

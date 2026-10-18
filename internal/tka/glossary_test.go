package tka_test

import (
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// The vocabulary is the standard, so it lives in the binary and these are the
// invariants that keep it honest: claim first because everything else is defined
// in terms of it, no dangling cross-reference, and nothing half-written.
func TestGlossary_IsWholeAndStartsAtClaim(t *testing.T) {
	v, err := tka.Glossary("")
	if err != nil {
		t.Fatal(err)
	}
	all := v.Definitions
	if len(all) == 0 || all[0].Term != "claim" {
		t.Fatalf("claim is the foundation and must come first: %v", terms(all))
	}
	have := map[string]bool{}
	for _, d := range all {
		have[strings.ToLower(d.Term)] = true
	}
	for _, d := range all {
		if strings.TrimSpace(d.Short) == "" || strings.TrimSpace(d.Definition) == "" {
			t.Errorf("%s: a term with no definition is worse than no term", d.Term)
		}
		if strings.Count(d.Short, ".") > 2 {
			t.Errorf("%s: short should be one line, got %q", d.Term, d.Short)
		}
		for _, ref := range d.SeeAlso {
			if !have[strings.ToLower(ref)] {
				t.Errorf("%s points at %q, which this store does not define", d.Term, ref)
			}
		}
	}
	// Every class and every kind of assumption has to be there: those are the
	// words a person meets in the first minute.
	for _, want := range []string{"claim", "T", "A", "A-r", "A-c", "A-p", "A-b", "P",
		"fact", "derivation", "evidence", "promote", "demote", "signer"} {
		if !have[strings.ToLower(want)] {
			t.Errorf("%q is a word the store uses and does not define", want)
		}
	}
}

// Lookup takes the shapes a model will actually send, and an unknown word comes
// back with the list rather than an empty answer.
func TestDefine_AcceptsTheShapesAModelSends(t *testing.T) {
	for _, in := range []string{"claim", "CLAIM", " Claim ", "truth", "T", "a_p", "A-P", "principle", "P.9"} {
		if _, err := tka.Define(in, ""); err != nil {
			t.Errorf("define(%q): %v", in, err)
		}
	}
	if d, err := tka.Define("truth", "en"); err != nil || d.Term != "T" {
		t.Errorf("an alias must land on its term: %+v %v", d, err)
	}
	_, err := tka.Define("vibes", "")
	if code(t, err) != "TERM_UNKNOWN" {
		t.Fatalf("expected TERM_UNKNOWN: %v", err)
	}
	if !strings.Contains(next(t, err), "claim") {
		t.Errorf("the refusal should list what is defined: %q", next(t, err))
	}
	// A language the store does not carry falls back, and says which language it
	// actually answered in, so a fallback is never read as a translation.
	fell, err := tka.Glossary("sv")
	if err != nil || fell.Lang != tka.DefaultLang {
		t.Errorf("an unknown language should fall back to %s and say so: %q %v", tka.DefaultLang, fell.Lang, err)
	}
	if code(t, mustErr(tka.Define("", ""))) != "NO_TERM" {
		t.Error("an empty term should say so rather than guess")
	}
}

// The three T's are the owner's own reading of the name, and the store must not
// overclaim: it guarantees trust and traceable, and says plainly that truth is
// not its to decide.
func TestDefine_TSaysWhatItDoesNotGuarantee(t *testing.T) {
	d, err := tka.Define("T", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.NotGiven) == 0 {
		t.Fatal("T must say what the store does not promise")
	}
	joined := strings.ToLower(strings.Join(append(d.NotGiven, d.Why...), " "))
	for _, want := range []string{"truth", "trust", "traceable"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the three T's should each be addressed; %q is missing from %q", want, joined)
		}
	}
}

func terms(all []tka.Definition) []string {
	out := make([]string, 0, len(all))
	for _, d := range all {
		out = append(out, d.Term)
	}
	return out
}

func mustErr(_ tka.Definition, err error) error { return err }

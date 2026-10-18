package setup

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The Principle List is its own trust class (P.9), so it is stored as claims —
// one subject per code — and the document is only how it is read. These two
// functions are the bridge: Parse reads a template's markdown into codes,
// Render writes the codes back as the canonical document.
//
// Canonical shape is the owner's table, one row per principle:
//
//	| **P.1** | *Strong architecture absorbs weak execution.* … `[A]` (gloss) |
//
// A numbered list ("1. …") is also read, taking the numbers as P.1, P.2, …

type Principle struct {
	Code      string `json:"code"`
	Statement string `json:"statement"`
	// Note is the `[A]` gloss: it adjusts how the principle is read and is
	// never used to derive from (the list says so itself).
	Note string `json:"note,omitempty"`
}

var (
	principleRow  = regexp.MustCompile(`^\|\s*\*\*(P\.[0-9]+)\*\*\s*\|(.*)\|\s*$`)
	principleItem = regexp.MustCompile(`^\s*([0-9]+)\.\s+(.*)$`)
	glossTail     = regexp.MustCompile("`\\[A\\]`\\s*\\((.*)\\)\\s*$")
	CodePattern   = regexp.MustCompile(`^P\.[0-9]{1,3}$`)
)

// ParsePrinciples reads a principle document. Numbering is never rewritten:
// the list's own note says a gap (P.6, P.7) is not an error.
func ParsePrinciples(body string) []Principle {
	out := []Principle{}
	for _, line := range strings.Split(body, "\n") {
		var code, rest string
		switch m := principleRow.FindStringSubmatch(line); {
		case m != nil:
			code, rest = m[1], m[2]
		default:
			m := principleItem.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			code, rest = "P."+m[1], m[2]
		}
		p := Principle{Code: code}
		if g := glossTail.FindStringSubmatch(rest); g != nil {
			p.Note = strings.TrimSpace(g[1])
			rest = rest[:len(rest)-len(g[0])]
		}
		p.Statement = strings.TrimSpace(strings.Trim(strings.TrimSpace(rest), "|"))
		if p.Statement != "" {
			out = append(out, p)
		}
	}
	return out
}

// RenderPrinciples is the document the store shows for a set of principles.
func RenderPrinciples(ps []Principle) string {
	var b strings.Builder
	b.WriteString("# Principle List\n\nOnly the canonical statement is `[T]`. The part in brackets is `[A]`: it calibrates the reading and is never used as a premise.\n\n")
	b.WriteString("|         | Canonical statement `[T]` |\n| ------- | ------------------------- |\n")
	for _, p := range ps {
		fmt.Fprintf(&b, "| **%s** | %s", p.Code, p.Statement)
		if p.Note != "" {
			fmt.Fprintf(&b, " `[A]` (%s)", p.Note)
		}
		b.WriteString(" |\n")
	}
	return b.String()
}

// CodeKey is the subject entity for a code: P.3 → p3.
func CodeKey(code string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), ".", ""))
}

// CodeFromKey is the inverse of CodeKey: p3 → P.3.
func CodeFromKey(key string) string {
	return "P." + strings.TrimPrefix(strings.TrimSpace(key), "p")
}

// NormalizeCode accepts P.3, p3 or 3 and returns P.3; ok is false for anything
// that is not a number (the list's own numbering, gaps included, is kept).
func NormalizeCode(raw string) (string, bool) {
	digits := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "p")
	digits = strings.TrimPrefix(digits, ".")
	if digits == "" || strings.TrimLeft(digits, "0123456789") != "" || len(digits) > 3 {
		return "", false
	}
	return "P." + digits, true
}

// CodeLess orders codes by their number, so P.10 follows P.9.
func CodeLess(a, b string) bool {
	na, _ := strconv.Atoi(strings.TrimPrefix(a, "P."))
	nb, _ := strconv.Atoi(strings.TrimPrefix(b, "P."))
	if na != nb {
		return na < nb
	}
	return a < b
}

// StarterTemplate is the template whose Principle List every new store starts
// from: the owner's seven (P.1–P.5, P.8, P.9), user decided 1/10/2026. A store
// that does not hold one of them retires it and adds its own under a new code;
// the retired code stays readable and is never reused.
const StarterTemplate = "job-search"

// StarterPrinciples reads that list from the templates shipped in the binary,
// so the list lives in one file and a build from the repo always seeds the same
// principles. The canonical statement is stored without the document's
// emphasis marks.
func StarterPrinciples() ([]Principle, error) {
	reg, err := Load(Embedded)
	if err != nil {
		return nil, err
	}
	t, ok := reg.Get(StarterTemplate)
	if !ok || t.Principles == nil {
		return nil, fmt.Errorf("template %s has no principle list", StarterTemplate)
	}
	ps := ParsePrinciples(t.Principles.Body)
	if len(ps) == 0 {
		return nil, fmt.Errorf("template %s: principle list is empty", StarterTemplate)
	}
	for i := range ps {
		ps[i].Statement = strings.Join(strings.Fields(strings.ReplaceAll(ps[i].Statement, "*", "")), " ")
	}
	return ps, nil
}

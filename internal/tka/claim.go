package tka

import (
	"fmt"
	"regexp"
	"strings"
)

// What a sentence has to be before it can be a claim at all. Three conditions,
// the ones claim-extraction for fact-checking uses, and the owner's two
// corrections of 28/9 are both the second one:
//
//	atomic            one proposition. Several only if they are one inseparable unit.
//	decontextualized  stands alone: pronouns resolved to proper nouns, relative
//	                  terms to absolute ones. Quine's eternal sentence — its truth
//	                  value does not move with the reading date or the reader.
//	checkable         an assertion about the world, checkable against evidence.
//	                  Not an opinion, not a private experience, not a promise.
//
// This file is the implementation layer under that invariant (P.5). The machine
// takes the part of each condition that is decidable from the sentence; the rest
// is named out loud and left to the signer, because hiding it would be the
// opposite of P.3. A signer carries a demoted claim, so they are owed the list
// of what they are actually signing for.

// Condition of a claim; the label a problem is filed under.
const (
	CondAtomic      = "atomic"
	CondStandsAlone = "decontextualized"
	CondCheckable   = "checkable"
)

// Problem is one reason a sentence is not yet a claim. Every field exists so an
// assistant can say what is wrong and what to write instead — a bare false tells
// a person nothing they can act on.
type Problem struct {
	Condition string `json:"condition"`
	Found     string `json:"found"`             // the words in the sentence that cause it
	Why       string `json:"why"`               // one sentence, why that cannot be a claim
	Fix       string `json:"fix"`               // what to write instead
	Example   string `json:"example,omitempty"` // the fix applied, when it can be shown
}

// ClaimCheck is what the store can and cannot say about a sentence.
type ClaimCheck struct {
	Claim string `json:"claim"`
	// Admissible is whether the machine found nothing against it. It is not a
	// verdict that the sentence is true, or even that it is a claim: SignerChecks
	// is the part no machine decides.
	Admissible   bool      `json:"admissible"`
	Problems     []Problem `json:"problems"`
	SignerChecks []string  `json:"signer_checks"`
	// Words is the claim's word count as the store's word cap counts it, and
	// MaxWords that cap when the caller's store has one — so the writer sees how
	// close a sentence is before a write is refused.
	Words    int  `json:"words"`
	MaxWords *int `json:"max_words,omitempty"`
}

// deictic terms move with the reading date: the same sentence says something
// different next month, so it has no fixed truth value to audit.
//
// Bare "vừa" and bare "sắp" are deliberately absent: "vừa" also means to fit
// ("vừa đủ") and "sắp" also means to arrange ("sắp xếp"), so matching them would
// refuse well-formed claims. "Halden vừa mở" ("Halden just opened") therefore gets past the machine,
// and that remainder is the signer's — which is why signer_checks names it.
var deictic = []string{
	"hiện tại", "hiện nay", "bây giờ", "lúc này", "dạo này", "gần đây", "mới đây",
	"vừa mới", "vừa rồi", "sắp tới", "tới đây", "hôm nay", "hôm qua", "ngày mai",
	"tuần này", "tuần trước", "tuần sau", "tháng này", "tháng trước", "tháng sau",
	"năm nay", "năm ngoái", "năm sau", "quý này", "quý trước", "quý sau",
	"currently", "at present", "nowadays", "right now", "recently", "lately", "soon",
	"today", "yesterday", "tomorrow", "this week", "last week", "next week",
	"this month", "last month", "next month", "this year", "last year", "next year",
	"this quarter", "last quarter", "next quarter",
}

// sinceMarkers count up to the moment of reading (spec T–D §7).
var sinceMarkers = []string{
	"so far", "to date", "until now", "up to now", "as of now", "as of today", "as of",
	"cho tới nay", "cho đến nay", "tính đến nay", "tính tới nay", "đến nay", "tới nay", "tính đến", "tính tới",
}

// anchorable markers are fine when a date follows: "as of 2026-10-01".
var anchorable = map[string]bool{"as of": true, "tính đến": true, "tính tới": true}

var startsWithDate = regexp.MustCompile(`^\s*(ngày\s+)?[0-9]`)

func followedByDate(lower, term string) bool {
	i := strings.Index(lower, term)
	return i >= 0 && startsWithDate.MatchString(lower[i+len(term):])
}

// standIns name a person without naming them. Who "owner" is depends on who is
// reading, which is the same defect as a relative date.
var standIns = []string{
	"owner", "tôi", "mình", "chúng tôi", "chúng ta", "anh ấy", "chị ấy", "ông ấy", "bà ấy",
	"họ", "nó", "người này", "người đó", "the owner", "the founder",
}

// promises are about the future, so there is nothing to check yet. They belong
// in assume as A-p, with the observation that would settle them.
var promises = []string{"sẽ ", "dự kiến", "will ", "shall ", "is going to", "plans to", "expects to"}

// opinions are evaluations. Nothing in the world settles them, so no evidence
// can be cited for or against.
var opinions = []string{
	"tốt hơn", "tốt nhất", "tệ hơn", "quan trọng nhất", "nên ", "đáng lẽ",
	"better", "best", "worse", "worst", "should ", "ought to", "beautiful", "terrible",
}

var (
	// sentenceBreak is a full stop, question or exclamation mark with more
	// sentence after it — two claims in one field. Chinese and Japanese write no
	// space after 。！？, so there any text that follows counts, except a closing
	// quote or bracket that only ends the same sentence.
	sentenceBreak = regexp.MustCompile(`[.!?]\s+\S|[。！？]\s*[^\s」』）)\]"'”’]`)
	wordish       = regexp.MustCompile(`[\p{L}\p{N}]`)
)

// CheckClaim reads a sentence and says what stands between it and being a claim.
// It touches no database and writes nothing: it is meant to be called before a
// write, so a refusal costs nothing.
//
// `basis` narrows the third condition, because what counts as checkable depends
// on what the claim will rest on: a fact about the future has nothing to cite
// yet, while a derivation from principles is allowed to be normative — that is
// what deriving from a principle produces. Pass "" to check only the first two.
func CheckClaim(claim, basis string) ClaimCheck {
	text := CleanClaim(claim)
	out := ClaimCheck{Claim: text, Problems: []Problem{}, SignerChecks: []string{}, Words: ClaimWords(text)}
	if text == "" {
		out.Problems = append(out.Problems, Problem{
			Condition: CondAtomic, Found: "", Why: "there is no sentence here",
			Fix: "Write the sentence the claim makes.",
		})
		return out
	}
	lower := " " + strings.ToLower(text) + " "

	// ---- atomic ----
	if loc := sentenceBreak.FindString(text); loc != "" {
		out.Problems = append(out.Problems, Problem{
			Condition: CondAtomic, Found: strings.TrimSpace(loc),
			Why: "the field holds more than one sentence, so it holds more than one claim, and they would stand or fall together for no reason",
			Fix: "Write one claim per entry. Admit the others separately, and use supports to tie them together.",
		})
	}
	if strings.ContainsAny(text, ";\n\r") {
		out.Problems = append(out.Problems, Problem{
			Condition: CondAtomic, Found: "; or a line break",
			Why: "a semicolon or a new line joins claims that can be true and false independently",
			Fix: "Split them into one claim each.",
		})
	}

	// ---- decontextualized ----
	for _, term := range deictic {
		if found := phrase(lower, term); found != "" {
			out.Problems = append(out.Problems, Problem{
				Condition: CondStandsAlone, Found: found,
				Why:     "a time said relative to the reader: the sentence means something else next month, so it has no fixed truth value to audit",
				Fix:     "Write the date or the period the claim is about.",
				Example: "\"Halden Ltd is a UAE legal entity since 09/2026\" instead of \"just opened\"",
			})
			break
		}
	}
	// A running total ("so far", "tính đến nay") is a state that keeps moving: the
	// sentence is true on the day it is written and wrong a week later. "as of"
	// and "tính đến" ("as of") with a date after them are fine — that is an absolute time.
	for _, term := range sinceMarkers {
		found := phrase(lower, term)
		if found == "" || (anchorable[term] && followedByDate(lower, term)) {
			continue
		}
		out.Problems = append(out.Problems, Problem{
			Condition: CondStandsAlone, Found: found,
			Why:     "a running state counted up to the moment of reading: it is true today and different next week, so it has no fixed truth value to audit",
			Fix:     "Admit what happened, each with its date. Keep where things stand now as a thread note, or as an A-p that says what would settle it.",
			Example: "\"Lan Nguyen had round 3 with Northwind on 2026-09-15\" instead of \"has passed three rounds so far\"",
		})
		break
	}
	for _, term := range standIns {
		if found := phrase(lower, term); found != "" {
			out.Problems = append(out.Problems, Problem{
				Condition: CondStandsAlone, Found: found,
				Why:     "a person referred to but not named: who this is depends on who reads the entry, and the claim cannot be checked without the surrounding chat",
				Fix:     "Name the person.",
				Example: "\"Lan Nguyen (founder) is a shareholder of Halden Ltd\" instead of \"the owner is a shareholder\"",
			})
			break
		}
	}

	// ---- checkable ----
	switch strings.ToLower(strings.TrimSpace(basis)) {
	case "fact":
		for _, term := range promises {
			if found := phrase(lower, term); found != "" {
				out.Problems = append(out.Problems, Problem{
					Condition: CondCheckable, Found: found,
					Why: "a claim about the future: no evidence exists for it yet, so nothing can be cited",
					Fix: "Write it with assume(type: \"A-p\") and say what would settle it. When that happens, promote carries this exact wording over — so write it now in a form that will still read correctly then.",
				})
				break
			}
		}
		for _, term := range opinions {
			if found := phrase(lower, term); found != "" {
				out.Problems = append(out.Problems, Problem{
					Condition: CondCheckable, Found: found,
					Why: "an evaluation rather than an observation: no document settles whether something is better",
					Fix: "State the measurable thing instead, or rest the judgement on principles with basis \"derivation\".",
				})
				break
			}
		}
		out.SignerChecks = append(out.SignerChecks,
			"checkable: the evidence you cite actually says this, and someone else reading it would agree that it does")
	case "derivation":
		out.SignerChecks = append(out.SignerChecks,
			"checkable: the chain really leads from these premises to this claim, and every step it makes is either a premise you named or something no reasonable reader would dispute")
	}
	// Whether the two sides of "and" can be checked apart is a question about the
	// world, not the words, so the machine names it and the signer answers.
	for _, j := range []string{" and ", " và "} {
		if strings.Contains(lower, j) {
			out.SignerChecks = append(out.SignerChecks, fmt.Sprintf(
				"atomic: %q joins two parts — if each could be checked on its own, they are two claims; admit them one by one",
				strings.TrimSpace(j)))
			break
		}
	}
	out.SignerChecks = append(out.SignerChecks,
		"atomic: the sentence makes exactly one claim — if half of it could be wrong while the other half is right, it is two",
		"decontextualized: nothing in it needs the conversation it came from. Read it cold in a year: every name, date and quantity is still there")

	out.Admissible = len(out.Problems) == 0
	return out
}

// phrase finds a term on word boundaries and returns it, so "sắp" does not match
// inside another word and "owner" does not match "downtown".
func phrase(haystackLower, term string) string {
	at := 0
	for {
		i := strings.Index(haystackLower[at:], term)
		if i < 0 {
			return ""
		}
		i += at
		before := haystackLower[i-1 : i]
		afterAt := i + len(term)
		after := haystackLower[afterAt : afterAt+1]
		if !wordish.MatchString(before) && (strings.HasSuffix(term, " ") || !wordish.MatchString(after)) {
			return strings.TrimSpace(term)
		}
		at = i + 1
	}
}

// failClaim turns a failed check into the store's ordinary refusal: a code a
// program acts on, one line a person reads, the move still open, and the
// reasons riding along. Every other refusal in this store has a code; this one
// is not special (owner decision, 28/9).
func failClaim(check ClaimCheck) error {
	conditions := make([]string, 0, 3)
	seen := map[string]bool{}
	for _, p := range check.Problems {
		if !seen[p.Condition] {
			seen[p.Condition] = true
			conditions = append(conditions, p.Condition)
		}
	}
	first := ""
	if len(check.Problems) > 0 {
		p := check.Problems[0]
		first = p.Fix
		if p.Found != "" {
			first = "\"" + p.Found + "\": " + p.Fix
		}
		if p.Example != "" {
			first += " " + p.Example
		}
	}
	return &Error{
		Code: "CLAIM_FAILED",
		Message: "this sentence is not a claim yet — it fails " +
			strings.Join(conditions, " and ") + ", so nothing in the ledger could ever check it",
		Next:  first,
		Check: &check,
	}
}

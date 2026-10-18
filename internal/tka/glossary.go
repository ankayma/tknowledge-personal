package tka

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// The store's vocabulary: the words, so an assistant can read a definition out
// to a person instead of paraphrasing from a tool description.
//
// Two things, and they do not touch each other (owner decision, 28/9):
//
//	the rule   lives in code — CheckClaim, the gate, the append-only triggers.
//	           Nobody can change what the store enforces without changing code.
//	the words  live in a JSON file per language. They explain the rule; they
//	           never decide anything. A new language, or a new word, is a file.
//
// That split is what makes both safe. Wording can be improved, argued with and
// translated without anyone being able to weaken a check by editing text — and
// the check can be strict without every reader having to read Go.
//
// CLAIM comes first on purpose (owner 28/9): it is the foundation, it is the
// word most easily misread, and every later word — T, A, promote, demote — is
// defined in terms of it. A reader who has the wrong idea of "claim" will have
// the wrong idea of everything else.
//
// Where a rule came from is recorded. Decided is the owner's ruling with its
// date; Source is outside work the rule rests on. Nothing here is attributed to
// the owner that he did not decide.

// Vocabulary is one language's edition of the words.
type Vocabulary struct {
	Lang        string       `json:"lang"`
	Version     string       `json:"version"`
	Definitions []Definition `json:"definitions"`
}

// Definition is one word of the vocabulary.
type Definition struct {
	Term       string   `json:"term"`
	Aliases    []string `json:"aliases,omitempty"`
	Short      string   `json:"short"`                    // one line
	Definition string   `json:"definition"`               // the paragraph
	Rules      []string `json:"rules,omitempty"`          // what it must satisfy
	Why        []string `json:"why,omitempty"`            // what breaks without it
	Good       []string `json:"good,omitempty"`           // examples that pass
	Bad        []string `json:"bad,omitempty"`            // examples that do not, and why
	SeeAlso    []string `json:"see_also,omitempty"`       // other terms
	Decided    string   `json:"decided,omitempty"`        // the owner's ruling, dated
	Source     string   `json:"source,omitempty"`         // outside work it rests on
	NotGiven   []string `json:"not_guaranteed,omitempty"` // what the store does NOT promise
}

// VocabularyVersion is the edition of the words this binary ships with. It goes
// up when a definition changes, so a reader can say which edition they were told
// about, and two stores disagreeing about a word becomes a visible fact.
const VocabularyVersion = "1.0.0 (2026-09-28)"

// DefaultLang is the language the words are written in, and the one every other
// language falls back to.
const DefaultLang = "en"

// definitionsFS are the languages that ship in the binary, so a store always has
// its vocabulary even with nothing mounted.
//
//go:embed definitions/*.json
var definitionsFS embed.FS

// DefinitionsDir, when set, is read before the embedded files. Dropping
// vi.json into it adds Vietnamese without rebuilding or redeploying anything —
// which is the whole point of keeping the words in files (owner decision, 28/9).
var DefinitionsDir = os.Getenv("TK_DEFINITIONS_DIR")

var (
	vocabMu sync.RWMutex
	vocabs  = map[string]*Vocabulary{}
)

// Languages lists what this store can answer in.
func Languages() []string {
	seen := map[string]bool{}
	add := func(name string) {
		if lang, ok := strings.CutSuffix(name, ".json"); ok {
			seen[lang] = true
		}
	}
	if DefinitionsDir != "" {
		if entries, err := os.ReadDir(DefinitionsDir); err == nil {
			for _, e := range entries {
				add(e.Name())
			}
		}
	}
	if entries, err := definitionsFS.ReadDir("definitions"); err == nil {
		for _, e := range entries {
			add(e.Name())
		}
	}
	out := make([]string, 0, len(seen))
	for lang := range seen {
		out = append(out, lang)
	}
	sort.Strings(out)
	return out
}

// load reads one language, a file on disk winning over the embedded copy.
func load(lang string) (*Vocabulary, error) {
	lang = normalizeLang(lang)
	vocabMu.RLock()
	cached, ok := vocabs[lang]
	vocabMu.RUnlock()
	if ok {
		return cached, nil
	}
	var raw []byte
	var err error
	if DefinitionsDir != "" {
		raw, err = os.ReadFile(filepath.Join(DefinitionsDir, lang+".json"))
	}
	if len(raw) == 0 {
		raw, err = definitionsFS.ReadFile("definitions/" + lang + ".json")
	}
	if err != nil || len(raw) == 0 {
		return nil, fail("LANG_UNKNOWN", "this store has no vocabulary in %q; it speaks %s",
			lang, strings.Join(Languages(), ", "))
	}
	var v Vocabulary
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("vocabulary %s: %w", lang, err)
	}
	if v.Lang == "" {
		v.Lang = lang
	}
	vocabMu.Lock()
	vocabs[lang] = &v
	vocabMu.Unlock()
	return &v, nil
}

func normalizeLang(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	lang, _, _ = strings.Cut(lang, "-") // en-GB reads the same as en
	if lang == "" {
		return DefaultLang
	}
	return lang
}

// Glossary is one language's words in reading order: claim first, because every
// other word is defined in terms of it, then the three classes, then what may be
// done to them. A language this store does not have falls back to English, and
// says so in Lang, so nobody mistakes a fallback for a translation.
func Glossary(lang string) (Vocabulary, error) {
	v, err := load(lang)
	if err != nil {
		if normalizeLang(lang) == DefaultLang {
			return Vocabulary{}, err
		}
		v, err = load(DefaultLang)
		if err != nil {
			return Vocabulary{}, err
		}
	}
	return *v, nil
}

// AliasesOf are the other spellings that land on a term.
func AliasesOf(term string) []string {
	v, err := load(DefaultLang)
	if err != nil {
		return nil
	}
	for _, d := range v.Definitions {
		if d.Term == term {
			return d.Aliases
		}
	}
	return nil
}

// Define looks one word up. It accepts "A-p", "a_p", "truth", "P.9" and the
// like; an unknown word comes back as a refusal listing what is defined, so an
// assistant never has to guess at a term.
func Define(term, lang string) (Definition, error) {
	v, err := Glossary(lang)
	if err != nil {
		return Definition{}, err
	}
	want := normalizeTerm(term)
	if want == "" {
		return Definition{}, failNext("NO_TERM",
			"Call define() with no term to read the whole vocabulary, or pass one of: "+termList(v),
			"which word should be defined?")
	}
	// Any principle code is the word "principle".
	if _, ok := NormalizeCode(term); ok {
		want = "p"
	}
	for _, d := range v.Definitions {
		if normalizeTerm(d.Term) == want {
			return d, nil
		}
		for _, alias := range d.Aliases {
			if normalizeTerm(alias) == want {
				return d, nil
			}
		}
	}
	return Definition{}, failNext("TERM_UNKNOWN",
		"The vocabulary is: "+termList(v)+". Call define() with no term to read all of it.",
		"%q is not a word this store defines", term)
}

func normalizeTerm(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("_", "-", " ", "-", ".", "").Replace(s)
	return s
}

func termList(v Vocabulary) string {
	names := make([]string, 0, len(v.Definitions))
	for _, d := range v.Definitions {
		names = append(names, d.Term)
	}
	return strings.Join(names, ", ")
}

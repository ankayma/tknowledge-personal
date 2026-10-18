package tka

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"
)

// Instructions reach an agent by situation, not all at once (owner 13/10/2026,
// store document D-116): every line carries server-side tags on six axes —
// who reads it, what it acts on, when in a session it applies, where (the
// medium), why it exists, how it is carried out — and help(situation) returns
// only the lines whose tags fit the situation, each in full. The text of a
// line is never edited; tags are metadata kept beside it, re-tagged by adding
// a row.
//
// A when tag may carry a qualifier, "before-write:work-status": the line
// applies at that moment only in that case, and help returns it only when the
// situation names the case. Without it a directive written for one case — the
// status of work done, a document with no link — would come back for every
// write.

// InstructionTags is one line's tags. Who, What, When and Where are drawn
// from TagVocabulary; Why and How are free text.
type InstructionTags struct {
	Who   []string `json:"who,omitempty"`
	What  []string `json:"what,omitempty"`
	When  []string `json:"when,omitempty"`
	Where []string `json:"where,omitempty"`
	Why   string   `json:"why,omitempty"`
	How   string   `json:"how,omitempty"`
	// Source says where the tags come from: tagged (a row in this store) or
	// default (the default list's own tags, for a default line unchanged).
	Source string `json:"source,omitempty"`
}

func (t InstructionTags) empty() bool {
	return len(t.Who)+len(t.What)+len(t.When)+len(t.Where) == 0 && t.Why == "" && t.How == ""
}

// TagVocabulary is the closed set for the four axes help matches on (D-116).
var TagVocabulary = map[string][]string{
	"who":   {"agent", "owner", "user", "signer", "cli"},
	"what":  {"claim", "document", "summary", "message", "analysis", "instruction", "principle", "term", "thread", "store"},
	"when":  {"session-start", "before-write", "before-draft", "before-analysis", "while-answering", "session-end", "on-correction", "on-cost"},
	"where": {"store", "chat", "linkedin", "email", "web-form", "file"},
}

var tagQualifier = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// validTags refuses a tag outside the vocabulary, naming the axis and what it
// takes, so the caller can correct it without guessing.
func validTags(t InstructionTags) error {
	for axis, vals := range map[string][]string{"who": t.Who, "what": t.What, "when": t.When, "where": t.Where} {
		for _, v := range vals {
			base, qual, hasQual := strings.Cut(v, ":")
			if hasQual && (axis != "when" || !tagQualifier.MatchString(qual)) {
				return failNext("INVALID_TAG", "Only a when tag takes a qualifier, written moment:case in lowercase, e.g. before-write:work-status.",
					"%s tag %q is not in the vocabulary", axis, v)
			}
			if !slices.Contains(TagVocabulary[axis], base) {
				return failNext("INVALID_TAG", axis+" takes: "+strings.Join(TagVocabulary[axis], ", ")+".",
					"%s tag %q is not in the vocabulary", axis, v)
			}
		}
	}
	if len([]rune(t.Why)) > 300 || len([]rune(t.How)) > 300 {
		return failNext("INVALID_TAG", "Keep each under 300 characters.", "why and how are one line each")
	}
	return nil
}

// defaultTags are the default list's own tags, by label (D-116 for the lines
// it classifies; 8 and 13 tagged the same way). They apply to a default line
// only while its text is the default text, so a store's own line under the
// same label never borrows them.
var defaultTags = map[string]InstructionTags{
	"M1": {Who: []string{"agent", "owner"}, What: []string{"instruction"}, When: []string{"session-start"}, Where: []string{"store"},
		Why: "purpose outranks wording; surface conflicts", How: "read all lines through M1"},
	"M2": {Who: []string{"agent", "owner"}, What: []string{"instruction"}, When: []string{"on-correction"}, Where: []string{"chat", "store"},
		Why: "the list grows only with approval", How: "propose in chat → owner approves → instruction_add"},
	"1": {Who: []string{"agent"}, What: []string{"claim"}, When: []string{"before-write"}, Where: []string{"chat"},
		Why: "nothing recorded unapproved", How: "list verbatim + label + source; record approved lines"},
	"2": {Who: []string{"agent"}, What: []string{"claim"}, When: []string{"before-write"}, Where: []string{"store"},
		Why: "T only when checkable", How: "document, source or commit → T; else A"},
	"3": {Who: []string{"agent"}, What: []string{"claim"}, When: []string{"before-write:work-status"}, Where: []string{"store"},
		Why: "work status without source is A", How: "label A"},
	"4": {Who: []string{"agent"}, What: []string{"claim"}, When: []string{"before-write"}, Where: []string{"store"},
		Why: "gate: atomic", How: "one idea per claim; split and/semicolon"},
	"5": {Who: []string{"agent"}, What: []string{"claim"}, When: []string{"before-write"}, Where: []string{"store"},
		Why: "gate: decontextualized", How: "names, dates, quantities in the sentence"},
	"6": {Who: []string{"agent"}, What: []string{"claim"}, When: []string{"while-answering"}, Where: []string{"chat"},
		Why: "abbreviation loses meaning", How: "quote recorded claims verbatim"},
	"7": {Who: []string{"agent"}, What: []string{"instruction", "term"}, When: []string{"while-answering"}, Where: []string{"chat"},
		Why: "owner must not search the store for decisive content", How: "show verbatim in chat"},
	"8": {Who: []string{"agent"}, What: []string{"claim", "thread", "document"}, When: []string{"while-answering"}, Where: []string{"chat"},
		Why: "a bare id sends the reader back to old chats", How: "content first, id in parentheses after"},
	"9": {Who: []string{"agent"}, What: []string{"term"}, When: []string{"before-analysis"}, Where: []string{"store"},
		Why: "do not coin what exists", How: "lookup + documents before naming anything new"},
	"10": {Who: []string{"agent"}, What: []string{"summary"}, When: []string{"session-end"}, Where: []string{"store"},
		Why: "summary is not evidence", How: "every sentence cites an id"},
	"11": {Who: []string{"agent"}, What: []string{"summary"}, When: []string{"session-end"}, Where: []string{"store"},
		Why: "stale summaries mislead", How: "overwrite at session close; rebuild from entries when stale"},
	"12": {Who: []string{"agent"}, What: []string{"document"}, When: []string{"before-write:unbound"}, Where: []string{"store"},
		Why: "unbound evidence is valid", How: "record without asking; one-time note to keep the original"},
	"13": {Who: []string{"agent", "user"}, What: []string{"store"}, When: []string{"on-cost"}, Where: []string{"chat"},
		Why: "a store that costs more than it keeps goes silent", How: "say so; stop proposing; point to feedback_url/email and support"},
}

// defaultDefinitionTags tag every default definition alike (D-116: "def:claim …
// def:dikw — agent, user, signer / term / while-answering, session-start / chat").
var defaultDefinitionTags = InstructionTags{Who: []string{"agent", "user", "signer"}, What: []string{"term"},
	When: []string{"while-answering", "session-start"}, Where: []string{"chat"},
	Why: "shared vocabulary; explain a term with its why", How: "define, rationale_ref to docs"}

// defaultTagsFor is the default list's tags for a line that is still the
// default line it was seeded as.
func defaultTagsFor(in Instruction) (InstructionTags, bool) {
	if in.Origin != "default" {
		return InstructionTags{}, false
	}
	for _, d := range DefaultInstructions() {
		if d.Label != in.Label || d.Text != in.Text {
			continue
		}
		t, ok := defaultTags[d.Label]
		if !ok && d.Kind == "definition" {
			t, ok = defaultDefinitionTags, true
		}
		if ok {
			t.Source = "default"
		}
		return t, ok
	}
	return InstructionTags{}, false
}

// --- help ---------------------------------------------------------------

// HelpLine is one instruction returned in full for a situation.
type HelpLine struct {
	ID    string          `json:"id"`
	Kind  string          `json:"kind"`
	Label string          `json:"label"`
	Term  *string         `json:"term,omitempty"`
	Text  string          `json:"text"`
	Why   *string         `json:"definition_why,omitempty"`
	Ref   *string         `json:"rationale_ref,omitempty"`
	Tags  InstructionTags `json:"tags"`
	// Incident is the dated event the line cites, as the line words it;
	// Cites the store ids it names (D-…, T-…).
	Incident string   `json:"incident,omitempty"`
	Cites    []string `json:"cites,omitempty"`
	// A store line taken from the catalog: the entry, and the person's
	// translation beside the English.
	CatalogRef   *string `json:"catalog_ref,omitempty"`
	Translation  *string `json:"translation,omitempty"`
	Lang         *string `json:"lang,omitempty"`
	TranslatedBy *string `json:"translated_by,omitempty"`
	// Evidence is a catalog suggestion's story, told without names.
	Evidence *string `json:"evidence,omitempty"`
}

// HelpResult is help(situation): the lines for it, and how the situation was
// read so a caller sees why those lines came back.
type HelpResult struct {
	Situation   string              `json:"situation"`
	ReadAs      map[string][]string `json:"read_as"`
	Directives  []HelpLine          `json:"directives"`
	Definitions []HelpLine          `json:"definitions"`
	// Untagged are lines that carry no tags yet and whose text shares words
	// with the situation; they are matched on text alone.
	Untagged []HelpLine `json:"untagged,omitempty"`
	Count    int        `json:"count"`
	// Suggested are catalog entries that fit the situation and that this
	// store has neither taken nor declined: offers, not instructions.
	Suggested     []HelpLine `json:"suggested"`
	SuggestedNext string     `json:"suggested_next,omitempty"`
	Next          string     `json:"next,omitempty"`
}

// situationWords map a word of the situation to a tag. English and
// Vietnamese, matched without diacritics. A phrase is matched before its
// words.
var situationPhrases = []struct {
	phrase string
	tags   []string
}{
	{"what is", []string{"what:term"}}, {"what are", []string{"what:term"}}, {"what does", []string{"what:term"}},
	{"connect note", []string{"where:linkedin", "what:message"}},
	{"close session", []string{"when:session-end"}}, {"end session", []string{"when:session-end"}},
	{"end of session", []string{"when:session-end"}}, {"closing session", []string{"when:session-end"}},
	{"close the session", []string{"when:session-end"}}, {"wrap up", []string{"when:session-end"}},
	{"start session", []string{"when:session-start"}}, {"new session", []string{"when:session-start"}},
	{"start of session", []string{"when:session-start"}}, {"new chat", []string{"when:session-start"}},
	{"work status", []string{"q:work-status"}}, {"already done", []string{"q:work-status"}},
	{"no link", []string{"q:unbound"}}, {"no url", []string{"q:unbound"}}, {"without link", []string{"q:unbound"}},
	{"too much", []string{"when:on-cost"}}, {"not worth", []string{"when:on-cost"}},
	{"ket thuc phien", []string{"when:session-end"}}, {"dong phien", []string{"when:session-end"}},
	{"cuoi phien", []string{"when:session-end"}}, {"bat dau phien", []string{"when:session-start"}},
	{"tin nhan", []string{"what:message"}}, {"bai dang", []string{"what:message"}}, {"binh luan", []string{"what:message"}},
	{"quyet dinh", []string{"what:claim", "what:document"}}, {"tai lieu", []string{"what:document"}},
	{"bang chung", []string{"what:document"}}, {"tom tat", []string{"what:summary"}},
	{"nguyen tac", []string{"what:principle"}}, {"thuat ngu", []string{"what:term"}}, {"dinh nghia", []string{"what:term"}},
	{"nghia la gi", []string{"what:term"}}, {"la gi", []string{"what:term"}},
	{"phan tich", []string{"when:before-analysis"}}, {"danh gia", []string{"when:before-analysis"}},
	{"trang thai", []string{"q:work-status"}}, {"tien do", []string{"q:work-status"}},
	{"khong co link", []string{"q:unbound"}}, {"anh chup", []string{"q:unbound"}},
	{"chien luoc", nil},
}

var situationWords = map[string][]string{
	// when
	"draft": {"when:before-draft"}, "drafting": {"when:before-draft"}, "drafts": {"when:before-draft"},
	"compose": {"when:before-draft"}, "outreach": {"when:before-draft", "what:message"},
	"revise": {"when:before-draft"}, "rewrite": {"when:before-draft"},
	"record": {"when:before-write"}, "recording": {"when:before-write"}, "save": {"when:before-write"},
	"admit": {"when:before-write", "what:claim"}, "assume": {"when:before-write", "what:claim"},
	"log": {"when:before-write"}, "file": {"when:before-write"},
	"analyse": {"when:before-analysis"}, "analyze": {"when:before-analysis"}, "analysing": {"when:before-analysis"},
	"analyzing": {"when:before-analysis"}, "analysis": {"when:before-analysis"}, "evaluate": {"when:before-analysis"},
	"evaluating": {"when:before-analysis"}, "assess": {"when:before-analysis"}, "assessing": {"when:before-analysis"},
	"answer": {"when:while-answering"}, "answering": {"when:while-answering"}, "quote": {"when:while-answering"},
	"quoting": {"when:while-answering"}, "recall": {"when:while-answering"}, "cite": {"when:while-answering"},
	"citing": {"when:while-answering"}, "present": {"when:while-answering"},
	"correct": {"when:on-correction"}, "correction": {"when:on-correction"}, "corrected": {"when:on-correction"},
	"corrects": {"when:on-correction"}, "correcting": {"when:on-correction"}, "misread": {"when:on-correction"},
	"cost": {"when:on-cost"}, "costs": {"when:on-cost"}, "tired": {"when:on-cost"}, "frustrated": {"when:on-cost"},
	"overwhelmed": {"when:on-cost"}, "burden": {"when:on-cost"},
	// what
	"message": {"what:message"}, "messages": {"what:message"}, "note": {"what:message"}, "email": {"what:message", "where:email"},
	"emails": {"what:message", "where:email"}, "post": {"what:message"}, "comment": {"what:message"},
	"dm": {"what:message"}, "letter": {"what:message"}, "reply": {"what:message"},
	"claim": {"what:claim"}, "claims": {"what:claim"}, "fact": {"what:claim"}, "facts": {"what:claim"},
	"decision": {"what:claim", "what:document"}, "decisions": {"what:claim", "what:document"},
	"assumption": {"what:claim"}, "assumptions": {"what:claim"},
	"document": {"what:document"}, "documents": {"what:document"}, "evidence": {"what:document"},
	"outcome": {"what:document"}, "pasted": {"what:document"}, "paste": {"what:document"}, "minutes": {"what:document"},
	"summary": {"what:summary"}, "summaries": {"what:summary"}, "summarise": {"what:summary"}, "summarize": {"what:summary"},
	"principle": {"what:principle"}, "principles": {"what:principle"},
	"instruction": {"what:instruction"}, "instructions": {"what:instruction"}, "directive": {"what:instruction"},
	"term": {"what:term"}, "terms": {"what:term"}, "word": {"what:term"}, "meaning": {"what:term"},
	"mean": {"what:term"}, "means": {"what:term"}, "define": {"what:term"}, "definition": {"what:term"},
	"thread": {"what:thread"}, "matter": {"what:thread"},
	// where
	"linkedin": {"where:linkedin"}, "form": {"where:web-form"}, "consultation": {"where:web-form", "q:standard-comment"},
	"standard": {"q:standard-comment"}, "standards": {"q:standard-comment"},
	// qualifiers
	"status": {"q:work-status"}, "progress": {"q:work-status"},
	"unbound": {"q:unbound"}, "screenshot": {"q:unbound"}, "paper": {"q:unbound"}, "photo": {"q:unbound"},
	// Vietnamese single words
	"ghi": {"when:before-write"}, "luu": {"when:before-write"}, "soan": {"when:before-draft"}, "nhap": {"when:before-draft"},
	"thu": {"what:message"},
}

// helpStop are words that carry no situation.
var helpStop = map[string]bool{"a": true, "an": true, "the": true, "to": true, "for": true, "of": true, "in": true,
	"on": true, "my": true, "me": true, "i": true, "and": true, "or": true, "with": true, "this": true, "that": true,
	"how": true, "should": true, "do": true, "you": true, "we": true, "it": true, "is": true, "about": true,
	"when": true, "before": true, "after": true, "store": true, "agent": true, "khi": true, "cho": true,
	"mot": true, "cua": true, "nay": true, "trong": true, "va": true, "kho": true}

// fold lowercases and strips diacritics, so "Kết thúc phiên" ("end session") reads as
// "ket thuc phien".
func fold(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case r == 'đ':
			b.WriteRune('d')
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return " " + strings.Join(strings.Fields(b.String()), " ") + " "
}

// readSituation turns the situation into tags per axis, qualifiers, the words
// of a definition term it names, and the words it could not read.
func readSituation(situation string, terms []string) (facets map[string][]string, unread []string, named []string) {
	facets = map[string][]string{}
	add := func(tags []string) {
		for _, t := range tags {
			axis, v, _ := strings.Cut(t, ":")
			if !slices.Contains(facets[axis], v) {
				facets[axis] = append(facets[axis], v)
			}
		}
	}
	text := fold(situation)
	for _, p := range situationPhrases {
		if strings.Contains(text, " "+p.phrase+" ") {
			add(p.tags)
			text = strings.ReplaceAll(text, " "+p.phrase+" ", " ")
		}
	}
	for _, w := range strings.Fields(text) {
		if tags, ok := situationWords[w]; ok {
			add(tags)
			continue
		}
		if !helpStop[w] {
			unread = append(unread, w)
		}
	}
	// A defined term the situation names ("T", "unbound", "dikw").
	raw := fold(situation)
	upper := " " + strings.Join(strings.FieldsFunc(situation, func(r rune) bool {
		return !unicode.IsLetter(r) && r != '-'
	}), " ") + " "
	for _, term := range terms {
		for _, w := range termWords(term) {
			// A one-letter term (T, A) is named only in capitals: "a" is an article.
			if len(w) == 1 && strings.Contains(upper, " "+strings.ToUpper(w)+" ") ||
				len(w) > 1 && strings.Contains(raw, " "+w+" ") {
				named = append(named, term)
				break
			}
		}
	}
	// Drafting is writing too, but its own directives are the ones that
	// apply: "draft a message" is not "record a claim".
	// So is "recording costs too much": the cost is the situation, not the
	// next write.
	if slices.Contains(facets["when"], "before-draft") || slices.Contains(facets["when"], "on-cost") {
		facets["when"] = slices.DeleteFunc(facets["when"], func(s string) bool { return s == "before-write" })
	}
	// The narrower of two named terms: "an unbound document" names
	// document/bound-vs-unbound, not document as well.
	named = slices.DeleteFunc(named, func(term string) bool {
		return slices.ContainsFunc(named, func(other string) bool {
			return other != term && strings.HasPrefix(other, term+"/")
		})
	})
	// A word that named a term was read.
	for _, term := range named {
		for _, w := range termWords(term) {
			unread = slices.DeleteFunc(unread, func(u string) bool { return u == w })
		}
	}
	for _, axis := range []string{"who", "what", "when", "where", "q"} {
		sort.Strings(facets[axis])
	}
	return facets, unread, named
}

// termWords are the words a definition term is named by: "truth [T]" →
// truth, t; "document/bound-vs-unbound" → bound, unbound.
func termWords(term string) []string {
	var out []string
	for _, w := range strings.Fields(fold(strings.NewReplacer("/", " ", "-vs-", " ", "[", " ", "]", " ").Replace(term))) {
		if w == "document" && strings.Contains(term, "/") {
			continue
		}
		out = append(out, w)
	}
	return out
}

// fits says whether a line's tags fit the situation: on every axis the
// situation names, the line shares a tag. A line with nothing on an axis
// does not restrict it; a line written for chat fits any medium (the draft is
// shown in chat whatever it is for). A qualified when tag fits only a
// situation that names the qualifier.
func fits(t InstructionTags, f map[string][]string) bool {
	if w := f["when"]; len(w) > 0 {
		ok := false
		for _, lw := range t.When {
			base, qual, hasQual := strings.Cut(lw, ":")
			if slices.Contains(w, base) && (!hasQual || slices.Contains(f["q"], qual)) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	} else if len(t.When) > 0 && !slices.ContainsFunc(t.When, func(s string) bool { return !strings.Contains(s, ":") }) &&
		!slices.ContainsFunc(t.When, func(s string) bool { _, q, _ := strings.Cut(s, ":"); return slices.Contains(f["q"], q) }) {
		return false
	}
	if w := f["what"]; len(w) > 0 && len(t.What) > 0 && !intersects(w, t.What) {
		return false
	}
	if w := f["where"]; len(w) > 0 && len(t.Where) > 0 && !intersects(w, t.Where) && !slices.Contains(t.Where, "chat") {
		return false
	}
	if w := f["who"]; len(w) > 0 && len(t.Who) > 0 && !intersects(w, t.Who) {
		return false
	}
	return true
}

func intersects(a, b []string) bool {
	return slices.ContainsFunc(a, func(s string) bool { return slices.Contains(b, s) })
}

var (
	incidentRe = regexp.MustCompile(`\((\d{1,2}/\d{1,2}/\d{4}[^)]*)\)`)
	// "On 13/10/2026 a session wrote …" — a dated sentence, when the line
	// gives its incident in running text.
	incidentSentenceRe = regexp.MustCompile(`(?:^|[.;] )((?:On|on) \d{1,2}/\d{1,2}/\d{4}[^.]*\.(?:[^.]*\(D-\d+\)\.)?)`)
	citeRe             = regexp.MustCompile(`\b([DTA]-\d+|P\.\d+)\b`)
)

func helpLine(in Instruction, t InstructionTags) HelpLine {
	en, _, _ := strings.Cut(in.Text, "||")
	l := HelpLine{ID: in.ID, Kind: in.Kind, Label: in.Label, Term: in.Term, Text: in.Text, Why: in.Why, Ref: in.RationaleRef, Tags: t,
		CatalogRef: in.CatalogRef, Translation: in.Translation, Lang: in.Lang, TranslatedBy: in.TranslatedBy}
	if m := incidentRe.FindStringSubmatch(en); m != nil {
		l.Incident = m[1]
	} else if m := incidentSentenceRe.FindStringSubmatch(en); m != nil {
		l.Incident = strings.TrimSpace(m[1])
	}
	for _, c := range citeRe.FindAllString(en, -1) {
		if !slices.Contains(l.Cites, c) {
			l.Cites = append(l.Cites, c)
		}
	}
	return l
}

// Help is help(situation) over a store's active instructions: the directives
// whose tags fit the situation and, when it asks what a word means, the
// definitions it names — each line in full. Meta lines are not returned:
// instructions() carries them.
func Help(list []Instruction, situation string) HelpResult {
	res := HelpResult{Situation: situation, Directives: []HelpLine{}, Definitions: []HelpLine{}, Suggested: []HelpLine{}}
	var terms []string
	for _, in := range list {
		if in.Kind == "definition" && in.Term != nil {
			terms = append(terms, *in.Term)
		}
	}
	facets, unread, named := readSituation(situation, terms)
	res.ReadAs = facets
	if len(unread) > 0 {
		res.ReadAs["unread"] = unread
	}
	asksMeaning := slices.Contains(facets["what"], "term")
	situational := len(facets["when"]) > 0 || len(facets["where"]) > 0 || len(facets["what"]) > 0

	for _, in := range list {
		if in.Status != "active" || in.Kind == "meta" {
			continue
		}
		t := in.Tags
		if in.Kind == "definition" {
			// A definition comes back when the situation asks what a word
			// means: the one it names, or all of them if it names none.
			if asksMeaning && (len(named) == 0 || (in.Term != nil && slices.Contains(named, *in.Term))) {
				res.Definitions = append(res.Definitions, helpLine(in, t))
			}
			continue
		}
		if asksMeaning && len(named) > 0 && len(facets["when"]) == 0 {
			// "what is a T" asks for a definition, not for every directive
			// that touches a term.
			continue
		}
		if t.empty() {
			if situational && sharesWords(in.Text, unread, facets) {
				res.Untagged = append(res.Untagged, helpLine(in, t))
			}
			continue
		}
		if situational && fits(t, facets) {
			res.Directives = append(res.Directives, helpLine(in, t))
		}
	}
	res.Count = len(res.Directives) + len(res.Definitions) + len(res.Untagged)
	if res.Count == 0 {
		res.Next = "Nothing in this store's instructions fits that situation as read (read_as). Name the moment and the object, " +
			"e.g. \"draft linkedin message\", \"record a decision\", \"analyse strategy\", \"close session\", \"what is a T\"; " +
			"instructions() lists every line with its tags."
	}
	return res
}

// sharesWords matches an untagged line on its text: a word of the situation,
// or one of the tags read from it, appears in the line.
func sharesWords(text string, unread []string, facets map[string][]string) bool {
	t := fold(text)
	words := append([]string{}, unread...)
	for _, axis := range []string{"what", "where"} {
		words = append(words, facets[axis]...)
	}
	for _, w := range words {
		if len(w) > 2 && strings.Contains(t, " "+w) {
			return true
		}
	}
	return false
}

// --- tags in the store --------------------------------------------------

// InstructionTagInput tags one line: id is I-… or the line's label (14,
// M3, def:claim). The new tags replace the line's current ones; the old row
// stays.
type InstructionTagInput struct {
	ID string `json:"id"`
	InstructionTags
}

// attachOwnTags reads each line's latest tag row in this store.
func attachOwnTags(ctx context.Context, q querier, tenant string, list []Instruction) error {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (instruction_id) instruction_id, tag_who, tag_what, tag_when, tag_where,
		coalesce(tag_why, ''), coalesce(tag_how, '')
		FROM instruction_tag WHERE tenant_id = $1 ORDER BY instruction_id, id DESC`, TenantOr(tenant))
	if err != nil {
		return err
	}
	defer rows.Close()
	tagged := map[string]InstructionTags{}
	for rows.Next() {
		var id string
		var t InstructionTags
		if err := rows.Scan(&id, &t.Who, &t.What, &t.When, &t.Where, &t.Why, &t.How); err != nil {
			return err
		}
		t.Source = "tagged"
		tagged[id] = t
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range list {
		if t, ok := tagged[list[i].ID]; ok {
			list[i].Tags = t
		}
	}
	return nil
}

// attachTags reads each line's current tags: the latest row tagged in this
// store; else, for a line that is a catalog entry word for word, the
// catalog's tags; else the default list's tags for a default line unchanged.
func attachTags(ctx context.Context, q querier, tenant string, list []Instruction, c catalog) error {
	if err := attachOwnTags(ctx, q, tenant, list); err != nil {
		return err
	}
	for i := range list {
		if !list[i].Tags.empty() {
			continue
		}
		if e := c.linked(list[i]); e != nil && e.Text == list[i].Text && !e.Tags.empty() {
			list[i].Tags = e.Tags
			list[i].Tags.Source = "catalog"
		} else if t, ok := defaultTagsFor(list[i]); ok {
			list[i].Tags = t
		}
	}
	return nil
}

func insertTags(ctx context.Context, q querier, a Actor, id string, t InstructionTags) error {
	nz := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	_, err := q.Exec(ctx, `INSERT INTO instruction_tag (tenant_id, instruction_id, tag_who, tag_what, tag_when, tag_where,
		tag_why, tag_how, signer, producer, grant_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		a.Tenant(), id, nz(t.Who), nz(t.What), nz(t.When), nz(t.Where), nullable(t.Why), nullable(t.How),
		a.UserID, a.ActorID, a.GrantID)
	return err
}

// TagInstruction sets one line's tags. The line must be active; a label names
// the active line that carries it.
func (r *Repo) TagInstruction(ctx context.Context, a Actor, in InstructionTagInput) (Instruction, error) {
	if err := r.checkSigner(a); err != nil {
		return Instruction{}, err
	}
	if err := validTags(in.InstructionTags); err != nil {
		return Instruction{}, err
	}
	if in.InstructionTags.empty() {
		return Instruction{}, failNext("INVALID_TAG", "Give at least one of who, what, when, where, why, how.", "no tags given")
	}
	in.Source = ""
	c, err := r.loadCatalog(ctx)
	if err != nil {
		return Instruction{}, err
	}
	var out Instruction
	err = r.withTenant(ctx, a.Tenant(), func(q querier) error {
		if err := r.writable(ctx, q, a); err != nil {
			return err
		}
		if err := seedInstructions(ctx, q, a.Tenant(), c); err != nil {
			return err
		}
		id, err := activeInstructionID(ctx, q, a.Tenant(), in.ID)
		if err != nil {
			return err
		}
		if err := insertTags(ctx, q, a, id, in.InstructionTags); err != nil {
			return err
		}
		got, err := readInstruction(ctx, q, a.Tenant(), id)
		if err != nil {
			return err
		}
		list := []Instruction{got}
		if err := attachTags(ctx, q, a.Tenant(), list, c); err != nil {
			return err
		}
		out = list[0]
		return nil
	})
	return out, err
}

// activeInstructionID resolves I-… or a label to the active line.
func activeInstructionID(ctx context.Context, q querier, tenant, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", failNext("INSTRUCTION_UNKNOWN", "Give I-… or the line's label (14, M3, def:claim).", "id is required")
	}
	var id string
	err := q.QueryRow(ctx, `SELECT i.id `+instructionFrom+`
		WHERE i.tenant_id = $1 AND r.instruction_id IS NULL AND (i.id = $2 OR i.label = $3)
		ORDER BY i.num DESC LIMIT 1`, TenantOr(tenant), canonInstructionID(ref), ref).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", failNext("INSTRUCTION_UNKNOWN", "instructions() lists the active lines with their ids and labels.",
				"no active instruction %s in this store", ref)
		}
		return "", err
	}
	return id, nil
}

// InstructionIndexRow is one line as instructions() lists it: enough to know
// the line exists and when it applies, not its text — help(situation) gives
// that. A line with no tags yet shows the start of its text instead.
type InstructionIndexRow struct {
	ID    string           `json:"id"`
	Kind  string           `json:"kind"`
	Label string           `json:"label"`
	Term  *string          `json:"term,omitempty"`
	Tags  *InstructionTags `json:"tags,omitempty"`
	Gist  string           `json:"gist,omitempty"`
	// A line from the catalog names its entry and the language it is
	// translated into.
	CatalogRef *string                `json:"catalog_ref,omitempty"`
	Lang       *string                `json:"lang,omitempty"`
	Status     string                 `json:"status"`
	Retired    *InstructionRetirement `json:"retired,omitempty"`
}

// IndexRow is in's row in the index.
func IndexRow(in Instruction) InstructionIndexRow {
	row := InstructionIndexRow{ID: in.ID, Kind: in.Kind, Label: in.Label, Term: in.Term, Status: in.Status, Retired: in.Retired,
		CatalogRef: in.CatalogRef, Lang: in.Lang}
	if in.Tags.empty() {
		en, _, _ := strings.Cut(in.Text, "||")
		words := strings.Fields(strings.TrimPrefix(strings.TrimSpace(en), "EN —"))
		if len(words) > 14 {
			words = append(words[:14], "…")
		}
		row.Gist = strings.Join(words, " ")
	} else {
		t := in.Tags
		row.Tags = &t
	}
	return row
}

// Package subject holds the vocabulary of claims as the MCP tools
// name them: subject strings, labels T / A / A-b, clm_ ids, statement
// normalisation for dedup, and the basis string. Shared by tk-api (which
// decides) and internal/kb (which must refuse early, before issuing a cause).
package subject

import (
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/ankayma/tknowledge-personal/internal/ledger"
)

// Subject is <namespace>:<entity>.<attribute>, each part normalised.
type Subject struct {
	Namespace, Entity, Attribute string
}

func (s Subject) String() string { return s.Namespace + ":" + s.Entity + "." + s.Attribute }

var ErrInvalid = errors.New("subject must be <namespace>:<entity>.<attribute>")

// Parse normalises and splits a subject. The attribute is after the last dot,
// so an entity may itself contain dots.
func Parse(raw string) (Subject, error) {
	ns, rest, ok := strings.Cut(strings.TrimSpace(raw), ":")
	if !ok {
		return Subject{}, ErrInvalid
	}
	i := strings.LastIndex(rest, ".")
	if i < 0 {
		return Subject{}, ErrInvalid
	}
	s := Subject{Namespace: Slug(ns), Entity: Slug(rest[:i]), Attribute: Slug(rest[i+1:])}
	if s.Namespace == "" || s.Entity == "" || s.Attribute == "" {
		return Subject{}, ErrInvalid
	}
	return s, nil
}

// Slug lowercases, strips diacritics (including Vietnamese đ, which NFD does
// not decompose) and turns whitespace runs into single hyphens.
func Slug(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if r == 'đ' {
			r = 'd'
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), "-")
}

// NormalizeStatement is the dedup key: trim, lowercase, collapse
// whitespace, drop trailing punctuation.
func NormalizeStatement(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	return strings.TrimRight(s, ".,;:!?… ")
}

// Labels.
const (
	LabelT  = "T"
	LabelA  = "A"
	LabelAB = "A-b"
)

// Label is a row's spec label.
func Label(mark string, ab bool) string {
	switch {
	case mark == "T":
		return LabelT
	case ab:
		return LabelAB
	default:
		return LabelA
	}
}

var ErrLabel = errors.New("label must be T, A or A-b")

// ParseLabel returns the canonical label and the ledger (mark, ab) it maps to.
func ParseLabel(s string) (label, mark string, ab bool, err error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "T":
		return LabelT, "T", false, nil
	case "A":
		return LabelA, "A", false, nil
	case "A-B":
		return LabelAB, "A", true, nil
	}
	return "", "", false, ErrLabel
}

// Concluded reports moves after which a claim is no longer open: an A-b was
// closed, a proposal rejected, a T refuted.
func Concluded(move string) bool {
	switch move {
	case "resolve", "drop", "reject", "refute":
		return true
	}
	return false
}

// FormatID is the spec id of a ledger row.
func FormatID(rowID int64) string { return "clm_" + strconv.FormatInt(rowID, 10) }

var ErrID = errors.New("claim_id must be clm_<n>")

// ParseID accepts clm_<n> or a bare row id.
func ParseID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(s), "clm_"), 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrID
	}
	return id, nil
}

// FormatDocID is the spec id of a document version.
func FormatDocID(id int64) string { return "doc_" + strconv.FormatInt(id, 10) }

// ParseDocID accepts doc_<n> or a bare id.
func ParseDocID(s string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(s), "doc_"), 10, 64)
	return id, err == nil && id > 0
}

// FormatConversationID / FormatEventID are the ids of conversations and their
// turns. A conversation was called a thread before 1/10/2026 (thr_<n>); that
// name now belongs to the meta-knowledge thread, TH-<n>.
func FormatConversationID(id int64) string { return "conv_" + strconv.FormatInt(id, 10) }
func FormatEventID(id int64) string        { return "evt_" + strconv.FormatInt(id, 10) }

// LegacyThreadSubject is the old-ledger subject prefix a conversation's claims
// were written under before the rename (thread:thr_<n>.); rows already in the
// ledger keep it, since the ledger is never rewritten.
func LegacyThreadSubject(id int64) string { return "thread:thr_" + strconv.FormatInt(id, 10) + "." }

// ParseConversationID accepts conv_<n>, the old thr_<n>, or a bare id.
func ParseConversationID(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "thr_"); ok {
		return parsePrefixed(rest, "")
	}
	return parsePrefixed(s, "conv_")
}

// ParseEventID accepts evt_<n> or a bare id.
func ParseEventID(s string) (int64, bool) { return parsePrefixed(s, "evt_") }

func parsePrefixed(s, prefix string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(s), prefix), 10, 64)
	return id, err == nil && id > 0
}

// ParseEventRef reads the event id out of a basis evt_<n>; ok is false when
// the basis is not an event reference.
func ParseEventRef(basis string) (int64, bool) {
	basis = strings.TrimSpace(basis)
	if !strings.HasPrefix(basis, "evt_") {
		return 0, false
	}
	return ParseEventID(basis)
}

// DerivationRefs reads the claim ids out of a basis derivation:clm_1,clm_2.
func DerivationRefs(ref string) ([]int64, bool) {
	list, ok := strings.CutPrefix(strings.TrimSpace(ref), DerivationPrefix)
	if !ok {
		return nil, false
	}
	ids := []int64{}
	for _, part := range strings.Split(list, ",") {
		id, err := ParseID(part)
		if err != nil {
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, len(ids) > 0
}

// PrincipleCode reads the code out of a basis string principle:<code>.
func PrincipleCode(ref string) (string, bool) {
	code, ok := strings.CutPrefix(strings.TrimSpace(ref), PrinciplePrefix)
	code = strings.TrimSpace(code)
	return code, ok && code != "" && !strings.ContainsAny(code, " \t\n")
}

// ParseDocRef reads the document id out of a basis like doc_<n>#section.
// ok is false when the basis is not a document reference.
func ParseDocRef(basis string) (int64, bool) {
	basis = strings.TrimSpace(basis)
	if !strings.HasPrefix(basis, "doc_") {
		return 0, false
	}
	id, _, _ := strings.Cut(basis, "#")
	return ParseDocID(id)
}

var (
	ErrNoBasis  = errors.New("label T requires basis")
	ErrBadBasis = errors.New("basis must be doc_…, evt_…, chat:<chat_id>#<turn>, template:<id>@<version>, principle:<code>, derivation:<clm_…,clm_…> or an http(s) URL")
)

var basisPrefixes = []string{"doc_", "evt_", "chat:", "template:", "http://", "https://", PrinciplePrefix, DerivationPrefix}

// DerivationPrefix marks a claim that follows from claims already admitted:
// derivation:clm_12,clm_15. tk-api resolves the ids into basis.refs, and
// is_truth then requires every premise to be eff=T (eff §4).
const DerivationPrefix = "derivation:"

// PrinciplePrefix marks the trust class of P.9: a claim resting on the
// Principle List, not on a sourced fact. tk-api resolves the code to the
// principle's own row (basis.refs), which is_truth then checks (eff §4).
const PrinciplePrefix = "principle:"

const basisDateSep = " @"

// Basis turns a basis string into a ledger basis. The source is checked for
// shape only (existence, not content; doc_/evt_ existence is checked once
// those tables exist, phase B/C). The note carries source and date, as
// is_truth requires of a fact.
func Basis(raw string, at time.Time) (ledger.Basis, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ledger.Basis{}, ErrNoBasis
	}
	for _, p := range basisPrefixes {
		if strings.HasPrefix(raw, p) && len(raw) > len(p) && !strings.ContainsAny(raw, " \t\n") {
			kind := "fact"
			switch p {
			case PrinciplePrefix:
				kind = "principle"
			case DerivationPrefix:
				kind = "derivation"
			}
			return ledger.Basis{Kind: kind, Note: raw + basisDateSep + at.UTC().Format("2006-01-02")}, nil
		}
	}
	return ledger.Basis{}, ErrBadBasis
}

// BasisRef gives back the spec basis string of a ledger basis (history).
func BasisRef(b *ledger.Basis) string {
	if b == nil {
		return ""
	}
	if b.Kind == "fact" || ((b.Kind == "principle" || b.Kind == "derivation") && b.Note != "") {
		if i := strings.LastIndex(b.Note, basisDateSep); i > 0 {
			if _, err := time.Parse("2006-01-02", b.Note[i+len(basisDateSep):]); err == nil {
				return b.Note[:i]
			}
		}
		return b.Note
	}
	refs := make([]string, 0, len(b.Refs))
	for _, r := range b.Refs {
		refs = append(refs, strconv.FormatInt(r, 10))
	}
	return b.Kind + ":" + strings.Join(refs, ",")
}

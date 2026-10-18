package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// The T/A store (spec mvp/spec-admit-assume-promote-19-09-26.md): assume,
// update_assumption, admit, promote, and the Principle List behind them. Every
// response is the record as stored — no summary field, nothing to interpret.
// Who writes is the grant: signer is the person, producer the agent.

func (s *Server) tka() *tka.Repo {
	if s.TKA == nil {
		s.TKA = tka.New(s.Repo.Pool)
		s.TKA.WriteTZ, s.TKA.PricingURL, s.TKA.Contact = s.WriteTZ, s.PricingURL, s.Contact
	}
	return s.TKA
}

func actorOf(r *http.Request) tka.Actor { return actorOfGrant(grantFrom(r.Context())) }

func actorOfGrant(g grant.Grant) tka.Actor {
	return tka.Actor{UserID: g.UserID, ActorID: g.ActorID, GrantID: g.GrantID, TenantID: g.TenantID}
}

// tenantOf is the store this request reads: the grant's tenant, never a
// parameter — asking for someone else's tenant is not a thing a caller can do.
func tenantOf(r *http.Request) string { return grantFrom(r.Context()).TenantID }

// writeStore returns a refusal in the shape both transports share:
// {"error": {"code", "message", "next"}}. A refusal is a result — the caller
// reads `next` and carries on without guessing.
func writeStore(w http.ResponseWriter, err error) {
	var e *tka.Error
	if !errors.As(err, &e) {
		e = &tka.Error{Code: "INTERNAL", Message: err.Error()}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": e})
		return
	}
	status := http.StatusConflict
	switch e.Code {
	case "NOT_FOUND", "PRINCIPLE_UNKNOWN", "PREMISE_NOT_FOUND", "THREAD_UNKNOWN", "LINK_TARGET_UNKNOWN",
		"DOCUMENT_UNKNOWN", "FLAG_UNKNOWN", "NOT_ATTACHED", "TENANT_UNKNOWN", "CATALOG_UNKNOWN":
		status = http.StatusNotFound
	case "EMPTY_CLAIM", "EMPTY_STATEMENT", "EMPTY_REASON", "INVALID_CODE", "INVALID_TYPE", "INVALID_SOURCE",
		"INVALID_BASIS", "INVALID_DATE", "INVALID_TAIL", "MISSING_TAIL", "NO_SOURCE", "EMPTY_DERIVATION",
		"CLAIM_FAILED", "SINGLE_PREMISE", "SOURCE_NOT_RETRIEVABLE", "INVALID_SOURCE_URL",
		"EMPTY_KEYWORD", "EMPTY_TITLE", "EMPTY_LINK", "EMPTY_OBSERVATION",
		"FILE_NOT_KEPT", "EMPTY_TEXT", "INVALID_TEXT_PRODUCER", "INVALID_METHOD", "INVALID_SOURCE_URI",
		"INVALID_SHA256", "INVALID_SIZE", "INVALID_MEDIA_TYPE", "DOCUMENT_TOO_LARGE", "USE_DOCUMENT",
		"EMPTY_WHERE", "INVALID_STATUS", "INVALID_SEAT", "INVALID_MEMBER", "INVALID_TAG", "EMPTY_SITUATION",
		"CATALOG_TEXT_CHANGED", "TRANSLATION_WITHOUT_LANG", "TRANSLATION_WITHOUT_CATALOG", "INVALID_DECISION":
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{"error": e})
}

// entryWith marshals the stored record and hangs `similar` off it — and the
// thread it was linked to, when the caller named one — so the caller reads one
// object: what was written, and what was already nearby.
func entryWith(w http.ResponseWriter, entry any, similar []tka.Similar, linked ...*tka.ThreadLinked) {
	raw, err := json.Marshal(entry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if len(similar) > 0 {
		out["similar"] = similar
	}
	if len(linked) > 0 && linked[0] != nil {
		out["thread"] = linked[0]
	}
	writeJSON(w, http.StatusOK, out)
}

func decodeInto[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var in T
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return in, false
	}
	return in, true
}

// POST /tka/assume
func (s *Server) postAssume(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.AssumeInput](w, r)
	if !ok {
		return
	}
	res, err := s.tka().Assume(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	if res.Entry == nil { // a close claim already exists; nothing written
		writeJSON(w, http.StatusOK, map[string]any{"similar": res.Similar})
		return
	}
	entryWith(w, res.Entry, res.Similar, res.Thread)
}

// POST /tka/assumptions/update
func (s *Server) postUpdateAssumption(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.UpdateInput](w, r)
	if !ok {
		return
	}
	res, err := s.tka().UpdateAssumption(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	if res.Entry == nil {
		writeJSON(w, http.StatusOK, map[string]any{"similar": res.Similar})
		return
	}
	entryWith(w, res.Entry, res.Similar)
}

// POST /tka/admit
func (s *Server) postTkaAdmit(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.AdmitInput](w, r)
	if !ok {
		return
	}
	res, err := s.tka().Admit(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	if res.Entry == nil {
		writeJSON(w, http.StatusOK, map[string]any{"similar": res.Similar})
		return
	}
	entryWith(w, res.Entry, nil, res.Thread)
}

// POST /tka/promote
func (s *Server) postTkaPromote(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.PromoteInput](w, r)
	if !ok {
		return
	}
	res, err := s.tka().Promote(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	raw, _ := json.Marshal(res.Entry)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	out["assumption"] = res.Assumption
	writeJSON(w, http.StatusOK, out)
}

// POST /tka/principles
func (s *Server) postTkaPrinciple(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[struct {
		Code      string `json:"code"`
		Statement string `json:"statement"`
		Note      string `json:"note"`
	}](w, r)
	if !ok {
		return
	}
	p, err := s.tka().AddPrinciple(r.Context(), actorOf(r), in.Code, in.Statement, in.Note)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// POST /tka/principles/retire — the response names every T that rested on it.
func (s *Server) postTkaPrincipleRetire(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}](w, r)
	if !ok {
		return
	}
	p, affected, err := s.tka().RetirePrinciple(r.Context(), actorOf(r), in.Code, in.Reason)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"principle": p, "standing_on_it": affected})
}

// GET /tka/principles?include_retired=true
func (s *Server) getTkaPrinciples(w http.ResponseWriter, r *http.Request) {
	list, err := s.tka().Principles(r.Context(), tenantOf(r), r.URL.Query().Get("include_retired") == "true")
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"principles": list, "count": len(list)})
}

// POST /tka/instructions — {text, supersedes?, reason?, chat_id?}.
func (s *Server) postTkaInstruction(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.InstructionAddInput](w, r)
	if !ok {
		return
	}
	got, err := s.tka().AddInstruction(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// POST /tka/instructions/retire — {id, reason}.
func (s *Server) postTkaInstructionRetire(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}](w, r)
	if !ok {
		return
	}
	got, err := s.tka().RetireInstruction(r.Context(), actorOf(r), in.ID, in.Reason)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// GET /tka/instructions?history=true[&view=index] — what an agent loads at
// the start of a session: the store's metadata, then the active instructions
// kept apart by kind (meta governs the list, directives are carried out,
// definitions explain); history adds the retired ones. view=index is what
// the MCP tool returns (owner 13/10/2026): the meta lines in full and one
// index row per other line — its label, tags and why — with the full text
// left to help(situation).
func (s *Server) getTkaInstructions(w http.ResponseWriter, r *http.Request) {
	list, offers, err := s.tka().InstructionsWithOffers(r.Context(), tenantOf(r), r.URL.Query().Get("history") == "true")
	if err != nil {
		writeStore(w, err)
		return
	}
	info, err := s.storeInfoFor(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if r.URL.Query().Get("view") == "index" {
		meta, index := []tka.Instruction{}, []tka.InstructionIndexRow{}
		for _, in := range list {
			if in.Kind == "meta" {
				meta = append(meta, in)
				continue
			}
			index = append(index, tka.IndexRow(in))
		}
		writeJSON(w, http.StatusOK, map[string]any{"store": info, "meta": meta, "index": index, "count": len(list), "catalog": offers,
			"help": "Call help(situation) for the full text of the lines that apply, e.g. help(\"draft linkedin message\"), " +
				"help(\"record a decision\"), help(\"close session\"). Situation words are read against the tags in this index."})
		return
	}
	kinds := map[string][]tka.Instruction{"meta": {}, "directive": {}, "definition": {}}
	for _, in := range list {
		kinds[in.Kind] = append(kinds[in.Kind], in)
	}
	writeJSON(w, http.StatusOK, map[string]any{"store": info, "meta": kinds["meta"],
		"directives": kinds["directive"], "definitions": kinds["definition"], "count": len(list)})
}

// GET /tka/help?situation=… — the instructions that apply to one situation,
// each in full (tka.Help).
func (s *Server) getTkaHelp(w http.ResponseWriter, r *http.Request) {
	situation := strings.TrimSpace(r.URL.Query().Get("situation"))
	if situation == "" {
		writeStore(w, &tka.Error{Code: "EMPTY_SITUATION", Message: "help needs a situation",
			Next: "Name what you are about to do, e.g. \"draft linkedin message\", \"record a decision\", \"close session\"."})
		return
	}
	res, err := s.tka().HelpFor(r.Context(), tenantOf(r), situation)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// POST /tka/instructions/tag — {id, who?, what?, when?, where?, why?, how?}:
// the line's tags from now on; the old ones stay as rows.
func (s *Server) postTkaInstructionTag(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.InstructionTagInput](w, r)
	if !ok {
		return
	}
	got, err := s.tka().TagInstruction(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// GET /tka/entries/{id} — whatever the id names, as stored.
func (s *Server) getTkaEntry(w http.ResponseWriter, r *http.Request) {
	entry, err := s.tka().Read(r.Context(), tenantOf(r), chi.URLParam(r, "id"))
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// GET /tka/entries/{id}/chain — the entry and everything under it, each link
// with whether it still stands.
func (s *Server) getTkaChain(w http.ResponseWriter, r *http.Request) {
	entry, nodes, err := s.tka().Chain(r.Context(), tenantOf(r), chi.URLParam(r, "id"))
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": entry, "premises": nodes})
}

// GET /tka/assumptions?type=A-p — what is still waiting.
func (s *Server) getTkaAssumptions(w http.ResponseWriter, r *http.Request) {
	list, err := s.tka().OpenAssumptions(r.Context(), tenantOf(r), r.URL.Query().Get("type"))
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assumptions": list, "count": len(list)})
}

// GET /tka/truths?standing=true&limit=50
func (s *Server) getTkaTruths(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.tka().Truths(r.Context(), tenantOf(r), r.URL.Query().Get("standing") == "true", limit)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"truths": list, "count": len(list)})
}

// POST /tka/demote — a [T] falls back to an assumption's standing, with a reason.
func (s *Server) postTkaDemote(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[struct {
		ID             string `json:"id"`
		Reason         string `json:"reason"`
		ContradictedBy string `json:"contradicted_by"`
	}](w, r)
	if !ok {
		return
	}
	t, err := s.tka().Demote(r.Context(), actorOf(r), in.ID, in.Reason, in.ContradictedBy)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// POST /tka/retire — closes an entry for good; the response names what rested on it.
func (s *Server) postTkaRetire(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}](w, r)
	if !ok {
		return
	}
	entry, affected, err := s.tka().Retire(r.Context(), actorOf(r), in.ID, in.Reason)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": entry, "standing_on_it": affected})
}

// POST /tka/recheck — someone went back to the source and it still holds.
func (s *Server) postTkaRecheck(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[struct {
		ID   string `json:"id"`
		Note string `json:"note"`
	}](w, r)
	if !ok {
		return
	}
	t, err := s.tka().Recheck(r.Context(), actorOf(r), in.ID, in.Note)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// POST /tka/documents — one piece of evidence: the text read from it, how it was
// looked at, and where the original lives (spec T–D §3). The body is JSON. A
// file sent as the body is the old shape and is refused: tk keeps no files.
func (s *Server) postTkaDocument(w http.ResponseWriter, r *http.Request) {
	media, _, _ := strings.Cut(strings.ToLower(r.Header.Get("Content-Type")), ";")
	if strings.TrimSpace(media) != "application/json" {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, tka.MaxDocument+1))
		writeStore(w, &tka.Error{Code: "FILE_NOT_KEPT",
			Message: "tk keeps no files: a document is the text read from the evidence and a pointer to where the original lives",
			Next:    nextFileNotKept})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2*tka.MaxDocument)
	in, ok := decodeInto[tka.DocumentInput](w, r)
	if !ok {
		return
	}
	doc, err := s.tka().AddDocument(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

const nextFileNotKept = "Read the file and send what it says as text, with method (how you looked at it) and " +
	"text_producer (who wrote the text). Point at the original with source_uri — https://, drive://, s3://, file:// — " +
	"and its sha256 if you have it. The original stays where it is."

// POST /tka/attest — attach a document the caller added to a claim.
func (s *Server) postTkaAttest(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.AttestInput](w, r)
	if !ok {
		return
	}
	out, err := s.tka().Attest(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /tka/evidence/withdraw — take a document back from a claim, with the reason.
func (s *Server) postTkaWithdrawEvidence(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.WithdrawInput](w, r)
	if !ok {
		return
	}
	out, err := s.tka().WithdrawEvidence(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /tka/flags?status=open|closed|all — work the machine set aside.
func (s *Server) getTkaFlags(w http.ResponseWriter, r *http.Request) {
	flags, err := s.tka().Flags(r.Context(), tenantOf(r), r.URL.Query().Get("status"))
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flags": flags, "count": len(flags)})
}

// POST /tka/flags/close — the named move that answers a flag.
func (s *Server) postTkaCloseFlag(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.CloseFlagInput](w, r)
	if !ok {
		return
	}
	out, err := s.tka().CloseFlag(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /tka/documents — what evidence this store holds.
func (s *Server) getTkaDocuments(w http.ResponseWriter, r *http.Request) {
	docs, err := s.tka().Documents(r.Context(), tenantOf(r))
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs, "count": len(docs)})
}

// GET /tka/documents/{id} — the evidence itself, so a person can look at it.
func (s *Server) getTkaDocument(w http.ResponseWriter, r *http.Request) {
	doc, body, err := s.tka().ReadDocument(r.Context(), tenantOf(r), chi.URLParam(r, "id"))
	if err != nil {
		writeStore(w, err)
		return
	}
	// A document added since 2/10/2026 has no file here: its record, text
	// included, is the evidence the store holds. Only a legacy document still
	// comes back as the file it was.
	if body == nil {
		writeJSON(w, http.StatusOK, doc)
		return
	}
	w.Header().Set("Content-Type", doc.MediaType)
	w.Header().Set("X-TK-SHA256", doc.SHA256)
	if doc.Filename != "" {
		w.Header().Set("Content-Disposition", "inline; filename="+strconv.Quote(doc.Filename))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// POST /tka/check-claim — is this sentence a claim at all? No store is touched:
// the answer depends on the sentence alone, so it costs nothing to ask first.
func (s *Server) postTkaCheckClaim(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[struct {
		Claim string `json:"claim"`
		Basis string `json:"basis"`
	}](w, r)
	if !ok {
		return
	}
	check := tka.CheckClaim(in.Claim, in.Basis)
	// The route needs no grant; when the caller sends one anyway, the answer
	// also carries their store's word cap.
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && s.Grants != nil {
		if g, err := s.Grants.Verify(strings.TrimSpace(token)); err == nil {
			if t, err := s.tka().ReadTenant(r.Context(), g.TenantID); err == nil {
				check.MaxWords = t.MaxWords
			}
		}
	}
	writeJSON(w, http.StatusOK, check)
}

// GET /tka/definitions — the store's vocabulary, claim first. The rules live in
// code and cannot be edited; these are the words that explain them, and they live
// in a JSON file per language, so a new language is a file and nothing else
// (owner decision, 28/9).
func (s *Server) getTkaDefinitions(w http.ResponseWriter, r *http.Request) {
	lang := r.URL.Query().Get("lang")
	if term := r.URL.Query().Get("term"); term != "" {
		d, err := tka.Define(term, lang)
		if err != nil {
			writeStore(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
		return
	}
	v, err := tka.Glossary(lang)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lang":        v.Lang,
		"version":     v.Version,
		"languages":   tka.Languages(),
		"definitions": v.Definitions,
	})
}

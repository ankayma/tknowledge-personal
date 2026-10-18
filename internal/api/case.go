package api

import (
	"net/http"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// POST /tka/cases — keep a copy of a lesson already stored as a document
// (owner 17/10/2026, D-239).
func (s *Server) postTkaCase(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.CaseInput](w, r)
	if !ok {
		return
	}
	c, err := s.tka().AddCase(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// POST /tka/cases/tag — a new line of kind and tags; the newest is current.
func (s *Server) postTkaCaseTag(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[struct {
		ID string `json:"id"`
		tka.CaseTags
	}](w, r)
	if !ok {
		return
	}
	c, err := s.tka().TagCase(r.Context(), actorOf(r), in.ID, in.CaseTags)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// POST /tka/cases/retire — out of cases(), still readable.
func (s *Server) postTkaCaseRetire(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}](w, r)
	if !ok {
		return
	}
	c, err := s.tka().RetireCase(r.Context(), actorOf(r), in.ID, in.Reason)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// GET /tka/cases?q=&kind=&keyword=&topic=&case_study=&retired=true
func (s *Server) getTkaCases(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	list, facets, err := s.tka().Cases(r.Context(), tenantOf(r), tka.CaseFilter{Q: v.Get("q"), Kind: v.Get("kind"),
		Keyword: v.Get("keyword"), Topic: v.Get("topic"), CaseStudy: v.Get("case_study"), Retired: v.Get("retired") == "true"})
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": list, "count": len(list), "facets": facets})
}

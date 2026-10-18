package api

import (
	"net/http"
	"strings"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// The instruction catalog (owner 14/10/2026, spec-instruction-catalog-14-10-26.md):
// best practices T-knowledge proposes, in English. Anyone reads the approved
// entries without a grant — a website renders them, a person checks them; the
// maintainer (tk:admin, CLI) proposes, approves, rejects and withdraws.

const catalogAbout = "The T-knowledge instruction catalog: practices proposed for agents working in a T-knowledge store, " +
	"curated from what people report. An entry is published only when it ran active in a real store, carries a dated incident, " +
	"has the consent of the person who signs that store and holds nothing company-specific; the store is named, the person is not. " +
	"Each entry is an offer: a person takes it into their own store " +
	"(instruction_add with catalog: BP-…, translated into their language) or declines it."

// statusesFor reads ?status=: current (the default), or a comma list, or all.
func statusesFor(raw string, maintainer bool) ([]string, bool) {
	public := []string{tka.CatalogCurrent, tka.CatalogSuperseded, tka.CatalogWithdrawn}
	every := append([]string{tka.CatalogProposed, tka.CatalogRejected}, public...)
	allowed := public
	if maintainer {
		allowed = every
	}
	raw = strings.TrimSpace(raw)
	switch raw {
	case "":
		return []string{tka.CatalogCurrent}, true
	case "all":
		return allowed, true
	}
	var out []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		ok := false
		for _, a := range allowed {
			ok = ok || a == s
		}
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func (s *Server) catalogList(w http.ResponseWriter, r *http.Request, maintainer bool) {
	statuses, ok := statusesFor(r.URL.Query().Get("status"), maintainer)
	if !ok {
		allowed := "current, superseded, withdrawn or all"
		if maintainer {
			allowed = "proposed, current, rejected, superseded, withdrawn or all"
		}
		writeStore(w, &tka.Error{Code: "INVALID_STATUS", Message: "unknown status", Next: "status is " + allowed + "."})
		return
	}
	list, err := s.tka().Catalog(r.Context(), tka.CatalogFilter{Statuses: statuses,
		Situation: r.URL.Query().Get("situation"), Maintainer: maintainer})
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"about": catalogAbout, "entries": list, "count": len(list)})
}

// GET /catalog?status=current|superseded|withdrawn|all&situation=… — no grant.
// Any page may read it from the browser (tknowledge.org/catalog renders it);
// it carries no credentials and holds nothing but the published entries.
func (s *Server) getCatalog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	s.catalogList(w, r, false)
}

// GET /admin/catalog?status=proposed|…|all&situation=… — with review notes.
func (s *Server) getAdminCatalog(w http.ResponseWriter, r *http.Request) { s.catalogList(w, r, true) }

// POST /admin/catalog — one proposed entry (tka.CatalogAddInput).
func (s *Server) postCatalog(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.CatalogAddInput](w, r)
	if !ok {
		return
	}
	got, err := s.tka().CatalogAdd(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// POST /admin/catalog/review — {id, decision: approve|reject, note, source, incident_on}.
func (s *Server) postCatalogReview(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.CatalogReviewInput](w, r)
	if !ok {
		return
	}
	got, err := s.tka().CatalogReview(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// POST /admin/catalog/withdraw — {id, reason}.
func (s *Server) postCatalogWithdraw(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}](w, r)
	if !ok {
		return
	}
	got, err := s.tka().CatalogWithdraw(r.Context(), actorOf(r), in.ID, in.Reason)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

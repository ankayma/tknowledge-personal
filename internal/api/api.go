// Package api implements the REST surface from SPEC-MVP.md §4. Base /v1,
// JSON in and out, errors as {error, detail}.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankayma/tknowledge-personal/internal/eff"
	"github.com/ankayma/tknowledge-personal/internal/flags"
	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/setup"
	"github.com/ankayma/tknowledge-personal/internal/subject"
	"github.com/ankayma/tknowledge-personal/internal/tka"
)

type Server struct {
	Repo      *ledger.Repo
	Eff       *eff.Evaluator
	Flags     *flags.Repo
	Grants    *grant.Verifier
	Templates *setup.Registry // nil = the templates embedded in the binary
	TKA       *tka.Repo       // T/A store; nil = one over Repo.Pool
	// Demo is the public door that opens a trial store. nil = no door: the
	// landing page and /v1/demo/open answer 404, which is what a customer's
	// deployment should do.
	Demo *Demo
	// OAuth is the authorization server behind /mcp (brief 2/10). nil = none:
	// discovery and /oauth/* answer 404. It needs Demo for the store terms and
	// the mailer.
	OAuth *OAuth
	// Contact is where a user reports tk misbehaving when the owner has not set
	// store.feedback_email (TK_CONTACT).
	Contact string
	// WriteTZ is the zone a seat's write month is counted in (TK_WRITE_TZ);
	// nil = tka.DefaultWriteTZ. PricingURL is where a refusal at the end of the
	// month points for a bigger seat (TK_PRICING_URL); "" = none.
	WriteTZ    *time.Location
	PricingURL string
}

func New(repo *ledger.Repo, evaluator *eff.Evaluator, flagsRepo *flags.Repo, grants *grant.Verifier) *Server {
	return &Server{Repo: repo, Eff: evaluator, Flags: flagsRepo, Grants: grants}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Get("/healthz", s.healthz)
		// The public door (V4): a page and one button. Open on purpose — there is
		// no account yet — so it is rate limited, every store it opens is new and
		// empty, and the grant it signs reaches only that store.
		r.Post("/demo/open", s.postDemoOpen)
		// Store-free reads, open before sign-in (lazy auth, owner 2/10): whether
		// a sentence is a claim yet, and what the store's words mean. Neither
		// touches a store, so neither needs a grant.
		r.Post("/tka/check-claim", s.postTkaCheckClaim)
		r.Get("/tka/definitions", s.getTkaDefinitions)
		// Writes need a grant with tk:write (SPEC §13).
		r.Group(func(r chi.Router) {
			r.Use(s.requireGrant(grant.ScopeWrite))
			r.Post("/principles", s.postPrinciples)
			r.Post("/claims", s.postClaims)
			r.Post("/claims/{claim_id}/moves", s.postMove)
			r.Post("/admit", s.postAdmit)
			r.Post("/docs", s.postDoc)
			r.Post("/interact", s.postInteract)
			r.Post("/posted", s.postPosted)
			r.Post("/setup/apply", s.postSetupApply)
			// T/A store (D-02, D-07): assume · update · admit · promote.
			r.Post("/tka/assume", s.postAssume)
			r.Post("/tka/assumptions/update", s.postUpdateAssumption)
			r.Post("/tka/admit", s.postTkaAdmit)
			r.Post("/tka/promote", s.postTkaPromote)
			r.Post("/tka/principles", s.postTkaPrinciple)
			r.Post("/tka/principles/retire", s.postTkaPrincipleRetire)
			r.Post("/tka/instructions", s.postTkaInstruction)
			r.Post("/tka/instructions/retire", s.postTkaInstructionRetire)
			r.Post("/tka/instructions/tag", s.postTkaInstructionTag)
			r.Post("/tka/demote", s.postTkaDemote)
			r.Post("/tka/retire", s.postTkaRetire)
			r.Post("/tka/recheck", s.postTkaRecheck)
			r.Post("/tka/documents", s.postTkaDocument)
			r.Post("/tka/cases", s.postTkaCase)
			r.Post("/tka/cases/tag", s.postTkaCaseTag)
			r.Post("/tka/cases/retire", s.postTkaCaseRetire)
			r.Post("/tka/attest", s.postTkaAttest)
			r.Post("/tka/evidence/withdraw", s.postTkaWithdrawEvidence)
			r.Post("/tka/flags/close", s.postTkaCloseFlag)
			r.Post("/tka/refute", s.postRefute)
			// Threads: one matter, its keywords, what it points at.
			r.Post("/tka/threads", s.postThreadOpen)
			r.Post("/tka/threads/tag", s.postThreadTag)
			r.Post("/tka/threads/link", s.postThreadLink)
			r.Post("/tka/threads/unlink", s.postThreadUnlink)
			r.Post("/tka/threads/close", s.postThreadClose)
			r.Post("/tka/threads/summarize", s.postThreadSummarize)
		})
		// The owner's knowledge reads (SPEC §14–§15) need tk:read. The claim
		// reads below stay open: cause-channel's admission reads /audit
		// without a grant.
		r.Group(func(r chi.Router) {
			r.Use(s.requireGrant(grant.ScopeRead))
			r.Get("/open", s.getOpen)
			r.Get("/history", s.getHistory)
			r.Get("/rows/{row_id}", s.getRow)
			r.Get("/find", s.getFind)
			r.Get("/docs/{doc_id}", s.getDoc)
			r.Get("/conversations/{conversation_id}", s.getConversationByID)
			r.Get("/conversation", s.getConversation)
			r.Get("/person", s.getPerson)
			r.Get("/principles", s.getPrinciples)
			r.Get("/tka/principles", s.getTkaPrinciples)
			r.Get("/tka/instructions", s.getTkaInstructions)
			r.Get("/tka/help", s.getTkaHelp)
			r.Get("/store", s.getStoreInfo)
			r.Get("/tka/entries/{id}", s.getTkaEntry)
			r.Get("/tka/entries/{id}/chain", s.getTkaChain)
			r.Get("/tka/assumptions", s.getTkaAssumptions)
			r.Get("/tka/truths", s.getTkaTruths)
			r.Get("/tka/documents", s.getTkaDocuments)
			r.Get("/tka/documents/{id}", s.getTkaDocument)
			r.Get("/tka/cases", s.getTkaCases)
			r.Get("/tka/flags", s.getTkaFlags)
			r.Get("/tka/threads/{thread}", s.getThread)
			r.Get("/tka/lookup", s.getLookup)
			r.Get("/setup/status", s.getSetupStatus)
			r.Get("/setup/templates", s.getSetupTemplates)
		})
		// Org administration (SPEC §17): tk:admin, CLI only — not through MCP.
		r.Group(func(r chi.Router) {
			r.Use(s.requireGrant(grant.ScopeAdmin))
			r.Post("/org/init", s.postOrgInit)
			r.Get("/org/config", s.getOrgConfig)
			r.Post("/org/config", s.postOrgConfig)
			r.Get("/org/namespaces", s.getOrgNamespaces)
			r.Post("/org/namespaces", s.postOrgNamespace)
			r.Get("/org/export", s.getOrgExport)
			r.Get("/org/template", s.getOrgTemplate)
			r.Get("/admin/seat", s.getSeat)
			r.Post("/admin/seat", s.postSeat)
			r.Get("/admin/catalog", s.getAdminCatalog)
			r.Post("/admin/catalog", s.postCatalog)
			r.Post("/admin/catalog/review", s.postCatalogReview)
			r.Post("/admin/catalog/withdraw", s.postCatalogWithdraw)
		})
		r.With(s.requireGrant("")).Get("/whoami", s.whoami)
		r.With(s.requireGrant(grant.ScopeAdmin)).Post("/grants/revoke", s.postRevoke)
		// The instruction catalog is public (owner 14/10/2026).
		r.Get("/catalog", s.getCatalog)
		r.Get("/claims/{claim_id}", s.getClaim)
		r.Get("/claims/{claim_id}/audit", s.getClaimAudit)
		r.Get("/flags", s.getFlags)
		r.Get("/stats", s.getStats)
	})
	// The landing page sits at the host root, outside /v1: it is the first thing
	// a stranger sees, and it needs no grant because it is how they get one.
	r.Get("/", s.getLanding)
	r.Get("/docs", s.getDocsPage)
	r.Get("/docs/{term}", s.getDocsTerm)
	r.Get("/privacy", s.getPrivacyPage)
	// OAuth for the standard door (brief 2/10): discovery at the well-known
	// paths, then authorize, token, register. Caddy routes everything but /mcp*
	// here, so these share the MCP server's host.
	r.Get("/.well-known/oauth-protected-resource", s.oauthProtectedResource)
	r.Get("/.well-known/oauth-protected-resource/mcp", s.oauthProtectedResource)
	r.Get("/.well-known/oauth-authorization-server", s.oauthServerMetadata)
	r.Get("/oauth/authorize", s.oauthAuthorize)
	r.Post("/oauth/authorize", s.oauthAuthorizePost)
	r.Post("/oauth/token", s.oauthToken)
	r.Post("/oauth/register", s.oauthRegister)
	return r
}

// --- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, errCode, detail string) {
	writeJSON(w, status, map[string]string{"error": errCode, "detail": detail})
}

func parseClaimID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "claim_id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_claim_id", "claim_id must be an integer")
		return 0, false
	}
	return id, true
}

func statusFor(move string) string {
	switch move {
	case "promote", "reapprove", "import":
		return "active"
	case "demote":
		return "demoted"
	case "refute":
		return "refuted"
	case "reject":
		return "rejected"
	case "refer":
		return "referred"
	case "propose":
		return "proposed"
	case "resolve":
		return "resolved"
	case "drop":
		return "dropped"
	default:
		return "unknown"
	}
}

// --- handlers ----------------------------------------------------------------

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Repo.Pool.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "db_unreachable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type principleReq struct {
	Code      string `json:"code"`
	Statement string `json:"statement"`
	Signer    string `json:"signer,omitempty"` // optional; must equal the grant's user_id
	CauseID   string `json:"cause_id"`
}

type claimIDResp struct {
	ClaimID int64 `json:"claim_id"`
}

// POST /principles — each row is move=import, mark=T, basis={kind:principle, refs:[], note:code}.
func (s *Server) postPrinciples(w http.ResponseWriter, r *http.Request) {
	var reqs []principleReq
	if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	g := grantFrom(r.Context())
	out := make([]claimIDResp, 0, len(reqs))
	for _, p := range reqs {
		if p.CauseID == "" {
			writeError(w, http.StatusBadRequest, "missing_cause", "principles require cause_id")
			return
		}
		signer, err := signerFor(g, p.Signer)
		if err != nil {
			writeIdentityError(w, err)
			return
		}
		producer, err := producerFor(g, "")
		if err != nil {
			writeIdentityError(w, err)
			return
		}
		causeID := p.CauseID
		id, err := s.Repo.InsertValidated(r.Context(), ledger.InsertParams{
			Statement: p.Statement,
			Mark:      "T",
			Basis:     &ledger.Basis{Kind: "principle", Refs: []int64{}, Note: p.Code},
			Signer:    &signer,
			Producer:  producer,
			Move:      "import",
			CauseID:   &causeID,
			GrantID:   &g.GrantID,
		})
		if err != nil {
			writeInsertError(w, err)
			return
		}
		out = append(out, claimIDResp{ClaimID: id})
	}
	writeJSON(w, http.StatusOK, out)
}

type claimReq struct {
	Statement string        `json:"statement"`
	Mark      string        `json:"mark"`
	Basis     *ledger.Basis `json:"basis"`
	Signer    *string       `json:"signer"`
	Producer  string        `json:"producer"`
	CauseID   *string       `json:"cause_id"`
}

// POST /claims — mark=A → move=propose; mark=T → move=import (needs signer+cause_id).
func (s *Server) postClaims(w http.ResponseWriter, r *http.Request) {
	var reqs []claimReq
	if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	g := grantFrom(r.Context())
	out := make([]claimIDResp, 0, len(reqs))
	for _, c := range reqs {
		producer, err := producerFor(g, c.Producer)
		if err != nil {
			writeIdentityError(w, err)
			return
		}
		var move string
		var signer *string
		switch c.Mark {
		case "A":
			move = "propose" // a proposal claims nothing: no signer, no cause
			if c.Signer != nil {
				writeError(w, http.StatusBadRequest, "signer_on_proposal", "mark=A is a proposal and carries no signer")
				return
			}
		case "T":
			move = "import"
			if c.Basis == nil {
				writeError(w, http.StatusBadRequest, "missing_basis", "mark=T requires basis")
				return
			}
			if c.CauseID == nil {
				writeError(w, http.StatusBadRequest, "missing_cause", "mark=T requires cause_id")
				return
			}
			u, err := signerFor(g, deref(c.Signer))
			if err != nil {
				writeIdentityError(w, err)
				return
			}
			signer = &u
		default:
			writeError(w, http.StatusBadRequest, "bad_mark", "mark must be A or T")
			return
		}

		id, err := s.Repo.InsertValidated(r.Context(), ledger.InsertParams{
			Statement: c.Statement,
			Mark:      c.Mark,
			Basis:     c.Basis,
			Signer:    signer,
			Producer:  producer,
			Move:      move,
			CauseID:   c.CauseID,
			GrantID:   &g.GrantID,
		})
		if err != nil {
			writeInsertError(w, err)
			return
		}
		out = append(out, claimIDResp{ClaimID: id})
	}
	writeJSON(w, http.StatusOK, out)
}

type moveReq struct {
	Move    string        `json:"move"`
	Signer  *string       `json:"signer"`
	CauseID *string       `json:"cause_id"`
	Reason  *string       `json:"reason"`
	Basis   *ledger.Basis `json:"basis"`
	// Producer is optional and defaults to the grant's actor. A person's own
	// grant may name another producer (an agent drafted the basis, the person
	// only signs) — without this, is_trust could never see producer != signer
	// through this endpoint. An agent's grant cannot. SPEC §4, §13.
	Producer *string `json:"producer"`
}

// validMoves is the set a *later* row may carry. `propose`
// and `import` are first-row-only moves and exist solely through POST /claims:
// letting `propose` in here would append an unsigned, cause-less `A` row on
// top of any claim (an unsigned demote in disguise), and `import` has no
// meaning on a claim that already exists.
var validMoves = map[string]bool{
	"promote": true, "demote": true, "refute": true,
	"reapprove": true, "reject": true, "refer": true,
	"resolve": true, "drop": true, // close an A-b (SPEC §14)
}

// POST /claims/{claim_id}/moves — one move = one new row.
func (s *Server) postMove(w http.ResponseWriter, r *http.Request) {
	claimID, ok := parseClaimID(w, r)
	if !ok {
		return
	}

	var req moveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !validMoves[req.Move] {
		writeError(w, http.StatusBadRequest, "bad_move", "unknown move: "+req.Move)
		return
	}

	current, found, err := s.Repo.Current(r.Context(), claimID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "claim_not_found", "no such claim_id")
		return
	}

	g := grantFrom(r.Context())
	signer, err := signerFor(g, deref(req.Signer))
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	producer, err := producerFor(g, deref(req.Producer))
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	if req.CauseID == nil {
		writeError(w, http.StatusBadRequest, "missing_cause", "this move requires cause_id")
		return
	}
	if (req.Move == "promote" || req.Move == "reapprove") && req.Basis == nil {
		writeError(w, http.StatusBadRequest, "missing_basis", "promote/reapprove require basis")
		return
	}
	if ok, ref, err := s.basisSourceExists(r.Context(), req.Basis); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	} else if !ok {
		writeError(w, http.StatusBadRequest, "basis_not_found", "basis "+ref+" names no stored document")
		return
	}

	var mark string
	switch req.Move {
	case "promote", "reapprove":
		mark = "T"
	case "demote", "reject", "refute", "resolve", "drop":
		mark = "A"
	case "refer":
		mark = current.Mark // refer hands the work on, it does not conclude
	}

	// Preconditions — 409 if the move does not fit the current state (T-03).
	// refer has no precondition.
	var badState bool
	switch req.Move {
	case "promote":
		badState = current.Mark != "A"
	case "reapprove":
		badState = current.Move != "demote"
	case "refute":
		badState = current.Mark != "T"
	case "demote":
		badState = current.Mark != "T"
	case "reject":
		badState = !(current.Mark == "A" && current.Move == "propose")
	case "resolve", "drop":
		badState = !(current.Mark == "A" && current.AB && !subject.Concluded(current.Move)) // an open A-b
	}
	if badState {
		writeError(w, http.StatusConflict, "invalid_move_for_state",
			"move "+req.Move+" is not valid from the claim's current state")
		return
	}

	// A later row copies the claim's attributes; only the move-specific fields change.
	p := ledger.InsertParams{
		ClaimID:    claimID,
		Statement:  current.Statement,
		Mark:       mark,
		Basis:      req.Basis,
		Signer:     &signer,
		Producer:   producer,
		Move:       req.Move,
		Supersedes: &current.ID,
		CauseID:    req.CauseID,
		Reason:     req.Reason,
		GrantID:    &g.GrantID,
		Subject:    current.Subject,
		Owner:      current.Owner,
		ChatID:     current.ChatID,
		AB:         current.AB,
		Scope:      current.Scope,
	}
	var id int64
	if current.Subject == nil {
		id, err = s.Repo.InsertValidated(r.Context(), p)
	} else {
		// A superseded claim is frozen: a row on it would become the subject's
		// newest and hide the claim that replaced it. Checked under the subject
		// lock so it cannot race an admit of the same subject.
		superseded := false
		err = s.Repo.WithSubjectLock(r.Context(), *current.Subject, func(tx *ledger.SubjectTx) error {
			latest, ok, err := tx.Latest(r.Context())
			if err != nil {
				return err
			}
			if ok && latest.ClaimID != claimID {
				superseded = true
				return nil
			}
			id, err = tx.Insert(r.Context(), p)
			return err
		})
		if err == nil && superseded {
			writeError(w, http.StatusConflict, "claim_superseded",
				"claim was superseded under subject "+*current.Subject+"; admit or move the newer claim")
			return
		}
	}
	if err != nil {
		writeInsertError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

func writeInsertError(w http.ResponseWriter, err error) {
	if errors.Is(err, ledger.ErrRefNotEarlier) {
		writeError(w, http.StatusBadRequest, "ref_not_earlier", "basis.refs must point at rows with a smaller id (Inv5)")
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", err.Error())
}

type claimResp struct {
	ClaimID int64        `json:"claim_id"`
	Current ledger.Row   `json:"current"`
	Status  string       `json:"status"`
	Mark    string       `json:"mark"`
	Eff     bool         `json:"eff"`
	History []ledger.Row `json:"history"`
}

// GET /claims/{claim_id}
func (s *Server) getClaim(w http.ResponseWriter, r *http.Request) {
	claimID, ok := parseClaimID(w, r)
	if !ok {
		return
	}
	current, found, err := s.Repo.Current(r.Context(), claimID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "claim_not_found", "no such claim_id")
		return
	}
	history, err := s.Repo.History(r.Context(), claimID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	effOK, err := s.Eff.Eff(r.Context(), claimID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, claimResp{
		ClaimID: claimID, Current: current, Status: statusFor(current.Move),
		Mark: current.Mark, Eff: effOK, History: history,
	})
}

type predicateResp struct {
	OK        bool   `json:"ok"`
	FailingID *int64 `json:"failing_id,omitempty"`
	Code      string `json:"code,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func toPredicateResp(r eff.Result) predicateResp {
	return predicateResp{OK: r.OK, FailingID: r.FailingID, Code: r.Code, Reason: r.Reason}
}

// GET /claims/{claim_id}/audit
func (s *Server) getClaimAudit(w http.ResponseWriter, r *http.Request) {
	claimID, ok := parseClaimID(w, r)
	if !ok {
		return
	}
	if _, found, err := s.Repo.Current(r.Context(), claimID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	} else if !found {
		writeError(w, http.StatusNotFound, "claim_not_found", "no such claim_id")
		return
	}

	truth, err := s.Eff.IsTruth(r.Context(), claimID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	trust, err := s.Eff.IsTrust(r.Context(), claimID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	traceable, err := s.Eff.IsTraceable(r.Context(), claimID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	effOK, err := s.Eff.Eff(r.Context(), claimID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	openFlags, err := s.Flags.ForClaim(r.Context(), claimID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"truth":     toPredicateResp(truth),
		"trust":     toPredicateResp(trust),
		"traceable": toPredicateResp(traceable),
		"eff":       effOK,
		"flags":     toFlagResps(openFlags),
	})
}

// flagResp is the JSON shape of a flag: the exact column names of the flags
// table, with evidence returned as raw jsonb rather than the base64 that
// []byte gets by default.
type flagResp struct {
	ID       int64           `json:"id"`
	ClaimID  int64           `json:"claim_id"`
	Kind     string          `json:"kind"`
	Evidence json.RawMessage `json:"evidence"`
	ToSigner *string         `json:"to_signer"`
	TS       time.Time       `json:"ts"`
	ClosedBy *int64          `json:"closed_by"`
}

func toFlagResps(list []flags.Flag) []flagResp {
	out := make([]flagResp, 0, len(list)) // an empty list is [], never null
	for _, f := range list {
		out = append(out, flagResp{
			ID: f.ID, ClaimID: f.ClaimID, Kind: f.Kind, Evidence: json.RawMessage(f.Evidence),
			ToSigner: f.ToSigner, TS: f.TS, ClosedBy: f.ClosedBy,
		})
	}
	return out
}

// GET /flags?open=true&to=<signer>
func (s *Server) getFlags(w http.ResponseWriter, r *http.Request) {
	openOnly := r.URL.Query().Get("open") == "true"
	toSigner := r.URL.Query().Get("to")
	list, err := s.Flags.List(r.Context(), openOnly, toSigner)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toFlagResps(list))
}

// GET /stats — {claims, t_marked, t_eff, orphans, open_flags}.
func (s *Server) getStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var claims, tMarked int64
	if err := s.Repo.Pool.QueryRow(ctx, `SELECT COUNT(DISTINCT claim_id) FROM ledger`).Scan(&claims); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	rows, err := s.Repo.Pool.Query(ctx, `
		SELECT DISTINCT ON (claim_id) claim_id, mark
		FROM ledger ORDER BY claim_id, id DESC`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var claimIDs []int64
	for rows.Next() {
		var cid int64
		var mark string
		if err := rows.Scan(&cid, &mark); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if mark == "T" {
			tMarked++
		}
		claimIDs = append(claimIDs, cid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	var tEff, orphans int64
	for _, cid := range claimIDs {
		ok, err := s.Eff.Eff(ctx, cid)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if ok {
			tEff++
		}
	}
	// orphans: claims marked T whose effective mark is A (T-ratio gap, F1).
	orphans = tMarked - tEff
	if orphans < 0 {
		orphans = 0
	}

	var openFlags int64
	if err := s.Repo.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM flags WHERE closed_by IS NULL`).Scan(&openFlags); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]int64{
		"claims": claims, "t_marked": tMarked, "t_eff": tEff, "orphans": orphans, "open_flags": openFlags,
	})
}

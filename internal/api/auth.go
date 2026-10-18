package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ankayma/tknowledge-personal/internal/grant"
)

// SPEC §13: every write carries a grant. tk-api is the only write path, so the
// grant is checked here — not in tk-mcp or the CLI, which only forward it.
// signer and producer come from the grant; a body that names someone else is
// refused, never silently overridden.

type grantKey struct{}

func grantFrom(ctx context.Context) grant.Grant {
	g, _ := ctx.Value(grantKey{}).(grant.Grant)
	return g
}

// requireGrant verifies `Authorization: Bearer <grant>`, refuses revoked
// grants, and requires scope (empty = any valid grant).
func (s *Server) requireGrant(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || strings.TrimSpace(token) == "" {
				writeError(w, http.StatusUnauthorized, "missing_grant", "Authorization: Bearer <grant> required")
				return
			}
			g, err := s.Grants.Verify(token)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "invalid_grant", err.Error())
				return
			}
			var revoked bool
			if err := s.Repo.Pool.QueryRow(r.Context(),
				`SELECT EXISTS(SELECT 1 FROM revoked_grants WHERE grant_id = $1)`, g.GrantID).Scan(&revoked); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			if revoked {
				writeError(w, http.StatusUnauthorized, "grant_revoked", "grant "+g.GrantID+" was revoked")
				return
			}
			if scope != "" && !g.Has(scope) {
				writeError(w, http.StatusForbidden, "insufficient_scope", "grant lacks scope "+scope)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), grantKey{}, g)))
		})
	}
}

var (
	errSignerMismatch   = errors.New("signer in the request is not the grant's user_id")
	errProducerMismatch = errors.New("an agent's grant produces as that agent; producer must be empty or equal actor_id")
)

// signerFor is the grant's user. A request may repeat it, never replace it.
func signerFor(g grant.Grant, requested string) (string, error) {
	if requested != "" && requested != g.UserID {
		return "", errSignerMismatch
	}
	return g.UserID, nil
}

// producerFor is the grant's actor. Only a person acting for themself may
// name a different producer — signing content an agent drafted (QUESTIONS #1);
// an agent cannot claim someone else wrote its row.
func producerFor(g grant.Grant, requested string) (string, error) {
	if requested == "" || requested == g.ActorID {
		return g.ActorID, nil
	}
	if g.IsHuman() {
		return requested, nil
	}
	return "", errProducerMismatch
}

func writeIdentityError(w http.ResponseWriter, err error) {
	code := "producer_mismatch"
	if errors.Is(err, errSignerMismatch) {
		code = "signer_mismatch"
	}
	writeError(w, http.StatusForbidden, code, err.Error())
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// GET /whoami — the verified grant, so clients (CLI, tk-mcp) learn the signer
// they will write as without parsing the token themselves.
func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, grantFrom(r.Context()))
}

type revokeReq struct {
	GrantID string `json:"grant_id"`
	Reason  string `json:"reason"`
}

// POST /grants/revoke — tk:admin. Insert-only; revoking twice is a no-op.
func (s *Server) postRevoke(w http.ResponseWriter, r *http.Request) {
	var req revokeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.GrantID) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "grant_id required")
		return
	}
	if _, err := s.Repo.Pool.Exec(r.Context(),
		`INSERT INTO revoked_grants (grant_id, reason, revoked_by) VALUES ($1, $2, $3) ON CONFLICT (grant_id) DO NOTHING`,
		req.GrantID, req.Reason, grantFrom(r.Context()).UserID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"grant_id": req.GrantID, "status": "revoked"})
}

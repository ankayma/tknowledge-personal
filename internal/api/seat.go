package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// Seats (owner 13/10/2026, spec-seat-writes-month-13-10-26.md): who writes how
// much a month in which store. tk:admin, CLI only — a person cannot raise
// their own seat through MCP.

// seatStore is the store a seat request is about: the one named, else the one
// the address opened (trial_owner keeps one store per address).
func (s *Server) seatStore(ctx context.Context, user, tenant string) (string, error) {
	if t := strings.TrimSpace(tenant); t != "" {
		return t, nil
	}
	var t string
	err := s.Repo.Pool.QueryRow(ctx, `SELECT tenant_id FROM trial_owner WHERE email = lower($1)`,
		strings.TrimSpace(user)).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", &tka.Error{Code: "TENANT_UNKNOWN", Message: user + " opened no store here",
			Next: "Name the store with tenant (tk seat <user> --tenant <id>)."}
	}
	return t, err
}

type seatView struct {
	Tenant  string           `json:"tenant"`
	User    string           `json:"user"`
	Metered bool             `json:"metered"`
	Usage   *tka.SeatUsage   `json:"usage,omitempty"`
	History []tka.SeatChange `json:"history"`
	Changed *tka.SeatChange  `json:"changed,omitempty"`
}

func (s *Server) seatView(ctx context.Context, tenant, user string) (seatView, error) {
	v := seatView{Tenant: tenant, User: user}
	u, ok, err := s.tka().SeatUsage(ctx, tenant, user)
	if err != nil {
		return v, err
	}
	if ok {
		v.Metered, v.Usage = true, &u
	}
	v.History, err = s.tka().SeatHistory(ctx, tenant, user)
	return v, err
}

// GET /admin/seat?user=…[&tenant=…] — the seat, this month's writes, the history.
func (s *Server) getSeat(w http.ResponseWriter, r *http.Request) {
	user := strings.TrimSpace(r.URL.Query().Get("user"))
	if user == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "user is required")
		return
	}
	tenant, err := s.seatStore(r.Context(), user, r.URL.Query().Get("tenant"))
	if err != nil {
		writeStore(w, err)
		return
	}
	v, err := s.seatView(r.Context(), tenant, user)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// POST /admin/seat {user, tenant?, seat, reason} — move a person to a seat.
func (s *Server) postSeat(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[struct {
		User   string `json:"user"`
		Tenant string `json:"tenant"`
		Seat   string `json:"seat"`
		Reason string `json:"reason"`
	}](w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(in.Reason) == "" {
		writeStore(w, &tka.Error{Code: "EMPTY_REASON", Message: "a seat change says why",
			Next: "Add a reason, e.g. \"paid Standard, invoice 2026-031\"."})
		return
	}
	tenant, err := s.seatStore(r.Context(), in.User, in.Tenant)
	if err != nil {
		writeStore(w, err)
		return
	}
	ch, err := s.tka().SetSeat(r.Context(), tenant, in.User, in.Seat, grantFrom(r.Context()).UserID, in.Reason)
	if err != nil {
		writeStore(w, err)
		return
	}
	v, err := s.seatView(r.Context(), tenant, ch.User)
	if err != nil {
		writeStore(w, err)
		return
	}
	v.Changed = &ch
	writeJSON(w, http.StatusOK, v)
}

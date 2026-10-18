// Package kb carries out the MCP claim functions on top of tk-api and
// cause-channel: admit, promote, close, open, history.
// tk-api decides (dedup, supersedes, refusals); kb adds what tk-api must not
// do itself: issuing the cause before a move (CJ-10, "T-knowledge calls
// no one"). Every refusal that can be known before a cause is checked first,
// so a refused call never leaves a cause without a row.
package kb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ankayma/tknowledge-personal/internal/ccclient"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/subject"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// Actor is the process name on causes issued from MCP.
const Actor = "tk-mcp"

// Error is the spec's error object: {code, message, candidates?}.
type Error struct {
	Code       string          `json:"code"`
	Message    string          `json:"message"`
	Next       string          `json:"next,omitempty"`
	Candidates json.RawMessage `json:"candidates,omitempty"`
	// Check rides along on CLAIM_FAILED: the reasons a sentence is not yet a
	// claim, so the assistant can say which words and what to write instead.
	Check json.RawMessage `json:"check,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func fail(code, msg string) *Error { return &Error{Code: code, Message: msg} }

// upstream turns a tk-api answer into a spec error, keeping its text.
func upstream(err error) error {
	var apiErr *tkclient.APIError
	if !errors.As(err, &apiErr) {
		return fail("UPSTREAM", err.Error())
	}
	switch apiErr.Code {
	case "ambiguous_person":
		return &Error{Code: "AMBIGUOUS_PERSON", Message: apiErr.Message, Candidates: apiErr.Candidates}
	case "claim_superseded":
		return fail("SUPERSEDED", apiErr.Error())
	case "invalid_move_for_state":
		return fail("MOVE_REFUSED", apiErr.Error())
	}
	// A store refusal (a thread that does not resolve, say) keeps its next move
	// and its candidates: the caller carries on from them.
	return &Error{Code: strings.ToUpper(apiErr.Code), Message: apiErr.Error(), Next: apiErr.Next,
		Candidates: apiErr.Candidates, Check: apiErr.Check}
}

type Service struct {
	CC  *ccclient.Client
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// AdmitOneInput is one claim written on its own — the ordinary case. Writing
// several claims of one turn at once is AdmitInput, a different call: batching
// is transport, not meaning.
type AdmitOneInput struct {
	Subject   string `json:"subject"`
	Statement string `json:"statement"`
	Label     string `json:"label"`
	Basis     string `json:"basis,omitempty"`
	Owner     string `json:"owner,omitempty"`
	ChatID    string `json:"chat_id,omitempty"`
}

type AdmitInput struct {
	Claims []tkclient.AdmitItem `json:"claims"`
}

type AdmitResult struct {
	Subject           string `json:"subject"`
	Outcome           string `json:"outcome"`
	ID                string `json:"id,omitempty"`
	PreviousID        string `json:"previous_id,omitempty"`
	PreviousStatement string `json:"previous_statement,omitempty"`
	Error             *Error `json:"error,omitempty"`
}

type AdmitOutput struct {
	Results []AdmitResult `json:"results"`
}

// Admit writes a turn's claims in one call. A T claim is written as its
// propose row, then promoted under the caller's grant with a cause; if that
// fails the claim stays A and the result says so (PROMOTE_FAILED) — it fails
// toward A, never toward an unjustified T.
func (s *Service) Admit(ctx context.Context, tk *tkclient.Client, in AdmitInput) (AdmitOutput, error) {
	if len(in.Claims) == 0 {
		return AdmitOutput{}, fail("BAD_REQUEST", "claims required")
	}
	results, err := tk.Admit(ctx, in.Claims)
	if err != nil {
		return AdmitOutput{}, upstream(err)
	}
	out := AdmitOutput{Results: make([]AdmitResult, 0, len(results))}
	for _, r := range results {
		out.Results = append(out.Results, s.finish(ctx, tk, r))
	}
	return out, nil
}

// AdmitOne writes a single claim.
func (s *Service) AdmitOne(ctx context.Context, tk *tkclient.Client, in AdmitOneInput) (AdmitResult, error) {
	out, err := s.Admit(ctx, tk, AdmitInput{Claims: []tkclient.AdmitItem{{
		Subject: in.Subject, Statement: in.Statement, Label: in.Label,
		Basis: in.Basis, Owner: in.Owner, ChatID: in.ChatID,
	}}})
	if err != nil {
		return AdmitResult{}, err
	}
	return out.Results[0], nil
}

// finish renders tk-api's decision for one claim and, for a T written as its
// propose row, issues the cause and promotes it.
func (s *Service) finish(ctx context.Context, tk *tkclient.Client, r tkclient.AdmitResult) AdmitResult {
	res := AdmitResult{Subject: r.Subject, Outcome: r.Outcome, ID: r.ID, PreviousID: r.PreviousID, PreviousStatement: r.PreviousStatement}
	if r.Error != nil {
		res.Error = &Error{Code: r.Error.Code, Message: r.Error.Message, Candidates: r.Error.Candidates}
	}
	if r.NeedsPromote && r.Basis != nil {
		rowID, err := s.move(ctx, tk, r.ClaimID, "promote", "admit "+r.Subject, r.Basis)
		if err != nil {
			res.Outcome = "rejected"
			res.Error = fail("PROMOTE_FAILED", "written as A ("+r.ID+"), not promoted to T: "+err.Error())
		} else {
			res.ID = subject.FormatID(rowID)
		}
	}
	return res
}

// move issues the cause under tk's grant (approver = the grant's user, a
// person; producer = its actor), then writes the move.
func (s *Service) move(ctx context.Context, tk *tkclient.Client, claimID int64, move, note string, basis *ledger.Basis) (int64, error) {
	me, err := tk.Whoami(ctx)
	if err != nil {
		return 0, err
	}
	causeID, err := s.CC.LedgerMove(ctx, ccclient.LedgerMove{
		ClaimID: claimID, Approver: me.UserID, Producer: me.ActorID, Actor: Actor, Note: move + ": " + note,
	})
	if err != nil {
		return 0, err
	}
	rowID, err := tk.Move(ctx, claimID, tkclient.MoveReq{Move: move, CauseID: causeID, Reason: note, Basis: basis})
	if err != nil {
		return 0, fmt.Errorf("%w (cause %s issued, no row written)", err, causeID)
	}
	return rowID, nil
}

func (s *Service) row(ctx context.Context, tk *tkclient.Client, claimID string) (tkclient.RowInfo, error) {
	id, err := subject.ParseID(claimID)
	if err != nil {
		return tkclient.RowInfo{}, fail("NOT_FOUND", err.Error())
	}
	info, err := tk.Row(ctx, id)
	var apiErr *tkclient.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return tkclient.RowInfo{}, fail("NOT_FOUND", "no claim "+claimID)
	}
	if err != nil {
		return tkclient.RowInfo{}, upstream(err)
	}
	if info.Superseded {
		return tkclient.RowInfo{}, fail("SUPERSEDED", claimID+" was superseded under "+info.Subject)
	}
	return info, nil
}

type PromoteInput struct {
	ClaimID string `json:"claim_id"`
	Basis   string `json:"basis"`
}

type MoveOutput struct {
	Outcome    string `json:"outcome"`
	ID         string `json:"id"`
	PreviousID string `json:"previous_id"`
}

// Promote: A → T with a basis.
func (s *Service) Promote(ctx context.Context, tk *tkclient.Client, in PromoteInput) (MoveOutput, error) {
	basis, err := subject.Basis(in.Basis, s.now())
	switch {
	case errors.Is(err, subject.ErrNoBasis):
		return MoveOutput{}, fail("T_WITHOUT_BASIS", err.Error())
	case err != nil:
		return MoveOutput{}, fail("INVALID_BASIS", err.Error())
	}
	info, err := s.row(ctx, tk, in.ClaimID)
	if err != nil {
		return MoveOutput{}, err
	}
	if info.Current.Mark == "T" {
		return MoveOutput{}, fail("ALREADY_T", info.Current.ID+" is already T")
	}
	if err := s.docExists(ctx, tk, in.Basis); err != nil {
		return MoveOutput{}, err
	}
	rowID, err := s.move(ctx, tk, info.ClaimID, "promote", "promote "+info.Current.ID, &basis)
	if err != nil {
		return MoveOutput{}, upstream(err)
	}
	return MoveOutput{Outcome: "promoted", ID: subject.FormatID(rowID), PreviousID: info.Current.ID}, nil
}

type CloseInput struct {
	ClaimID string `json:"claim_id"`
	Status  string `json:"status"` // resolved | dropped
	Note    string `json:"note"`
}

// Close ends an open A-b.
func (s *Service) Close(ctx context.Context, tk *tkclient.Client, in CloseInput) (MoveOutput, error) {
	var move string
	switch in.Status {
	case "resolved":
		move = "resolve"
	case "dropped":
		move = "drop"
	default:
		return MoveOutput{}, fail("INVALID_STATUS", "status must be resolved or dropped")
	}
	info, err := s.row(ctx, tk, in.ClaimID)
	if err != nil {
		return MoveOutput{}, err
	}
	if info.Current.Label != subject.LabelAB || subject.Concluded(info.Current.Move) {
		return MoveOutput{}, fail("NOT_A_B", info.Current.ID+" is not an open A-b ("+info.Current.Label+", "+info.Current.Status+")")
	}
	note := strings.TrimSpace(in.Note)
	if note == "" {
		note = in.Status
	}
	rowID, err := s.move(ctx, tk, info.ClaimID, move, note, nil)
	if err != nil {
		return MoveOutput{}, upstream(err)
	}
	return MoveOutput{Outcome: "closed", ID: subject.FormatID(rowID), PreviousID: info.Current.ID}, nil
}

type OpenInput struct {
	Scope  string   `json:"scope"`
	Since  string   `json:"since"`
	Labels []string `json:"labels"`
}

// Open lists what is still open.
func (s *Service) Open(ctx context.Context, tk *tkclient.Client, in OpenInput) (json.RawMessage, error) {
	raw, err := tk.Open(ctx, in.Scope, in.Since, in.Labels)
	if err != nil {
		return nil, upstream(err)
	}
	return raw, nil
}

// docExists refuses a doc_… basis naming no stored document before any cause
// is issued (tk-api checks again at the move).
func (s *Service) docExists(ctx context.Context, tk *tkclient.Client, basis string) error {
	id, ok := subject.ParseDocRef(basis)
	if !ok {
		return nil
	}
	_, err := tk.GetDoc(ctx, subject.FormatDocID(id))
	var apiErr *tkclient.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return fail("BASIS_NOT_FOUND", "basis "+strings.TrimSpace(basis)+" names no stored document")
	}
	if err != nil {
		return upstream(err)
	}
	return nil
}

type StoreInput struct {
	Title string   `json:"title"`
	Kind  string   `json:"kind"`
	Body  string   `json:"body"`
	Refs  []string `json:"refs"`
}

// Store saves a document; same title → new version. A rejected
// store is returned as the spec error.
func (s *Service) Store(ctx context.Context, tk *tkclient.Client, in StoreInput) (tkclient.StoreResult, error) {
	res, err := tk.Store(ctx, tkclient.StoreReq{Title: in.Title, Kind: in.Kind, Body: in.Body, Refs: in.Refs})
	if err != nil {
		return tkclient.StoreResult{}, upstream(err)
	}
	if res.Outcome == "rejected" && res.Error != nil {
		return res, fail(res.Error.Code, res.Error.Message)
	}
	return res, nil
}

type FindInput struct {
	Query      string `json:"query"`
	Kind       string `json:"kind"`
	Person     string `json:"person"`
	Since      string `json:"since"`
	LatestOnly bool   `json:"latest_only"`
}

// Find lists documents (metadata only).
func (s *Service) Find(ctx context.Context, tk *tkclient.Client, in FindInput) (json.RawMessage, error) {
	raw, err := tk.Find(ctx, tkclient.FindQuery{Query: in.Query, Kind: in.Kind, Person: in.Person, Since: in.Since, LatestOnly: in.LatestOnly})
	if err != nil {
		return nil, upstream(err)
	}
	return raw, nil
}

type ReadInput struct {
	DocID string `json:"doc_id"`
}

// Read returns a document in full.
func (s *Service) Read(ctx context.Context, tk *tkclient.Client, in ReadInput) (json.RawMessage, error) {
	raw, err := tk.GetDoc(ctx, in.DocID)
	var apiErr *tkclient.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusBadRequest) {
		return nil, fail("NOT_FOUND", "no document "+in.DocID)
	}
	if err != nil {
		return nil, upstream(err)
	}
	return raw, nil
}

type HistoryInput struct {
	Subject string `json:"subject"`
}

// History is a subject's version chain.
func (s *Service) History(ctx context.Context, tk *tkclient.Client, in HistoryInput) (json.RawMessage, error) {
	raw, err := tk.History(ctx, in.Subject)
	if err != nil {
		return nil, upstream(err)
	}
	return raw, nil
}

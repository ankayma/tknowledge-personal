// Package tkclient is a thin HTTP client for tk-api. It is shared by
// tk-mcp and the CLI `tk`. It never talks to the
// database: every write still goes through the REST gateway, the only write
// path into `ledger`. The caller's grant rides as a bearer token.
package tkclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
)

type Client struct {
	BaseURL string // e.g. http://localhost:8081 — /v1 is appended here
	HTTP    *http.Client
	Token   string // grant sent as Authorization: Bearer; empty = none
}

func New(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: http.DefaultClient}
}

// WithToken returns a copy that sends token as the caller's grant.
func (c *Client) WithToken(token string) *Client {
	cp := *c
	cp.Token = token
	return &cp
}

// APIError is a non-2xx answer from tk-api, carrying its {error, detail} body.
type APIError struct {
	Status     int
	Code       string          `json:"error"`
	Message    string          `json:"detail"`
	Next       string          `json:"-"`                    // the move still open, from the T/A store
	Check      json.RawMessage `json:"-"`                    // CLAIM_FAILED: why the sentence is not a claim
	Candidates json.RawMessage `json:"candidates,omitempty"` // ambiguous_person
}

func (e *APIError) Error() string {
	return fmt.Sprintf("tk-api %d %s: %s", e.Status, e.Code, e.Message)
}

// ClaimReq mirrors the POST /claims element. Signer and producer
// default to the grant's user and actor.
type ClaimReq struct {
	Statement string        `json:"statement"`
	Mark      string        `json:"mark"`
	Basis     *ledger.Basis `json:"basis,omitempty"`
	Signer    *string       `json:"signer,omitempty"`
	Producer  string        `json:"producer,omitempty"`
	CauseID   *string       `json:"cause_id,omitempty"`
}

// ProposeClaim posts one mark=A claim (→ move=propose) and returns its
// claim_id. producer is optional: only a person's own grant may name one.
func (c *Client) ProposeClaim(ctx context.Context, statement string, basis *ledger.Basis, producer string) (int64, error) {
	body, err := c.do(ctx, http.MethodPost, "/v1/claims", []ClaimReq{{
		Statement: statement, Mark: "A", Basis: basis, Producer: producer,
	}})
	if err != nil {
		return 0, err
	}
	var out []struct {
		ClaimID int64 `json:"claim_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("decode /claims response: %w", err)
	}
	if len(out) != 1 {
		return 0, fmt.Errorf("expected 1 claim_id, got %d", len(out))
	}
	return out[0].ClaimID, nil
}

// MoveReq mirrors the POST /claims/{id}/moves body.
type MoveReq struct {
	Move     string        `json:"move"`
	Signer   string        `json:"signer,omitempty"`
	CauseID  string        `json:"cause_id"`
	Reason   string        `json:"reason,omitempty"`
	Basis    *ledger.Basis `json:"basis,omitempty"`
	Producer string        `json:"producer,omitempty"`
}

// Move posts one move and returns the new row id.
func (c *Client) Move(ctx context.Context, claimID int64, req MoveReq) (int64, error) {
	body, err := c.do(ctx, http.MethodPost, "/v1/claims/"+strconv.FormatInt(claimID, 10)+"/moves", req)
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("decode move response: %w", err)
	}
	return out.ID, nil
}

// Whoami returns the grant tk-api verified for this client's token.
func (c *Client) Whoami(ctx context.Context) (grant.Grant, error) {
	body, err := c.do(ctx, http.MethodGet, "/v1/whoami", nil)
	if err != nil {
		return grant.Grant{}, err
	}
	var g grant.Grant
	if err := json.Unmarshal(body, &g); err != nil {
		return grant.Grant{}, fmt.Errorf("decode whoami: %w", err)
	}
	return g, nil
}

// RevokeGrant revokes grantID (needs a tk:admin grant).
func (c *Client) RevokeGrant(ctx context.Context, grantID, reason string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/grants/revoke", map[string]string{"grant_id": grantID, "reason": reason})
	return err
}

// AdmitItem is one claim of POST /admit.
type AdmitItem struct {
	Subject   string `json:"subject"`
	Statement string `json:"statement"`
	Label     string `json:"label"`
	Basis     string `json:"basis,omitempty"`
	Owner     string `json:"owner,omitempty"`
	ChatID    string `json:"chat_id,omitempty"`
}

// ItemError is a per-claim refusal.
type ItemError struct {
	Code       string          `json:"code"`
	Message    string          `json:"message"`
	Candidates json.RawMessage `json:"candidates,omitempty"`
}

// AdmitResult is tk-api's decision for one AdmitItem. NeedsPromote means a T
// claim was written as its propose row and waits for cause + promote.
type AdmitResult struct {
	Subject           string        `json:"subject"`
	Outcome           string        `json:"outcome"`
	ID                string        `json:"id,omitempty"`
	ClaimID           int64         `json:"claim_id,omitempty"`
	Label             string        `json:"label,omitempty"`
	PreviousID        string        `json:"previous_id,omitempty"`
	PreviousStatement string        `json:"previous_statement,omitempty"`
	NeedsPromote      bool          `json:"needs_promote,omitempty"`
	Basis             *ledger.Basis `json:"basis,omitempty"`
	Error             *ItemError    `json:"error,omitempty"`
}

func (c *Client) Admit(ctx context.Context, items []AdmitItem) ([]AdmitResult, error) {
	body, err := c.do(ctx, http.MethodPost, "/v1/admit", map[string]any{"claims": items})
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []AdmitResult `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode /admit: %w", err)
	}
	return out.Results, nil
}

// RowInfo is GET /rows/{id}: a row, its claim's current row, and whether the
// claim has been superseded under its subject.
type RowInfo struct {
	ID         string `json:"id"`
	RowID      int64  `json:"row_id"`
	ClaimID    int64  `json:"claim_id"`
	Subject    string `json:"subject"`
	Statement  string `json:"statement"`
	Label      string `json:"label"`
	Superseded bool   `json:"superseded"`
	Current    struct {
		ID     string `json:"id"`
		RowID  int64  `json:"row_id"`
		Label  string `json:"label"`
		Move   string `json:"move"`
		Mark   string `json:"mark"`
		Status string `json:"status"`
	} `json:"current"`
}

func (c *Client) Row(ctx context.Context, rowID int64) (RowInfo, error) {
	body, err := c.do(ctx, http.MethodGet, "/v1/rows/"+strconv.FormatInt(rowID, 10), nil)
	if err != nil {
		return RowInfo{}, err
	}
	var info RowInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return RowInfo{}, fmt.Errorf("decode /rows: %w", err)
	}
	return info, nil
}

// Open returns GET /open as raw JSON.
func (c *Client) Open(ctx context.Context, scope, since string, labels []string) (json.RawMessage, error) {
	q := url.Values{}
	if scope != "" {
		q.Set("scope", scope)
	}
	if since != "" {
		q.Set("since", since)
	}
	if len(labels) > 0 {
		q.Set("labels", strings.Join(labels, ","))
	}
	path := "/v1/open"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

// History returns GET /history?subject= as raw JSON.
func (c *Client) History(ctx context.Context, subject string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/history?"+url.Values{"subject": {subject}}.Encode(), nil)
}

// StoreReq is POST /docs.
type StoreReq struct {
	Title string   `json:"title"`
	Kind  string   `json:"kind"`
	Body  string   `json:"body"`
	Refs  []string `json:"refs,omitempty"`
}

type StoreResult struct {
	Outcome    string     `json:"outcome"`
	DocID      string     `json:"doc_id,omitempty"`
	Version    int        `json:"version,omitempty"`
	PreviousID string     `json:"previous_id,omitempty"`
	Error      *ItemError `json:"error,omitempty"`
}

func (c *Client) Store(ctx context.Context, req StoreReq) (StoreResult, error) {
	body, err := c.do(ctx, http.MethodPost, "/v1/docs", req)
	if err != nil {
		return StoreResult{}, err
	}
	var out StoreResult
	if err := json.Unmarshal(body, &out); err != nil {
		return StoreResult{}, fmt.Errorf("decode /docs: %w", err)
	}
	return out, nil
}

// FindQuery is GET /find.
type FindQuery struct {
	Query      string
	Kind       string
	Person     string
	Since      string
	LatestOnly bool
}

func (c *Client) Find(ctx context.Context, fq FindQuery) (json.RawMessage, error) {
	q := url.Values{}
	for k, v := range map[string]string{"query": fq.Query, "kind": fq.Kind, "person": fq.Person, "since": fq.Since} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if fq.LatestOnly {
		q.Set("latest_only", "true")
	}
	path := "/v1/find"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

// GetDoc returns GET /docs/{doc_id} as raw JSON.
func (c *Client) GetDoc(ctx context.Context, docID string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/docs/"+url.PathEscape(strings.TrimSpace(docID)), nil)
}

// ConversationKey names a conversation: url for post_thread, person for
// dm/email/connect. (A conversation was called a thread until 1/10/2026.)
type ConversationKey struct {
	Kind   string `json:"kind"`
	URL    string `json:"url,omitempty"`
	Person string `json:"person,omitempty"`
	Owner  string `json:"owner,omitempty"`
}

// EventReq is one turn of POST /interact.
type EventReq struct {
	Actor     string `json:"actor"`
	Type      string `json:"type"`
	Body      string `json:"body,omitempty"`
	Verbatim  bool   `json:"verbatim,omitempty"`
	At        string `json:"at,omitempty"`
	InReplyTo string `json:"in_reply_to,omitempty"`
	ChatID    string `json:"chat_id,omitempty"`
}

type EventResult struct {
	EvtID   string `json:"evt_id"`
	Outcome string `json:"outcome"`
	Actor   string `json:"actor"`
	Type    string `json:"type"`
	At      string `json:"at"`
}

type InteractResult struct {
	ConversationID string          `json:"conversation_id"`
	EvtID          string          `json:"evt_id"`
	Events         []EventResult   `json:"events"`
	MyTouches      int             `json:"my_touches_since_their_last"`
	Thread         json.RawMessage `json:"thread,omitempty"`
}

// Interact records turns in a conversation; thread, when given, links the
// conversation to that thread (TH-… or a keyword) in the same transaction.
func (c *Client) Interact(ctx context.Context, conversation ConversationKey, events []EventReq, thread string) (InteractResult, error) {
	req := map[string]any{"conversation": conversation, "events": events}
	if strings.TrimSpace(thread) != "" {
		req["thread"] = thread
	}
	body, err := c.do(ctx, http.MethodPost, "/v1/interact", req)
	if err != nil {
		return InteractResult{}, err
	}
	var out InteractResult
	if err := json.Unmarshal(body, &out); err != nil {
		return InteractResult{}, fmt.Errorf("decode /interact: %w", err)
	}
	return out, nil
}

type PostedReq struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	URL      string `json:"url"`
	PostedAt string `json:"posted_at,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
}

type PostedResult struct {
	DocID          string      `json:"doc_id"`
	DocOutcome     string      `json:"doc_outcome"`
	ConversationID string      `json:"conversation_id"`
	Subject        string      `json:"subject"`
	Claim          AdmitResult `json:"claim"`
}

func (c *Client) Posted(ctx context.Context, req PostedReq) (PostedResult, error) {
	body, err := c.do(ctx, http.MethodPost, "/v1/posted", req)
	if err != nil {
		return PostedResult{}, err
	}
	var out PostedResult
	if err := json.Unmarshal(body, &out); err != nil {
		return PostedResult{}, fmt.Errorf("decode /posted: %w", err)
	}
	return out, nil
}

// Conversation returns a conversation with its turns: by id,
// by url, or by kind + person.
func (c *Client) Conversation(ctx context.Context, conversationID, postURL, kind, person string) (json.RawMessage, error) {
	if strings.TrimSpace(conversationID) != "" {
		return c.do(ctx, http.MethodGet, "/v1/conversations/"+url.PathEscape(strings.TrimSpace(conversationID)), nil)
	}
	q := url.Values{}
	for k, v := range map[string]string{"url": postURL, "kind": kind, "person": person} {
		if strings.TrimSpace(v) != "" {
			q.Set(k, v)
		}
	}
	return c.do(ctx, http.MethodGet, "/v1/conversation?"+q.Encode(), nil)
}

// Person returns everything known about one person.
func (c *Client) Person(ctx context.Context, query string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/person?"+url.Values{"query": {query}}.Encode(), nil)
}

// SetupStatus is GET /setup/status.
func (c *Client) SetupStatus(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/setup/status", nil)
}

// SetupTemplates is GET /setup/templates.
func (c *Client) SetupTemplates(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/setup/templates", nil)
}

type SetupApplyResult struct {
	Docs            []string      `json:"docs"`
	NamespacesAdded []string      `json:"namespaces_added"`
	Claims          []AdmitResult `json:"claims"`
	Missing         []string      `json:"missing"`
}

// SetupApply is POST /setup/apply; adoption claims wait for cause + promote.
func (c *Client) SetupApply(ctx context.Context, templateID, owner string) (SetupApplyResult, error) {
	body, err := c.do(ctx, http.MethodPost, "/v1/setup/apply", map[string]any{
		"template_id": templateID, "overrides": map[string]string{"owner": owner},
	})
	if err != nil {
		return SetupApplyResult{}, err
	}
	var out SetupApplyResult
	if err := json.Unmarshal(body, &out); err != nil {
		return SetupApplyResult{}, fmt.Errorf("decode /setup/apply: %w", err)
	}
	return out, nil
}

// Principles is GET /principles: the Principle List as it stands.
func (c *Client) Principles(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/principles", nil)
}

// The T/A store (spec 19/9): tk-mcp forwards, tk-api decides.
func (c *Client) TkaAssume(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/assume", in)
}

func (c *Client) TkaUpdateAssumption(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/assumptions/update", in)
}

func (c *Client) TkaAdmit(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/admit", in)
}

func (c *Client) TkaPromote(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/promote", in)
}

func (c *Client) TkaPrincipleAdd(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/principles", in)
}

func (c *Client) TkaPrincipleRetire(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/principles/retire", in)
}

func (c *Client) TkaPrinciples(ctx context.Context, includeRetired bool) (json.RawMessage, error) {
	q := ""
	if includeRetired {
		q = "?include_retired=true"
	}
	return c.do(ctx, http.MethodGet, "/v1/tka/principles"+q, nil)
}

func (c *Client) TkaInstructionAdd(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/instructions", in)
}

func (c *Client) TkaInstructionRetire(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/instructions/retire", in)
}

// TkaInstructions is the active set; history adds the retired ones.
func (c *Client) TkaInstructions(ctx context.Context, history bool) (json.RawMessage, error) {
	q := ""
	if history {
		q = "?history=true"
	}
	return c.do(ctx, http.MethodGet, "/v1/tka/instructions"+q, nil)
}

// TkaInstructionIndex is what the MCP tool returns: meta lines in full and an
// index of the rest with their tags.
func (c *Client) TkaInstructionIndex(ctx context.Context, history bool) (json.RawMessage, error) {
	q := "?view=index"
	if history {
		q += "&history=true"
	}
	return c.do(ctx, http.MethodGet, "/v1/tka/instructions"+q, nil)
}

// TkaHelp is the instructions that apply to one situation, in full.
func (c *Client) TkaHelp(ctx context.Context, situation string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/tka/help?situation="+url.QueryEscape(situation), nil)
}

// TkaInstructionTag sets one instruction's tags.
func (c *Client) TkaInstructionTag(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/instructions/tag", in)
}

// Catalog reads the instruction catalog: the public read, or with admin the
// maintainer's (proposed and rejected entries, review notes).
func (c *Client) Catalog(ctx context.Context, status, situation string, admin bool) (json.RawMessage, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if situation != "" {
		q.Set("situation", situation)
	}
	path := "/v1/catalog"
	if admin {
		path = "/v1/admin/catalog"
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

// CatalogAdd proposes one catalog entry (tk:admin).
func (c *Client) CatalogAdd(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/admin/catalog", in)
}

// CatalogReview approves or rejects a proposed entry (tk:admin).
func (c *Client) CatalogReview(ctx context.Context, id, decision, note, source, incidentOn string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/admin/catalog/review", map[string]string{"id": id, "decision": decision, "note": note,
		"source": source, "incident_on": incidentOn})
}

// CatalogWithdraw takes a current entry out of the catalog (tk:admin).
func (c *Client) CatalogWithdraw(ctx context.Context, id, reason string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/admin/catalog/withdraw", map[string]string{"id": id, "reason": reason})
}

// StoreInfo is GET /store: maintainer, feedback address, build.
func (c *Client) StoreInfo(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/store", nil)
}

func (c *Client) TkaEntry(ctx context.Context, id string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/tka/entries/"+url.PathEscape(id), nil)
}

func (c *Client) TkaChain(ctx context.Context, id string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/tka/entries/"+url.PathEscape(id)+"/chain", nil)
}

func (c *Client) TkaAssumptions(ctx context.Context, kind string) (json.RawMessage, error) {
	q := ""
	if kind != "" {
		q = "?type=" + url.QueryEscape(kind)
	}
	return c.do(ctx, http.MethodGet, "/v1/tka/assumptions"+q, nil)
}

func (c *Client) TkaTruths(ctx context.Context, standing bool, limit int) (json.RawMessage, error) {
	q := url.Values{}
	if standing {
		q.Set("standing", "true")
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/tka/truths"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *Client) TkaDemote(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/demote", in)
}

func (c *Client) TkaRetire(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/retire", in)
}

func (c *Client) TkaRecheck(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/recheck", in)
}

// TkaDocumentAdd stores one piece of evidence: its text, how it was looked at,
// and where the original lives (spec T–D §3). tk keeps no files.
func (c *Client) TkaDocumentAdd(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/documents", in)
}

// TkaCaseAdd keeps a copy of a lesson stored as a document as a Case.
func (c *Client) TkaCaseAdd(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/cases", in)
}

// TkaCaseTag replaces a Case's kind and tags with a new line.
func (c *Client) TkaCaseTag(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/cases/tag", in)
}

// TkaCaseRetire takes a Case out of cases() with a reason.
func (c *Client) TkaCaseRetire(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/cases/retire", in)
}

// TkaCases lists Cases filtered by the given query values.
func (c *Client) TkaCases(ctx context.Context, q url.Values) (json.RawMessage, error) {
	path := "/v1/tka/cases"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

// TkaAttest attaches a document the caller added to a claim.
func (c *Client) TkaAttest(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/attest", in)
}

// TkaWithdrawEvidence takes a document back from a claim, with the reason.
func (c *Client) TkaWithdrawEvidence(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/evidence/withdraw", in)
}

// TkaFlags lists the store's flags: open (default), closed or all.
func (c *Client) TkaFlags(ctx context.Context, status string) (json.RawMessage, error) {
	path := "/v1/tka/flags"
	if s := strings.TrimSpace(status); s != "" {
		path += "?" + url.Values{"status": {s}}.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

// TkaCloseFlag answers a flag with what was found.
func (c *Client) TkaCloseFlag(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/flags/close", in)
}

// Threads (spec-thread-conversation-01-10-26.md).

func (c *Client) TkaThreadOpen(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/threads", in)
}

func (c *Client) TkaThreadTag(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/threads/tag", in)
}

func (c *Client) TkaLink(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/threads/link", in)
}

func (c *Client) TkaUnlink(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/threads/unlink", in)
}

func (c *Client) TkaThreadSummarize(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/threads/summarize", in)
}

func (c *Client) TkaThreadClose(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/threads/close", in)
}

// TkaThread reads one thread: TH-… or a keyword naming exactly one.
// summaries "all" adds the whole summary chain; "" leaves only the current one.
func (c *Client) TkaThread(ctx context.Context, ref string, depth int, summaries string) (json.RawMessage, error) {
	path := "/v1/tka/threads/" + url.PathEscape(strings.TrimSpace(ref))
	v := url.Values{}
	if depth > 0 {
		v.Set("depth", strconv.Itoa(depth))
	}
	if summaries != "" {
		v.Set("summaries", summaries)
	}
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

// TkaLookup searches what the person typed; scope "summaries" returns only
// current summaries, "" every block.
func (c *Client) TkaLookup(ctx context.Context, q, scope string) (json.RawMessage, error) {
	v := url.Values{"q": {q}}
	if scope != "" {
		v.Set("scope", scope)
	}
	return c.do(ctx, http.MethodGet, "/v1/tka/lookup?"+v.Encode(), nil)
}

func (c *Client) TkaRefute(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/refute", in)
}

func (c *Client) TkaCheckClaim(ctx context.Context, in any) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/tka/check-claim", in)
}

func (c *Client) TkaDefinitions(ctx context.Context, term, lang string) (json.RawMessage, error) {
	q := url.Values{}
	if term != "" {
		q.Set("term", term)
	}
	if lang != "" {
		q.Set("lang", lang)
	}
	path := "/v1/tka/definitions"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *Client) TkaDocuments(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/tka/documents", nil)
}

// OrgInit is POST /org/init (tk:admin).
func (c *Client) OrgInit(ctx context.Context, name, licenseKey string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/org/init", map[string]string{"name": name, "license_key": licenseKey})
}

// OrgConfig is GET /org/config.
func (c *Client) OrgConfig(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/org/config", nil)
}

// OrgConfigSet is POST /org/config.
func (c *Client) OrgConfigSet(ctx context.Context, key string, value json.RawMessage) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/org/config", map[string]any{"key": key, "value": value})
}

// Seat is GET /admin/seat: a person's seat, this month's writes, the history.
func (c *Client) Seat(ctx context.Context, user, tenant string) (json.RawMessage, error) {
	q := url.Values{"user": {user}}
	if tenant != "" {
		q.Set("tenant", tenant)
	}
	return c.do(ctx, http.MethodGet, "/v1/admin/seat?"+q.Encode(), nil)
}

// SetSeat is POST /admin/seat: move a person to free, standard or unlimited.
func (c *Client) SetSeat(ctx context.Context, user, tenant, seat, reason string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/admin/seat",
		map[string]any{"user": user, "tenant": tenant, "seat": seat, "reason": reason})
}

// OrgNamespaces is GET /org/namespaces.
func (c *Client) OrgNamespaces(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/org/namespaces", nil)
}

// OrgNamespaceAdd is POST /org/namespaces.
func (c *Client) OrgNamespaceAdd(ctx context.Context, name, description string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/org/namespaces", map[string]string{"name": name, "description": description})
}

// OrgTemplate is GET /org/template: the store's namespaces and rule documents
// as a template {id, version, files}.
func (c *Client) OrgTemplate(ctx context.Context, id, name, description string) (json.RawMessage, error) {
	q := url.Values{"id": {id}, "name": {name}, "description": {description}}
	return c.do(ctx, http.MethodGet, "/v1/org/template?"+q.Encode(), nil)
}

// OrgExport is GET /org/export.
func (c *Client) OrgExport(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/org/export", nil)
}

// GetClaim returns GET /claims/{id} as raw JSON (read-only pass-through).
func (c *Client) GetClaim(ctx context.Context, id int64) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/claims/"+strconv.FormatInt(id, 10), nil)
}

// GetAudit returns GET /claims/{id}/audit as raw JSON.
func (c *Client) GetAudit(ctx context.Context, id int64) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/claims/"+strconv.FormatInt(id, 10)+"/audit", nil)
}

// GetFlags returns GET /flags[?open=true][&to=signer] as raw JSON.
func (c *Client) GetFlags(ctx context.Context, openOnly bool, toSigner string) (json.RawMessage, error) {
	q := url.Values{}
	if openOnly {
		q.Set("open", "true")
	}
	if toSigner != "" {
		q.Set("to", toSigner)
	}
	path := "/v1/flags"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

// GetStats returns GET /stats as raw JSON.
func (c *Client) GetStats(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/stats", nil)
}

func (c *Client) do(ctx context.Context, method, path string, in any) (json.RawMessage, error) {
	if in == nil {
		return c.doRaw(ctx, method, path, "", nil)
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	return c.doRaw(ctx, method, path, "application/json", b)
}

// doRaw sends the body as it is, so a document crosses the wire as itself.
func (c *Client) doRaw(ctx context.Context, method, path, mediaType string, body []byte) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		apiErr := &APIError{Status: resp.StatusCode}
		// The T/A store answers {"error": {code, message, next}}; the older
		// endpoints answer {"error": "<code>", "detail": "…"}. Read both.
		var wrapped struct {
			Error struct {
				Code       string          `json:"code"`
				Message    string          `json:"message"`
				Next       string          `json:"next"`
				Check      json.RawMessage `json:"check"`
				Candidates json.RawMessage `json:"candidates"`
			} `json:"error"`
		}
		switch {
		case json.Unmarshal(body, &wrapped) == nil && wrapped.Error.Code != "":
			apiErr.Code, apiErr.Message, apiErr.Next = wrapped.Error.Code, wrapped.Error.Message, wrapped.Error.Next
			apiErr.Check, apiErr.Candidates = wrapped.Error.Check, wrapped.Error.Candidates
		case json.Unmarshal(body, apiErr) == nil && apiErr.Code != "":
		default:
			apiErr.Code = "http_" + strconv.Itoa(resp.StatusCode)
			apiErr.Message = strings.TrimSpace(string(body))
		}
		return nil, apiErr
	}
	return json.RawMessage(body), nil
}

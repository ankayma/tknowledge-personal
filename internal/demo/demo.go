// Package demo walks the store the way a stranger's assistant would: ten
// turns, each asserted. It exists so the store is known good before a person is
// asked to try it — and it speaks the MCP wire, not the Go API, because that is
// the surface the stranger actually touches.
//
// Two acts. In the first the store refuses to let a guess become a fact, and
// refuses again until the evidence is something a stranger could open. In the
// second a principle is withdrawn and the store says what no longer stands.
package demo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Options say where to run and who as.
type Options struct {
	BaseURL string    // http://localhost:8083 or https://tk.example.com
	Grant   string    // the caller's grant; goes in the path
	Out     io.Writer // step-by-step log; nil = silent
}

// Client is one MCP conversation over Streamable HTTP.
type Client struct {
	url  string
	http *http.Client
	id   int
}

func newClient(o Options) *Client {
	base := strings.TrimRight(o.BaseURL, "/")
	return &Client{url: base + "/mcp/t/" + o.Grant, http: &http.Client{}}
}

// call sends one JSON-RPC request and returns the tool's JSON payload. A
// refusal is not an error here: it comes back as the result, because refusals
// are what this scenario is mostly about.
func (c *Client) call(ctx context.Context, tool string, args map[string]any) (map[string]any, error) {
	c.id++
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": c.id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d: %s", tool, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	payload := lastDataLine(raw)

	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("%s: %w — %s", tool, err, payload)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("%s: %s", tool, env.Error.Message)
	}
	if len(env.Result.Content) == 0 {
		return nil, fmt.Errorf("%s: empty result", tool)
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(env.Result.Content[0].Text), &out); err != nil {
		return nil, fmt.Errorf("%s: %w — %s", tool, err, env.Result.Content[0].Text)
	}
	return out, nil
}

// lastDataLine unwraps a Streamable HTTP event, or returns the body as it came.
func lastDataLine(raw []byte) []byte {
	if !bytes.Contains(raw, []byte("data:")) {
		return raw
	}
	var last []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			last = bytes.TrimSpace(after)
		}
	}
	return last
}

// Run walks the scenario. The first failure stops it and is returned.
func Run(ctx context.Context, o Options) error {
	c := newClient(o)
	s := &scenario{c: c, out: o.Out}

	if err := s.act1(ctx); err != nil {
		return err
	}
	return s.act2(ctx)
}

type scenario struct {
	c   *Client
	out io.Writer
}

func (s *scenario) step(n int, said, note string) {
	if s.out == nil {
		return
	}
	fmt.Fprintf(s.out, "\n%2d. %s\n    %s\n", n, said, note)
}

// act1: a guess stays a guess until something settles it.
func (s *scenario) act1(ctx context.Context) error {
	claim := "Acme renews the contract before 2026-12-31"

	s.step(1, `"I think Acme will renew this quarter — not confirmed."`,
		"assume, kind A-p, with what would settle it. The wording is written to hold: \"before 2026-12-31\", "+
			"not \"this quarter\", because promote will carry it into the ledger unchanged")
	out, err := s.c.call(ctx, "assume", map[string]any{
		"claim": claim, "type": "A-p", "source": "owner",
		"if": "a signed renewal arrives", "not": "2026-12-31 passes with no signature",
	})
	if err != nil {
		return err
	}
	assumptionID, _ := out["id"].(string)
	if assumptionID == "" || out["status"] != "open" {
		return fmt.Errorf("step 1: expected an open assumption, got %v", out)
	}
	s.note("wrote %s, still open", assumptionID)

	s.step(2, `"Actually just record it as a fact."`, "admit refuses: the claim is already held as a guess")
	out, err = s.c.call(ctx, "admit", map[string]any{"claim": claim, "basis": "fact", "source": "gut feeling"})
	if err != nil {
		return err
	}
	if code, next := refusal(out); code != "USE_PROMOTE" || !strings.Contains(next, assumptionID) {
		return fmt.Errorf("step 2: expected USE_PROMOTE naming %s, got %v", assumptionID, out)
	}
	s.note("refused USE_PROMOTE, and said which assumption to raise")

	s.step(3, `"Record: Acme will renew this quarter."`, "near-identical wording — nothing is written")
	out, err = s.c.call(ctx, "assume", map[string]any{
		"claim": "Acme renews the contract before 2026-12-31 for sure", "type": "A-p", "source": "owner",
		"if": "a signed renewal arrives", "not": "2026-12-31 passes with no signature",
	})
	if err != nil {
		return err
	}
	similar, _ := out["similar"].([]any)
	if out["id"] != nil || len(similar) == 0 {
		return fmt.Errorf("step 3: expected nothing written and a similar list, got %v", out)
	}
	s.note("wrote nothing, returned %d neighbour(s) instead", len(similar))

	s.step(4, `"They signed — I saw the renewal email."`, "a fact needs evidence someone else can open")
	out, err = s.c.call(ctx, "promote", map[string]any{
		"assumption_id": assumptionID, "basis": "fact",
		"source": "renewal email from Acme", "source_date": "2026-10-02",
	})
	if err != nil {
		return err
	}
	if code, next := refusal(out); code != "SOURCE_NOT_RETRIEVABLE" || !strings.Contains(next, "document_add") {
		return fmt.Errorf("step 4: expected SOURCE_NOT_RETRIEVABLE naming document_add, got %v", out)
	}
	s.note("refused SOURCE_NOT_RETRIEVABLE — the writer's own word does not settle a fact")

	s.step(5, `"Here is the email itself."`, "document_add records what the email says and where it lives, then promote cites it")
	email := "From: contracts@acme.example\nDate: 2026-10-02\nSubject: Renewal signed\n\nCountersigned copy attached.\n"
	sum := sha256.Sum256([]byte(email))
	out, err = s.c.call(ctx, "document_add", map[string]any{
		"text": email, "text_producer": "owner", "method": "opened_source",
		"source_uri": "file:///mail/acme-renewal-2026-10-02.eml", "sha256": hex.EncodeToString(sum[:]),
		"filename": "acme-renewal-2026-10-02.eml", "media_type": "message/rfc822",
	})
	if err != nil {
		return err
	}
	docID, _ := out["id"].(string)
	if docID == "" || out["bound"] != true {
		return fmt.Errorf("step 5: expected a document bound to its original, got %v", out)
	}
	s.note("recorded %s: the text, how it was read, and the sha256 of the original that stays in the mailbox", docID)

	out, err = s.c.call(ctx, "promote", map[string]any{
		"assumption_id": assumptionID, "basis": "fact",
		"where": "Subject line: Renewal signed", "source_doc": docID, "source_date": "2026-10-02",
	})
	if err != nil {
		return err
	}
	truthID, _ := out["id"].(string)
	if truthID == "" || out["claim"] != claim || out["promoted_from"] != assumptionID {
		return fmt.Errorf("step 5: expected %s promoted to a T with the same wording, got %v", assumptionID, out)
	}
	s.note("%s → %s, wording unchanged", assumptionID, truthID)

	s.step(6, `"Show me where that came from."`, "entry: the record and its whole history")
	out, err = s.c.call(ctx, "entry", map[string]any{"id": truthID})
	if err != nil {
		return err
	}
	truth, _ := out["truth"].(map[string]any)
	if truth == nil || truth["source"] == nil {
		return fmt.Errorf("step 6: expected the stored record, got %v", out)
	}
	history, _ := truth["history"].([]any)
	if len(history) == 0 {
		return fmt.Errorf("step 6: expected history on %s", truthID)
	}
	s.note("read %s: source, verified_at, and %d history line(s)", truthID, len(history))
	return nil
}

// act2: when a principle is withdrawn, the store says what stops standing.
func (s *scenario) act2(ctx context.Context) error {
	s.step(7, `"We only trust contract data from the CRM, and we decide from the record."`,
		"two principles, then a claim that needs both — one premise would only be a rereading")
	for code, statement := range map[string]string{
		"P.1": "Only contract data from the CRM is trusted.",
		"P.2": "A decision is taken from the record, not from memory.",
	} {
		if _, err := s.c.call(ctx, "principle_add", map[string]any{"code": code, "statement": statement}); err != nil {
			return err
		}
	}
	if out, err := s.c.call(ctx, "admit", map[string]any{
		"claim": "The Acme renewal date in the CRM is the one we act on", "basis": "derivation",
		"premises":   []string{"P.1"},
		"derivation": "P.1 ⟹ its renewal date is the one to act on",
	}); err != nil {
		return err
	} else if code, _ := refusal(out); code != "SINGLE_PREMISE" {
		return fmt.Errorf("step 7: one premise is a rereading, not a derivation: %v", out)
	}
	s.note("refused SINGLE_PREMISE — from one premise nothing new follows")

	out, err := s.c.call(ctx, "admit", map[string]any{
		"claim":    "The Acme renewal date in the CRM is the one we act on",
		"basis":    "derivation",
		"premises": []string{"P.1", "P.2"},
		"derivation": "P.1 ⟹ the CRM is the trusted record for contract dates; " +
			"P.2 ⟹ the decision is taken from that record ⟹ its renewal date is the one to act on",
	})
	if err != nil {
		return err
	}
	derivedID, _ := out["id"].(string)
	if derivedID == "" || out["effective"] != "T" {
		return fmt.Errorf("step 7: expected a standing claim, got %v", out)
	}
	s.note("P.1 added, %s stands on it", derivedID)

	s.step(8, `"Drop that principle — the CRM is unreliable."`, "principle_retire names what was resting on it")
	out, err = s.c.call(ctx, "principle_retire", map[string]any{
		"code": "P.1", "reason": "CRM contract data turned out unreliable",
	})
	if err != nil {
		return err
	}
	standing, _ := out["standing_on_it"].([]any)
	if len(standing) != 1 || standing[0] != derivedID {
		return fmt.Errorf("step 8: expected %s named as resting on P.1, got %v", derivedID, out)
	}
	s.note("retired P.1 and named %s as the claim that now needs re-reading", derivedID)

	s.step(9, `"What do we still know about Acme?"`, "the claim is flagged, not silently kept")
	out, err = s.c.call(ctx, "entry", map[string]any{"id": derivedID})
	if err != nil {
		return err
	}
	truth, _ := out["truth"].(map[string]any)
	fell, _ := truth["premises_fell"].([]any)
	if truth["effective"] != "needs_review" || len(fell) != 1 {
		return fmt.Errorf("step 9: expected needs_review with the fallen premise, got %v", truth)
	}
	out, err = s.c.call(ctx, "ledger", map[string]any{"standing": true})
	if err != nil {
		return err
	}
	truths, _ := out["truths"].([]any)
	for _, t := range truths {
		if m, ok := t.(map[string]any); ok && m["id"] == derivedID {
			return fmt.Errorf("step 9: %s should be out of the standing ledger", derivedID)
		}
	}
	s.note("%s reads needs_review, and drops out of ledger(standing)", derivedID)

	s.step(10, `"Build a new conclusion on that."`, "a fallen premise carries nothing new")
	out, err = s.c.call(ctx, "admit", map[string]any{
		"claim": "Acme gets the same renewal terms in 2027", "basis": "derivation",
		"premises": []string{derivedID, "P.2"}, "derivation": derivedID + " + P.2 ⟹ same terms hold next year",
	})
	if err != nil {
		return err
	}
	if code, _ := refusal(out); code != "PREMISE_NOT_T" {
		return fmt.Errorf("step 10: expected PREMISE_NOT_T, got %v", out)
	}
	s.note("refused PREMISE_NOT_T — the store will not build on what fell")
	return nil
}

func (s *scenario) note(format string, args ...any) {
	if s.out != nil {
		fmt.Fprintf(s.out, "    ✓ "+format+"\n", args...)
	}
}

// refusal reads {error: {code, message, next}} — the shape both transports use.
func refusal(out map[string]any) (code, next string) {
	e, ok := out["error"].(map[string]any)
	if !ok {
		return "", ""
	}
	code, _ = e["code"].(string)
	next, _ = e["next"].(string)
	return code, next
}

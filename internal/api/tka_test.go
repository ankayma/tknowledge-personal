package api

import (
	"net/http"
	"strings"
	"testing"
)

// The T/A store over REST: every response is the record as stored, the gate's
// refusals come back as {error: {code, message, next}} — the same shape the MCP
// tools return, carrying the move that is still open.
func TestTka_AssumeAdmitPromote(t *testing.T) {
	env := newTestServer(t)
	writer := env.token(t, "bao", "claude-desktop")

	type entry struct {
		ID       string   `json:"id"`
		Claim    string   `json:"claim"`
		Type     string   `json:"type"`
		Status   string   `json:"status"`
		Basis    string   `json:"basis"`
		Signer   string   `json:"signer"`
		Producer string   `json:"producer"`
		Premises []string `json:"premises"`
		Revision int      `json:"revision"`
		Similar  []struct {
			ID string `json:"id"`
		} `json:"similar"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Next    string `json:"next"`
		} `json:"error"`
		Assumption *struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"assumption"`
	}
	post := func(path string, body map[string]any) (int, entry) {
		t.Helper()
		resp := postJSON(t, env.URL+"/v1/tka/"+path, writer, body)
		return resp.StatusCode, decode[entry](t, resp)
	}

	if code, out := post("principles", map[string]any{"code": "P.1", "statement": "Strong architecture absorbs weak execution."}); code != 200 {
		t.Fatalf("principle add: %d %+v", code, out)
	}
	if code, out := post("principles", map[string]any{"code": "P.1", "statement": "khác"}); code != 409 || out.Error == nil || out.Error.Code != "PRINCIPLE_EXISTS" || out.Error.Next == "" {
		t.Fatalf("a principle is added once: %d %+v", code, out)
	}

	code, a := post("assume", map[string]any{
		"claim": "Mira gửi invite trước 26/9", "type": "A-p", "source": "owner",
		"if": "nhận được email invite", "not": "hết 26/9 không có gì",
	})
	if code != 200 || a.ID != "A-1" || a.Status != "open" || a.Signer != "bao" || a.Producer != "claude-desktop" {
		t.Fatalf("assume: %d %+v", code, a)
	}
	if code, out := post("assume", map[string]any{"claim": "x", "type": "A-p", "source": "owner"}); code != http.StatusBadRequest ||
		out.Error == nil || out.Error.Code != "MISSING_TAIL" || out.Error.Next == "" {
		t.Fatalf("A-p must declare its way out: %d %+v", code, out)
	}

	newClaim := "Mira gửi invite trước 30/9"
	if code, out := post("assumptions/update", map[string]any{"assumption_id": a.ID, "claim": newClaim, "confirm_new": true}); code != 409 ||
		out.Error == nil || out.Error.Code != "RESTATE_TAIL" || out.Error.Next == "" {
		t.Fatalf("đổi câu mà không khai lại if/not: %d %+v", code, out)
	}
	if code, out := post("assumptions/update", map[string]any{
		"assumption_id": a.ID, "claim": newClaim, "confirm_new": true,
		"if": "nhận được email invite", "not": "hết 30/9 không có gì",
	}); code != 200 || out.Revision != 2 || out.Claim != newClaim {
		t.Fatalf("update: %d %+v", code, out)
	}

	if code, out := post("admit", map[string]any{"claim": "Postgres 16 có logical replication", "basis": "fact"}); code != http.StatusBadRequest ||
		out.Error == nil || out.Error.Code != "NO_SOURCE" || out.Error.Next == "" {
		t.Fatalf("a fact needs a source: %d %+v", code, out)
	}
	// A bare link is not taken (spec T–D, B1): the page is read into a document.
	if code, out := post("admit", map[string]any{
		"claim": "Postgres 16 có logical replication", "basis": "fact", "where": "chapter 31",
		"source_url": "https://www.postgresql.org/docs/16/logical-replication.html",
	}); code != http.StatusBadRequest || out.Error == nil || out.Error.Code != "USE_DOCUMENT" || out.Error.Next == "" {
		t.Fatalf("a link alone: %d %+v", code, out)
	}
	if code, page := post("documents", map[string]any{
		"text": "PostgreSQL 16, Chapter 31. Logical Replication", "text_producer": "claude", "method": "opened_source",
		"source_uri": "https://www.postgresql.org/docs/16/logical-replication.html",
	}); code != 200 || page.ID != "D-1" {
		t.Fatalf("record the page: %d %+v", code, page)
	}
	code, fact := post("admit", map[string]any{
		"claim": "Postgres 16 có logical replication", "basis": "fact", "where": "chapter 31", "source_doc": "D-1",
	})
	if code != 200 || fact.ID != "T-1" || fact.Status != "T" || fact.Basis != "fact" {
		t.Fatalf("admit fact: %d %+v", code, fact)
	}
	if code, out := post("admit", map[string]any{
		"claim": "Sổ chỉ ghi thêm", "basis": "derivation", "premises": []string{"P.1", fact.ID},
		"derivation": "P.1 + " + fact.ID + " ⟹ change feed đọc được mọi thay đổi",
	}); code != 200 || len(out.Premises) != 2 {
		t.Fatalf("admit derivation: %d %+v", code, out)
	}
	if code, out := post("admit", map[string]any{
		"claim": "Mira gửi invite trước 30/9", "basis": "fact", "source": "email",
	}); code != 409 || out.Error == nil || out.Error.Code != "USE_PROMOTE" || out.Error.Next == "" {
		t.Fatalf("an open assumption is raised, not re-admitted: %d %+v", code, out)
	}

	// The email that settles it stays in the mailbox. The file itself is not
	// taken; what it says, and where it lives, is.
	req, err := http.NewRequest(http.MethodPost,
		env.URL+"/v1/tka/documents?filename=mira-21-09.eml", strings.NewReader("From: mira\nInvite đã gửi.\n"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+writer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if e := decode[struct {
		Error struct{ Code, Next string } `json:"error"`
	}](t, resp); resp.StatusCode != http.StatusBadRequest || e.Error.Code != "FILE_NOT_KEPT" || e.Error.Next == "" {
		t.Fatalf("a file as the body: %d %+v", resp.StatusCode, e)
	}
	resp = postJSON(t, env.URL+"/v1/tka/documents", writer, map[string]any{
		"text": "From: mira\nInvite đã gửi.\n", "text_producer": "owner", "method": "opened_source",
		"source_uri": "file:///mail/mira-21-09.eml", "sha256": strings.Repeat("c4", 32), "filename": "mira-21-09.eml",
	})
	doc := decode[struct {
		ID     string `json:"id"`
		SHA256 string `json:"sha256"`
		Text   string `json:"text"`
		Bound  bool   `json:"bound"`
	}](t, resp)
	if resp.StatusCode != 200 || doc.ID != "D-2" || len(doc.SHA256) != 64 || doc.Text == "" || !doc.Bound {
		t.Fatalf("record the email: %d %+v", resp.StatusCode, doc)
	}

	code, promoted := post("promote", map[string]any{
		"assumption_id": a.ID, "basis": "fact", "source": "email 2026-09-21 từ mira",
		"source_doc": doc.ID, "source_date": "2026-09-21",
	})
	if code != 200 || promoted.Claim != newClaim || promoted.Status != "T" ||
		promoted.Assumption == nil || promoted.Assumption.Status != "promoted" {
		t.Fatalf("promote: %d %+v", code, promoted)
	}

	// retiring a principle names what stood on it
	resp = postJSON(t, env.URL+"/v1/tka/principles/retire", writer, map[string]any{"code": "P.1", "reason": "viết lại"})
	out := decode[struct {
		Principle struct {
			Status string `json:"status"`
		} `json:"principle"`
		StandingOnIt []string `json:"standing_on_it"`
	}](t, resp)
	if resp.StatusCode != 200 || out.Principle.Status != "retired" || len(out.StandingOnIt) != 1 {
		t.Fatalf("retire: %d %+v", resp.StatusCode, out)
	}
	_, list := getJSON[struct {
		Count int `json:"count"`
	}](t, env, env.URL+"/v1/tka/principles")
	if list.Count != 0 {
		t.Fatalf("the retired principle is out of the active list: %+v", list)
	}
}

package api

import (
	"net/http"
	"strings"
	"testing"
)

// A lesson stored as a document, kept as a Case, filtered by its tags and read
// back as an entry, over HTTP (owner 17/10/2026, D-239).
func TestTkaCase_AddFilterRead(t *testing.T) {
	env := newTestServer(t)
	tok := env.token(t, "bao", "claude-desktop")

	resp := postJSON(t, env.URL+"/v1/tka/documents", tok, map[string]any{
		"text": "The agent quoted last year's price list; we refunded 1,200 USD.", "text_producer": "owner", "method": "read_text"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lesson document: %d %+v", resp.StatusCode, decode[storeErr](t, resp))
	}
	doc := decode[map[string]any](t, resp)

	resp = postJSON(t, env.URL+"/v1/tka/cases", tok, map[string]any{
		"from_doc": doc["id"], "title": "Stale price list", "text": "Name the price list's date in the instruction.",
		"cost_usd": 1200, "kind": "Price paid", "topics": []string{"AI Enablement"}, "case_studies": []string{"Acme"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("case_add: %d %+v", resp.StatusCode, decode[storeErr](t, resp))
	}
	c := decode[map[string]any](t, resp)
	if c["id"] != "C-1" || c["kind"] != "price paid" {
		t.Fatalf("case: %+v", c)
	}

	st, list := getJSON[map[string]any](t, env, env.URL+"/v1/tka/cases?topic=ai+enablement")
	if st != http.StatusOK || list["count"] != float64(1) {
		t.Fatalf("cases by topic: %d %+v", st, list)
	}
	if st, list := getJSON[map[string]any](t, env, env.URL+"/v1/tka/cases?case_study=other"); st != http.StatusOK || list["count"] != float64(0) {
		t.Fatalf("cases by another case study: %d %+v", st, list)
	}

	st, e := getJSON[map[string]any](t, env, env.URL+"/v1/tka/entries/C-1")
	if st != http.StatusOK || e["kind"] != "case" || !strings.Contains(e["case"].(map[string]any)["title"].(string), "price list") {
		t.Fatalf("entry C-1: %d %+v", st, e)
	}

	resp = postJSON(t, env.URL+"/v1/tka/cases/retire", tok, map[string]any{"id": "C-1", "reason": "kept by mistake"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("case_retire: %d %+v", resp.StatusCode, decode[storeErr](t, resp))
	}
	resp.Body.Close()
	if st, list := getJSON[map[string]any](t, env, env.URL+"/v1/tka/cases"); st != http.StatusOK || list["count"] != float64(0) {
		t.Fatalf("retired Case still listed: %d %+v", st, list)
	}
}

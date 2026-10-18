package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"

	"github.com/ankayma/tknowledge-personal/internal/ccclient"
	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/kb"
	"github.com/ankayma/tknowledge-personal/internal/subject"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call drives the MCP server over JSON-RPC exactly as a transport would —
// no mocks between the model's request and the ledger row.
func call(t *testing.T, s *server.MCPServer, id int, method string, params any) rpcResp {
	t.Helper()
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	raw, err := json.Marshal(s.HandleMessage(context.Background(), msg))
	if err != nil {
		t.Fatal(err)
	}
	var out rpcResp
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v\n%s", method, err, raw)
	}
	return out
}

type toolResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Structured json.RawMessage `json:"structuredContent"`
}

func tool(t *testing.T, s *server.MCPServer, name string, args any) toolResult {
	t.Helper()
	r := call(t, s, 1, "tools/call", map[string]any{"name": name, "arguments": args})
	if r.Error != nil {
		t.Fatalf("%s: rpc error %+v", name, r.Error)
	}
	var tr toolResult
	if err := json.Unmarshal(r.Result, &tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

func into[T any](t *testing.T, tr toolResult) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(tr.Structured, &v); err != nil {
		t.Fatalf("structured %s: %v", tr.Structured, err)
	}
	return v
}

type admitOut struct {
	Results []struct {
		Subject    string `json:"subject"`
		Outcome    string `json:"outcome"`
		ID         string `json:"id"`
		PreviousID string `json:"previous_id"`
		Error      *struct {
			Code string `json:"code"`
		} `json:"error"`
	} `json:"results"`
}

// newMCP is the stdio shape: the client holds the caller's grant (TK_TOKEN) —
// bao, acting through claude-desktop.
func newMCP(t *testing.T) (*server.MCPServer, *testapi.Env, *testapi.FakeCC) {
	t.Helper()
	env := testapi.Start(t)
	cc := testapi.StartCC(t)
	s := New(tkclient.New(env.Server.URL).WithToken(env.Token(t, "bao", "claude-desktop")), ccclient.New(cc.URL))
	call(t, s, 0, "initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "0"},
	})
	return s, env, cc
}

// The tool list is the contract with the model: exactly the functions of the
// T/A store and the ones the spec still keeps, nothing invented on the side.
func TestTools_SpecFunctionsOnly(t *testing.T) {
	s, _, _ := newMCP(t)
	r := call(t, s, 2, "tools/list", map[string]any{})
	if r.Error != nil {
		t.Fatalf("tools/list: %+v", r.Error)
	}
	var out struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &out); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range out.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := "admit,assume,attest,case_add,case_retire,case_tag,cases,chain,check_claim,close_flag,conversation,define,demote,document_add,documents,entry," +
		"flags,help,instruction_add,instruction_retire,instructions,interact,ledger,link,lookup,open_items,person,principle_add,principle_retire,principles," +
		"promote,recheck,refute,retire,store_info,summarize,thread,thread_close,thread_open," +
		"thread_tag,unlink,update_assumption,withdraw_evidence"
	if strings.Join(names, ",") != want {
		t.Fatalf("tool list drifted:\n got %s\nwant %s", strings.Join(names, ","), want)
	}
}

// A turn given inline through the tool (conversation object + top-level
// actor/type), then the conversation and the person it opened. No claim is
// written: when the last turn happened is read from the turns.
func TestFlow_InteractConversationPerson(t *testing.T) {
	s, env, cc := newMCP(t)

	type interactOut struct {
		ConversationID string `json:"conversation_id"`
		EvtID          string `json:"evt_id"`
		MyTouches      int    `json:"my_touches_since_their_last"`
	}
	r := tool(t, s, ToolInteract, map[string]any{
		"conversation": map[string]any{"kind": "dm", "person": "Arjun Mehta"},
		"actor":        "me", "type": "dm", "body": "Hi Arjun", "verbatim": true, "chat_id": "c1",
	})
	out := into[interactOut](t, r)
	if r.IsError || out.MyTouches != 1 || !strings.HasPrefix(out.ConversationID, "conv_") || !strings.HasPrefix(out.EvtID, "evt_") {
		t.Fatalf("interact: %s", r.Structured)
	}
	var rows int
	if err := env.Admin.QueryRow(context.Background(), `SELECT count(*) FROM ledger`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("interact writes no claim: %d %v", rows, err)
	}
	if len(cc.Causes()) != 0 {
		t.Fatalf("causes: %d", len(cc.Causes()))
	}

	pasted := tool(t, s, ToolInteract, map[string]any{
		"conversation": map[string]any{"kind": "dm", "person": "arjun"},
		"events":       []map[string]any{{"actor": "Arjun", "type": "dm", "body": "Hello Bao", "in_reply_to": out.EvtID}},
	})
	if pasted.IsError || into[interactOut](t, pasted).MyTouches != 0 {
		t.Fatalf("pasted reply: %s", pasted.Structured)
	}

	conv := tool(t, s, ToolConversation, map[string]any{"conversation_id": out.ConversationID})
	if conv.IsError || !strings.Contains(string(conv.Structured), `"participants":["arjun-mehta","me"]`) {
		t.Fatalf("conversation: %s", conv.Structured)
	}
	p := tool(t, s, ToolPerson, map[string]any{"query": "AM"})
	if p.IsError || !strings.Contains(string(p.Structured), `"slug":"arjun-mehta"`) || !strings.Contains(string(p.Structured), out.ConversationID) {
		t.Fatalf("person: %s", p.Structured)
	}
	missing := tool(t, s, ToolInteract, map[string]any{"conversation": map[string]any{"kind": "dm"}, "actor": "me", "type": "dm", "body": "x"})
	if !missing.IsError || !strings.Contains(string(missing.Structured), "CONVERSATION_KEY_MISSING") {
		t.Fatalf("conversation key missing: %s", missing.Structured)
	}
}

func TestToolErrors_SpecShape(t *testing.T) {
	s, _, cc := newMCP(t)
	type errOut struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	for _, c := range []struct {
		tool string
		args map[string]any
		code string
	}{
		{ToolPromote, map[string]any{"assumption_id": "A-9999", "basis": "fact", "source": "x"}, "NOT_FOUND"},
		{ToolAdmit, map[string]any{"claim": "x", "basis": "fact"}, "NO_SOURCE"},
		{ToolAssume, map[string]any{"claim": "x", "type": "A-p", "source": "owner"}, "MISSING_TAIL"},
	} {
		r := tool(t, s, c.tool, c.args)
		if !r.IsError || into[errOut](t, r).Error.Code != c.code {
			t.Fatalf("%s %v: want %s, got %s", c.tool, c.args, c.code, r.Structured)
		}
	}
	if len(cc.Causes()) != 0 {
		t.Fatalf("errors issued causes: %d", len(cc.Causes()))
	}
}

// SPEC §13 over http: the server holds no grant of its own; the caller's
// grant arrives in the URL (dev URL / Ankayma privUrl) or as Bearer, and a
// privUrl-style Host header is not refused.
func TestHTTP_GrantFromURLOrBearer(t *testing.T) {
	env := testapi.Start(t)
	cc := testapi.StartCC(t)
	ts := httptest.NewServer(HTTPHandler(New(tkclient.New(env.Server.URL), ccclient.New(cc.URL))))
	t.Cleanup(ts.Close)
	ctx := context.Background()

	admitVia := func(path, bearer, statement string) toolResult {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": ToolAssume, "arguments": map[string]any{
				"claim": statement, "type": "A-c", "source": "owner", "via": "amendment", "confirm_new": true,
			}}})
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(body))
		req.Host = "tk-mcp.bao.priv.example" // what Ankayma's relay forwards to the loopback port
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: HTTP %d %s", path, resp.StatusCode, raw)
		}
		var rpc rpcResp
		if err := json.Unmarshal(raw, &rpc); err != nil || rpc.Error != nil {
			t.Fatalf("%s: rpc %v %s", path, err, raw)
		}
		var tr toolResult
		if err := json.Unmarshal(rpc.Result, &tr); err != nil {
			t.Fatal(err)
		}
		return tr
	}
	producerOf := func(tr toolResult) string {
		t.Helper()
		out := into[struct {
			ID string `json:"id"`
		}](t, tr)
		if tr.IsError || out.ID == "" {
			t.Fatalf("assume: %s", tr.Structured)
		}
		var p string
		if err := env.Admin.QueryRow(ctx, `SELECT producer FROM assumption WHERE id = $1`, out.ID).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if p := producerOf(admitVia("/mcp/t/"+env.Token(t, "bao", "claude-desktop"), "", "from privUrl")); p != "claude-desktop" {
		t.Fatalf("grant in URL: producer %s", p)
	}
	if p := producerOf(admitVia("/mcp", env.Token(t, "bao", "cursor"), "from bearer")); p != "cursor" {
		t.Fatalf("grant as Bearer: producer %s", p)
	}
	if none := admitVia("/mcp", "", "no grant"); !none.IsError || !strings.Contains(string(none.Structured), "missing_grant") {
		t.Fatalf("no grant: %s", none.Structured)
	}
	if bad := admitVia("/mcp/t/not-a-grant", "", "bad grant"); !bad.IsError || !strings.Contains(string(bad.Structured), "invalid_grant") {
		t.Fatalf("bad grant: %s", bad.Structured)
	}
}

// The four resources round-trip real tk-api JSON (SPEC §8).
func TestResources_ReadThrough(t *testing.T) {
	s, env, cc := newMCP(t)
	// One promoted T in the old ledger: a post that went out. (interact used to
	// write one; since 1/10/2026 it writes no claim.)
	// posted left MCP on 14/10/2026; the old ledger still takes it through tk-api.
	svc := &kb.Service{CC: ccclient.New(cc.URL)}
	if _, err := svc.Posted(context.Background(), tkclient.New(env.Server.URL).WithToken(env.Token(t, "bao", "claude-desktop")),
		kb.PostedInput{Title: "Probe", Body: "r", URL: "https://example.com/p"}); err != nil {
		t.Fatalf("posted: %v", err)
	}
	var claimID int64
	if err := env.Admin.QueryRow(context.Background(), `SELECT claim_id FROM ledger ORDER BY id LIMIT 1`).Scan(&claimID); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(claimID, 10)

	type contents struct {
		Contents []struct {
			URI      string `json:"uri"`
			MIMEType string `json:"mimeType"`
			Text     string `json:"text"`
		} `json:"contents"`
	}
	read := func(uri string) map[string]any {
		t.Helper()
		r := call(t, s, 6, "resources/read", map[string]any{"uri": uri})
		if r.Error != nil {
			t.Fatalf("read %s: %+v", uri, r.Error)
		}
		var c contents
		if err := json.Unmarshal(r.Result, &c); err != nil || len(c.Contents) != 1 {
			t.Fatalf("read %s: %v %s", uri, err, r.Result)
		}
		if c.Contents[0].URI != uri || c.Contents[0].MIMEType != "application/json" {
			t.Fatalf("read %s: bad envelope %+v", uri, c.Contents[0])
		}
		var body any
		if err := json.Unmarshal([]byte(c.Contents[0].Text), &body); err != nil {
			t.Fatalf("read %s: body not JSON: %v", uri, err)
		}
		if m, ok := body.(map[string]any); ok {
			return m
		}
		return map[string]any{"_list": body}
	}

	claim := read("tk://claims/" + id)
	if claim["mark"] != "T" || claim["status"] != "active" {
		t.Fatalf("claim resource: %v", claim)
	}
	if _, ok := read("tk://claims/" + id + "/audit")["eff"]; !ok {
		t.Fatal("audit resource")
	}
	if _, ok := read("tk://flags")["_list"]; !ok {
		t.Fatal("flags resource should be a list")
	}
	if _, ok := read("tk://flags?to=bao")["_list"]; !ok {
		t.Fatal("flags?to resource should be a list")
	}
	if stats := read("tk://stats"); stats["claims"] != float64(1) {
		t.Fatalf("stats resource: %v", stats)
	}
	if r := call(t, s, 7, "resources/read", map[string]any{"uri": "tk://claims/abc"}); r.Error == nil {
		t.Fatal("bad id should error")
	}
}

func mustID(t *testing.T, id string) int64 {
	t.Helper()
	n, err := subject.ParseID(id)
	if err != nil {
		t.Fatalf("%q: %v", id, err)
	}
	return n
}

// A demo grant is what a stranger holds: the tool list shows the T/A store, so
// attention lands there. Nothing is fenced off — a tool that is not listed
// still runs if the model names it.
func TestDemoGrant_ListShowsTheStore(t *testing.T) {
	env := testapi.Start(t)
	cc := testapi.StartCC(t)
	s := New(tkclient.New(env.Server.URL), ccclient.New(cc.URL))
	demo := env.Token(t, "stranger", "claude", grant.ScopeRead, grant.ScopeWrite, grant.ScopeDemo)
	ctx := ContextWithToken(context.Background(), demo)

	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	raw, _ := json.Marshal(s.HandleMessage(ctx, msg))
	var listed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range listed.Result.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	// Threads, lookup and refute joined the demo on 1/10/2026, with the
	// conversation tools lookup points at (people and conversations belong to
	// the store since then, so a stranger only ever sees their own).
	// attest, withdraw_evidence, flags and close_flag joined on 2/10/2026 (spec T–D phase 1);
	// summarize on 3/10/2026; the three instruction tools and store_info on 5/10/2026;
	// help on 13/10/2026; case_add, cases, case_tag and case_retire on 17/10/2026 (D-239).
	want := "admit,assume,attest,case_add,case_retire,case_tag,cases,chain,check_claim,close_flag,conversation,define,demote,document_add,documents,entry," +
		"flags,help,instruction_add,instruction_retire,instructions,interact,ledger,link,lookup,open_items,person,principle_add,principle_retire,principles,promote,recheck," +
		"refute,retire,store_info,summarize,thread,thread_close,thread_open,thread_tag,unlink,update_assumption,withdraw_evidence"
	if strings.Join(names, ",") != want {
		t.Fatalf("demo tool list:\n got %s\nwant %s", strings.Join(names, ","), want)
	}

	// Every caller sees the same 43 (owner 14/10/2026): the owner's own grant,
	// and a caller with no token at all — the store-free door before sign-in.
	for name, c := range map[string]context.Context{
		"owner":    ContextWithToken(context.Background(), env.Token(t, "bao", "claude-desktop")),
		"no token": context.Background(),
	} {
		raw, _ := json.Marshal(s.HandleMessage(c, msg))
		if err := json.Unmarshal(raw, &listed); err != nil {
			t.Fatal(err)
		}
		if len(listed.Result.Tools) != 43 {
			t.Fatalf("%s sees %d tools, want 43", name, len(listed.Result.Tools))
		}
	}
	// A removed tool is gone, not hidden.
	call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "store", "arguments": map[string]any{"title": "x", "kind": "rule", "body": "y"}}})
	if out, _ := json.Marshal(s.HandleMessage(ctx, call)); strings.Contains(string(out), `"doc_id"`) {
		t.Fatalf("store must no longer run through MCP: %s", out)
	}
}

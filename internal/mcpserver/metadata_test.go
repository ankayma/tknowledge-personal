package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// What the directory review reads (owner 13/10/2026): every tool — 43, the
// same list signed in or not (owner 14/10/2026; the four Case tools 17/10) — carries a title and annotations that say what it does to the store,
// and a description of 40–90 words that names a neighbouring tool it is not;
// the initialize string is short and points at lookup and help.
func TestToolMetadata_ForReview(t *testing.T) {
	s := New(tkclient.New("http://127.0.0.1:0"), nil)
	ctx := context.Background()
	init := s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
	raw, _ := json.Marshal(init)
	var in struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(in.Result.Instructions)); n > 120 || !strings.Contains(in.Result.Instructions, "lookup") ||
		!strings.Contains(in.Result.Instructions, "help") {
		t.Fatalf("initialize instructions: %d words, must be ≤ 120 and name lookup and help:\n%s", n, in.Result.Instructions)
	}

	raw, _ = json.Marshal(s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)))
	var list struct {
		Result struct {
			Tools []mcp.Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	takesOut := []string{ToolDemote, ToolRetire, ToolRefute, ToolWithdrawEvidence, ToolUnlink, ToolThreadClose,
		ToolCloseFlag, ToolInstructionRetire, ToolPrincipleRetire, ToolCaseRetire}
	reads := []string{ToolEntry, ToolChain, ToolOpenItems, ToolLedger, ToolDocuments, ToolFlags, ToolPrinciples,
		ToolInstructions, ToolStoreInfo, ToolCheckClaim, ToolDefine, ToolLookup, ToolThread, ToolPerson,
		ToolConversation, ToolHelp, ToolCases}
	names := []string{}
	for _, tl := range list.Result.Tools {
		names = append(names, tl.Name)
	}
	mentions := regexp.MustCompile(`\b[a-z_]+\b`)
	shown := 0
	for _, tl := range list.Result.Tools {
		a := tl.Annotations
		if tl.Title == "" || a.Title != tl.Title || a.ReadOnlyHint == nil || a.DestructiveHint == nil || a.IdempotentHint == nil ||
			a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s: title and all four hints, open-world false: %+v", tl.Name, a)
			continue
		}
		if *a.ReadOnlyHint != slices.Contains(reads, tl.Name) {
			t.Errorf("%s: readOnlyHint %v", tl.Name, *a.ReadOnlyHint)
		}
		if *a.DestructiveHint != slices.Contains(takesOut, tl.Name) {
			t.Errorf("%s: destructiveHint %v", tl.Name, *a.DestructiveHint)
		}
		if *a.ReadOnlyHint && !*a.IdempotentHint {
			t.Errorf("%s: a read is idempotent", tl.Name)
		}
		shown++
		if n := len(strings.Fields(tl.Description)); n < 40 || n > 90 {
			t.Errorf("%s: description is %d words, want 40–90", tl.Name, n)
		}
		neighbour := false
		for _, w := range mentions.FindAllString(tl.Description, -1) {
			if w != tl.Name && slices.Contains(names, w) && len(w) > 3 {
				neighbour = true
			}
		}
		if !neighbour || !strings.Contains(tl.Description, "Not ") {
			t.Errorf("%s: the description says when not to call it and names the tool for that", tl.Name)
		}
	}
	if shown != 43 {
		t.Fatalf("tools/list shows %d tools, want 43 (38 + help + 4 Case tools)", shown)
	}
}

package mcpserver

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// linkKinds are the ids an answer may carry a link for, by prefix.
var linkKinds = []struct{ prefix, kind string }{
	{"TH-", "thread"}, {"T-", "entry"}, {"A-", "entry"}, {"D-", "document"},
}

// addLinks gives an answer that is one thing in the store — a thread, an entry,
// a document — a `link` to it, made by LinkFor so it has the form the caller's
// door opens (brief 2/10 V8). Tools never write URLs themselves.
func addLinks(h *server.Hooks) {
	h.AddAfterCallTool(func(ctx context.Context, _ any, _ *mcp.CallToolRequest, result any) {
		res, ok := result.(*mcp.CallToolResult)
		if !ok || res == nil || res.IsError || len(res.Content) == 0 {
			return
		}
		text, ok := res.Content[0].(mcp.TextContent)
		if !ok {
			return
		}
		var obj map[string]any
		if json.Unmarshal([]byte(text.Text), &obj) != nil {
			return
		}
		id, _ := obj["id"].(string)
		kind := ""
		for _, k := range linkKinds {
			if strings.HasPrefix(id, k.prefix) {
				kind = k.kind
				break
			}
		}
		if kind == "" {
			return
		}
		link := LinkFor(ctx, kind, id)
		if link == "" {
			return
		}
		obj["link"] = link
		b, err := json.Marshal(obj)
		if err != nil {
			return
		}
		text.Text = string(b)
		res.Content[0] = text
		if res.StructuredContent != nil {
			res.StructuredContent = obj
		}
	})
}

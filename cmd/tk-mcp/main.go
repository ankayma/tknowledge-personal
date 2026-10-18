// tk-mcp is the MCP server for T-knowledge: a client of tk-api that
// lets a model *propose* claims (always mark=A) and read claims, audits, flags
// and stats. Two transports in one binary:
//
//	tk-mcp --transport stdio   # Claude Desktop, Cursor (default)
//	tk-mcp --transport http    # Streamable HTTP on TK_MCP_ADDR (default 127.0.0.1:8083)
//
// Who is calling is the caller's grant, forwarded to tk-api:
// stdio reads TK_TOKEN; http reads it from the URL (/mcp/t/<grant>, dev URL or
// Ankayma privUrl) or `Authorization: Bearer` on /mcp.
//
// Env: TK_API_URL (default http://localhost:8081), TK_TOKEN (stdio), TK_MCP_ADDR (http),
// TK_PUBLIC_URL (http: the public address, for the 401's discovery pointer and links).
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/mark3labs/mcp-go/server"

	"github.com/ankayma/tknowledge-personal/internal/ccclient"
	"github.com/ankayma/tknowledge-personal/internal/mcpserver"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
	"github.com/ankayma/tknowledge-personal/internal/version"
)

func main() {
	transport := flag.String("transport", "stdio", "stdio | http")
	flag.Parse()

	apiURL := os.Getenv("TK_API_URL")
	if apiURL == "" {
		apiURL = "http://localhost:8081"
	}

	ccURL := os.Getenv("CC_URL")
	if ccURL == "" {
		ccURL = "http://localhost:8082"
	}

	// stdout is the stdio transport's wire; keep logs on stderr in both modes.
	log.SetOutput(os.Stderr)
	client := tkclient.New(apiURL)
	cc := ccclient.New(ccURL) // causes for every move (CJ-10)

	switch *transport {
	case "stdio":
		token := os.Getenv("TK_TOKEN")
		if token == "" {
			log.Fatal("TK_TOKEN not set (mint one with: tk token issue --user <you> --actor claude-desktop)")
		}
		if err := server.ServeStdio(mcpserver.New(client.WithToken(token), cc)); err != nil {
			log.Fatal(err)
		}
	case "http":
		addr := os.Getenv("TK_MCP_ADDR")
		if addr == "" {
			addr = "127.0.0.1:8083"
		}
		// Three doors (brief 2/10): /mcp with an OAuth access token or a grant in
		// the Authorization header, /mcp/t/<grant> for a private link. Grants are
		// checked against tk-api before the MCP SDK runs, so /mcp can answer 401
		// (and Claude shows its Connect card). TK_PUBLIC_URL is the address the
		// 401 points discovery at and links are made with.
		publicURL := os.Getenv("TK_PUBLIC_URL")
		log.Printf("tk-mcp %s (http) listening on %s — /mcp (OAuth or Bearer grant) and /mcp/t/<grant>; tk-api at %s; public URL %q",
			version.String(), addr, apiURL, publicURL)
		handler := mcpserver.HTTPHandler(mcpserver.New(client, cc), mcpserver.WithGate(apiURL, publicURL))
		if err := http.ListenAndServe(addr, handler); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("unknown --transport %q (stdio | http)", *transport)
	}
}

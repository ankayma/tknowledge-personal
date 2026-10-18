// tk-proxy puts tk-api and tk-mcp behind one origin, the way a reverse proxy
// does in front of a hosted store: /mcp and /mcp/… go to tk-mcp, everything
// else (sign-in, /v1, pages) to tk-api. The Personal image runs it so a store
// has one address to connect to.
package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

func main() {
	listen, api, mcp := env("TK_PROXY_ADDR", "0.0.0.0:8080"), env("TK_API_URL", "http://127.0.0.1:8081"), env("TK_MCP_URL", "http://127.0.0.1:8083")
	toAPI, toMCP := proxy(api), proxy(mcp)
	log.Printf("tk-proxy %s → /mcp %s, rest %s", listen, mcp, api)
	log.Fatal(http.ListenAndServe(listen, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/mcp/") {
			toMCP.ServeHTTP(w, r)
			return
		}
		toAPI.ServeHTTP(w, r)
	})))
}

func proxy(raw string) *httputil.ReverseProxy {
	u, err := url.Parse(raw)
	if err != nil {
		log.Fatal(err)
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.FlushInterval = -1 // stream MCP responses as they come
	return p
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

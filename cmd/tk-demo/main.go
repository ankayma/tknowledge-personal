// tk-demo walks the demo scenario against a running store — the local docker
// stack or a hosted one — and fails on the first step that does not behave.
// Run it before asking a person to try the store, and again after every deploy.
//
//	tk-demo -url http://localhost:8083 -key <seed-file>
//	tk-demo -url https://tk.example.com -grant <grant>
//
// With -key it mints its own grant in a fresh tenant, so a run never touches
// anyone else's store and can be repeated as often as you like.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/ankayma/tknowledge-personal/internal/demo"
	"github.com/ankayma/tknowledge-personal/internal/grant"
)

func main() {
	url := flag.String("url", "http://localhost:8083", "base URL of tk-mcp")
	token := flag.String("grant", "", "an existing grant; its tenant must be empty")
	key := flag.String("key", "", "signing seed; mints a grant in a fresh tenant instead")
	tenant := flag.String("tenant", "", "tenant for the minted grant (default: a random one)")
	flag.Parse()

	if *token == "" && *key == "" {
		log.Fatal("need -grant <token> or -key <seed file>")
	}
	if *key != "" {
		minted, name, err := mint(*key, *tenant)
		if err != nil {
			log.Fatalf("mint grant: %v", err)
		}
		*token = minted
		fmt.Printf("tenant %s\n", name)
	}

	fmt.Printf("store  %s\n", *url)
	if err := demo.Run(context.Background(), demo.Options{BaseURL: *url, Grant: *token, Out: os.Stdout}); err != nil {
		fmt.Printf("\nFAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\nAll ten turns behaved as the store promises.")
}

func mint(seedFile, tenant string) (token, name string, err error) {
	raw, err := os.ReadFile(seedFile)
	if err != nil {
		return "", "", err
	}
	priv, err := grant.ParseSeed(string(raw))
	if err != nil {
		return "", "", err
	}
	if tenant == "" {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return "", "", err
		}
		tenant = "scenario-" + hex.EncodeToString(b)
	}
	token, err = grant.Sign(priv, grant.Grant{
		UserID: "demo-user", ActorID: "claude", NodeID: "local", Aud: "tk",
		TenantID: tenant, Scope: []string{grant.ScopeRead, grant.ScopeWrite, grant.ScopeDemo},
	})
	return token, tenant, err
}

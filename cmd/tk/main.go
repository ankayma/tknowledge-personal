// tk is the owner's CLI: named moves on the ledger, each preceded
// by the owner's cause on cause-channel. See internal/cli.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ankayma/tknowledge-personal/internal/cli"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

func main() {
	tkURL := os.Getenv("TK_API_URL")
	if tkURL == "" {
		tkURL = "http://localhost:8081"
	}
	ccURL := os.Getenv("CC_URL")
	if ccURL == "" {
		ccURL = "http://localhost:8082"
	}
	// TK_TOKEN is the caller's grant: it names the signer.
	app := &cli.App{
		TK: tkclient.New(tkURL).WithToken(os.Getenv("TK_TOKEN")), CC: cli.NewCauseChannel(ccURL), Out: os.Stdout,
		AdminDSN: os.Getenv("TK_ADMIN_DSN"), // org import only
	}
	if err := app.Run(context.Background(), os.Args[1:]); err != nil {
		if cli.IsUsage(err) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "tk:", err)
		os.Exit(1)
	}
}

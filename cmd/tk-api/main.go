// tk-api is the REST gateway: the only write path into the `ledger` table.
package main

import (
	"context"
	"crypto/ed25519"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ankayma/tknowledge-personal/internal/api"
	"github.com/ankayma/tknowledge-personal/internal/config"
	"github.com/ankayma/tknowledge-personal/internal/eff"
	"github.com/ankayma/tknowledge-personal/internal/flags"
	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/migrate"
	"github.com/ankayma/tknowledge-personal/internal/setup"
	"github.com/ankayma/tknowledge-personal/internal/version"
)

func main() {
	ctx := context.Background()

	// TK_ADMIN_DSN, if set, runs migrations once at startup using a role with
	// DDL privileges — TK_DSN (the scoped tk_api role) cannot create tables/roles.
	if adminDSN := os.Getenv("TK_ADMIN_DSN"); adminDSN != "" {
		adminPool, err := pgxpool.New(ctx, adminDSN)
		if err != nil {
			log.Fatalf("connect admin dsn: %v", err)
		}
		if err := migrate.Apply(ctx, adminPool); err != nil {
			log.Fatalf("apply migrations: %v", err)
		}
		adminPool.Close()
	}

	dsn := os.Getenv("TK_DSN")
	if dsn == "" {
		log.Fatal("TK_DSN not set")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	cfgPath := os.Getenv("TK_CONFIG")
	if cfgPath == "" {
		cfgPath = "config.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config %s: %v", cfgPath, err)
	}

	// Grants: every write is verified against these keys — dev key,
	// or Ankayma's control-plane key. No keys means no writes can succeed, so
	// refuse to start rather than run a gateway that rejects everything.
	keys, err := grant.ParsePublicKeys(os.Getenv("TK_GRANT_PUBKEYS"))
	if err != nil {
		log.Fatalf("TK_GRANT_PUBKEYS: %v", err)
	}
	aud := os.Getenv("TK_GRANT_AUD")
	if aud == "" {
		aud = "tk"
	}

	repo := ledger.New(pool)
	evaluator := eff.New(repo, cfg.AgentsSet())
	flagsRepo := flags.New(pool)
	server := api.New(repo, evaluator, flagsRepo, &grant.Verifier{Keys: keys, Aud: aud})
	// TK_TEMPLATES_DIR: more templates (e.g. from `tk org template export`),
	// one directory each; an id there overrides the embedded one.
	if dir := os.Getenv("TK_TEMPLATES_DIR"); dir != "" {
		reg, err := setup.Load(setup.Embedded, os.DirFS(dir))
		if err != nil {
			log.Fatalf("TK_TEMPLATES_DIR: %v", err)
		}
		server.Templates = reg
	}

	// TK_DEMO_KEY turns on the public door that opens a trial store: a landing
	// page at / and POST /v1/demo/open. It signs grants, which is why it is off
	// unless a deployment asks for it. TK_PUBLIC_URL is what the link says.
	demo, err := api.DemoFromEnv(os.Getenv("TK_DEMO_KEY"), os.Getenv("TK_PUBLIC_URL"),
		os.Getenv("TK_CONTACT"), os.Getenv("TK_DEMO_SHOW_LINK") == "1")
	if err != nil {
		log.Fatalf("TK_DEMO_KEY: %v", err)
	}
	server.Demo = demo
	server.Contact = os.Getenv("TK_CONTACT")
	// TK_SITE_URL is the operator's own site: the pages link to it and every
	// message signs off with it. Unset leaves both out.
	if demo != nil {
		demo.Site = strings.TrimRight(os.Getenv("TK_SITE_URL"), "/")
	}
	// TK_WRITE_TZ is the zone a seat's write month closes in (owner 13/10:
	// Asia/Dubai, the default). TK_PRICING_URL is where the refusal at the end
	// of a month points for a bigger seat.
	if v := strings.TrimSpace(os.Getenv("TK_WRITE_TZ")); v != "" {
		loc, err := time.LoadLocation(v)
		if err != nil {
			log.Fatalf("TK_WRITE_TZ: %v", err)
		}
		server.WriteTZ = loc
	}
	server.PricingURL = strings.TrimSpace(os.Getenv("TK_PRICING_URL"))
	if demo != nil {
		// TK_DEMO_LIFETIME is how long a trial store lives, and with it every link
		// to it: a link handed out again for the same address ends when the store
		// does. "30d" or a Go duration ("720h"); unset keeps the default.
		if v := os.Getenv("TK_DEMO_LIFETIME"); v != "" {
			d, err := api.ParseLifetime(v)
			if err != nil {
				log.Fatalf("TK_DEMO_LIFETIME: %v", err)
			}
			demo.Lifetime = d
		}
		// TK_DEMO_MAX_ENTRIES is how many claims a new store holds — from the
		// door or from signing in through Claude (owner 2/10: 1000 by default).
		if v := os.Getenv("TK_DEMO_MAX_ENTRIES"); v != "" {
			n, err := api.ParseMaxEntries(v)
			if err != nil {
				log.Fatalf("TK_DEMO_MAX_ENTRIES: %v", err)
			}
			demo.MaxEntries = n
		}
		// Writes are counted per person and month by seat since 13/10 (free 100,
		// standard 1000, unlimited); the old store-wide cap is gone.
		if os.Getenv("TK_DEMO_MAX_WRITES") != "" {
			log.Printf("TK_DEMO_MAX_WRITES is ignored: writes are counted by seat, per person and month")
		}
		// TK_DEMO_MAX_WORDS is how many words one claim in a new store may have
		// (owner 3/10: 100 by default; 600 characters stay as the backstop).
		if v := os.Getenv("TK_DEMO_MAX_WORDS"); v != "" {
			n, err := api.ParseMaxWords(v)
			if err != nil {
				log.Fatalf("TK_DEMO_MAX_WORDS: %v", err)
			}
			demo.MaxWords = n
		}
		// The link is handed over by mail only, so a door with no mailer is a door
		// that cannot let anyone in. Say so at startup rather than at the first
		// person who tries.
		demo.Mail = api.MailerFromEnv(os.Getenv("RESEND_API_KEY"), os.Getenv("MAIL_FROM"))
		// Answers to any message go to the contact address (TK_CONTACT).
		if m, ok := demo.Mail.(*api.Resend); ok {
			m.ReplyTo = demo.Contact
		}
		if demo.Mail == nil && !demo.ShowLink {
			log.Printf("demo door: RESEND_API_KEY not set — nobody can be handed a store until it is")
		}
		if demo.ShowLink {
			log.Printf("demo door: TK_DEMO_SHOW_LINK=1 — the link is shown on the page, so any " +
				"address typed in gets a working store. Testing only; unset it once mail works.")
		}
		log.Printf("demo door open: a trial store is %s", demo.Terms())
	}

	// OAuth behind /mcp (brief 2/10): sign-in by email code for Claude and any
	// MCP client that follows the authorization spec. TK_LOGIN_MODE=off keeps
	// only the header and URL doors — a sovereign deployment with no outside
	// sign-in. It needs the door for its store terms and its mailer.
	if mode := os.Getenv("TK_LOGIN_MODE"); mode != "off" {
		if mode != "" && mode != "otp" {
			log.Fatalf("TK_LOGIN_MODE: %q is not otp or off", mode)
		}
		issuer := strings.TrimRight(os.Getenv("TK_OAUTH_ISSUER"), "/")
		if issuer == "" {
			issuer = strings.TrimRight(os.Getenv("TK_PUBLIC_URL"), "/")
		}
		seed := os.Getenv("TK_OAUTH_KEY")
		if seed == "" {
			seed = os.Getenv("TK_DEMO_KEY")
		}
		switch {
		case demo == nil:
			log.Printf("oauth: off — it opens stores on the door's terms, and TK_DEMO_KEY is not set")
		case issuer == "" || seed == "":
			log.Printf("oauth: off — set TK_OAUTH_ISSUER (or TK_PUBLIC_URL) and TK_OAUTH_KEY (or TK_DEMO_KEY)")
		default:
			key, err := grant.ParseSeed(seed)
			if err != nil {
				log.Fatalf("TK_OAUTH_KEY: %v", err)
			}
			// The access tokens this server signs are grants; tk-api must trust
			// the key that signs them.
			pub := key.Public().(ed25519.PublicKey)
			if !slices.ContainsFunc(server.Grants.Keys, func(k ed25519.PublicKey) bool { return k.Equal(pub) }) {
				server.Grants.Keys = append(server.Grants.Keys, pub)
			}
			server.OAuth = &api.OAuth{
				Issuer: issuer, Resource: issuer + "/mcp", Key: key, Aud: aud,
				ReviewEmail: os.Getenv("TK_REVIEW_EMAIL"), ReviewCode: os.Getenv("TK_REVIEW_CODE"),
				// TK_OAUTH_CIMD=off: this host cannot fetch the client's metadata URL,
				// so clients are told to register (DCR) instead.
				NoCIMD: os.Getenv("TK_OAUTH_CIMD") == "off",
			}
			review := "no review account"
			if server.OAuth.ReviewEmail != "" && server.OAuth.ReviewCode != "" {
				review = "review account set"
			}
			registration := "CIMD and DCR"
			if server.OAuth.NoCIMD {
				registration = "DCR only (TK_OAUTH_CIMD=off)"
			}
			log.Printf("oauth: on — issuer %s, resource %s/mcp, email code sign-in, %s, %s", issuer, issuer, review, registration)
		}
	}

	addr := os.Getenv("TK_API_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	log.Printf("tk-api %s listening on %s", version.String(), addr)
	if err := http.ListenAndServe(addr, server.Router()); err != nil {
		log.Fatal(err)
	}
}

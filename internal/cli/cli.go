// Package cli is the owner's command line `tk`. Every move goes to
// cause-channel first — the owner's cause is issued before the row exists —
// then to tk-api with the cause_id. Who signs is the grant in TK_TOKEN,
// never a name typed on the command line. The CLI holds no
// database credential; it is a client of two REST APIs, plus the grant
// signing key when it mints dev grants.
package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ankayma/tknowledge-personal/internal/ccclient"
	"github.com/ankayma/tknowledge-personal/internal/fixtures"
	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

type App struct {
	TK  *tkclient.Client // carries TK_TOKEN
	CC  *CauseChannel
	Out io.Writer

	AdminDSN string // TK_ADMIN_DSN, only for org import

	signers map[string]string // grant token → user_id, from /whoami
}

const usage = `tk — the owner's named moves on a T-knowledge store

  tk whoami
  tk propose   "<statement>" [--producer <name>] [--refs 102,880] [--note ...]
  tk import    fixtures/<case>/ [--tokens tokens.json]   # principles.json + claims.jsonl
  tk promote   <claim_id> --reason "..." --basis-kind derivation|fact|principle [--refs 102,880] [--note ...] [--producer ...]
  tk demote    <claim_id> --reason "..."
  tk reapprove <claim_id> --reason "..." --basis-kind ... [--refs ...] [--note ...]
  tk reject | refute | refer <claim_id> --reason "..."
  tk show <claim_id> | tk audit <claim_id> | tk flags [--to <signer>] [--all] | tk stats

  tk doc add <file> [--text ...] [--method opened_source] [--locator ...] | tk doc list  # text + where the original lives

  tk token keygen --out <seed-file>                      # prints the public key for TK_GRANT_PUBKEYS
  tk token issue  --user <u> [--actor <a>] [--key <seed-file>] [--node local] [--aud tk]
                  [--scope tk:read,tk:write] [--ttl 0] [--tenant ...] [--anchor ...]
  tk token revoke <grant_id> --reason "..."              # needs a tk:admin grant

  tk org init --name <name>                              # needs a tk:admin grant, free license
  tk org config get [key] | tk org config set <key> <value>
  tk store info | tk store set <maintainer_name|feedback_email|feedback_url> <value>
  tk seat <email|user> [free|standard|unlimited] [--tenant <id>] [--reason "..."]   # needs a tk:admin grant
  tk instructions [--index] | tk instructions help "<situation>"
  tk instructions tag <tags.json>                        # [{"id":"I-29"|"14", "who":[...], "what":[...], "when":[...], "where":[...], "why":"...", "how":"..."}]
  tk instructions adopt <BP-n> [--translation "..." --lang vi] | tk instructions decline <BP-n> --reason "..."
  tk catalog [list] [--status current|proposed|all] [--situation "..."] [--admin]   # instruction library, publicly readable
  tk catalog add <entries.json> | tk catalog approve|reject|withdraw <BP-n> [--note|--reason "..."]   # needs a tk:admin grant
  tk catalog approve <BP-n> --source "a founder's store, UAE" --on YYYY-MM-DD   # source (no personal names) + incident date
  tk org namespaces list | tk org namespaces add <name> --description "..."
  tk org export [--out dump.json]
  tk org template export --id <id> --out <dir> [--name ...] [--description ...]   # store → template
  tk org import --file dump.json                         # TK_ADMIN_DSN, empty store, before tk-cdc runs

Env: TK_API_URL (http://localhost:8081), CC_URL (http://localhost:8082),
TK_TOKEN (the caller's grant: signer = user_id, producer = actor_id),
TK_GRANT_KEY (seed file for token issue). Each move first emits a ledger_move
cause on the cause-channel, then writes the row with its cause_id.`

var errUsage = errors.New("usage")

// Run dispatches one command. It returns errUsage for a malformed command
// line after printing the usage text.
func (a *App) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(a.Out, usage)
		return errUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "whoami":
		g, err := a.TK.Whoami(ctx)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(g)
		return a.printJSON(raw)
	case "token":
		return a.token(ctx, rest)
	case "org":
		return a.org(ctx, rest)
	case "store":
		return a.store(ctx, rest)
	case "seat":
		return a.seat(ctx, rest)
	case "instructions":
		return a.instructions(ctx, rest)
	case "catalog":
		return a.catalog(ctx, rest)
	case "doc":
		return a.doc(ctx, rest)
	case "propose":
		return a.propose(ctx, rest)
	case "import":
		return a.importCmd(ctx, rest)
	case "promote", "reapprove":
		return a.moveWithBasis(ctx, cmd, rest)
	case "demote", "reject", "refute", "refer":
		return a.move(ctx, cmd, rest)
	case "show", "audit":
		if len(rest) != 1 {
			return a.bad(cmd + " <claim_id>")
		}
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			return a.bad(cmd + " <claim_id>")
		}
		var raw json.RawMessage
		if cmd == "show" {
			raw, err = a.TK.GetClaim(ctx, id)
		} else {
			raw, err = a.TK.GetAudit(ctx, id)
		}
		if err != nil {
			return err
		}
		return a.printJSON(raw)
	case "flags":
		fs := flag.NewFlagSet("flags", flag.ContinueOnError)
		fs.SetOutput(a.Out)
		to := fs.String("to", "", "only flags sent to this signer")
		all := fs.Bool("all", false, "include closed flags")
		if err := fs.Parse(rest); err != nil {
			return errUsage
		}
		raw, err := a.TK.GetFlags(ctx, !*all, *to)
		if err != nil {
			return err
		}
		return a.printJSON(raw)
	case "stats":
		raw, err := a.TK.GetStats(ctx)
		if err != nil {
			return err
		}
		return a.printJSON(raw)
	case "help", "-h", "--help":
		fmt.Fprintln(a.Out, usage)
		return nil
	default:
		return a.bad("unknown command " + cmd)
	}
}

func (a *App) bad(msg string) error {
	fmt.Fprintf(a.Out, "tk: %s\n\n%s\n", msg, usage)
	return errUsage
}

func (a *App) printJSON(raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(a.Out, string(b))
	return nil
}

// signer is the user_id of c's grant, as tk-api verified it. Asked before a
// cause is issued, so a bad grant never leaves a cause without a row.
func (a *App) signer(ctx context.Context, c *tkclient.Client) (string, error) {
	if c.Token == "" {
		return "", errors.New("no grant: set TK_TOKEN (tk token issue --user <you>)")
	}
	if u, ok := a.signers[c.Token]; ok {
		return u, nil
	}
	g, err := c.Whoami(ctx)
	if err != nil {
		return "", err
	}
	if a.signers == nil {
		a.signers = map[string]string{}
	}
	a.signers[c.Token] = g.UserID
	return g.UserID, nil
}

func parseRefs(s string) ([]int64, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []int64
	for _, part := range strings.Split(s, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("--refs: %q is not a claim_id", part)
		}
		out = append(out, id)
	}
	return out, nil
}

// propose: POST /claims with mark A — the only command that needs no cause (a
// proposal claims nothing). Producer defaults to the grant's actor.
func (a *App) propose(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("propose", flag.ContinueOnError)
	fs.SetOutput(a.Out)
	producer := fs.String("producer", "", "who wrote the content, if not you")
	refs := fs.String("refs", "", "supporting claim_ids, comma-separated")
	note := fs.String("note", "", "basis note")
	statement, rest := splitPositional(args)
	if err := fs.Parse(rest); err != nil {
		return errUsage
	}
	if statement == "" {
		return a.bad(`propose "<statement>"`)
	}
	ids, err := parseRefs(*refs)
	if err != nil {
		return err
	}
	var basis *ledger.Basis
	switch {
	case len(ids) > 0:
		basis = &ledger.Basis{Kind: "derivation", Refs: ids, Note: *note}
	case *note != "":
		basis = &ledger.Basis{Kind: "fact", Note: *note}
	}
	id, err := a.TK.ProposeClaim(ctx, statement, basis, *producer)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "claim_id=%d mark=A move=propose\n", id)
	return nil
}

func (a *App) move(ctx context.Context, move string, args []string) error {
	fs := flag.NewFlagSet(move, flag.ContinueOnError)
	fs.SetOutput(a.Out)
	reason := fs.String("reason", "", "reason")
	producer := fs.String("producer", "", "who wrote the content, if not you")
	claimID, rest, ok := splitClaim(args)
	if err := fs.Parse(rest); err != nil {
		return errUsage
	}
	if !ok {
		return a.bad(move + " <claim_id> --reason \"...\"")
	}
	return a.Move(ctx, a.TK, move, claimID, *reason, nil, *producer)
}

func (a *App) moveWithBasis(ctx context.Context, move string, args []string) error {
	fs := flag.NewFlagSet(move, flag.ContinueOnError)
	fs.SetOutput(a.Out)
	reason := fs.String("reason", "", "reason")
	kind := fs.String("basis-kind", "", "derivation | fact | principle (required)")
	refs := fs.String("refs", "", "supporting claim_ids, comma-separated")
	note := fs.String("note", "", "source and date (fact) or a note")
	producer := fs.String("producer", "", "who wrote the content, if not you")
	claimID, rest, ok := splitClaim(args)
	if err := fs.Parse(rest); err != nil {
		return errUsage
	}
	if !ok || *kind == "" {
		return a.bad(move + " <claim_id> --basis-kind <kind>")
	}
	ids, err := parseRefs(*refs)
	if err != nil {
		return err
	}
	return a.Move(ctx, a.TK, move, claimID, *reason, &ledger.Basis{Kind: *kind, Refs: ids, Note: *note}, *producer)
}

// splitClaim takes the positional <claim_id> that the usage puts before the
// flags (Go's flag package stops at the first non-flag argument, so the id is
// peeled off first).
func splitClaim(args []string) (int64, []string, bool) {
	pos, rest := splitPositional(args)
	if pos == "" {
		return 0, rest, false
	}
	id, err := strconv.ParseInt(pos, 10, 64)
	return id, rest, err == nil
}

func splitPositional(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}

// Move is one named move under c's grant: cause first, row second. If the row
// is refused (409 invalid state, 400 missing basis…) the cause stays on
// cause-channel with no row — exactly what CJCM reports as missing_change,
// the honest record of an attempted move that did not happen.
func (a *App) Move(ctx context.Context, c *tkclient.Client, move string, claimID int64, reason string, basis *ledger.Basis, producer string) error {
	signer, err := a.signer(ctx, c)
	if err != nil {
		return err
	}
	causeID, err := a.CC.LedgerMove(ctx, ccclient.LedgerMove{
		ClaimID: claimID, Approver: signer, Producer: signer, Actor: causeActor, Note: move + ": " + reason,
	})
	if err != nil {
		return err
	}
	req := tkclient.MoveReq{Move: move, CauseID: causeID, Reason: reason, Basis: basis, Producer: producer}
	rowID, err := c.Move(ctx, claimID, req)
	if err != nil {
		return fmt.Errorf("%w (cause %s issued, no row written)", err, causeID)
	}
	fmt.Fprintf(a.Out, "%s claim_id=%d row=%d signer=%s cause_id=%s\n", move, claimID, rowID, signer, causeID)
	return nil
}

func (a *App) importCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(a.Out)
	tokensPath := fs.String("tokens", "", `JSON {"<user_id>": "<grant>"} for signers other than the caller`)
	dir, rest := splitPositional(args)
	if err := fs.Parse(rest); err != nil {
		return errUsage
	}
	if dir == "" {
		return a.bad("import <dir> [--tokens tokens.json]")
	}
	tokens := map[string]string{}
	if *tokensPath != "" {
		raw, err := os.ReadFile(*tokensPath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &tokens); err != nil {
			return fmt.Errorf("--tokens: %w", err)
		}
	}
	return a.Import(ctx, dir, tokens)
}

// Import loads fixtures/<case>/. Each line is written under the
// grant of whoever the fixture says owns it — tokens[user], or the caller's
// own grant when it is that user; a fixture cannot sign in someone's name
// without their grant. A first row cannot carry a valid cause — the cause's
// scope must name the claim id, which does not exist until the row is written
// — so a `T` line becomes two named moves: `propose` then `promote` by its
// signer with a cause for the real id. `ref` placeholders in basis.refs are
// translated in file order.
func (a *App) Import(ctx context.Context, dir string, tokens map[string]string) error {
	principles, err := fixtures.LoadPrinciples(filepath.Join(dir, "principles.json"))
	if err != nil {
		return fmt.Errorf("principles.json: %w", err)
	}
	claims, err := fixtures.LoadClaims(filepath.Join(dir, "claims.jsonl"))
	if err != nil {
		return fmt.Errorf("claims.jsonl: %w", err)
	}
	clientFor := func(user string) (*tkclient.Client, error) {
		if tok, ok := tokens[user]; ok {
			return a.TK.WithToken(tok), nil
		}
		if a.TK.Token != "" {
			me, err := a.signer(ctx, a.TK)
			if err != nil {
				return nil, err
			}
			if me == user {
				return a.TK, nil
			}
		}
		return nil, fmt.Errorf("no grant for %q: add it to --tokens", user)
	}
	refs := map[string]int64{}

	for _, p := range principles {
		if p.Signer == "" {
			return fmt.Errorf("principle %s: no signer", p.Code)
		}
		c, err := clientFor(p.Signer)
		if err != nil {
			return fmt.Errorf("principle %s: %w", p.Code, err)
		}
		basis := &ledger.Basis{Kind: "principle", Refs: []int64{}, Note: p.Code}
		id, err := c.ProposeClaim(ctx, p.Statement, basis, "")
		if err != nil {
			return fmt.Errorf("principle %s: %w", p.Code, err)
		}
		if err := a.Move(ctx, c, "promote", id, "import principle "+p.Code, basis, ""); err != nil {
			return fmt.Errorf("principle %s: %w", p.Code, err)
		}
		refs[p.Code] = id
		fmt.Fprintf(a.Out, "principle %s → claim_id=%d\n", p.Code, id)
	}

	for _, c := range claims {
		var basis *ledger.Basis
		if c.Basis != nil {
			b := &ledger.Basis{Kind: c.Basis.Kind, Note: c.Basis.Note, Refs: []int64{}}
			for _, r := range c.Basis.Refs {
				id, ok := refs[r]
				if !ok {
					return fmt.Errorf("claim %s: refs %q not defined earlier in the file", c.Ref, r)
				}
				b.Refs = append(b.Refs, id)
			}
			basis = b
		}
		switch c.Mark {
		case "A":
			// The producer's own grant if we hold it; otherwise the caller, a
			// person, records that producer drafted it.
			client, producer := a.TK, c.Producer
			if tok, ok := tokens[c.Producer]; ok {
				client, producer = a.TK.WithToken(tok), ""
			}
			id, err := client.ProposeClaim(ctx, c.Statement, basis, producer)
			if err != nil {
				return fmt.Errorf("claim %s: %w", c.Ref, err)
			}
			refs[c.Ref] = id
			fmt.Fprintf(a.Out, "claim %s → claim_id=%d mark=A (propose, producer=%s)\n", c.Ref, id, c.Producer)
		case "T":
			if c.Signer == nil || *c.Signer == "" {
				return fmt.Errorf("claim %s: mark T without signer", c.Ref)
			}
			if basis == nil {
				return fmt.Errorf("claim %s: mark T without basis", c.Ref)
			}
			client, err := clientFor(*c.Signer)
			if err != nil {
				return fmt.Errorf("claim %s: %w", c.Ref, err)
			}
			producer := ""
			if c.Producer != *c.Signer {
				producer = c.Producer
			}
			id, err := client.ProposeClaim(ctx, c.Statement, basis, producer)
			if err != nil {
				return fmt.Errorf("claim %s: %w", c.Ref, err)
			}
			if err := a.Move(ctx, client, "promote", id, "import "+c.Ref, basis, producer); err != nil {
				return fmt.Errorf("claim %s: %w", c.Ref, err)
			}
			refs[c.Ref] = id
			fmt.Fprintf(a.Out, "claim %s → claim_id=%d mark=T (propose + promote by %s)\n", c.Ref, id, *c.Signer)
		default:
			return fmt.Errorf("claim %s: mark must be A or T", c.Ref)
		}
	}
	return nil
}

// token: keygen / issue / revoke. keygen and issue are local — a
// dev signing key standing in for Ankayma's control plane; revoke goes to
// tk-api under a tk:admin grant.
func (a *App) token(ctx context.Context, args []string) error {
	sub, rest := splitPositional(args)
	switch sub {
	case "keygen":
		fs := flag.NewFlagSet("token keygen", flag.ContinueOnError)
		fs.SetOutput(a.Out)
		out := fs.String("out", "", "file to write the seed to (never overwritten)")
		if err := fs.Parse(rest); err != nil {
			return errUsage
		}
		if *out == "" {
			return a.bad("token keygen --out <seed-file>")
		}
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(f, grant.EncodeKey(priv.Seed())); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Fprintln(a.Out, grant.EncodeKey(pub))
		return nil
	case "issue":
		fs := flag.NewFlagSet("token issue", flag.ContinueOnError)
		fs.SetOutput(a.Out)
		keyPath := fs.String("key", os.Getenv("TK_GRANT_KEY"), "seed file (default TK_GRANT_KEY)")
		user := fs.String("user", "", "user_id → signer (required)")
		actor := fs.String("actor", "", "actor_id → producer (default = user)")
		node := fs.String("node", "local", "node_id")
		aud := fs.String("aud", "tk", "audience, must equal tk-api's TK_GRANT_AUD")
		scope := fs.String("scope", grant.ScopeRead+","+grant.ScopeWrite, "comma-separated")
		ttl := fs.Duration("ttl", 0, "lifetime, 0 = no expiry")
		tenant := fs.String("tenant", "", "tenant_id")
		anchor := fs.String("anchor", "", "anchor_ref")
		if err := fs.Parse(rest); err != nil {
			return errUsage
		}
		if *user == "" || *keyPath == "" {
			return a.bad("token issue --user <u> --key <seed-file>")
		}
		raw, err := os.ReadFile(*keyPath)
		if err != nil {
			return err
		}
		key, err := grant.ParseSeed(string(raw))
		if err != nil {
			return err
		}
		g := grant.Grant{UserID: *user, ActorID: *actor, NodeID: *node, Aud: *aud, TenantID: *tenant, AnchorRef: *anchor}
		if g.ActorID == "" {
			g.ActorID = g.UserID
		}
		for _, s := range strings.Split(*scope, ",") {
			if s = strings.TrimSpace(s); s != "" {
				g.Scope = append(g.Scope, s)
			}
		}
		if *ttl > 0 {
			g.ExpiresAt = time.Now().Add(*ttl).Unix()
		}
		tok, err := grant.Sign(key, g)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.Out, tok)
		return nil
	case "revoke":
		fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
		fs.SetOutput(a.Out)
		reason := fs.String("reason", "", "reason")
		id, rest := splitPositional(rest)
		if err := fs.Parse(rest); err != nil {
			return errUsage
		}
		if id == "" {
			return a.bad("token revoke <grant_id> --reason \"...\"")
		}
		if err := a.TK.RevokeGrant(ctx, id, *reason); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "revoked %s\n", id)
		return nil
	default:
		return a.bad("token keygen | issue | revoke")
	}
}

// IsUsage reports whether err is the usage error (exit 2).
func IsUsage(err error) bool { return errors.Is(err, errUsage) }

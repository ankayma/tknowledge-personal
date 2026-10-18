package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ankayma/tknowledge-personal/internal/migrate"
	"github.com/ankayma/tknowledge-personal/internal/orgio"
)

// org: the admin functions (grant tk:admin). All go through
// tk-api except import, which restores rows with their ids and therefore runs
// as the database superuser (TK_ADMIN_DSN), like migrations.
func (a *App) org(ctx context.Context, args []string) error {
	sub, rest := splitPositional(args)
	switch sub {
	case "init":
		fs := flag.NewFlagSet("org init", flag.ContinueOnError)
		fs.SetOutput(a.Out)
		name := fs.String("name", "", "org name (required)")
		key := fs.String("license-key", "", "paid license (not supported yet)")
		if err := fs.Parse(rest); err != nil {
			return errUsage
		}
		if *name == "" {
			return a.bad("org init --name <name>")
		}
		raw, err := a.TK.OrgInit(ctx, *name, *key)
		if err != nil {
			return err
		}
		return a.printJSON(raw)

	case "config":
		op, rest := splitPositional(rest)
		switch op {
		case "", "get":
			raw, err := a.TK.OrgConfig(ctx)
			if err != nil {
				return err
			}
			if key, _ := splitPositional(rest); key != "" {
				var all map[string]json.RawMessage
				if err := json.Unmarshal(raw, &all); err != nil {
					return err
				}
				v, ok := all[key]
				if !ok {
					return fmt.Errorf("unknown config key %s", key)
				}
				fmt.Fprintln(a.Out, string(v))
				return nil
			}
			return a.printJSON(raw)
		case "set":
			if len(rest) != 2 {
				return a.bad("org config set <key> <value>")
			}
			value := json.RawMessage(rest[1])
			if !json.Valid(value) {
				value, _ = json.Marshal(rest[1]) // a bare word is a string
			}
			raw, err := a.TK.OrgConfigSet(ctx, rest[0], value)
			if err != nil {
				return err
			}
			return a.printJSON(raw)
		}
		return a.bad("org config get [key] | set <key> <value>")

	case "namespaces":
		op, rest := splitPositional(rest)
		switch op {
		case "", "list":
			raw, err := a.TK.OrgNamespaces(ctx)
			if err != nil {
				return err
			}
			return a.printJSON(raw)
		case "add":
			fs := flag.NewFlagSet("org namespaces add", flag.ContinueOnError)
			fs.SetOutput(a.Out)
			desc := fs.String("description", "", "description (required)")
			name, rest := splitPositional(rest)
			if err := fs.Parse(rest); err != nil {
				return errUsage
			}
			if name == "" || *desc == "" {
				return a.bad("org namespaces add <name> --description \"...\"")
			}
			raw, err := a.TK.OrgNamespaceAdd(ctx, name, *desc)
			if err != nil {
				return err
			}
			return a.printJSON(raw)
		}
		return a.bad("org namespaces list | add <name> --description \"...\"")

	case "export":
		fs := flag.NewFlagSet("org export", flag.ContinueOnError)
		fs.SetOutput(a.Out)
		out := fs.String("out", "", "file to write the dump to (empty = print)")
		if err := fs.Parse(rest); err != nil {
			return errUsage
		}
		raw, err := a.TK.OrgExport(ctx)
		if err != nil {
			return err
		}
		if *out == "" {
			fmt.Fprintln(a.Out, string(raw))
			return nil
		}
		if err := os.WriteFile(*out, raw, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "exported to %s\n", *out)
		return nil

	case "template":
		op, rest := splitPositional(rest)
		if op != "export" {
			return a.bad("org template export --id <id> --out <dir> [--name ...] [--description ...]")
		}
		fs := flag.NewFlagSet("org template export", flag.ContinueOnError)
		fs.SetOutput(a.Out)
		id := fs.String("id", "", "template id (required, also the directory name)")
		name := fs.String("name", "", "display name (default = id)")
		desc := fs.String("description", "", "description")
		out := fs.String("out", "", "parent directory; writes <out>/<id>/ (required)")
		if err := fs.Parse(rest); err != nil {
			return errUsage
		}
		if *id == "" || *out == "" {
			return a.bad("org template export --id <id> --out <dir>")
		}
		raw, err := a.TK.OrgTemplate(ctx, *id, *name, *desc)
		if err != nil {
			return err
		}
		var res struct {
			ID      string            `json:"id"`
			Version int               `json:"version"`
			Files   map[string]string `json:"files"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return err
		}
		dir := filepath.Join(*out, res.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		names := make([]string, 0, len(res.Files))
		for f := range res.Files {
			if f != filepath.Base(f) {
				return fmt.Errorf("unexpected file name %q", f)
			}
			names = append(names, f)
		}
		sort.Strings(names)
		for _, f := range names {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(res.Files[f]), 0o644); err != nil {
				return err
			}
		}
		fmt.Fprintf(a.Out, "template %s@%d → %s (%s)\n", res.ID, res.Version, dir, strings.Join(names, ", "))
		return nil

	case "import":
		fs := flag.NewFlagSet("org import", flag.ContinueOnError)
		fs.SetOutput(a.Out)
		file := fs.String("file", "", "dump from org export (required)")
		if err := fs.Parse(rest); err != nil {
			return errUsage
		}
		if *file == "" {
			return a.bad("org import --file <dump.json>")
		}
		if strings.TrimSpace(a.AdminDSN) == "" {
			return errors.New("TK_ADMIN_DSN required: import restores rows with their ids as the database superuser")
		}
		raw, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		var b orgio.Bundle
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Errorf("%s: %w", *file, err)
		}
		pool, err := pgxpool.New(ctx, a.AdminDSN)
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := migrate.Apply(ctx, pool); err != nil {
			return err
		}
		counts, err := orgio.Import(ctx, pool, b)
		if err != nil {
			return err
		}
		for _, t := range orgio.Tables {
			fmt.Fprintf(a.Out, "%-16s %d\n", t.Name, counts[t.Name])
		}
		return nil
	}
	return a.bad("org init | config | namespaces | template export | export | import")
}

// seat: how much a person writes a month in a store (owner 13/10/2026). With
// no seat named it shows where the person stands; with one it moves them, which
// needs a reason and keeps the change in the history. tk:admin.
func (a *App) seat(ctx context.Context, args []string) error {
	user, rest := splitPositional(args)
	seat, rest := splitPositional(rest)
	fs := flag.NewFlagSet("seat", flag.ContinueOnError)
	fs.SetOutput(a.Out)
	tenant := fs.String("tenant", "", "store; empty = the store that email opened")
	reason := fs.String("reason", "", "reason for the seat change (required when changing)")
	if err := fs.Parse(rest); err != nil || user == "" || fs.NArg() > 0 {
		return a.bad("seat <email|user> [free|standard|unlimited] [--tenant <id>] [--reason \"...\"]")
	}
	var raw json.RawMessage
	var err error
	if seat == "" {
		raw, err = a.TK.Seat(ctx, user, *tenant)
	} else {
		if strings.TrimSpace(*reason) == "" {
			return a.bad("seat " + user + " " + seat + " --reason \"...\"")
		}
		raw, err = a.TK.SetSeat(ctx, user, *tenant, seat, *reason)
	}
	if err != nil {
		return err
	}
	return a.printJSON(raw)
}

// store: the store metadata (owner 5/10/2026). info reads what an
// agent gets at session start; set writes store.<field> through org config,
// so it needs tk:admin and keeps every earlier value.
func (a *App) store(ctx context.Context, args []string) error {
	sub, rest := splitPositional(args)
	switch sub {
	case "", "info":
		raw, err := a.TK.StoreInfo(ctx)
		if err != nil {
			return err
		}
		return a.printJSON(raw)
	case "set":
		if len(rest) != 2 || (rest[0] != "maintainer_name" && rest[0] != "feedback_email" && rest[0] != "feedback_url") {
			return a.bad("store set <maintainer_name|feedback_email|feedback_url> <value>")
		}
		value, _ := json.Marshal(rest[1])
		raw, err := a.TK.OrgConfigSet(ctx, "store."+rest[0], value)
		if err != nil {
			return err
		}
		return a.printJSON(raw)
	}
	return a.bad("store info | store set <field> <value>")
}

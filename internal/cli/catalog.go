package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// catalog: the instruction catalog (owner 14/10/2026,
// spec-instruction-catalog-14-10-26.md). list reads it — the public read, or
// with --admin (or a status only the maintainer sees) proposed and rejected
// entries with their review notes; add proposes entries from a file; approve,
// reject and withdraw are the maintainer's decisions. All but the public list
// need a tk:admin grant.
func (a *App) catalog(ctx context.Context, args []string) error {
	sub, rest := splitPositional(args)
	switch sub {
	case "", "list":
		fs := flag.NewFlagSet("catalog list", flag.ContinueOnError)
		fs.SetOutput(a.Out)
		status := fs.String("status", "", "current (default), proposed, rejected, superseded, withdrawn, all, or a comma list")
		situation := fs.String("situation", "", "only the entries help would offer for this situation")
		admin := fs.Bool("admin", false, "the maintainer's read: proposed and rejected entries, review notes")
		if err := fs.Parse(rest); err != nil || fs.NArg() > 0 {
			return a.bad(`catalog list [--status ...] [--situation "..."] [--admin]`)
		}
		maint := *admin || strings.Contains(*status, "proposed") || strings.Contains(*status, "rejected")
		raw, err := a.TK.Catalog(ctx, *status, *situation, maint)
		if err != nil {
			return err
		}
		return a.printJSON(raw)
	case "add":
		if len(rest) != 1 {
			return a.bad("catalog add <entries.json>   # one entry or an array: {kind?, text, title?, term?, why?, tags, evidence, starter?, supersedes?, review_note?}")
		}
		body, err := os.ReadFile(rest[0])
		if err != nil {
			return err
		}
		var entries []json.RawMessage
		if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '{' {
			entries = []json.RawMessage{trimmed}
		} else if err := json.Unmarshal(body, &entries); err != nil {
			return fmt.Errorf("%s: one entry or a JSON array of entries: %w", rest[0], err)
		}
		var ids []string
		for i, e := range entries {
			raw, err := a.TK.CatalogAdd(ctx, e)
			if err != nil {
				return fmt.Errorf("entry %d: %w — entries before it are proposed: %s", i+1, err, strings.Join(ids, ", "))
			}
			var got struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(raw, &got)
			ids = append(ids, got.ID)
		}
		fmt.Fprintf(a.Out, "proposed %d: %s — tk catalog approve <id> once read\n", len(ids), strings.Join(ids, ", "))
		return nil
	case "approve", "reject", "withdraw":
		id, more := splitPositional(rest)
		fs := flag.NewFlagSet("catalog "+sub, flag.ContinueOnError)
		fs.SetOutput(a.Out)
		note := fs.String("note", "", "why (required to reject)")
		reason := fs.String("reason", "", "why (required to withdraw)")
		source := fs.String("source", "", `approve: the store it came from, without the person ("a founder's store, UAE")`)
		on := fs.String("on", "", "approve: the incident's date, YYYY-MM-DD")
		if err := fs.Parse(more); err != nil || id == "" || fs.NArg() > 0 {
			return a.bad("catalog " + sub + ` <BP-n> [--source "..." --on YYYY-MM-DD] [--note "..."] [--reason "..."]`)
		}
		var raw json.RawMessage
		var err error
		if sub == "withdraw" {
			raw, err = a.TK.CatalogWithdraw(ctx, id, firstNonEmpty(*reason, *note))
		} else {
			raw, err = a.TK.CatalogReview(ctx, id, sub, firstNonEmpty(*note, *reason), *source, *on)
		}
		if err != nil {
			return err
		}
		return a.printJSON(raw)
	}
	return a.bad(`catalog [list] | catalog add <file> | catalog approve|reject|withdraw <BP-n>`)
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

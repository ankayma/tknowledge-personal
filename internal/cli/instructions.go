package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// instructions: the store's instructions as an agent reads them, help for one
// situation, and tags applied from a file (owner 13/10/2026, D-116) — a
// classification is many lines, so it is written once in a file and applied
// line by line, each through the same write the agent's grant would make.
func (a *App) instructions(ctx context.Context, args []string) error {
	sub, rest := "", args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	switch sub {
	case "", "--index":
		raw, err := a.TK.TkaInstructions(ctx, false)
		if sub == "--index" {
			raw, err = a.TK.TkaInstructionIndex(ctx, false)
		}
		if err != nil {
			return err
		}
		return a.printJSON(raw)
	case "help":
		if len(rest) == 0 || strings.TrimSpace(strings.Join(rest, " ")) == "" {
			return a.bad(`instructions help "<situation>"`)
		}
		raw, err := a.TK.TkaHelp(ctx, strings.Join(rest, " "))
		if err != nil {
			return err
		}
		return a.printJSON(raw)
	case "tag":
		if len(rest) != 1 {
			return a.bad("instructions tag <tags.json>")
		}
		body, err := os.ReadFile(rest[0])
		if err != nil {
			return err
		}
		var lines []map[string]any
		if err := json.Unmarshal(body, &lines); err != nil {
			return fmt.Errorf("%s: a JSON array of {id, who, what, when, where, why, how}: %w", rest[0], err)
		}
		for i, l := range lines {
			if _, err := a.TK.TkaInstructionTag(ctx, l); err != nil {
				return fmt.Errorf("line %d (%v): %w — lines before it are tagged", i+1, l["id"], err)
			}
		}
		fmt.Fprintf(a.Out, "tagged %d instructions\n", len(lines))
		return nil
	case "adopt", "decline":
		id, more := splitPositional(rest)
		fs := flag.NewFlagSet("instructions "+sub, flag.ContinueOnError)
		fs.SetOutput(a.Out)
		translation := fs.String("translation", "", "adopt: the entry in the person's language")
		lang := fs.String("lang", "", "adopt: the translation's language, e.g. vi")
		by := fs.String("translated-by", "", "adopt: who translated (default: the grant's actor)")
		reason := fs.String("reason", "", "decline: why")
		if err := fs.Parse(more); err != nil || id == "" || fs.NArg() > 0 || (sub == "decline" && strings.TrimSpace(*reason) == "") {
			return a.bad(`instructions adopt <BP-n> [--translation "..." --lang vi] | instructions decline <BP-n> --reason "..."`)
		}
		var raw json.RawMessage
		var err error
		if sub == "adopt" {
			raw, err = a.TK.TkaInstructionAdd(ctx, map[string]any{"catalog": id, "translation": *translation, "lang": *lang, "translated_by": *by})
		} else {
			raw, err = a.TK.TkaInstructionRetire(ctx, map[string]any{"id": id, "reason": *reason})
		}
		if err != nil {
			return err
		}
		return a.printJSON(raw)
	}
	return a.bad(`instructions [--index] | instructions help "<situation>" | instructions tag <tags.json> | instructions adopt|decline <BP-n>`)
}

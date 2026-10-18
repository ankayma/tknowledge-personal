package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// tk doc — the evidence behind a fact. tk keeps no files (spec T–D §1): a
// document is the text read from the evidence, how it was looked at, and where
// the original lives. The file stays on disk; the store records its path and its
// sha256, so an audit can open it and check it is the same file.
//
//	tk doc add <file> [--text "..."] [--method opened_source] [--locator "..."] [--source-uri ...]
//	tk doc list
//
// A text file is read as it is. Anything else — a PDF, a screenshot — needs
// --text with what it says. After it, admit(...) cites source_doc: D-….
func (a *App) doc(ctx context.Context, args []string) error {
	const usage = "doc add <file> [--text <what it says>] [--method opened_source|rehashed|read_text] " +
		"[--locator <where in the system of record>] [--source-uri <uri>] | doc list"
	if len(args) == 0 {
		return a.bad(usage)
	}
	switch args[0] {
	case "list":
		raw, err := a.TK.TkaDocuments(ctx)
		if err != nil {
			return err
		}
		return a.printJSON(raw)
	case "add":
		fs := flag.NewFlagSet("doc add", flag.ContinueOnError)
		fs.SetOutput(a.Out)
		text := fs.String("text", "", "what the evidence says; required unless the file is text")
		method := fs.String("method", "opened_source", "how you looked at it: opened_source, rehashed, read_text")
		locator := fs.String("locator", "", "where in the system of record, e.g. \"LinkedIn > My jobs > Applied\"")
		sourceURI := fs.String("source-uri", "", "where the original lives; defaults to file://<the file>")
		mediaType := fs.String("type", "", "media type of the original; read from the file's name or its bytes when left out")
		if err := fs.Parse(args[1:]); err != nil {
			return errUsage
		}
		if fs.NArg() != 1 {
			return a.bad(usage)
		}
		path := fs.Arg(0)
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name := filepath.Base(path)
		media := strings.TrimSpace(*mediaType)
		if media == "" {
			media = byExtension(name)
		}
		if media == "" {
			media, _, _ = strings.Cut(http.DetectContentType(body), ";")
		}
		content := strings.TrimSpace(*text)
		if content == "" {
			if !strings.HasPrefix(media, "text/") || !utf8.Valid(body) {
				return fmt.Errorf("%s is %s, not text: give --text with what it says — tk keeps the text, not the file", name, media)
			}
			content = string(body)
		}
		uri := strings.TrimSpace(*sourceURI)
		if uri == "" {
			abs, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			uri = "file://" + filepath.ToSlash(abs)
		}
		sum := sha256.Sum256(body)
		size := len(body)
		raw, err := a.TK.TkaDocumentAdd(ctx, map[string]any{
			"text": content, "text_producer": "owner", "method": *method,
			"locator": *locator, "source_uri": uri, "sha256": hex.EncodeToString(sum[:]),
			"size": size, "media_type": media, "filename": name,
		})
		if err != nil {
			return err
		}
		return a.printJSON(raw)
	}
	return a.bad(usage)
}

// byExtension guesses from the file name; an empty answer lets the bytes decide.
func byExtension(name string) string {
	media := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if media == "" {
		return ""
	}
	if base, _, err := mime.ParseMediaType(media); err == nil {
		return base
	}
	return media
}

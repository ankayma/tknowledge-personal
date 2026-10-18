package tka_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/migrate"
	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// onDoc records what a page says as a document of a, bound to the page, and
// returns a fact basis resting on it. Since 2/10/2026 a fact rests on a document
// its signer added; a bare link is no longer taken (spec T–D, B1).
func onDoc(t *testing.T, r *tka.Repo, a tka.Actor, uri, text string) tka.BasisInput {
	t.Helper()
	sum := sha256.Sum256([]byte(uri + "\n" + text))
	d, err := r.AddDocument(context.Background(), a, tka.DocumentInput{
		Text: text, TextProducer: "owner", Method: "opened_source",
		SourceURI: uri, SHA256: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatalf("record %s: %v", uri, err)
	}
	return tka.BasisInput{Basis: "fact", Where: "the page", SourceDoc: d.ID}
}

// migrateAgain applies every migration once more, as the next boot does.
func migrateAgain(ctx context.Context, r *tka.Repo) error {
	return migrate.Apply(ctx, r.Pool)
}

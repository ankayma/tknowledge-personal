package kb

import (
	"context"
	"encoding/json"
	"strings"

	tpl "github.com/ankayma/tknowledge-personal/internal/setup"
	"github.com/ankayma/tknowledge-personal/internal/subject"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// The Principle List is a trust class of its own (P.9), so it is written as
// claims — one series per code, signed by the owner with a cause like any T —
// and the document principle-list is regenerated from them, to be read.

type PrincipleItem struct {
	Code      string `json:"code"`
	Statement string `json:"statement"`
	// Note is the `[A]` gloss: it adjusts the reading, never derives.
	Note string `json:"note,omitempty"`
}

type PrinciplesSetInput struct {
	Items []PrincipleItem `json:"items"`
}

type PrinciplesSetOutput struct {
	Principles []AdmitResult `json:"principles"`
	DocID      string        `json:"doc_id"`
	Version    int           `json:"version"`
}

// PrinciplesSet writes or revises principles. A code already in the list gets
// a new version of its own series (history per principle); the others are
// untouched.
func (s *Service) PrinciplesSet(ctx context.Context, tk *tkclient.Client, in PrinciplesSetInput) (PrinciplesSetOutput, error) {
	if len(in.Items) == 0 {
		return PrinciplesSetOutput{}, fail("BAD_REQUEST", "items: [{code, statement, note?}] required")
	}
	claims := make([]tkclient.AdmitItem, 0, len(in.Items))
	for _, it := range in.Items {
		code, ok := tpl.NormalizeCode(it.Code)
		if !ok {
			return PrinciplesSetOutput{}, fail("INVALID_CODE", "code must be P.<n>, got "+it.Code)
		}
		if strings.TrimSpace(it.Statement) == "" {
			return PrinciplesSetOutput{}, fail("EMPTY_STATEMENT", code+": statement is required")
		}
		key := "principle:" + tpl.CodeKey(code)
		claims = append(claims, tkclient.AdmitItem{
			Subject: key + ".statement", Statement: it.Statement,
			Label: subject.LabelT, Basis: subject.PrinciplePrefix + code,
		})
		if strings.TrimSpace(it.Note) != "" {
			claims = append(claims, tkclient.AdmitItem{
				Subject: key + ".gloss", Statement: it.Note, Label: subject.LabelA,
			})
		}
	}
	results, err := tk.Admit(ctx, claims)
	if err != nil {
		return PrinciplesSetOutput{}, upstream(err)
	}
	out := PrinciplesSetOutput{Principles: make([]AdmitResult, 0, len(results))}
	for _, r := range results {
		out.Principles = append(out.Principles, s.finish(ctx, tk, r))
	}
	doc, err := s.renderPrincipleList(ctx, tk)
	if err != nil {
		return out, err
	}
	out.DocID, out.Version = doc.DocID, doc.Version
	return out, nil
}

// Principles reads the list as it stands: what Claude loads at the start of a
// chat, and what P.9 lets it trust without auditing.
func (s *Service) Principles(ctx context.Context, tk *tkclient.Client) (json.RawMessage, error) {
	raw, err := tk.Principles(ctx)
	if err != nil {
		return nil, upstream(err)
	}
	return raw, nil
}

// renderPrincipleList rewrites the principle-list document from the claims, so
// the document is a view and the claims are the source.
func (s *Service) renderPrincipleList(ctx context.Context, tk *tkclient.Client) (tkclient.StoreResult, error) {
	raw, err := tk.Principles(ctx)
	if err != nil {
		return tkclient.StoreResult{}, upstream(err)
	}
	var list struct {
		Principles []tpl.Principle `json:"principles"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return tkclient.StoreResult{}, fail("INTERNAL", "read principles: "+err.Error())
	}
	res, err := tk.Store(ctx, tkclient.StoreReq{
		Title: "principle-list", Kind: "rule", Body: tpl.RenderPrinciples(list.Principles),
	})
	if err != nil {
		return tkclient.StoreResult{}, upstream(err)
	}
	return res, nil
}

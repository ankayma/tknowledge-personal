package kb

import (
	"context"
	"encoding/json"

	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// SetupStatus: what the store already holds.
func (s *Service) SetupStatus(ctx context.Context, tk *tkclient.Client) (json.RawMessage, error) {
	raw, err := tk.SetupStatus(ctx)
	if err != nil {
		return nil, upstream(err)
	}
	return raw, nil
}

// SetupTemplates: the templates available, and whether each is complete.
func (s *Service) SetupTemplates(ctx context.Context, tk *tkclient.Client) (json.RawMessage, error) {
	raw, err := tk.SetupTemplates(ctx)
	if err != nil {
		return nil, upstream(err)
	}
	return raw, nil
}

type SetupApplyInput struct {
	TemplateID string `json:"template_id"`
	Overrides  struct {
		Owner string `json:"owner"`
	} `json:"overrides"`
}

type SetupApplyOutput struct {
	Docs            []string      `json:"docs"`
	NamespacesAdded []string      `json:"namespaces_added"`
	Claims          []string      `json:"claims"`
	Results         []AdmitResult `json:"results"`
	// Missing: titles of documents the template lacks — the user writes them
	// (store kind=rule, or principles_set for the principle list).
	Missing []string `json:"missing"`
}

// SetupApply writes a template into the store and promotes its adoption
// claims with causes, like any admitted T. A template missing
// files is applied for what it has.
func (s *Service) SetupApply(ctx context.Context, tk *tkclient.Client, in SetupApplyInput) (SetupApplyOutput, error) {
	res, err := tk.SetupApply(ctx, in.TemplateID, in.Overrides.Owner)
	if err != nil {
		return SetupApplyOutput{}, upstream(err)
	}
	out := SetupApplyOutput{Docs: res.Docs, NamespacesAdded: res.NamespacesAdded, Claims: []string{}, Results: []AdmitResult{}, Missing: res.Missing}
	if out.Docs == nil {
		out.Docs = []string{}
	}
	if out.Missing == nil {
		out.Missing = []string{}
	}
	if out.NamespacesAdded == nil {
		out.NamespacesAdded = []string{}
	}
	for _, c := range res.Claims {
		r := s.finish(ctx, tk, c)
		if r.Error == nil {
			out.Claims = append(out.Claims, r.ID)
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}

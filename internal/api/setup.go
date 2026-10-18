package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ankayma/tknowledge-personal/internal/docs"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/people"
	"github.com/ankayma/tknowledge-personal/internal/setup"
	"github.com/ankayma/tknowledge-personal/internal/subject"
)

// SPEC §17 — setup.status / templates / apply / principles.set. apply writes
// through the same primitives as a hand-built store (namespaces, documents,
// claims), all resting on template:<id>@<version>, so the two cannot be told
// apart afterwards.

var defaultTemplates = setup.MustLoad(setup.Embedded)

func (s *Server) templates() *setup.Registry {
	if s.Templates != nil {
		return s.Templates
	}
	return defaultTemplates
}

type templateView struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Version         int      `json:"version"`
	Namespaces      []string `json:"namespaces"`
	Rules           []string `json:"rules"`
	PrinciplesCount int      `json:"principles_count"`
	Complete        bool     `json:"complete"`
	Missing         []string `json:"missing"`
}

// GET /setup/templates
func (s *Server) getSetupTemplates(w http.ResponseWriter, r *http.Request) {
	out := []templateView{}
	for _, t := range s.templates().All() {
		v := templateView{
			ID: t.ID, Name: t.Name, Description: t.Description, Version: t.Version,
			Namespaces: []string{}, Rules: []string{}, PrinciplesCount: t.PrinciplesCount(),
			Complete: len(t.Missing) == 0, Missing: append([]string{}, t.Missing...),
		}
		for _, ns := range t.Namespaces {
			v.Namespaces = append(v.Namespaces, ns.Name)
		}
		for _, d := range t.Rules {
			v.Rules = append(v.Rules, d.Title)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

// GET /setup/status — what the store already holds (spec §5.13).
func (s *Server) getSetupStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	g := grantFrom(ctx)
	pool := s.Repo.Pool
	fail := func(err error) { writeError(w, http.StatusInternalServerError, "internal", err.Error()) }

	var registered bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, g.UserID).Scan(&registered); err != nil {
		fail(err)
		return
	}
	var principles *string
	var docID int64
	switch err := pool.QueryRow(ctx, `SELECT id FROM docs WHERE title_key = 'principle-list' ORDER BY version DESC LIMIT 1`).Scan(&docID); {
	case err == nil:
		id := subject.FormatDocID(docID)
		principles = &id
	case !errors.Is(err, pgx.ErrNoRows):
		fail(err)
		return
	}
	var rules, claims int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM (SELECT DISTINCT ON (title_key) title_key, kind FROM docs ORDER BY title_key, version DESC) d
		WHERE d.kind = 'rule' AND d.title_key <> 'principle-list'`).Scan(&rules); err != nil {
		fail(err)
		return
	}
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT claim_id) FROM ledger`).Scan(&claims); err != nil {
		fail(err)
		return
	}
	namespaces := []string{}
	rows, err := pool.Query(ctx, `SELECT name FROM namespaces ORDER BY name`)
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			fail(err)
			return
		}
		namespaces = append(namespaces, n)
	}
	rows.Close()
	var applied *string
	var appliedID string
	switch err := pool.QueryRow(ctx, `SELECT value->>'id' FROM org_config WHERE key = 'setup.template_applied' ORDER BY id DESC LIMIT 1`).Scan(&appliedID); {
	case err == nil:
		applied = &appliedID
	case !errors.Is(err, pgx.ErrNoRows):
		fail(err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_registered": registered, "principles": principles, "rules_count": rules,
		"namespaces": namespaces, "claims_count": claims, "template_applied": applied,
	})
}

// POST /setup/apply — {template_id, overrides: {owner}} → {docs, namespaces_added,
// claims, missing}. Documents whose file the template lacks are skipped and
// named in missing (titles) for the user to write.
func (s *Server) postSetupApply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		TemplateID string `json:"template_id"`
		Overrides  struct {
			Owner string `json:"owner"`
		} `json:"overrides"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	tpl, ok := s.templates().Get(req.TemplateID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown_template", "no template "+req.TemplateID)
		return
	}
	g := grantFrom(ctx)
	owner := strings.TrimSpace(req.Overrides.Owner)
	if owner == "" {
		owner = g.UserID
	}
	now := time.Now()

	out, err := func() (map[string]any, error) {
		tx, err := s.Repo.Pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)

		added := []string{}
		for _, ns := range tpl.Namespaces {
			tag, err := tx.Exec(ctx, `INSERT INTO namespaces (name, description, added_by) VALUES ($1, $2, $3) ON CONFLICT (name) DO NOTHING`,
				ns.Name, ns.Description, g.UserID)
			if err != nil {
				return nil, err
			}
			if tag.RowsAffected() == 1 {
				added = append(added, ns.Name)
			}
		}
		basis, err := subject.Basis(tpl.Basis(), now)
		if err != nil {
			return nil, err
		}
		docIDs := []string{}
		claims := []admitResult{}
		propose := func(subj string, p proposal) error {
			parsed, err := subject.Parse(subj)
			if err != nil {
				return err
			}
			stx, err := ledger.LockSubject(ctx, tx, parsed.String())
			if err != nil {
				return err
			}
			p.Subject = parsed.String()
			claim, err := s.proposeInSubject(ctx, stx, g, p)
			if err != nil {
				return err
			}
			claims = append(claims, claim)
			return nil
		}
		for _, d := range tpl.Docs() {
			res, err := docs.StoreTx(ctx, tx, docs.StoreParams{
				Title: d.Title, Kind: "rule", Body: d.Body, UserID: g.UserID, ActorID: g.ActorID, GrantID: g.GrantID, Scope: "user:" + g.UserID,
			})
			if err != nil {
				return nil, err
			}
			docID := subject.FormatDocID(res.Doc.ID)
			docIDs = append(docIDs, docID)

			// The principle list is its own trust class (P.9): one claim series
			// per code, signed by whoever applies the template, resting on
			// template:<id>@<v> — not one "adopted" claim for the document.
			if tpl.Principles != nil && d.Title == tpl.Principles.Title {
				for _, p := range setup.ParsePrinciples(d.Body) {
					axiom := ledger.Basis{Kind: "principle", Refs: []int64{}, Note: basis.Note}
					if err := propose(principleSubject(p.Code, "statement"), proposal{
						Label: subject.LabelT, Mark: "T", Statement: p.Statement, Owner: owner, Basis: &axiom,
					}); err != nil {
						return nil, err
					}
					if p.Note == "" {
						continue
					}
					if err := propose(principleSubject(p.Code, "gloss"), proposal{
						Label: subject.LabelA, Mark: "A", Statement: p.Note, Owner: owner,
					}); err != nil {
						return nil, err
					}
				}
				continue
			}
			b := basis
			if err := propose("rule:"+people.Key(d.Title)+".adopted", proposal{
				Label: subject.LabelT, Mark: "T",
				Statement: d.Title + " adopted from " + tpl.Basis() + " (" + docID + ")", Owner: owner, Basis: &b,
			}); err != nil {
				return nil, err
			}
		}
		value, _ := json.Marshal(map[string]any{"id": tpl.ID, "version": tpl.Version})
		if _, err := tx.Exec(ctx, `INSERT INTO org_config (key, value, set_by, grant_id) VALUES ('setup.template_applied', $1, $2, $3)`,
			value, g.UserID, g.GrantID); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"docs": docIDs, "namespaces_added": added, "claims": claims, "missing": tpl.MissingTitles()}, nil
	}()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

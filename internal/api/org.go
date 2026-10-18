package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/orgio"
	"github.com/ankayma/tknowledge-personal/internal/setup"
	"github.com/ankayma/tknowledge-personal/internal/tka"
	"github.com/ankayma/tknowledge-personal/internal/version"
)

// SPEC §17 — org.init, org.config, org.namespaces, org.export. Admin only
// (grant tk:admin); not exposed through MCP (spec §5.0). Configuration is an
// append-only log: the value of a key is its newest row, else the default.

var namespacePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)

type configSpec struct {
	def   any
	parse func(json.RawMessage) (any, error)
}

func intSetting(def, min, max int) configSpec {
	return configSpec{def: def, parse: func(raw json.RawMessage) (any, error) {
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil || f != math.Trunc(f) || f < float64(min) || f > float64(max) {
			return nil, fmt.Errorf("must be an integer in [%d, %d]", min, max)
		}
		return int(f), nil
	}}
}

func boolSetting(def bool) configSpec {
	return configSpec{def: def, parse: func(raw json.RawMessage) (any, error) {
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, errors.New("must be true or false")
		}
		return b, nil
	}}
}

func stringSetting(max int, check func(string) error) configSpec {
	return configSpec{def: "", parse: func(raw json.RawMessage) (any, error) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, errors.New("must be a string")
		}
		s = strings.TrimSpace(s)
		if len(s) > max {
			return nil, fmt.Errorf("must be at most %d bytes", max)
		}
		if s != "" && check != nil {
			if err := check(s); err != nil {
				return nil, err
			}
		}
		return s, nil
	}}
}

func emailAddress(s string) error {
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s {
		return errors.New("must be a bare email address")
	}
	return nil
}

var configSpecs = map[string]configSpec{
	"open.window_days":   intSetting(7, 1, 365),  // spec §5.4, §11 #3
	"license.grace_days": intSetting(30, 0, 365), // spec §9.2 (license enforcement: later)
	"namespaces.closed":  boolSetting(true),      // spec §4.2, §11 #1
	// Store metadata (owner 5/10/2026, spec §D): who maintains this
	// deployment and where a user reports tk misbehaving. One record for the
	// whole deployment, set by the owner with tk:admin; "" = not set, and an
	// unset feedback address falls back to TK_CONTACT.
	"store.maintainer_name": stringSetting(200, nil),
	"store.feedback_email":  stringSetting(320, emailAddress),
	// A page with the full contact details, for when an address alone is not
	// what the owner wants shown (owner 5/10/2026).
	"store.feedback_url": stringSetting(500, webURL),
}

func webURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("must be an http(s) URL")
	}
	return nil
}

func configKeys() []string {
	keys := make([]string, 0, len(configSpecs))
	for k := range configSpecs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s *Server) config(ctx context.Context, key string) (any, error) {
	spec, ok := configSpecs[key]
	if !ok {
		return nil, fmt.Errorf("unknown config key %s", key)
	}
	var raw []byte
	err := s.Repo.Pool.QueryRow(ctx, `SELECT value FROM org_config WHERE key = $1 ORDER BY id DESC LIMIT 1`, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return spec.def, nil
	}
	if err != nil {
		return nil, err
	}
	return spec.parse(raw)
}

func (s *Server) openWindowDays(ctx context.Context) (int, error) {
	v, err := s.config(ctx, "open.window_days")
	if err != nil {
		return 0, err
	}
	return v.(int), nil
}

// acceptNamespace: a namespace in the catalogue is accepted; one outside it
// only when namespaces.closed is false — and then added to the catalogue when
// createBy is given (admit), not when only referenced (document refs).
func (s *Server) acceptNamespace(ctx context.Context, ns string, createBy *grant.Grant) (bool, error) {
	var known bool
	if err := s.Repo.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM namespaces WHERE name = $1)`, ns).Scan(&known); err != nil {
		return false, err
	}
	if known {
		return true, nil
	}
	closed, err := s.config(ctx, "namespaces.closed")
	if err != nil {
		return false, err
	}
	if closed.(bool) || !namespacePattern.MatchString(ns) {
		return false, nil
	}
	if createBy != nil {
		if _, err := s.Repo.Pool.Exec(ctx, `INSERT INTO namespaces (name, description, added_by) VALUES ($1, $2, $3) ON CONFLICT (name) DO NOTHING`,
			ns, "added by admit (namespaces.closed = false)", createBy.UserID); err != nil {
			return false, err
		}
	}
	return true, nil
}

// POST /org/init — {name, license_key?} → {org_id, admin_user_id, license}.
func (s *Server) postOrgInit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Name       string `json:"name"`
		LicenseKey string `json:"license_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "empty_name", "name is required")
		return
	}
	if strings.TrimSpace(req.LicenseKey) != "" {
		writeError(w, http.StatusNotImplemented, "not_implemented", "paid licenses are not implemented yet; org.init runs with the free license")
		return
	}
	g := grantFrom(ctx)
	idBytes := make([]byte, 6)
	if _, err := rand.Read(idBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	orgID := "org_" + hex.EncodeToString(idBytes)
	license := map[string]any{
		"plan": "free", "seats": map[string]int{"approver": 1, "contributor": 0, "reader": -1},
		"features": []string{}, "issued_at": time.Now().UTC(),
	}
	licenseJSON, _ := json.Marshal(license)

	err := func() error {
		tx, err := s.Repo.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM org)`).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return &requestError{status: http.StatusConflict, code: "already_initialized", detail: "the org exists"}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, name, seat_type) VALUES ($1, $1, 'approver') ON CONFLICT (id) DO NOTHING`, g.UserID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO org (org_id, name, license_json, admin_user_id, grant_id) VALUES ($1, $2, $3, $4, $5)`,
			orgID, strings.TrimSpace(req.Name), licenseJSON, g.UserID, g.GrantID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}()
	if err != nil {
		if !writeRequestError(w, err) {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "admin_user_id": g.UserID, "license": license})
}

// GET /org/config — every key's effective value.
func (s *Server) getOrgConfig(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	for _, k := range configKeys() {
		v, err := s.config(r.Context(), k)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /org/config — {key, value}; a new row, never an overwrite.
func (s *Server) postOrgConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	spec, ok := configSpecs[req.Key]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown_config_key", "key must be one of "+strings.Join(configKeys(), ", "))
		return
	}
	v, err := spec.parse(req.Value)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_config_value", req.Key+" "+err.Error())
		return
	}
	g := grantFrom(r.Context())
	value, _ := json.Marshal(v)
	if _, err := s.Repo.Pool.Exec(r.Context(), `INSERT INTO org_config (key, value, set_by, grant_id) VALUES ($1, $2, $3, $4)`,
		req.Key, value, g.UserID, g.GrantID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": req.Key, "value": v})
}

// GET /org/namespaces
func (s *Server) getOrgNamespaces(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Repo.Pool.Query(r.Context(), `SELECT name, description, added_by, created_at FROM namespaces ORDER BY name`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name, description string
		var addedBy *string
		var created time.Time
		if err := rows.Scan(&name, &description, &addedBy, &created); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		out = append(out, map[string]any{"name": name, "description": description, "added_by": addedBy, "created_at": created})
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /org/namespaces — {name, description} → {name, outcome: new | duplicate}.
func (s *Server) postOrgNamespace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	name := strings.ToLower(strings.TrimSpace(req.Name))
	if !namespacePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_namespace", "name must be a–z, 0–9, _ or -, starting with a letter, at most 40 characters")
		return
	}
	if strings.TrimSpace(req.Description) == "" {
		writeError(w, http.StatusBadRequest, "empty_description", "description is required")
		return
	}
	tag, err := s.Repo.Pool.Exec(r.Context(), `INSERT INTO namespaces (name, description, added_by) VALUES ($1, $2, $3) ON CONFLICT (name) DO NOTHING`,
		name, strings.TrimSpace(req.Description), grantFrom(r.Context()).UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	outcome := "duplicate"
	if tag.RowsAffected() == 1 {
		outcome = "new"
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "outcome": outcome})
}

// GET /org/template?id=&name=&description= — the store as a template for other
// users: namespaces added after the seed, and the latest version of every rule
// document (principle-list as the principle list). No claims, people, threads
// or posts. Version = the loaded template's version plus one, 1 for a new id.
func (s *Server) getOrgTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	id := strings.TrimSpace(q.Get("id"))
	if !setup.IDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_template_id", "id must match "+setup.IDPattern.String())
		return
	}
	tpl := setup.Template{
		ID: id, Name: strings.TrimSpace(q.Get("name")), Description: strings.TrimSpace(q.Get("description")),
		Version: s.templates().NextVersion(id), Namespaces: []setup.Namespace{}, Rules: []setup.Doc{},
	}
	if tpl.Name == "" {
		tpl.Name = id
	}
	fail := func(err error) { writeError(w, http.StatusInternalServerError, "internal", err.Error()) }

	rows, err := s.Repo.Pool.Query(ctx, `SELECT name, description FROM namespaces WHERE added_by IS NOT NULL ORDER BY name`)
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var ns setup.Namespace
		if err := rows.Scan(&ns.Name, &ns.Description); err != nil {
			rows.Close()
			fail(err)
			return
		}
		tpl.Namespaces = append(tpl.Namespaces, ns)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		fail(err)
		return
	}

	// The latest version of each document, kept when it is a rule; in the
	// order the documents were first stored.
	rows, err = s.Repo.Pool.Query(ctx, `
		SELECT title, title_key, body FROM (
			SELECT DISTINCT ON (title_key) title, title_key, kind, body,
			       min(id) OVER (PARTITION BY title_key) AS first_id
			FROM docs ORDER BY title_key, version DESC) d
		WHERE kind = 'rule' ORDER BY first_id`)
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var d setup.Doc
		var key string
		if err := rows.Scan(&d.Title, &key, &d.Body); err != nil {
			rows.Close()
			fail(err)
			return
		}
		if key == "principle-list" {
			tpl.Principles = &d
		} else {
			tpl.Rules = append(tpl.Rules, d)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		fail(err)
		return
	}
	files, err := tpl.Files()
	if err != nil {
		fail(err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "version": tpl.Version, "files": files})
}

// GET /org/export — every table (orgio.Format).
func (s *Server) getOrgExport(w http.ResponseWriter, r *http.Request) {
	// People, conversations and events sit behind row-level security, so the
	// dump runs in the caller's store: it exports that store's rows of them.
	var b orgio.Bundle
	err := s.inTenant(r.Context(), func(q pgx.Tx) error {
		var err error
		b, err = orgio.Export(r.Context(), q)
		return err
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// StoreInfo is the store metadata an agent gets at the start of a session:
// configuration, not knowledge — the newest value of each store.* key, the
// build this deployment runs, and where to send feedback.
type StoreInfo struct {
	MaintainerName string `json:"maintainer_name"`
	FeedbackEmail  string `json:"feedback_email"`
	FeedbackURL    string `json:"feedback_url,omitempty"`
	StoreVersion   string `json:"store_version"`
	// Seat is where the caller stands this month: their plan, writes used and
	// left, and when the month resets. Absent in a store not metered by seat.
	Seat *tka.SeatUsage `json:"seat,omitempty"`
}

// storeInfoFor is storeInfo with the caller's seat in it.
func (s *Server) storeInfoFor(r *http.Request) (StoreInfo, error) {
	info, err := s.storeInfo(r.Context())
	if err != nil {
		return info, err
	}
	a := actorOf(r)
	if u, ok, err := s.tka().SeatUsage(r.Context(), a.Tenant(), a.UserID); err != nil {
		return info, err
	} else if ok {
		info.Seat = &u
	}
	return info, nil
}

func (s *Server) storeInfo(ctx context.Context) (StoreInfo, error) {
	name, err := s.config(ctx, "store.maintainer_name")
	if err != nil {
		return StoreInfo{}, err
	}
	email, err := s.config(ctx, "store.feedback_email")
	if err != nil {
		return StoreInfo{}, err
	}
	page, err := s.config(ctx, "store.feedback_url")
	if err != nil {
		return StoreInfo{}, err
	}
	info := StoreInfo{MaintainerName: name.(string), FeedbackEmail: email.(string), FeedbackURL: page.(string), StoreVersion: version.String()}
	if info.FeedbackEmail == "" {
		info.FeedbackEmail = s.Contact
	}
	return info, nil
}

// GET /store — read by any grant; set with POST /org/config (tk:admin).
func (s *Server) getStoreInfo(w http.ResponseWriter, r *http.Request) {
	info, err := s.storeInfoFor(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

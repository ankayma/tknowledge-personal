// Package people is the person register:
// insert-only records, aliases derived at read — generated from the name plus
// every claim under person:<slug>.alias — and name resolution: one match is
// used, several are refused with candidates, none creates a person only when
// the caller asks (interact, admit), never from a lookup.
package people

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ankayma/tknowledge-personal/internal/subject"
)

type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Stamp is who wrote a record: the grant's user, actor and id, and the store
// (tenant) it was written into. The tenant comes from the grant, and the
// transaction must carry it too (app.tenant_id), or row-level security hides
// every row and refuses every insert.
type Stamp struct {
	UserID, ActorID, GrantID, Scope, TenantID string
}

// Tenant of a stamp; empty is the default store.
func (s Stamp) Tenant() string {
	if strings.TrimSpace(s.TenantID) == "" {
		return DefaultTenant
	}
	return s.TenantID
}

// DefaultTenant is the single-tenant store, the only one the old subject
// ledger belongs to.
const DefaultTenant = "default"

type Person struct {
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	Aliases   []string  `json:"aliases"`
	CreatedAt time.Time `json:"created_at"`
}

// Me is the owner's own actor name; never a person.
const Me = "me"

var ErrName = errors.New("person name is empty, not latin-script, or the reserved word me")

// Key is the comparable form of a name, slug or alias: lowercase, no
// diacritics, only a–z 0–9 _ joined by single hyphens.
func Key(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range subject.Slug(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// AutoAliases: first name, last name, initials.
func AutoAliases(name string) []string {
	words := strings.FieldsFunc(Key(name), func(r rune) bool { return r == '-' })
	if len(words) == 0 {
		return nil
	}
	out := []string{words[0]}
	if len(words) > 1 {
		out = append(out, words[len(words)-1])
		var initials strings.Builder
		for _, w := range words {
			initials.WriteByte(w[0])
		}
		out = append(out, initials.String())
	}
	return out
}

// All returns every person with aliases resolved.
func All(ctx context.Context, q Querier) ([]Person, error) {
	rows, err := q.Query(ctx, `SELECT slug, name, created_at FROM persons ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var all []Person
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.Slug, &p.Name, &p.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Aliases added as claims live in the old subject ledger, which has no
	// tenant: it is the default store's. Another store never reads them — the
	// same slug there is a different person.
	extra := map[string][]string{}
	arows, err := q.Query(ctx, `
		SELECT subject, statement FROM ledger
		WHERE left(subject, 7) = 'person:' AND right(subject, 6) = '.alias'
		  AND move NOT IN ('reject', 'refute', 'resolve', 'drop')
		  AND coalesce(nullif(current_setting('app.tenant_id', true), ''), 'default') = 'default'
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for arows.Next() {
		var subj, statement string
		if err := arows.Scan(&subj, &statement); err != nil {
			arows.Close()
			return nil, err
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(subj, "person:"), ".alias")
		extra[slug] = append(extra[slug], Key(statement))
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return nil, err
	}

	for i := range all {
		aliases := AutoAliases(all[i].Name)
		for _, a := range extra[all[i].Slug] {
			if a != "" && !slices.Contains(aliases, a) {
				aliases = append(aliases, a)
			}
		}
		all[i].Aliases = aliases
	}
	return all, nil
}

// Match compares query with every slug, name and alias.
func Match(all []Person, query string) []Person {
	k := Key(query)
	if k == "" {
		return nil
	}
	var out []Person
	for _, p := range all {
		if k == p.Slug || k == Key(p.Name) || slices.Contains(p.Aliases, k) {
			out = append(out, p)
		}
	}
	return out
}

// fuzzyMin is how close a typed name must come to a slug or a name before a
// lookup offers that person. Lookup only offers; it never resolves.
const fuzzyMin = 0.35

// Lookup is the search behind lookup(q): exact matches on slug, name or alias
// first, then people whose slug or name is close (trigram), for a name typed
// half-remembered or misspelt. It never creates anyone.
func Lookup(ctx context.Context, q Querier, query string) ([]Person, error) {
	all, err := All(ctx, q)
	if err != nil {
		return nil, err
	}
	out := Match(all, query)
	k := Key(query)
	if k == "" {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT slug FROM persons
		WHERE greatest(similarity(slug, $1), similarity(lower(name), $2)) >= $3
		ORDER BY greatest(similarity(slug, $1), similarity(lower(name), $2)) DESC, id`,
		k, strings.ToLower(strings.TrimSpace(query)), fuzzyMin)
	if err != nil {
		return nil, err
	}
	var near []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			rows.Close()
			return nil, err
		}
		near = append(near, slug)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, slug := range near {
		if slices.ContainsFunc(out, func(p Person) bool { return p.Slug == slug }) {
			continue
		}
		for _, p := range all {
			if p.Slug == slug {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// Resolve finds the persons a name refers to; it never creates one.
func Resolve(ctx context.Context, q Querier, query string) ([]Person, error) {
	all, err := All(ctx, q)
	if err != nil {
		return nil, err
	}
	return Match(all, query), nil
}

// Ensure resolves name to exactly one person inside tx, creating one when
// nothing matches. Several matches: returns the candidates and no person.
func Ensure(ctx context.Context, tx pgx.Tx, name string, st Stamp) (Person, []Person, error) {
	key := Key(name)
	if key == "" || key == Me {
		return Person{}, nil, ErrName
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "persons:"+st.Tenant()); err != nil {
		return Person{}, nil, err
	}
	matches, err := Resolve(ctx, tx, name)
	if err != nil {
		return Person{}, nil, err
	}
	switch {
	case len(matches) == 1:
		return matches[0], nil, nil
	case len(matches) > 1:
		return Person{}, matches, nil
	}
	p := Person{}
	if err := tx.QueryRow(ctx, `
		INSERT INTO persons (tenant_id, slug, name, user_id, actor_id, grant_id, scope) VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING slug, name, created_at`, st.Tenant(), key, strings.TrimSpace(name), st.UserID, st.ActorID, st.GrantID, st.Scope,
	).Scan(&p.Slug, &p.Name, &p.CreatedAt); err != nil {
		return Person{}, nil, err
	}
	p.Aliases = AutoAliases(p.Name)
	return p, nil, nil
}

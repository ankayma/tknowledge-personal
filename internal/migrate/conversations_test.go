package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/migrate"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
)

// A store written before 1/10/2026 has `threads`, events.thread_id,
// owner_of_thread, global person slugs and no tenant. Applying the migrations
// renames it to conversations in place — rows, ids and links kept — moves the
// three tables into the default store with row-level security, and a second
// run (every boot) changes nothing and never brings back an empty `threads`.
func TestConversations_RenamesALegacyStore(t *testing.T) {
	ctx := context.Background()
	pool, _, _ := testapi.StartPostgres(t)
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	scalar := func(sql string) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, sql).Scan(&s); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
		return s
	}

	// Put the store back in the shape 0007 used to create.
	exec(`
		DROP POLICY tenant_isolation ON persons; DROP POLICY tenant_isolation ON conversations; DROP POLICY tenant_isolation ON events;
		ALTER TABLE persons DISABLE ROW LEVEL SECURITY; ALTER TABLE conversations DISABLE ROW LEVEL SECURITY; ALTER TABLE events DISABLE ROW LEVEL SECURITY;
		ALTER TABLE conversations DROP CONSTRAINT conversations_person_fkey;
		ALTER TABLE persons DROP CONSTRAINT persons_tenant_slug_key;
		DROP INDEX conversations_post_url_uq, conversations_person_kind_uq, events_conversation_at_idx, events_tenant_actor_at_idx, persons_slug_trgm;
		ALTER TABLE persons DROP COLUMN tenant_id; ALTER TABLE conversations DROP COLUMN tenant_id; ALTER TABLE events DROP COLUMN tenant_id;
		ALTER TABLE persons ADD CONSTRAINT persons_slug_key UNIQUE (slug);
		ALTER TABLE conversations RENAME COLUMN owner_of_conversation TO owner_of_thread;
		ALTER TABLE events RENAME COLUMN conversation_id TO thread_id;
		DROP TRIGGER conversations_append_only ON conversations; DROP TRIGGER conversations_no_truncate ON conversations;
		ALTER TABLE conversations RENAME TO threads;
		ALTER SEQUENCE conversations_id_seq RENAME TO threads_id_seq;
		ALTER TABLE threads ADD CONSTRAINT threads_person_slug_fkey FOREIGN KEY (person_slug) REFERENCES persons(slug);
		CREATE UNIQUE INDEX threads_post_url_uq ON threads (url) WHERE kind = 'post_thread';
		CREATE UNIQUE INDEX threads_person_kind_uq ON threads (kind, person_slug) WHERE kind <> 'post_thread';
		CREATE INDEX events_thread_at_idx ON events (thread_id, at, id);
		CREATE TRIGGER threads_append_only BEFORE UPDATE OR DELETE ON threads FOR EACH ROW EXECUTE FUNCTION tk_append_only();`)
	exec(`
		INSERT INTO persons (id, slug, name, user_id, actor_id, grant_id) VALUES (7, 'mira-patel', 'Mira Patel', 'bao', 'claude', 'g');
		INSERT INTO threads (id, kind, person_slug, owner_of_thread, user_id, actor_id, grant_id)
		  VALUES (12, 'email', 'mira-patel', 'mira-patel', 'bao', 'claude', 'g');
		INSERT INTO events (id, thread_id, actor, type, body, at, user_id, actor_id, grant_id)
		  VALUES (40, 12, 'mira-patel', 'email', 'We would like to make you an offer', '2026-10-01T09:00:00+04', 'bao', 'claude', 'g'),
		         (41, 12, 'me', 'email', 'Thank you', '2026-10-01T10:00:00+04', 'bao', 'claude', 'g');
		SELECT setval('threads_id_seq', 12); SELECT setval('events_id_seq', 41); SELECT setval('persons_id_seq', 7);`)

	for run := 1; run <= 2; run++ {
		if err := migrate.Apply(ctx, pool); err != nil {
			t.Fatalf("apply (run %d): %v", run, err)
		}
	}

	if got := scalar(`SELECT coalesce(to_regclass('threads')::text, 'gone')`); got != "gone" {
		t.Fatalf("threads is back after a re-run: %s", got)
	}
	if got := scalar(`SELECT c.id || '/' || c.owner_of_conversation || '/' || c.tenant_id || '/' ||
		(SELECT string_agg(e.id || ':' || e.conversation_id || ':' || e.tenant_id, ',' ORDER BY e.id) FROM events e) || '/' ||
		(SELECT slug || ':' || tenant_id FROM persons)
		FROM conversations c`); got != "12/mira-patel/default/40:12:default,41:12:default/mira-patel:default" {
		t.Fatalf("rows after the rename: %s", got)
	}
	if got := scalar(`SELECT string_agg(relname || ':' || relrowsecurity, ',' ORDER BY relname) FROM pg_class
		WHERE relname IN ('persons', 'conversations', 'events')`); got != "conversations:true,events:true,persons:true" {
		t.Fatalf("row-level security: %s", got)
	}
	// The sequence kept its place: the next conversation is 13, not 1.
	exec(`INSERT INTO conversations (kind, person_slug, owner_of_conversation, user_id, actor_id, grant_id)
		VALUES ('dm', 'mira-patel', 'mira-patel', 'bao', 'claude', 'g')`)
	if got := scalar(`SELECT max(id)::text FROM conversations`); got != "13" {
		t.Fatalf("sequence: %s", got)
	}
	// The same slug in another store is another person.
	exec(`INSERT INTO persons (tenant_id, slug, name, user_id, actor_id, grant_id) VALUES ('acme', 'mira-patel', 'Mira Patel', 'lan', 'claude', 'g')`)
	if _, err := pool.Exec(ctx, `INSERT INTO persons (slug, name, user_id, actor_id, grant_id) VALUES ('mira-patel', 'x', 'bao', 'claude', 'g')`); err == nil {
		t.Fatal("a slug is still unique inside one store")
	}
	if _, err := pool.Exec(ctx, `UPDATE conversations SET kind = 'dm'`); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("conversations stay append-only: %v", err)
	}
	if got := scalar(`SELECT count(*)::text FROM pg_trigger WHERE tgname LIKE 'threads_%' AND NOT tgisinternal`); got != "0" {
		t.Fatalf("old trigger names left behind: %s", got)
	}
}

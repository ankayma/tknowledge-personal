// Package deploytest checks the credentials in deploy/ against real containers:
// the Redis ACL file and the Postgres init scripts are configuration that
// decides who can forge what, so they get tests like code does.
package deploytest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Redis ACL (deploy/redis/users.acl). The one that matters most: auditd
// (CJCM) must be able to read the four input streams and write aud:*, and must
// NOT be able to XADD to tk:ledger or cc:* — otherwise the monitor could
// manufacture the evidence it audits (C-06).
func TestRedisACL(t *testing.T) {
	ctx := context.Background()
	acl, err := filepath.Abs("../../deploy/redis/users.acl")
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(t.TempDir(), "redis.conf")
	if err := os.WriteFile(conf, []byte("aclfile /etc/redis/users.acl\nappendonly no\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctr, err := tcredis.Run(ctx, "redis:7",
		tcredis.WithConfigFile(conf),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath: acl, ContainerFilePath: "/etc/redis/users.acl", FileMode: 0o644,
		}),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")),
	)
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })
	host, _ := ctr.Host(ctx)
	port, _ := ctr.MappedPort(ctx, "6379/tcp")
	addr := host + ":" + port.Port()

	as := func(user string) *redis.Client {
		c := redis.NewClient(&redis.Options{Addr: addr, Username: user, Password: "dev"})
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	xadd := func(c *redis.Client, stream string) error {
		return c.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: map[string]any{"k": "v"}}).Err()
	}
	mustOK := func(name string, err error) {
		t.Helper()
		if err != nil && err != redis.Nil {
			t.Fatalf("%s: expected success, got %v", name, err)
		}
	}
	mustNoPerm := func(name string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "NOPERM") {
			t.Fatalf("%s: expected NOPERM, got %v", name, err)
		}
	}

	// The default user is off: an unauthenticated client can do nothing.
	if err := as("").Ping(ctx).Err(); err == nil {
		t.Fatal("default user should be disabled")
	}

	cdc, outbox, checker, auditd := as("tk-cdc"), as("cc-outbox"), as("tk-checker"), as("auditd")
	for name, c := range map[string]*redis.Client{"tk-cdc": cdc, "cc-outbox": outbox, "tk-checker": checker, "auditd": auditd} {
		mustOK(name+" handshake", c.Ping(ctx).Err())
	}

	// Producers write exactly their own stream.
	mustOK("tk-cdc XADD tk:ledger", xadd(cdc, "tk:ledger"))
	mustNoPerm("tk-cdc XADD cc:causes", xadd(cdc, "cc:causes"))
	mustNoPerm("tk-cdc XADD aud:alerts", xadd(cdc, "aud:alerts"))
	for _, s := range []string{"cc:causes", "cc:admit", "cc:effects"} {
		mustOK("cc-outbox XADD "+s, xadd(outbox, s))
	}
	mustNoPerm("cc-outbox XADD tk:ledger", xadd(outbox, "tk:ledger"))
	mustNoPerm("cc-outbox XADD aud:alerts", xadd(outbox, "aud:alerts"))

	// auditd: consumer group over the four inputs, writes only under aud:*.
	for _, s := range []string{"tk:ledger", "cc:causes", "cc:admit", "cc:effects"} {
		mustOK("auditd XGROUP "+s, auditd.XGroupCreateMkStream(ctx, s, "auditd", "0").Err())
		mustOK("auditd XREADGROUP "+s, auditd.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: "auditd", Consumer: "t", Streams: []string{s, ">"}, Count: 1, Block: -1}).Err())
		mustNoPerm("auditd XADD "+s, xadd(auditd, s))
	}
	mustOK("auditd XADD aud:alerts", xadd(auditd, "aud:alerts"))
	mustOK("auditd SETBIT aud:c:changed:1", auditd.SetBit(ctx, "aud:c:changed:1", 41, 1).Err())
	mustNoPerm("auditd SET foo", auditd.Set(ctx, "foo", "bar", 0).Err())
	mustNoPerm("auditd FLUSHALL", auditd.FlushAll(ctx).Err())
	mustNoPerm("auditd DEL tk:ledger", auditd.Del(ctx, "tk:ledger").Err())

	// tk-checker: reads aud:alerts only, forges nothing.
	mustOK("tk-checker XGROUP aud:alerts", checker.XGroupCreateMkStream(ctx, "aud:alerts", "tk-checker", "0").Err())
	mustOK("tk-checker XREADGROUP aud:alerts", checker.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: "tk-checker", Consumer: "t", Streams: []string{"aud:alerts", ">"}, Count: 1, Block: -1}).Err())
	mustNoPerm("tk-checker XADD aud:alerts", xadd(checker, "aud:alerts"))
	mustNoPerm("tk-checker XADD tk:ledger", xadd(checker, "tk:ledger"))
	mustNoPerm("tk-checker XREADGROUP tk:ledger", checker.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: "auditd", Consumer: "t", Streams: []string{"tk:ledger", ">"}, Count: 1, Block: -1}).Err())
}

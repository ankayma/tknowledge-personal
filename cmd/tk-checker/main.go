// tk-checker runs the 5s predicate sweep and the CJCM aud:alerts consumer.
// It never writes to `ledger`, only to `flags` and
// `violations_seen` (see migrations/0001_init.sql, 0002_violations_seen.sql
// grants; T-06 proves it can't touch ledger).
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ankayma/tknowledge-personal/internal/checker"
	"github.com/ankayma/tknowledge-personal/internal/config"
	"github.com/ankayma/tknowledge-personal/internal/eff"
	"github.com/ankayma/tknowledge-personal/internal/flags"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("TK_DSN")
	if dsn == "" {
		log.Fatal("TK_DSN not set")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	redisURL := os.Getenv("REDIS")
	if redisURL == "" {
		log.Fatal("REDIS not set")
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("parse REDIS url: %v", err)
	}
	rdb := redis.NewClient(opt)
	defer rdb.Close()

	cfgPath := os.Getenv("TK_CONFIG")
	if cfgPath == "" {
		cfgPath = "config.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config %s: %v", cfgPath, err)
	}

	repo := ledger.New(pool)
	evaluator := eff.New(repo, cfg.AgentsSet())
	flagsRepo := flags.New(pool)
	c := checker.New(repo, evaluator, flagsRepo, rdb)

	// Two independent loops, first one to exit (error or ctx cancel) stops both.
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)

	go func() {
		log.Println("tk-checker: sweep loop starting (poll 5s)")
		errCh <- c.RunSweeps(gctx)
	}()
	go func() {
		log.Println("tk-checker: aud:alerts consumer starting")
		errCh <- c.ConsumeAlerts(gctx)
	}()

	err = <-errCh
	cancel()
	<-errCh // wait for the other loop to unwind too
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("tk-checker: %v", err)
	}
	log.Println("tk-checker: shut down")
}

// tk-cdc relays `ledger` changes from Postgres logical replication into the
// Redis Stream tk:ledger. It is the change channel CJCM audits —
// a feed of the database, not of the API — which is what makes a row inserted
// behind the API's back visible (CJ-10). Env: TK_DSN (role tk_cdc, REPLICATION),
// REDIS (user tk-cdc, XADD on tk:ledger only).
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ankayma/tknowledge-personal/internal/cdc"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("TK_DSN")
	if dsn == "" {
		log.Fatal("TK_DSN not set")
	}
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
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis: %v", err)
	}

	// The publication is created by tk-api's migration; on a cold start we may
	// be ahead of it, so retry instead of dying — compose restarts us anyway,
	// but a clean loop keeps the slot logic in one place.
	for {
		log.Println("tk-cdc: relaying ledger → tk:ledger")
		err := cdc.New(dsn, rdb).Run(ctx)
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		log.Printf("tk-cdc: %v — retrying in 3s", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

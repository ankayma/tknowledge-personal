// Package cdc relays changes to the `ledger` table from Postgres logical
// replication into the Redis Stream tk:ledger. This is the CJ-10 change
// channel: it reads the table's own WAL, not the API, so a row inserted
// straight into the database (bypassing tk-api) still reaches CJCM.
package cdc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/redis/go-redis/v9"
)

const (
	slotName        = "tk_slot"
	publicationName = "tk_ledger_pub"
	outputPlugin    = "pgoutput"
	relayTable      = "ledger"
	streamKey       = "tk:ledger"

	standbyInterval = 5 * time.Second
)

// Relay streams `ledger` changes into the Redis Stream tk:ledger.
type Relay struct {
	pgDSN string
	rdb   redis.Cmdable
}

func New(pgDSN string, rdb redis.Cmdable) *Relay {
	return &Relay{pgDSN: pgDSN, rdb: rdb}
}

// Run connects, ensures the replication slot exists, and streams changes
// until ctx is cancelled or an unrecoverable error occurs.
func (r *Relay) Run(ctx context.Context) error {
	conn, err := pgconn.Connect(ctx, withReplication(r.pgDSN))
	if err != nil {
		return fmt.Errorf("cdc: connect replication: %w", err)
	}
	defer conn.Close(ctx)

	sysident, err := pglogrepl.IdentifySystem(ctx, conn)
	if err != nil {
		return fmt.Errorf("cdc: identify system: %w", err)
	}
	startLSN := sysident.XLogPos

	createResult, err := pglogrepl.CreateReplicationSlot(ctx, conn, slotName, outputPlugin,
		pglogrepl.CreateReplicationSlotOptions{Mode: pglogrepl.LogicalReplication})
	switch {
	case err == nil:
		if lsn, parseErr := pglogrepl.ParseLSN(createResult.ConsistentPoint); parseErr == nil {
			startLSN = lsn
		}
	case isSlotExistsErr(err):
		// Relay restart: slot already tracks its own confirmed_flush_lsn server-side;
		// START_REPLICATION with any startLSN <= that position resumes from there.
	default:
		return fmt.Errorf("cdc: create replication slot: %w", err)
	}

	pluginArgs := []string{"proto_version '1'", fmt.Sprintf("publication_names '%s'", publicationName)}
	if err := pglogrepl.StartReplication(ctx, conn, slotName, startLSN,
		pglogrepl.StartReplicationOptions{PluginArgs: pluginArgs}); err != nil {
		return fmt.Errorf("cdc: start replication: %w", err)
	}

	relations := map[uint32]*pglogrepl.RelationMessage{}
	clientXLogPos := startLSN
	nextStandbyDeadline := time.Now()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if !nextStandbyDeadline.After(time.Now()) {
			if err := pglogrepl.SendStandbyStatusUpdate(ctx, conn, pglogrepl.StandbyStatusUpdate{
				WALWritePosition: clientXLogPos,
				WALFlushPosition: clientXLogPos,
				WALApplyPosition: clientXLogPos,
				ClientTime:       time.Now(),
			}); err != nil {
				return fmt.Errorf("cdc: standby status update: %w", err)
			}
			nextStandbyDeadline = time.Now().Add(standbyInterval)
		}

		recvCtx, cancel := context.WithTimeout(ctx, standbyInterval)
		msg, err := conn.ReceiveMessage(recvCtx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue
			}
			return fmt.Errorf("cdc: receive message: %w", err)
		}

		cd, ok := msg.(*pgproto3.CopyData)
		if !ok || len(cd.Data) == 0 {
			continue
		}

		switch cd.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(cd.Data[1:])
			if err != nil {
				return fmt.Errorf("cdc: parse keepalive: %w", err)
			}
			if pkm.ReplyRequested {
				nextStandbyDeadline = time.Time{}
			}

		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(cd.Data[1:])
			if err != nil {
				return fmt.Errorf("cdc: parse xlogdata: %w", err)
			}
			if err := r.handle(ctx, xld.WALData, relations); err != nil {
				return fmt.Errorf("cdc: handle wal data: %w", err)
			}
			if end := xld.WALStart + pglogrepl.LSN(len(xld.WALData)); end > clientXLogPos {
				clientXLogPos = end
			}
		}
	}
}

func (r *Relay) handle(ctx context.Context, walData []byte, relations map[uint32]*pglogrepl.RelationMessage) error {
	logicalMsg, err := pglogrepl.Parse(walData)
	if err != nil {
		return err
	}

	switch m := logicalMsg.(type) {
	case *pglogrepl.RelationMessage:
		relations[m.RelationID] = m

	case *pglogrepl.InsertMessage:
		rel, ok := relations[m.RelationID]
		if !ok || rel.RelationName != relayTable {
			return nil
		}
		return r.publish(ctx, "INSERT", decodeTuple(rel, m.Tuple))

	case *pglogrepl.UpdateMessage:
		rel, ok := relations[m.RelationID]
		if !ok || rel.RelationName != relayTable {
			return nil
		}
		return r.publish(ctx, "UPDATE", decodeTuple(rel, m.NewTuple))

	case *pglogrepl.DeleteMessage:
		rel, ok := relations[m.RelationID]
		if !ok || rel.RelationName != relayTable {
			return nil
		}
		return r.publish(ctx, "DELETE", decodeTuple(rel, m.OldTuple))
	}
	return nil
}

func (r *Relay) publish(ctx context.Context, op string, fields map[string]string) error {
	return r.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		Values: map[string]interface{}{
			"op":       op,
			"id":       fields["id"],
			"claim_id": fields["claim_id"],
			"move":     fields["move"],
			"signer":   fields["signer"],
			"cause_id": fields["cause_id"],
			"ts":       fields["ts"],
		},
	}).Err()
}

// decodeTuple maps a tuple's text-formatted columns to their names via the
// preceding Relation message. Unchanged TOASTed values ('u') and unrecognized
// formats are omitted — none of the fields this relay publishes are ever
// TOASTed (all short scalars), so this never silently drops a field we need.
func decodeTuple(rel *pglogrepl.RelationMessage, tuple *pglogrepl.TupleData) map[string]string {
	out := make(map[string]string, len(tuple.Columns))
	for i, col := range tuple.Columns {
		if i >= len(rel.Columns) {
			break
		}
		name := rel.Columns[i].Name
		switch col.DataType {
		case 't':
			out[name] = string(col.Data)
		case 'n':
			out[name] = ""
		}
	}
	return out
}

func isSlotExistsErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}

func withReplication(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "replication=database"
}

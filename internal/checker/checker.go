// Package checker implements tk-checker: a 5s sweep that
// re-evaluates the three predicates for claims touched since the last sweep
// (and everything that refs them, transitively) and opens/closes flags —
// plus a separate consumer for CJCM's aud:alerts stream, since an UPDATE or
// DELETE on `ledger` never shows up as a "new row" a sweep would catch.
package checker

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/redis/go-redis/v9"

	"github.com/ankayma/tknowledge-personal/internal/eff"
	"github.com/ankayma/tknowledge-personal/internal/flags"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
)

const (
	PollInterval      = 5 * time.Second
	alertsStream      = "aud:alerts"
	alertsGroup       = "tk-checker"
	previouslyRefuted = "previously_refuted"
)

type Checker struct {
	Repo  *ledger.Repo
	Eff   *eff.Evaluator
	Flags *flags.Repo
	Rdb   redis.Cmdable

	lastSeenID int64
}

func New(repo *ledger.Repo, evaluator *eff.Evaluator, flagsRepo *flags.Repo, rdb redis.Cmdable) *Checker {
	return &Checker{Repo: repo, Eff: evaluator, Flags: flagsRepo, Rdb: rdb}
}

// RunSweeps polls every 5s until ctx is cancelled, running one Sweep each tick.
func (c *Checker) RunSweeps(ctx context.Context) error {
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.Sweep(ctx); err != nil {
				return err
			}
		}
	}
}

// Sweep implements sweep steps 1-4.
func (c *Checker) Sweep(ctx context.Context) error {
	newRows, err := c.Repo.RowsSince(ctx, c.lastSeenID)
	if err != nil {
		return err
	}
	if len(newRows) == 0 {
		return nil
	}

	touched := map[int64]bool{}
	for _, row := range newRows {
		touched[row.ClaimID] = true
	}

	all, err := c.Repo.AllCurrent(ctx)
	if err != nil {
		return err
	}
	for id := range findDependents(touched, all) {
		touched[id] = true
	}

	for claimID := range touched {
		if err := c.checkClaim(ctx, claimID); err != nil {
			return err
		}
	}

	// step 3: a named reapprove/reject/refute answers every open flag on its claim.
	for _, row := range newRows {
		if row.Move == "reapprove" || row.Move == "reject" || row.Move == "refute" {
			if err := c.Flags.CloseAll(ctx, row.ClaimID, row.ID); err != nil {
				return err
			}
		}
	}

	if err := c.checkPreviouslyRefuted(ctx, newRows, all); err != nil {
		return err
	}

	for _, row := range newRows {
		if row.ID > c.lastSeenID {
			c.lastSeenID = row.ID
		}
	}
	return nil
}

// checkClaim re-evaluates the three predicates for a T-marked claim and opens
// a flag for each one that fails. Claims currently marked A aren't checked —
// they aren't claiming to be true, so is_truth's "mark is not T" branch would
// otherwise fire as noise on every ordinary proposal.
func (c *Checker) checkClaim(ctx context.Context, claimID int64) error {
	cur, found, err := c.Repo.Current(ctx, claimID)
	if err != nil {
		return err
	}
	if !found || cur.Mark != "T" {
		return nil
	}

	truth, err := c.Eff.IsTruth(ctx, claimID)
	if err != nil {
		return err
	}
	trust, err := c.Eff.IsTrust(ctx, claimID)
	if err != nil {
		return err
	}
	traceable, err := c.Eff.IsTraceable(ctx, claimID)
	if err != nil {
		return err
	}

	for _, res := range []eff.Result{truth, trust, traceable} {
		if res.OK {
			continue
		}
		if err := c.Flags.Open(ctx, claimID, res.Code, res.FailingID, res.Reason, cur.Signer); err != nil {
			return err
		}
	}
	return nil
}

// findDependents walks the ref graph backwards from seed: every claim
// (transitively) that refs a claim in seed also needs re-checking, since its
// eff may have changed even though it has no new row of its own (F2 cascade).
func findDependents(seed map[int64]bool, all []ledger.Row) map[int64]bool {
	revAdj := map[int64][]int64{}
	for _, row := range all {
		if row.Basis == nil {
			continue
		}
		for _, ref := range row.Basis.Refs {
			revAdj[ref] = append(revAdj[ref], row.ClaimID)
		}
	}

	visited := map[int64]bool{}
	queue := make([]int64, 0, len(seed))
	for id := range seed {
		visited[id] = true
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, dep := range revAdj[id] {
			if !visited[dep] {
				visited[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	return visited
}

// checkPreviouslyRefuted implements sweep step 4: a new propose/import row
// whose statement (normalized) matches a currently-refuted claim gets flagged.
func (c *Checker) checkPreviouslyRefuted(ctx context.Context, newRows, all []ledger.Row) error {
	refuted := map[string]int64{}
	for _, row := range all {
		if row.Move == "refute" {
			refuted[normalize(row.Statement)] = row.ClaimID
		}
	}
	if len(refuted) == 0 {
		return nil
	}

	for _, row := range newRows {
		if row.Move != "propose" && row.Move != "import" {
			continue
		}
		refutedID, ok := refuted[normalize(row.Statement)]
		if !ok || refutedID == row.ClaimID {
			continue
		}
		if err := c.Flags.Open(ctx, row.ClaimID, previouslyRefuted, &refutedID,
			"statement matches a previously refuted claim", nil); err != nil {
			return err
		}
	}
	return nil
}

func normalize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// --- aud:alerts consumer -----------------------------------

var rowViolationKinds = map[string]bool{
	"unjustified_ledger_row": true,
	"ledger_update":          true,
	"ledger_delete":          true,
	"reused_cause":           true,
}

// ConsumeAlerts reads aud:alerts (CJCM's output)
// via consumer group "tk-checker" until ctx is cancelled. Each relevant alert
// populates violations_seen (which is_traceable reads) and immediately opens
// an unjustified_row flag — this is the only path that catches an UPDATE or
// DELETE, since Sweep only ever sees new rows.
func (c *Checker) ConsumeAlerts(ctx context.Context) error {
	if err := c.Rdb.XGroupCreateMkStream(ctx, alertsStream, alertsGroup, "0").Err(); err != nil {
		if !strings.Contains(err.Error(), "BUSYGROUP") {
			return err
		}
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := c.Rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    alertsGroup,
			Consumer: "tk-checker-1",
			Streams:  []string{alertsStream, ">"},
			Block:    5 * time.Second,
			Count:    10,
		}).Result()
		if err != nil {
			if err == redis.Nil {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		for _, s := range res {
			for _, msg := range s.Messages {
				if err := c.handleAlert(ctx, msg.Values); err != nil {
					return err
				}
				if err := c.Rdb.XAck(ctx, alertsStream, alertsGroup, msg.ID).Err(); err != nil {
					return err
				}
			}
		}
	}
}

func (c *Checker) handleAlert(ctx context.Context, values map[string]interface{}) error {
	kind, _ := values["kind"].(string)
	if !rowViolationKinds[kind] {
		return nil
	}
	rowID, err := parseInt64Field(values, "row_id")
	if err != nil {
		return nil // malformed alert — skip rather than stop the consumer
	}
	claimID, err := parseInt64Field(values, "claim_id")
	if err != nil {
		return nil
	}

	if _, err := c.Repo.Pool.Exec(ctx,
		`INSERT INTO violations_seen (row_id, kind) VALUES ($1, $2) ON CONFLICT (row_id) DO NOTHING`,
		rowID, kind,
	); err != nil {
		return err
	}

	var toSigner *string
	if cur, found, err := c.Repo.Current(ctx, claimID); err != nil {
		return err
	} else if found {
		toSigner = cur.Signer
	}

	return c.Flags.Open(ctx, claimID, eff.CodeUnjustifiedRow, &rowID, "CJCM alert: "+kind, toSigner)
}

func parseInt64Field(values map[string]interface{}, key string) (int64, error) {
	v, ok := values[key]
	if !ok {
		return 0, fmt.Errorf("missing field %s", key)
	}
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("field %s is not a string", key)
	}
	return strconv.ParseInt(s, 10, 64)
}

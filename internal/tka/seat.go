package tka

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // the write month is read in one zone, whatever the host has installed

	"github.com/jackc/pgx/v5"
)

// A seat is one person's writes in a store (owner 13/10/2026,
// spec-seat-writes-month-13-10-26.md). A tenant is an organisation; a member is
// a person in it, and the seat is what that person's writes cost: free takes
// 100 a month, standard 1000, unlimited is never counted. The month is the
// calendar month in one zone for the whole deployment (Asia/Dubai unless
// TK_WRITE_TZ says otherwise), so everyone's month closes at the same moment
// and a refusal can name it.

const (
	SeatFree      = "free"
	SeatStandard  = "standard"
	SeatUnlimited = "unlimited"
)

// DefaultWriteTZ is the zone the write month is counted in (owner 13/10/2026).
const DefaultWriteTZ = "Asia/Dubai"

// seatLimits are the writes a month of each seat; a seat missing here is not
// counted.
var seatLimits = map[string]int{SeatFree: 100, SeatStandard: 1000}

// SeatLimit is the writes a month a seat takes; nil for unlimited.
func SeatLimit(seat string) *int {
	if n, ok := seatLimits[seat]; ok {
		return &n
	}
	return nil
}

// ValidSeat reports whether s names a seat.
func ValidSeat(s string) bool {
	return s == SeatFree || s == SeatStandard || s == SeatUnlimited
}

// meteredTier is the tier whose writes are counted by seat. Only the stores
// the door opens are metered in this phase: the default store, a self-hosted
// one and a paid one keep the terms they run on today.
const meteredTier = "trial"

func (r *Repo) writeLoc() *time.Location {
	if r.WriteTZ != nil {
		return r.WriteTZ
	}
	loc, err := time.LoadLocation(DefaultWriteTZ)
	if err != nil {
		panic("tzdata: " + err.Error()) // embedded above; cannot fail
	}
	return loc
}

func (r *Repo) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// WritePeriod is the write month that holds at: its first day in the write
// zone, and the moment the next one starts.
func (r *Repo) WritePeriod(at time.Time) (start, next time.Time) {
	t := at.In(r.writeLoc())
	start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
	return start, start.AddDate(0, 1, 0)
}

// SeatUsage is where one person stands this month.
type SeatUsage struct {
	Plan     string    `json:"plan"`
	Used     int64     `json:"used"`
	Limit    *int      `json:"limit"`
	Left     *int64    `json:"left"`
	ResetsAt time.Time `json:"resets_at"`
	TZ       string    `json:"tz"`
}

// seatOf is the seat of user in tenant; a person who is not a member writes as
// free, so a grant nobody added cannot write without a count.
func seatOf(ctx context.Context, q querier, tenant, user string) (string, error) {
	var seat string
	err := q.QueryRow(ctx, `SELECT seat FROM member WHERE tenant_id = $1 AND user_id = $2`,
		TenantOr(tenant), user).Scan(&seat)
	if errors.Is(err, pgx.ErrNoRows) {
		return SeatFree, nil
	}
	return seat, err
}

// metered reports whether writes in this store are counted by seat.
func metered(t Tenant) bool { return t.Tier == meteredTier }

// SeatUsage reads where user stands this month in tenant. ok is false when the
// store does not count writes by seat.
func (r *Repo) SeatUsage(ctx context.Context, tenant, user string) (u SeatUsage, ok bool, err error) {
	err = r.withTenant(ctx, tenant, func(q querier) error {
		t, known, err := terms(ctx, q, tenant)
		if err != nil || !known || !metered(t) {
			return err
		}
		ok = true
		u, err = r.usage(ctx, q, tenant, user)
		return err
	})
	return u, ok, err
}

func (r *Repo) usage(ctx context.Context, q querier, tenant, user string) (SeatUsage, error) {
	seat, err := seatOf(ctx, q, tenant, user)
	if err != nil {
		return SeatUsage{}, err
	}
	start, next := r.WritePeriod(r.now())
	u := SeatUsage{Plan: seat, Limit: SeatLimit(seat), ResetsAt: next.UTC(), TZ: r.writeLoc().String()}
	err = q.QueryRow(ctx, `SELECT coalesce((SELECT n FROM member_writes
		WHERE tenant_id = $1 AND user_id = $2 AND period = $3), 0)`,
		TenantOr(tenant), user, start.Format("2006-01-02")).Scan(&u.Used)
	if err != nil {
		return SeatUsage{}, err
	}
	if u.Limit != nil {
		left := max(int64(*u.Limit)-u.Used, 0)
		u.Left = &left
	}
	return u, nil
}

// SeatChange is one change of seat, as the history keeps it.
type SeatChange struct {
	Tenant string  `json:"tenant"`
	User   string  `json:"user"`
	From   *string `json:"from"`
	To     string  `json:"to"`
	By     string  `json:"by"`
	Reason string  `json:"reason,omitempty"`
}

// SetSeat puts user on seat in tenant, adding them as a member if they are not
// one yet, and keeps the change in the history. Setting the seat a member
// already has changes nothing and writes no history. A change takes effect on
// the next write; this month's count stays.
func (r *Repo) SetSeat(ctx context.Context, tenant, user, seat, by, reason string) (SeatChange, error) {
	user, seat, by = strings.TrimSpace(user), strings.TrimSpace(seat), strings.TrimSpace(by)
	if user == "" {
		return SeatChange{}, fail("INVALID_MEMBER", "a seat belongs to a person: name one")
	}
	if !ValidSeat(seat) {
		return SeatChange{}, fail("INVALID_SEAT", "%q is not a seat: free, standard or unlimited", seat)
	}
	if by == "" {
		return SeatChange{}, fail("NO_SIGNER", "a seat change carries the name of who made it")
	}
	ch := SeatChange{Tenant: TenantOr(tenant), User: user, To: seat, By: by, Reason: strings.TrimSpace(reason)}
	err := r.withTenant(ctx, tenant, func(q querier) error {
		if _, known, err := terms(ctx, q, tenant); err != nil {
			return err
		} else if !known {
			return fail("TENANT_UNKNOWN", "%s is not a store on this server", TenantOr(tenant))
		}
		var from string
		err := q.QueryRow(ctx, `SELECT seat FROM member WHERE tenant_id = $1 AND user_id = $2 FOR UPDATE`,
			ch.Tenant, user).Scan(&from)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if _, err := q.Exec(ctx, `INSERT INTO member (tenant_id, user_id, seat) VALUES ($1, $2, $3)`,
				ch.Tenant, user, seat); err != nil {
				return err
			}
		case err != nil:
			return err
		case from == seat:
			ch.From = &from
			return nil
		default:
			ch.From = &from
			if _, err := q.Exec(ctx, `UPDATE member SET seat = $3 WHERE tenant_id = $1 AND user_id = $2`,
				ch.Tenant, user, seat); err != nil {
				return err
			}
		}
		_, err = q.Exec(ctx, `INSERT INTO member_seat_history (tenant_id, user_id, seat_from, seat_to, by, reason)
			VALUES ($1, $2, $3, $4, $5, nullif($6, ''))`, ch.Tenant, user, ch.From, seat, by, ch.Reason)
		return err
	})
	return ch, err
}

// SeatHistory lists the seat changes of user in tenant, oldest first.
func (r *Repo) SeatHistory(ctx context.Context, tenant, user string) ([]SeatChange, error) {
	out := []SeatChange{}
	err := r.withTenant(ctx, tenant, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT seat_from, seat_to, by, coalesce(reason, '') FROM member_seat_history
			WHERE tenant_id = $1 AND user_id = $2 ORDER BY id`, TenantOr(tenant), user)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c := SeatChange{Tenant: TenantOr(tenant), User: user}
			if err := rows.Scan(&c.From, &c.To, &c.By, &c.Reason); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// chargeSeat counts one write against the writer's seat for this month. The
// caller has already made sure it runs once per transaction.
func (r *Repo) chargeSeat(ctx context.Context, q querier, a Actor) error {
	tenant := a.Tenant()
	seat, err := seatOf(ctx, q, tenant, a.UserID)
	if err != nil {
		return err
	}
	limit := SeatLimit(seat)
	if limit == nil {
		return nil
	}
	start, next := r.WritePeriod(r.now())
	var n int64
	if err := q.QueryRow(ctx, `
		INSERT INTO member_writes (tenant_id, user_id, period, n) VALUES ($1, $2, $3, 1)
		ON CONFLICT (tenant_id, user_id, period) DO UPDATE SET n = member_writes.n + 1
		RETURNING n`, TenantOr(tenant), a.UserID, start.Format("2006-01-02")).Scan(&n); err != nil {
		return err
	}
	if n <= int64(*limit) {
		return nil
	}
	return failNext("WRITE_LIMIT_REACHED", r.seatLimitNext(ctx, q, seat),
		"your %s seat takes %d writes a month and this month's %d are used; writes open again on %s %s (%s UTC)",
		seat, *limit, *limit, next.Format("2006-01-02 15:04"), r.writeLoc(), next.UTC().Format("2006-01-02 15:04"))
}

// seatLimitNext is the move still open once a seat's month is used up: wait,
// or move the seat up through the deployment's contact.
func (r *Repo) seatLimitNext(ctx context.Context, q querier, seat string) string {
	up := "Standard (1,000 writes a month) or Unlimited"
	if seat == SeatStandard {
		up = "Unlimited"
	}
	contact := r.Contact
	var set string
	if err := q.QueryRow(ctx, `SELECT value #>> '{}' FROM org_config WHERE key = 'store.feedback_email'
		ORDER BY id DESC LIMIT 1`).Scan(&set); err == nil && strings.TrimSpace(set) != "" {
		contact = strings.TrimSpace(set)
	}
	ask := "ask the maintainer named in store_info"
	if contact != "" {
		ask = "write to " + contact
	}
	if r.PricingURL != "" {
		ask += fmt.Sprintf(" (plans: %s)", r.PricingURL)
	}
	return fmt.Sprintf("Reads keep working: ledger, entry, lookup, thread and documents. To write more this month, move this seat to %s: %s.", up, ask)
}

package tka_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ankayma/tknowledge-personal/internal/testapi"
	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// Seats (owner 13/10/2026, spec-seat-writes-month-13-10-26.md): a trial store
// counts writes per person and calendar month in Asia/Dubai — free 100,
// standard 1000, unlimited never counted.

// seatStore opens a metered store and returns a writer in it, with the clock
// stopped at at.
func seatStore(t *testing.T, r *tka.Repo, base tka.Actor, id, user string, at time.Time) tka.Actor {
	t.Helper()
	if _, err := r.CreateTenant(context.Background(), tka.Tenant{ID: id, Tier: "trial"}); err != nil {
		t.Fatal(err)
	}
	r.Clock = func() time.Time { return at }
	a := base
	a.TenantID, a.UserID = id, user
	return a
}

var principleN atomic.Int64

// write is one cheap write: a new principle.
func write(ctx context.Context, r *tka.Repo, a tka.Actor) error {
	n := principleN.Add(1)
	_, err := r.AddPrinciple(ctx, a, fmt.Sprintf("P.%d", n%800+1), fmt.Sprintf("Principle %d holds in every case.", n), "")
	return err
}

func usage(t *testing.T, r *tka.Repo, a tka.Actor) tka.SeatUsage {
	t.Helper()
	u, ok, err := r.SeatUsage(context.Background(), a.TenantID, a.UserID)
	if err != nil || !ok {
		t.Fatalf("usage: %v %v", ok, err)
	}
	return u
}

// setUsed puts this month's count of a at n, as if n writes had been made.
func setUsed(t *testing.T, r *tka.Repo, a tka.Actor, n int) {
	t.Helper()
	start, _ := r.WritePeriod(r.Clock())
	if _, err := r.Pool.Exec(context.Background(), `
		INSERT INTO member_writes (tenant_id, user_id, period, n) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, user_id, period) DO UPDATE SET n = $4`,
		a.TenantID, a.UserID, start.Format("2006-01-02"), n); err != nil {
		t.Fatal(err)
	}
}

func TestSeat_MonthClosesOnTheLastDayInDubai(t *testing.T) {
	r := tka.New(nil)
	for _, c := range []struct {
		at, start, next string
	}{
		// 19:59:59 UTC on 31/10 is 23:59:59 in Dubai: still October.
		{"2026-10-31T19:59:59Z", "2026-10-01", "2026-10-31T20:00:00Z"},
		// 20:00 UTC is midnight in Dubai: November.
		{"2026-10-31T20:00:00Z", "2026-11-01", "2026-11-30T20:00:00Z"},
		// Over the year.
		{"2026-12-31T19:59:59Z", "2026-12-01", "2026-12-31T20:00:00Z"},
		{"2026-12-31T20:00:00Z", "2027-01-01", "2027-01-31T20:00:00Z"},
		// A leap February.
		{"2028-02-29T12:00:00Z", "2028-02-01", "2028-02-29T20:00:00Z"},
		{"2028-02-29T20:00:00Z", "2028-03-01", "2028-03-31T20:00:00Z"},
		// 1/11 02:00 in Dubai is still 31/10 in UTC: November already.
		{"2026-10-31T22:00:00Z", "2026-11-01", "2026-11-30T20:00:00Z"},
	} {
		at, _ := time.Parse(time.RFC3339, c.at)
		start, next := r.WritePeriod(at)
		if got := start.Format("2006-01-02"); got != c.start {
			t.Errorf("%s: month starts %s, want %s", c.at, got, c.start)
		}
		if got := next.UTC().Format(time.RFC3339); got != c.next {
			t.Errorf("%s: next month at %s, want %s", c.at, got, c.next)
		}
	}
	// Another zone, when a deployment sets one.
	hcm, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatal(err)
	}
	r.WriteTZ = hcm
	at, _ := time.Parse(time.RFC3339, "2026-10-31T17:00:00Z")
	if start, _ := r.WritePeriod(at); start.Format("2006-01-02") != "2026-11-01" {
		t.Fatalf("UTC+7: 17:00 UTC on 31/10 is November: %s", start)
	}
}

func TestSeat_FreeTakes100AMonthThenReadsOnly(t *testing.T) {
	ctx := context.Background()
	r, base := newRepo(t)
	r.Contact, r.PricingURL = "hello@example.test", "https://example.test/#pricing"
	at, _ := time.Parse(time.RFC3339, "2026-10-20T10:00:00Z")
	a := seatStore(t, r, base, "free-store", "ann@example.test", at)

	if _, err := r.AddPrinciple(ctx, a, "P.900", "Small moves, each one checkable.", ""); err != nil {
		t.Fatal(err)
	}
	// A move refused for its own reason is not a write.
	if _, err := r.AddPrinciple(ctx, a, "P.900", "Something else entirely.", ""); code(t, err) != "PRINCIPLE_EXISTS" {
		t.Fatalf("taken code: %v", err)
	}
	if u := usage(t, r, a); u.Plan != "free" || u.Used != 1 || *u.Limit != 100 || *u.Left != 99 ||
		u.TZ != "Asia/Dubai" || u.ResetsAt.Format(time.RFC3339) != "2026-10-31T20:00:00Z" {
		t.Fatalf("after one write: %+v", u)
	}

	setUsed(t, r, a, 99)
	if err := write(ctx, r, a); err != nil {
		t.Fatalf("the 100th write: %v", err)
	}
	err := write(ctx, r, a)
	if code(t, err) != "WRITE_LIMIT_REACHED" {
		t.Fatalf("the 101st write: %v", err)
	}
	var e *tka.Error
	errors.As(err, &e)
	for _, want := range []string{"free seat", "100 writes a month", "2026-11-01 00:00 Asia/Dubai", "2026-10-31 20:00 UTC"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("refusal must say %q: %s", want, e.Message)
		}
	}
	for _, want := range []string{"Reads keep working", "Standard", "hello@example.test", "https://example.test/#pricing"} {
		if !strings.Contains(e.Next, want) {
			t.Errorf("next must say %q: %s", want, e.Next)
		}
	}
	if u := usage(t, r, a); u.Used != 100 || *u.Left != 0 {
		t.Fatalf("a refused write over the limit is not counted: %+v", u)
	}
	// Reads keep working.
	if ps, err := r.Principles(ctx, a.TenantID, false); err != nil || len(ps) != 2 {
		t.Fatalf("reads after the limit: %d %v", len(ps), err)
	}

	// The address set on the deployment wins over the built-in contact.
	if _, err := r.Pool.Exec(ctx, `INSERT INTO org_config (key, value, set_by, grant_id)
		VALUES ('store.feedback_email', '"seats@example.test"', 'owner', 'g')`); err != nil {
		t.Fatal(err)
	}
	if err := write(ctx, r, a); !errors.As(err, &e) || !strings.Contains(e.Next, "seats@example.test") {
		t.Fatalf("next must name store.feedback_email: %v", err)
	}

	// The month turns at midnight in Dubai and the writes come back.
	r.Clock = func() time.Time { return time.Date(2026, 10, 31, 20, 0, 0, 0, time.UTC) }
	if err := write(ctx, r, a); err != nil {
		t.Fatalf("a new month: %v", err)
	}
	if u := usage(t, r, a); u.Used != 1 || u.ResetsAt.Format(time.RFC3339) != "2026-11-30T20:00:00Z" {
		t.Fatalf("November: %+v", u)
	}
}

// One move is one write however many guards it runs, and a rolled-back
// transaction costs nothing.
func TestSeat_OnceperMoveRollbackFree(t *testing.T) {
	ctx := context.Background()
	r, base := newRepo(t)
	a := seatStore(t, r, base, "guards", "ben@example.test", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	for _, commit := range []bool{true, false} {
		tx, err := r.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := tka.SetTenant(ctx, tx, a.TenantID); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if err := r.Writable(ctx, tx, a); err != nil {
				t.Fatal(err)
			}
		}
		if commit {
			err = tx.Commit(ctx)
		} else {
			err = tx.Rollback(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if u := usage(t, r, a); u.Used != 1 {
		t.Fatalf("three guards committed once count one, a rollback none: %d", u.Used)
	}
	// An admit linked to a thread runs two guards in one transaction.
	th, err := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "Renewal", Keywords: []string{"acme"}})
	if err != nil {
		t.Fatal(err)
	}
	b := onDoc(t, r, a, "https://example.com/acme-renewal", "Acme renewal email, 2026-10-02: the contract is renewed.")
	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Acme renewed its contract on 2026-10-02",
		BasisInput: b, Thread: th.ID}); err != nil {
		t.Fatal(err)
	}
	if u := usage(t, r, a); u.Used != 4 {
		t.Fatalf("thread, document, admit+link: 1+3 writes, got %d", u.Used)
	}
}

// Writes at the same moment queue on the counter row: with five left, exactly
// five of twenty go through.
func TestSeat_ConcurrentWritesStopAtTheLimit(t *testing.T) {
	ctx := context.Background()
	r, base := newRepo(t)
	a := seatStore(t, r, base, "busy", "cat@example.test", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	setUsed(t, r, a, 95)
	var ok, refused atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			err := write(ctx, r, a)
			var e *tka.Error
			switch {
			case err == nil:
				ok.Add(1)
			case errors.As(err, &e) && e.Code == "WRITE_LIMIT_REACHED":
				refused.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 5 || refused.Load() != 15 {
		t.Fatalf("five left: %d through, %d refused", ok.Load(), refused.Load())
	}
	if u := usage(t, r, a); u.Used != 100 {
		t.Fatalf("count at the limit: %d", u.Used)
	}
}

// Each person counts on their own seat; the same person in two stores counts
// twice; the store's door seed ("trial") counts on nobody's seat.
func TestSeat_CountedPerPersonAndStore(t *testing.T) {
	ctx := context.Background()
	r, base := newRepo(t)
	at := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	ann := seatStore(t, r, base, "org-1", "ann@example.test", at)
	bob := ann
	bob.UserID = "bob@example.test"
	setUsed(t, r, ann, 100)
	if err := write(ctx, r, ann); code(t, err) != "WRITE_LIMIT_REACHED" {
		t.Fatalf("ann is used up: %v", err)
	}
	if err := write(ctx, r, bob); err != nil {
		t.Fatalf("bob writes on his own seat: %v", err)
	}
	ann2 := seatStore(t, r, base, "org-2", "ann@example.test", at)
	if err := write(ctx, r, ann2); err != nil {
		t.Fatalf("ann in another store: %v", err)
	}
	if u := usage(t, r, bob); u.Used != 1 {
		t.Fatalf("bob: %+v", u)
	}
	if u := usage(t, r, ann2); u.Used != 1 {
		t.Fatalf("ann in org-2: %+v", u)
	}
}

func TestSeat_UnlimitedAndChangesMidMonth(t *testing.T) {
	ctx := context.Background()
	r, base := newRepo(t)
	a := seatStore(t, r, base, "owner-store", "owner@example.test", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))

	// Not a member yet: writes as free.
	if u := usage(t, r, a); u.Plan != "free" {
		t.Fatalf("a non-member writes as free: %+v", u)
	}
	setUsed(t, r, a, 100)
	if err := write(ctx, r, a); code(t, err) != "WRITE_LIMIT_REACHED" {
		t.Fatalf("free, used up: %v", err)
	}
	// Up to standard mid-month: the count stays, the limit moves.
	ch, err := r.SetSeat(ctx, a.TenantID, a.UserID, tka.SeatStandard, "admin@example.test", "paid Standard")
	if err != nil || ch.From != nil || ch.To != "standard" {
		t.Fatalf("first seat: %+v %v", ch, err)
	}
	if err := write(ctx, r, a); err != nil {
		t.Fatalf("standard after 100: %v", err)
	}
	if u := usage(t, r, a); u.Used != 101 || *u.Limit != 1000 || *u.Left != 899 {
		t.Fatalf("standard: %+v", u)
	}
	setUsed(t, r, a, 1000)
	err = write(ctx, r, a)
	var e *tka.Error
	if !errors.As(err, &e) || e.Code != "WRITE_LIMIT_REACHED" || strings.Contains(e.Next, "Standard") || !strings.Contains(e.Next, "Unlimited") {
		t.Fatalf("standard used up points to Unlimited only: %v", err)
	}
	// Unlimited: never counted, no row written.
	if _, err := r.SetSeat(ctx, a.TenantID, a.UserID, tka.SeatUnlimited, "admin@example.test", "owner store"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := write(ctx, r, a); err != nil {
			t.Fatalf("unlimited: %v", err)
		}
	}
	if u := usage(t, r, a); u.Plan != "unlimited" || u.Used != 1000 || u.Limit != nil || u.Left != nil {
		t.Fatalf("unlimited counts nothing: %+v", u)
	}
	// Down to free with 1000 used this month: refused again at once.
	if _, err := r.SetSeat(ctx, a.TenantID, a.UserID, tka.SeatFree, "admin@example.test", "ended"); err != nil {
		t.Fatal(err)
	}
	if err := write(ctx, r, a); code(t, err) != "WRITE_LIMIT_REACHED" {
		t.Fatalf("free over its limit: %v", err)
	}
	// Setting the seat a member already has writes no history.
	if _, err := r.SetSeat(ctx, a.TenantID, a.UserID, tka.SeatFree, "admin@example.test", "again"); err != nil {
		t.Fatal(err)
	}
	h, err := r.SeatHistory(ctx, a.TenantID, a.UserID)
	if err != nil || len(h) != 3 || h[0].To != "standard" || *h[1].From != "standard" || h[2].To != "free" ||
		h[0].By != "admin@example.test" || h[0].Reason != "paid Standard" {
		t.Fatalf("history: %+v %v", h, err)
	}
	// Refusals.
	if _, err := r.SetSeat(ctx, a.TenantID, a.UserID, "gold", "admin", "x"); code(t, err) != "INVALID_SEAT" {
		t.Fatalf("unknown seat: %v", err)
	}
	if _, err := r.SetSeat(ctx, "no-such-store", a.UserID, tka.SeatFree, "admin", "x"); code(t, err) != "TENANT_UNKNOWN" {
		t.Fatalf("unknown store: %v", err)
	}
	if _, err := r.SetSeat(ctx, a.TenantID, " ", tka.SeatFree, "admin", "x"); code(t, err) != "INVALID_MEMBER" {
		t.Fatalf("no person: %v", err)
	}
}

// Only the door's trial stores are metered by seat in this phase: the default
// store, a paid store and a self-hosted store keep the terms they run on.
func TestSeat_OnlyTrialStoresAreMetered(t *testing.T) {
	ctx := context.Background()
	r, base := newRepo(t)
	r.Clock = func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }
	if _, err := r.CreateTenant(ctx, tka.Tenant{ID: "acme", Tier: "paid"}); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"", "acme"} {
		a := base
		a.TenantID = tenant
		for range 3 {
			if err := write(ctx, r, a); err != nil {
				t.Fatalf("%q: %v", tenant, err)
			}
		}
		if _, ok, err := r.SeatUsage(ctx, tenant, a.UserID); ok || err != nil {
			t.Fatalf("%q is not metered by seat: %v %v", tenant, ok, err)
		}
	}
	var rows int
	if err := r.Pool.QueryRow(ctx, `SELECT count(*) FROM member_writes`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("no seat counted outside trial stores: %d %v", rows, err)
	}
}

// Seats and counts are kept apart by store at the database, as the ledgers
// are: the API role sees only the store its transaction is set to.
func TestSeat_RowLevelSecurity(t *testing.T) {
	ctx := context.Background()
	admin, _, hostport := testapi.StartPostgres(t)
	r := tka.New(admin)
	r.Clock = func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }
	base := tka.Actor{UserID: "x", ActorID: "claude", GrantID: "g"}
	for _, id := range []string{"org-a", "org-b"} {
		a := seatStore(t, r, base, id, "p@"+id+".test", r.Clock())
		if _, err := r.SetSeat(ctx, id, a.UserID, tka.SeatStandard, "admin", "x"); err != nil {
			t.Fatal(err)
		}
		if err := write(ctx, r, a); err != nil {
			t.Fatal(err)
		}
	}
	api, err := pgxpool.New(ctx, "postgres://tk_api:dev@"+hostport+"/tk?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	tx, err := api.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := tka.SetTenant(ctx, tx, "org-a"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"member", "member_writes", "member_seat_history"} {
		var n int
		var others int
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE tenant_id <> 'org-a') FROM `+table).Scan(&n, &others); err != nil {
			t.Fatal(err)
		}
		if n != 1 || others != 0 {
			t.Fatalf("%s: org-a sees %d rows, %d of another store", table, n, others)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO member (tenant_id, user_id) VALUES ('org-b', 'intruder')`); err == nil {
		t.Fatal("org-a must not add a member to org-b")
	}
}

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// Seats end to end (owner 13/10/2026): a person who opens a store through the
// door writes on a free seat, sees where they stand in store_info, is refused
// with the reset time and the way up at the end of the month, and the owner
// moves them with tk:admin — never through their own grant.
func TestSeat_DoorStoreInfoRefusalAndAdmin(t *testing.T) {
	ctx := context.Background()
	env := newTestServer(t)
	env.door(t)
	env.Server.Contact, env.Server.PricingURL = "hello@example.test", "https://example.test/#pricing-personal"
	env.Server.tka().Clock = func() time.Time { return time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC) }

	if code, out := env.open(t, "ann@example.test"); code != 200 || out.Seat != "free" {
		t.Fatalf("open: %d %+v", code, out)
	}
	var tenant string
	if err := env.Admin.QueryRow(ctx, `SELECT tenant_id FROM trial_owner WHERE email = 'ann@example.test'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	ann := signGrant(t, env.Key, grant.Grant{UserID: "ann@example.test", ActorID: "claude", NodeID: "test",
		Aud: testAud, TenantID: tenant, Scope: []string{grant.ScopeRead, grant.ScopeWrite}})
	admin := env.token(t, "owner", "owner", grant.ScopeAdmin)
	get := func(path, tok string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, env.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	type seatOut struct {
		Plan     string `json:"plan"`
		Used     int64  `json:"used"`
		Limit    *int   `json:"limit"`
		Left     *int64 `json:"left"`
		ResetsAt string `json:"resets_at"`
		TZ       string `json:"tz"`
	}
	storeSeat := func() *seatOut {
		t.Helper()
		resp := get("/v1/store", ann)
		if resp.StatusCode != 200 {
			t.Fatalf("store: %d", resp.StatusCode)
		}
		return decode[struct {
			Seat *seatOut `json:"seat"`
		}](t, resp).Seat
	}
	assume := func(claim string) (int, string, string) {
		t.Helper()
		resp := postJSON(t, env.URL+"/v1/tka/assume", ann, map[string]any{"claim": claim, "type": "A-r", "source": "owner"})
		out := decode[struct {
			Error *struct{ Code, Message, Next string } `json:"error"`
		}](t, resp)
		if out.Error == nil {
			return resp.StatusCode, "", ""
		}
		return resp.StatusCode, out.Error.Code, out.Error.Message + " | " + out.Error.Next
	}

	// store_info: a fresh free seat; the seven seeded principles cost nothing.
	if s := storeSeat(); s == nil || s.Plan != "free" || s.Used != 0 || *s.Limit != 100 || *s.Left != 100 ||
		s.ResetsAt != "2026-10-31T20:00:00Z" || s.TZ != "Asia/Dubai" {
		t.Fatalf("store_info seat: %+v", s)
	}
	if code, c, _ := assume("Ann renews the lease before 2027-01-01"); code != 200 || c != "" {
		t.Fatalf("a write: %d %s", code, c)
	}
	if s := storeSeat(); s.Used != 1 || *s.Left != 99 {
		t.Fatalf("one write: %+v", s)
	}
	// The same seat at session start.
	inst := decode[struct {
		Store struct {
			Seat *seatOut `json:"seat"`
		} `json:"store"`
	}](t, get("/v1/tka/instructions", ann))
	if inst.Store.Seat == nil || inst.Store.Seat.Used != 1 {
		t.Fatalf("instructions carry the seat: %+v", inst.Store.Seat)
	}

	// The month used up: refused with the reset and the way up; reads go on.
	if _, err := env.Admin.Exec(ctx, `UPDATE member_writes SET n = 100 WHERE tenant_id = $1`, tenant); err != nil {
		t.Fatal(err)
	}
	code, c, text := assume("Ann moves office before 2027-02-01")
	if code != http.StatusConflict || c != "WRITE_LIMIT_REACHED" {
		t.Fatalf("over the month: %d %s", code, c)
	}
	for _, want := range []string{"2026-11-01 00:00 Asia/Dubai", "2026-10-31 20:00 UTC", "hello@example.test", "#pricing-personal"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal must say %q: %s", want, text)
		}
	}
	if resp := get("/v1/tka/assumptions", ann); resp.StatusCode != 200 {
		t.Fatalf("reads after the limit: %d", resp.StatusCode)
	}

	// A person cannot move their own seat.
	resp := postJSON(t, env.URL+"/v1/admin/seat", ann, map[string]any{"user": "ann@example.test", "seat": "unlimited", "reason": "me"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("own grant on /admin/seat: %d", resp.StatusCode)
	}

	type view struct {
		Tenant  string `json:"tenant"`
		Metered bool   `json:"metered"`
		Usage   *seatOut
		History []tka.SeatChange `json:"history"`
		Changed *tka.SeatChange  `json:"changed"`
		Error   *struct{ Code string }
	}
	// The owner reads it by address alone: the store is the one the address opened.
	v := decode[view](t, get("/v1/admin/seat?user=ann@example.test", admin))
	if v.Tenant != tenant || !v.Metered || v.Usage == nil || v.Usage.Used != 100 || len(v.History) != 1 ||
		v.History[0].By != "door" || v.History[0].To != "free" {
		t.Fatalf("admin view: %+v", v)
	}
	// A change says why.
	resp = postJSON(t, env.URL+"/v1/admin/seat", admin, map[string]any{"user": "ann@example.test", "seat": "standard"})
	if resp.StatusCode != http.StatusBadRequest || decode[view](t, resp).Error.Code != "EMPTY_REASON" {
		t.Fatalf("no reason: %d", resp.StatusCode)
	}
	resp = postJSON(t, env.URL+"/v1/admin/seat", admin, map[string]any{"user": "ann@example.test", "seat": "gold", "reason": "x"})
	if resp.StatusCode != http.StatusBadRequest || decode[view](t, resp).Error.Code != "INVALID_SEAT" {
		t.Fatalf("unknown seat: %d", resp.StatusCode)
	}
	resp = postJSON(t, env.URL+"/v1/admin/seat", admin, map[string]any{"user": "nobody@example.test", "seat": "free", "reason": "x"})
	if resp.StatusCode != http.StatusNotFound || decode[view](t, resp).Error.Code != "TENANT_UNKNOWN" {
		t.Fatalf("an address with no store: %d", resp.StatusCode)
	}
	resp = postJSON(t, env.URL+"/v1/admin/seat", admin, map[string]any{"user": "ann@example.test", "seat": "unlimited", "reason": "owner store"})
	v = decode[view](t, resp)
	if resp.StatusCode != 200 || v.Changed == nil || v.Changed.From == nil || *v.Changed.From != "free" ||
		v.Changed.By != "owner" || len(v.History) != 2 || v.Usage.Plan != "unlimited" {
		t.Fatalf("to unlimited: %d %+v", resp.StatusCode, v)
	}
	// Writes again at once, uncounted.
	if code, c, _ := assume("Ann moves office before 2027-02-01"); code != 200 || c != "" {
		t.Fatalf("unlimited writes: %d %s", code, c)
	}
	if s := storeSeat(); s.Plan != "unlimited" || s.Limit != nil || s.Left != nil || s.Used != 100 {
		t.Fatalf("unlimited seat: %+v", s)
	}
}

// The default store (a single-store deployment, the owner's own grants) is not
// metered: no seat in store_info, writes as before.
func TestSeat_DefaultStoreNotMetered(t *testing.T) {
	env := newTestServer(t)
	tok := env.token(t, "bao", "bao", grant.ScopeRead, grant.ScopeWrite)
	for i := range 3 {
		resp := postJSON(t, env.URL+"/v1/tka/assume", tok, map[string]any{
			"claim": []string{"Bao renews the lease before 2027-01-01", "Bao moves office before 2027-02-01", "Bao hires an editor before 2027-03-01"}[i],
			"type":  "A-r", "source": "owner"})
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("write %d: %d", i, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, env.URL+"/v1/store", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if out := decode[map[string]any](t, resp); out["seat"] != nil {
		t.Fatalf("no seat in the default store: %v", out)
	}
}

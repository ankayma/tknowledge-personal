package tka_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// A summary is a person's reading of a thread (owner 3/10/2026): an A with an
// author, every sentence tied to an id the thread holds, replaced by a newer
// one rather than edited, and read as stale once the thread changes after it.
func TestSummary_GatesAsOfStaleAndSuperseded(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)

	th, err := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "Martin answers agent risk with identity and scope", Keywords: []string{"battista"}})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Leo Martin answered agent risk with identity, scope, accountability and logging on 2026-10-02",
		BasisInput: fact(t, r, a, "https://example.com/battista-post"), Thread: th.ID})
	if err != nil || answer.Entry == nil {
		t.Fatalf("admit: %+v %v", answer, err)
	}
	reply, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "Leo Martin replies to the comment of 2026-10-03 before 2026-10-08",
		Type: "A-p", Source: "owner", If: "a reply under the comment", Not: "2026-10-08 passes with no reply", Thread: th.ID})
	if err != nil || reply.Entry == nil {
		t.Fatalf("assume: %+v %v", reply, err)
	}
	// Another matter, whose claim this thread does not hold.
	other, err := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "Acme renews its contract", Keywords: []string{"acme"}})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Acme renewed its contract on 2026-10-01",
		BasisInput: fact(t, r, a, "https://example.com/acme"), Thread: other.ID})
	if err != nil || foreign.Entry == nil {
		t.Fatalf("admit foreign: %+v %v", foreign, err)
	}
	T, A, F := answer.Entry.ID, reply.Entry.ID, foreign.Entry.ID

	write := func(text, author, asOf string) (tka.Summary, error) {
		return r.Summarize(ctx, a, tka.SummarizeInput{Thread: th.ID, Text: text, Author: author, AsOf: asOf})
	}
	for _, c := range []struct{ text, author, asOf, code string }{
		{"Martin answered with four controls (" + T + ").", "bot", "", "INVALID_AUTHOR"},
		{"   ", "owner", "", "EMPTY_SUMMARY"},
		{"Martin trả lời bằng identity, scope, owner, log (" + T + "). Đang chờ phản hồi.", "claude", "", "SUMMARY_UNCITED"},
		{"巴蒂斯塔用身份和范围回答（" + T + "）。正在等待回复。", "claude", "", "SUMMARY_UNCITED"},
		{"Martin answered (" + T + "), like Acme did (" + F + ").", "claude", "", "SUMMARY_FOREIGN_ID"},
		{"Martin answered (" + T + ").", "claude", F, "SUMMARY_AS_OF_UNKNOWN"},
		{strings.Repeat("Martin answered "+T+". ", 300), "claude", "", "SUMMARY_TOO_LONG"},
	} {
		if _, err := write(c.text, c.author, c.asOf); code(t, err) != c.code {
			t.Fatalf("%q: want %s, got %v", c.text[:min(len(c.text), 60)], c.code, err)
		}
	}

	// Written: the store stamps the newest entry of the thread as as_of. A
	// sentence may cite the thread itself, but that alone is no citation.
	s1, err := write("Trong "+th.ID+": Martin trả lời bằng identity, scope, owner, log ("+T+"). Đang chờ phản hồi 5 ngày ("+A+").", "claude", "")
	if err != nil || s1.ID != "S-1" || s1.AsOf == nil || *s1.AsOf != A || s1.Status != "current" || s1.Author != "claude" || s1.Signer != "bao" {
		t.Fatalf("summarize: %+v %v", s1, err)
	}
	v, err := r.ReadThread(ctx, "", th.ID, 1, false)
	if err != nil || v.Summary == nil || v.Summary.ID != "S-1" || v.Summary.Stale || !strings.Contains(v.Counts, T) {
		t.Fatalf("thread shows the summary, fresh, and the counts apart: %+v %v", v.Summary, err)
	}

	// A move on a claim the thread holds makes the summary stale.
	if _, err := r.Demote(ctx, a, T, "the post was edited to drop the log", ""); err != nil {
		t.Fatal(err)
	}
	if v, _ = r.ReadThread(ctx, "", th.ID, 1, false); v.Summary == nil || !v.Summary.Stale {
		t.Fatalf("a demote after the summary makes it stale: %+v", v.Summary)
	}

	// A new summary is current and fresh; it may stop earlier than the newest
	// entry. The old one stays readable as superseded.
	s2, err := write("Martin's answer no longer stands ("+T+"); still waiting for a reply ("+A+").", "owner", T)
	if err != nil || s2.ID != "S-2" || *s2.AsOf != T {
		t.Fatalf("second summary: %+v %v", s2, err)
	}
	if v, _ = r.ReadThread(ctx, "", th.ID, 1, false); v.Summary == nil || v.Summary.ID != "S-2" || v.Summary.Stale {
		t.Fatalf("the newer summary is current and fresh: %+v", v.Summary)
	}
	old, err := r.Read(ctx, "", "s-0001")
	if err != nil || old.Kind != "summary" || old.Summary.Status != "superseded" || old.Summary.Text != s1.Text {
		t.Fatalf("entry(S-1): %+v %v", old.Summary, err)
	}
	cur, err := r.Read(ctx, "", "S-2")
	if err != nil || cur.Summary.Status != "current" || cur.Summary.Stale {
		t.Fatalf("entry(S-2): %+v %v", cur.Summary, err)
	}

	// A link on the thread is a write to it too.
	if _, err := r.Link(ctx, a, tka.LinkInput{Thread: th.ID, IDs: []string{F}}); err != nil {
		t.Fatal(err)
	}
	if v, _ = r.ReadThread(ctx, "", th.ID, 1, false); !v.Summary.Stale {
		t.Fatalf("a link after the summary makes it stale: %+v", v.Summary)
	}
	// Now that F belongs to the thread, a summary may cite it.
	if _, err := write("Acme's renewal now sits in this matter too ("+F+").", "claude", ""); err != nil {
		t.Fatalf("cite a newly linked claim: %v", err)
	}

	// A closed thread takes no summary.
	if _, err := r.CloseThread(ctx, a, tka.ThreadCloseInput{Thread: th.ID, Reason: "answered"}); err != nil {
		t.Fatal(err)
	}
	if _, err := write("Closed after the answer ("+T+").", "owner", ""); code(t, err) != "THREAD_CLOSED" {
		t.Fatalf("closed thread: %v", err)
	}
	if _, err := r.Read(ctx, "", "S-99"); code(t, err) != "NOT_FOUND" {
		t.Fatalf("unknown summary: %v", err)
	}
}

// A summary is one write against a trial store's cap.
func TestSummary_CountsAsOneWrite(t *testing.T) {
	ctx := context.Background()
	r, base := newRepo(t)
	if _, err := r.CreateTenant(ctx, tka.Tenant{ID: "capped", Tier: "trial"}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetWriteCap(ctx, "capped", 100); err != nil {
		t.Fatal(err)
	}
	a := base
	a.TenantID = "capped"
	th, err := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "A small matter", Keywords: []string{"small"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Link(ctx, a, tka.LinkInput{Thread: th.ID, IDs: []string{"P.1"}}); code(t, err) != "LINK_TARGET_UNKNOWN" {
		t.Fatalf("no principles in this store: %v", err)
	}
	if _, err := r.AddPrinciple(ctx, a, "P.1", "Small moves, each one checkable.", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Link(ctx, a, tka.LinkInput{Thread: th.ID, IDs: []string{"P.1"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := r.ReadTenant(ctx, "capped")
	if _, err := r.Summarize(ctx, a, tka.SummarizeInput{Thread: th.ID, Text: "We work in small moves (P.1).", Author: "owner"}); err != nil {
		t.Fatal(err)
	}
	after, _ := r.ReadTenant(ctx, "capped")
	if after.Writes != before.Writes+1 {
		t.Fatalf("a summary is one write: %d → %d", before.Writes, after.Writes)
	}
}

// One word cap means about the same sentence in every language: tokens between
// spaces, and two characters per word for scripts written without spaces.
func TestClaimWords(t *testing.T) {
	for _, c := range []struct {
		lang, s string
		want    int
	}{
		{"English", "Acme renewed the contract on 2026-10-02", 6},
		{"Vietnamese", "Nguyễn Văn Bảo phỏng vấn vòng ba ngày 2026-10-02", 9},
		{"German", "Die Vertragsverlängerungsvereinbarung wurde am 2026-10-02 unterzeichnet", 6},
		// 阿克米公司于 (6) → 3, 年 → 1, 月 → 1, 日续签了合同 (6) → 3, plus the digits → 1
		{"Chinese", "阿克米公司于2026年10月2日续签了合同", 9},
		// アクメ社は (5) → 3, 年 → 1, 月 → 1, 日に契約を更新した (9) → 5, plus the digits → 1
		{"Japanese", "アクメ社は2026年10月2日に契約を更新した", 11},
		{"Korean", "애크미는 2026년 10월 2일 계약을 갱신했다", 6},
		{"Hindi", "एक्मे ने 2026-10-02 को अनुबंध का नवीनीकरण किया", 8},
		{"Emoji only", "✅ 👩‍👩‍👧‍👦 🇻🇳", 0},
		{"Mixed", "Team 👩‍👩‍👧‍👦 signed", 2},
	} {
		if got := tka.ClaimWords(c.s); got != c.want {
			t.Errorf("%s: ClaimWords(%q) = %d, want %d", c.lang, c.s, got, c.want)
		}
	}
	// Thai has no spaces between words: about two characters a word, so a
	// sentence counts close to its English rendering, never one word per token.
	if n := tka.ClaimWords("แอคมีต่อสัญญาเมื่อวันที่ 2 ตุลาคม 2569"); n < 8 || n > 16 {
		t.Errorf("Thai: %d words", n)
	}
	// NFD and NFC count the same.
	if tka.ClaimWords("Nguyễn Văn") != tka.ClaimWords("Nguyễn Văn") {
		t.Error("decomposed Vietnamese counts differently")
	}
}

// A claim is one sentence: past the store's word cap it is refused, and so is
// a reworded assumption; check_claim says the count first.
func TestClaim_TooManyWords(t *testing.T) {
	ctx := context.Background()
	r, base := newRepo(t)
	five, backstop := 5, 600
	if _, err := r.CreateTenant(ctx, tka.Tenant{ID: "short", Tier: "trial", MaxWords: &five, MaxClaim: &backstop}); err != nil {
		t.Fatal(err)
	}
	a := base
	a.TenantID = "short"
	long := "Acme renewed its large contract with Globex on 2026-10-02"
	if n := tka.CheckClaim(long, "").Words; n != 9 {
		t.Fatalf("check_claim reports the words: %d", n)
	}
	if _, err := r.Assume(ctx, a, tka.AssumeInput{Claim: long, Type: "A-r", Source: "owner"}); code(t, err) != "CLAIM_TOO_MANY_WORDS" {
		t.Fatalf("over the word cap: %v", err)
	}
	ok, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "Acme renewed on 2026-10-02", Type: "A-r", Source: "owner"})
	if err != nil || ok.Entry == nil {
		t.Fatalf("under the cap: %+v %v", ok, err)
	}
	if _, err := r.UpdateAssumption(ctx, a, tka.UpdateInput{AssumptionID: ok.Entry.ID, Claim: &long}); code(t, err) != "CLAIM_TOO_MANY_WORDS" {
		t.Fatalf("rewording past the cap: %v", err)
	}
	ten, err := r.ReadTenant(ctx, "short")
	if err != nil || ten.MaxWords == nil || *ten.MaxWords != 5 {
		t.Fatalf("terms carry the word cap: %+v %v", ten, err)
	}
}

package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ankayma/tknowledge-personal/internal/flags"
)

// A flag sent over REST must use the exact column names of the flags table,
// with evidence as inline JSON — demo.sh and every jq client read .kind,
// .claim_id and .evidence.failing_id.
func TestFlagResp_SnakeCaseAndInlineEvidence(t *testing.T) {
	to := "CRO"
	list := []flags.Flag{{
		ID: 1, ClaimID: 5, Kind: "premise_demoted",
		Evidence: []byte(`{"failing_id": 3, "reason": "derivation ref is not eff=T"}`),
		ToSigner: &to, TS: time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC),
	}}
	b, err := json.Marshal(toFlagResps(list))
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	f := got[0]
	if f["kind"] != "premise_demoted" || f["claim_id"] != float64(5) || f["to_signer"] != "CRO" || f["closed_by"] != nil {
		t.Fatalf("unexpected keys/values: %s", b)
	}
	ev, ok := f["evidence"].(map[string]any)
	if !ok || ev["failing_id"] != float64(3) {
		t.Fatalf("evidence must be inline JSON, got %s", b)
	}
	if string(must(json.Marshal(toFlagResps(nil)))) != "[]" {
		t.Fatalf("empty list must be [], got %s", must(json.Marshal(toFlagResps(nil))))
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

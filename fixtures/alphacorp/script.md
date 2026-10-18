# AlphaCorp — script

Seven steps. Steps 1–3 run inside `t-knowledge-system` on their own (they
demonstrate F1 and F2). Steps 4–7 need `cause-channel` and
`cause-justified-change-monitoring` (CJCM); at the time of writing these were
not built yet, so they are marked "pending" below.

| # | Step | Status |
|---|---|---|
| 1 | `tk import fixtures/alphacorp/`: 102 and 880 (`fact`, T) each become a `propose` + `promote` by the CRO with a cause; 905 (`A`, loan-agent) is a `propose`. Placeholder `refs` are translated to real claim_ids | runs — `internal/cli` |
| 2 | First sweep: 102, 880 `eff=T` (facts with a note, not expired); 905 still `A` (not promoted) | runs |
| 3 | `TK_TOKEN=<CRO grant> tk promote <905> --reason "facility approved" --basis-kind derivation --refs <102>,<880>` → `eff(905)=T` | runs — `internal/cli` |
| 4 | The CRO issues cause `C1` (`grant_limit`, 5,000,000, `claims: [905]`) on cause-channel; the agent disburses through `/admit` → allow | pending — needs cause-channel |
| 5 | `TK_TOKEN=<Alice grant> tk demote <880> --reason "valuation revised down"` → `eff(905)=A`, `premise_demoted` flag to the CRO, `failing_id=880` | runs (F2) — `internal/cli` |
| 5b | The agent withdraws 200k through `/admit` under cause `C1` → deny, with the reason pointing at `failing_claim=905`, `failing_id=880` | pending — needs cause-channel |
| 6 | The CRO issues cause `C2` (`revoke_limit`); the agent carries out the revocation → allow | pending — needs cause-channel |
| 7 | Insert a `[T]` row straight into `ledger` as superuser (bypassing the API) → `Traceable` goes red and the row appears in V(w); a money transfer that skips `/admit` → its own V(w) | the ledger part runs (proven in T-08, `internal/cdc`); the "transfer that skips /admit" part needs cause-channel + CJCM |

`internal/cli/cli_test.go` runs steps 1, 3 and 5 through the real CLI (real tk-api, a fake cause-channel that records causes); `internal/fixtures/fixtures_test.go` runs steps 1–3 and 5 end to end through the real REST API
(no shortcut through the repo layer), confirming F1 (the T-ratio after the first sweep) and F2
(cascade demote → `premise_demoted` flag, with `failing_id` pointing at claim 880).

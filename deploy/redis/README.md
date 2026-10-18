# Redis ACL (`users.acl`)

Redis does not allow comments in an ACL file (every line must start with `user`), so the notes live here.
Checked by `internal/deploytest/redisacl_test.go` against a real Redis 7 container: if you edit `users.acl`,
that test must stay green. `+hello +ping +client|setinfo` is the go-redis handshake and touches no data.

| User | Allowed | Not allowed | Why |
|---|---|---|---|
| `default` | disabled | everything | nobody gets in anonymously |
| `tk-cdc` | `XADD tk:ledger` | every other key | CJ-10: the change channel is the DB's feed, pushed only by the CDC relay |
| `cc-outbox` | `XADD` on the three `cc:*` streams | `tk:ledger`, `aud:*` | the outbox only publishes cause-channel messages |
| `tk-checker` | `XREADGROUP/XACK/XGROUP` on `aud:alerts` | any `XADD` | only receives alerts from CJCM |
| `auditd` (CJCM) | selector 1: read the four input streams through a consumer group; selector 2: write within `aud:*` (`XADD`, bitmap) | `XADD` to the four input streams, `SET`/`DEL` outside `aud:*`, `FLUSHALL` | CJCM must not be able to create the evidence it checks (C-06) |

Dev only: shared password `dev` (`>dev`); a password is required because `default` is disabled and go-redis only sends `AUTH` when a password is set. A real deployment changes each user's password or uses TLS client certs, keeping the same permission matrix.

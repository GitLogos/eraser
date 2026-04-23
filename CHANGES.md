# Multi-profile changes — drop-in overlay

This folder contains **only files that changed or were added**. Overlay it
onto your `eraser` fork root (the folder layout already mirrors the fork).
Unchanged files stay as they are.

## Fully-rewritten files (direct replacements)

| Path | Change |
|------|--------|
| `config.example.yaml` | Annotated template showing `profile.emails` + `additional_profiles` |
| `internal/config/config.go` | `Emails []string`, `ID string`, `AdditionalProfiles`, `AllProfiles()`, `FindProfile()`, `AllEmails()`, `normaliseProfiles()` |
| `internal/config/config_test.go` | New — 7 tests covering normalisation, lookup, legacy compat |
| `internal/email/sender.go` | `Message.ReplyTo` field + validation |
| `internal/email/smtp.go` | Emits `Reply-To:` header when set |
| `internal/history/history.go` | `profile_id` column on 3 tables, composite indexes, ~10 profile-scoped query methods, `GetInFlightBrokerRequests` |
| `internal/template/template.go` | Accepts `[]string` emails with backward-compat fallback |
| `internal/template/template_test.go` | New |
| `internal/template/templates/*.tmpl` | `{{if gt (len .Emails) 1}}` block; byte-identical output for single-email profiles |
| `internal/web/job.go` | `PersistentJobState` v2, `RemainingItems []RemainingItem`, transparent legacy migration in `Load()` |

## New files

| Path | Purpose |
|------|---------|
| `internal/inbox/profilematch.go` | 4-rule disambiguator (single-in-flight → body email match → FIFO → needs-review) |
| `internal/inbox/profilematch_test.go` | Covers all four rule paths |

## Patches to apply by hand

Two originals are too large to merge safely without a Go compiler here, so
the patches ship as `.md` files. Apply in order, `go build ./...` after each:

| File to edit (in your fork) | Apply patches from |
|------------------------------|--------------------|
| `cmd/eraser/main.go` | `cmd/eraser/pr4_send_patch.md` (send loop), then `cmd/eraser/pr6_browser_patch.md` (fill/confirm `--profile`) |
| `cmd/eraser/main.go` + `internal/web/server.go` (inbox monitor paths) | `internal/inbox/pr7_integration_patch.md` |
| `internal/web/server.go` | `internal/web/pr5_server_patch.md` (persistent job + `/api/send-all`), then `internal/web/pr8_ui_patch.md` (profile switcher, setup wizard, needs-review lane) |

## New templates referenced by PR 8 (create when applying)

PR 8 references HTML files that don't exist yet; create them when you apply
the patch:

* `internal/web/templates/partials/profile-switcher.html` (contents in `pr8_ui_patch.md` §1)
* `internal/web/templates/setup-profiles.html` (sketch in §3 — mirror the style of the existing `setup/profile.html`)

## Design

See `MULTI_PROFILE_PLAN.md` for the reasoning behind the hybrid config
schema, Reply-To routing, per-(profile, broker) dedup, and the
disambiguator rules.

## Recommended PR order

1. `config.go` + test (non-breaking; no callers yet)
2. `history.go` (schema migration; other code still compiles against old API)
3. `template.go` + tests + `.tmpl` files
4. `sender.go` + `smtp.go` + apply `pr4_send_patch.md` to `main.go`
5. `job.go` + apply `pr5_server_patch.md` to `server.go`
6. Apply `pr6_browser_patch.md` to `main.go`
7. `profilematch.go` + test + apply `pr7_integration_patch.md`
8. Apply `pr8_ui_patch.md` to `server.go` + create the two new templates

Each step should `go build ./... && go test ./...` cleanly before moving on.

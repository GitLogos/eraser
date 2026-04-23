# Eraser Multi-Profile / Multi-Email Implementation Plan (v2)

**Fork:** `digisamroc/eraser` (your OneDrive copy)
**Goal:** Native support for multiple profiles, each with multiple associated email addresses, all sending from a single shared account.
**Overall design confidence:** ~90

---

## Design Summary

- **Config shape:** hybrid — keep existing `profile:` block untouched, add optional `additional_profiles: []` and `emails: []` per profile. Zero migration for existing single-profile users.
- **Sender:** unchanged single Gmail/SMTP account. Add per-profile `Reply-To` header so broker replies route back to the right person's address.
- **History DB:** add `profile_id` column to `removal_requests`, `broker_responses`, `pending_tasks` with `DEFAULT 'default'`. Legacy rows remain attributed to the primary profile.
- **Templates:** widen `EmailData.Emails []string`; .tmpl files list all known addresses when >1, otherwise render identical to today.
- **Send loop:** nested `for broker { for profile { ... } }`. Rate-limit between brokers, not between profiles, to spread duplicate broker hits over time.
- **Browser automation:** unchanged struct — just select the right `Profile` at the call site.
- **Inbox classifier:** unchanged — disambiguation happens one level up, in the code that writes `broker_responses`.

---

## Phase 0 — Guardrails

1. Branch `feat/multi-profile`.
2. Back up DB: `cp ~/.eraser/history.db ~/.eraser/history.db.pre-multiprofile`.
3. Preflight check in `migrate()`: verify the existing schema matches what we expect before altering.
4. Write config round-trip tests **first** (legacy single-profile + new multi-profile YAML).

---

## Phase 1 — Config layer (`internal/config/config.go`)

**Changes:**
- Add to `Profile` struct:
  ```go
  Emails []string `yaml:"emails,omitempty"`
  ID     string   `yaml:"id,omitempty"`
  ```
- Add to `Config` struct:
  ```go
  AdditionalProfiles []Profile `yaml:"additional_profiles,omitempty"`
  ```
- Helper:
  ```go
  func (c *Config) AllProfiles() []Profile {
      out := []Profile{c.Profile}
      out = append(out, c.AdditionalProfiles...)
      return out
  }
  ```
- In `Load()`, post-unmarshal backfill:
  - If `p.ID == ""` → `slugify(FirstName-LastName)`, dedupe with `-2`, `-3`…
  - If `len(p.Emails) == 0 && p.Email != ""` → `Emails = []string{Email}`.
  - If `p.Email == "" && len(p.Emails) > 0` → `Email = Emails[0]`.
  - Primary (legacy) profile with empty ID defaults to `"default"` for history continuity.
- `Validate()` loops over `AllProfiles()`; enforces unique IDs.

**Est:** ~60 LoC. Risk: Low.

---

## Phase 2 — Templates (`internal/template/template.go` + `templates/*.tmpl`)

- Add `Emails []string` to `EmailData`; populate in `Render`.
- In each `.tmpl`, replace the single email line with:
  ```
  {{- if gt (len .Emails) 1 }}
  - Email Addresses:
  {{- range .Emails }}
    - {{ . }}
  {{- end }}
  {{- else }}
  - Email Address: {{.Email}}
  {{- end }}
  ```
- Golden-file test: single-email render is byte-identical to pre-change output.

**Est:** ~40 LoC. Risk: Low.

---

## Phase 3 — History schema (`internal/history/history.go`) — KEYSTONE

**Changes:**
- In `migrate()`, add idempotent ALTERs (top of function, next to existing ones):
  ```go
  s.db.Exec(`ALTER TABLE removal_requests ADD COLUMN profile_id TEXT NOT NULL DEFAULT 'default'`)
  s.db.Exec(`ALTER TABLE broker_responses ADD COLUMN profile_id TEXT NOT NULL DEFAULT 'default'`)
  s.db.Exec(`ALTER TABLE pending_tasks   ADD COLUMN profile_id TEXT NOT NULL DEFAULT 'default'`)
  ```
- New composite index: `CREATE INDEX IF NOT EXISTS idx_profile_broker ON removal_requests(profile_id, broker_id)`.
- Add `ProfileID string` to `Record`, `BrokerResponse`, `PendingTask`.
- Update every `SELECT`, `INSERT`, `scanRecord` call to include `profile_id`.
- Add profile-scoped helpers:
  ```go
  func (s *Store) ListByProfile(profileID string, limit int) ([]*Record, error)
  func (s *Store) LastByProfileAndBroker(profileID, brokerID string) (*Record, error)
  ```
- Update `GetAllBrokerStatuses` → `GetAllBrokerStatuses(profileID string)` with `WHERE profile_id = ?`.
- All 5+ call sites in `server.go` pass the active profile.

**Est:** ~150 LoC. Risk: Medium–High (largest PR; DB backup required).

---

## Phase 4 — Send loop

### 4a — CLI `runSend` (`cmd/eraser/main.go:242`)

```go
profiles := cfg.AllProfiles()
total := len(brokers) * len(profiles)
seq := 0
for _, b := range brokers {
    for _, p := range profiles {
        seq++
        // skip logic using store.LastByProfileAndBroker(p.ID, b.ID)
        emailMsg, _ := tmplEngine.Render(cfg.Options.Template, p, b)
        msg := email.Message{
            To:      b.Email,
            From:    cfg.Email.From,       // unchanged shared sender
            ReplyTo: p.Emails[0],          // NEW: per-profile reply-to
            Subject: emailMsg.Subject,
            Body:    emailMsg.Body,
        }
        // ... send, record with ProfileID: p.ID
    }
    time.Sleep(rateLimit)  // rate-limit between brokers, not profiles
}
```

- Add `--profile <id>` flag to filter to one profile.

### 4b — Web send handler (`internal/web/server.go:860`)

- Dashboard gets a profile switcher in top nav + optional multi-select for send dialog.
- Default: selected profile only. Opt-in: multi-select.
- `processSendJob` becomes nested over `toSend` and selected profile list.

### 4c — Email message shape (`internal/email/sender.go`, `smtp.go`)

- Add `ReplyTo string` to `Message`.
- In SMTP builder: `message.WriteString(fmt.Sprintf("Reply-To: %s\r\n", msg.ReplyTo))` when non-empty.

**Est:** ~100 LoC. Risk: Medium.

---

## Phase 5 — Chunker + persistent job state

### 5a — Chunker scope (`internal/web/server.go:921 processSendJob`)

The 250/day cap currently wraps a single flat broker list. Must now see `len(brokers) × len(selectedProfiles)` as the total queue.

### 5b — `PersistentJobState` shape migration (`internal/web/job.go:217`)

Current shape:
```go
RemainingBrokers []string
```

New shape:
```go
type RemainingItem struct {
    ProfileID string `json:"profile_id"`
    BrokerID  string `json:"broker_id"`
}
RemainingBrokers []RemainingItem
```

- Bump JSON version byte or use a distinguishable field.
- On `Load()`, if shape mismatch → discard state, log: *"In-flight send job reset due to multi-profile upgrade. Re-run send to continue."*
- Document this one-time reset in the release notes.

### 5c — Quota estimate surfaced to user

Pre-send confirmation in CLI and web:
```
About to send 2,250 emails (750 brokers × 3 profiles).
At 250/day, this will take ~9 days. Continue?
```

**Est:** ~80 LoC combined. Risk: Medium (in-flight jobs reset).

---

## Phase 6 — Interactive CLI wizard (`main.go:187`)

- After single-profile prompts, loop:
  ```
  Add another person? [y/N]
  ```
  Collect each into `cfg.AdditionalProfiles`.
- Within each profile, loop:
  ```
  Email address (leave blank to finish):
  ```
  to build the `Emails` slice.
- Leave the single-profile path byte-identical for existing users.

**Est:** ~40 LoC. Risk: Low.

---

## Phase 7 — Browser / pipeline / forms

Files: `cmd/eraser/main.go:1014, 1112–1120`, `internal/browser/browser.go:66`, `internal/browser/filler.go:34`, `internal/browser/confirm.go`.

- Commands `fill-form`, `confirm`: add `--profile <id>` flag. Error if omitted when multiple profiles exist.
- Load `PendingTask` by `(profile_id, broker_id)`.
- Pass the resolved `Profile` to `browser.New()` and the form-fill map.
- `BrowserState` blob remains opaque — per-profile coherence guaranteed by loading with correct `profile_id`.

**Est:** ~60 LoC. Risk: Medium.

---

## Phase 8 — Inbox monitor (`internal/inbox/monitor.go`)

Monitor matches by sender domain (unchanged). Profile attribution is a new step **after** classification:

### Disambiguation rules (priority order)

1. If only one in-flight request exists across all profiles for that broker → attribute to that profile.
2. Else, scan email body for any `profile.Emails[*]` value → match profile.
3. Else, attribute to the profile with the oldest unresolved request for that broker (FIFO).
4. Else → write `profile_id = ''`, `needs_review = 1`.

### Code placement

- New helper `func (m *Monitor) attributeProfile(classified ClassifiedResponse, store *history.Store, profiles []config.Profile) (profileID string, needsReview bool)`.
- Called before every `broker_responses` insert.

**Est:** ~80 LoC. Risk: Medium.

---

## Phase 9 — Web UI

### 9a — Setup wizard (`internal/web/session.go:21`)

- Extend `Session` struct:
  ```go
  Profile            config.Profile
  AdditionalProfiles []config.Profile
  ```
- Wizard gains a "profiles collected" step + "add another" button. Single-profile flow unchanged.

### 9b — Profile switcher

Add to top nav in `layout.html`. Query param `?profile=<id>` on all list pages.

Pages requiring switcher awareness:
- `dashboard.html`, `brokers.html`, `history.html`, `pipeline.html`, `tasks.html`, `forms.html`, `task-detail.html`, `task-helper.html`

### 9c — Settings page (`settings.html`)

Add "Profiles" tab:
- List existing profiles.
- "Add profile" button (reuses wizard).
- Edit/remove per profile (confirm destructive).

**Est:** ~150 LoC. Risk: Low–Medium.

---

## Phase 10 — GitHub Actions

Replace flat `ERASER_*` secret scheme in README + `.github/` workflow with a single `ERASER_CONFIG_YAML` secret that's decoded into `~/.eraser/config.yaml` at job start. Flat env vars don't scale past one profile.

**Est:** ~20 LoC + README section. Risk: Low.

---

## Phase 11 — Docs & examples

- `config.example.yaml`: add commented-out `additional_profiles:` block and `emails:` array.
- `README.md`: new "Multiple people in one household" section covering shared-sender model, Reply-To, sending-time estimate, Gmail-quota math.
- `CLAUDE.md`: note the `(profile_id, broker_id)` dedup invariant.

**Est:** ~100 lines of prose. Risk: Low.

---

## Phase 12 — Test matrix

Golden invariants to lock in:

1. **Legacy config unchanged → identical output** (byte-identical rendered email).
2. **Dedup scoped by `(profile_id, broker_id)`**: sending profile A to broker X does NOT mark the same broker "sent" for profile B.
3. **Paused job resume** preserves profile attribution row-by-row.
4. **Inbox disambiguation**: fixture emails exercise all 4 rules.
5. **Quota across profiles**: 3 profiles × 300 brokers = 900; expect 250/day pacing with correct resume.
6. **Migration idempotence**: running `migrate()` twice on an already-migrated DB is a no-op.

---

## PR Sequence

| PR | Scope | LoC | Risk |
|----|-------|-----|------|
| 1 | Config hybrid + tests | ~60 | Low |
| 2 | History `profile_id` column + migration + updated queries + tests | ~150 | Medium–High |
| 3 | Template `.Emails` + .tmpl updates + golden tests | ~40 | Low |
| 4 | Send loop (CLI + web) + Reply-To + chunker around full queue | ~100 | Medium |
| 5 | Persistent job state shape migration | ~30 | Medium |
| 6 | Browser/pipeline/forms `--profile` scoping | ~60 | Medium |
| 7 | Inbox disambiguation + profile_id on broker_responses | ~80 | Medium |
| 8 | Web UI switcher + wizard + settings profiles tab + docs + Actions | ~150 | Low–Medium |

- PRs 1, 3, 5, 8 are independently reviewable.
- PR 2 is the keystone; PRs 4, 6, 7 depend on it.

**Total: ~670 LoC of production code + ~100 LoC of tests.**

---

## Risks (scored)

| Risk | Rating | Notes |
|------|--------|-----|
| DB migration | Medium | `ALTER TABLE ... ADD COLUMN` is safe; preflight schema check before ALTERs. |
| In-flight paused jobs | Medium | `PersistentJobState` shape change; reset-on-load is user-visible once. |
| Inbox ambiguity | Medium | 4-step disambiguation handles ~95% of cases; residue goes to `needs_review`. |
| Gmail quota | Medium | Multi-profile multiplies send volume; surface estimate before send. |
| UI surface | Low–Medium | 8 templates need `currentProfile` threading — tedious but mechanical. |
| Reply-Path mismatch | Low | Some brokers may SPF-bounce based on return-path ≠ sender; rare. |

---

## Open items (worth a 15-min check before coding PR 2)

- Run `sqlite3 ~/.eraser/history.db .schema` on your actual DB to confirm no hand-modifications.
- Decide whether to support `--profile all` (default) vs. requiring explicit selection when >1 profile exists.
- Decide whether to allow per-profile `regions`/`excluded_brokers` overrides (v3 feature; not in this plan).

---

## Not in scope (deliberately deferred)

- Per-profile sending accounts (all profiles share one SMTP account — changing this is a v3 concern).
- Profile groups / tags.
- Parallel sending across profiles (adds complexity; single-threaded is fine for 250/day cap).
- Database schema rollback path (forward-only migrations).

---

*Last updated: 2026-04-22 by design session.*
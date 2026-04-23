# PR 8 — Web UI profile switcher + README + CLAUDE.md updates

PR 8 is the polish layer that makes multi-profile visible in the browser UI.
It deliberately lands LAST so the server, job, and history migrations
(PRs 1, 2, 5) have settled. None of the earlier PRs depend on this one.

---

## 1. Profile picker — a single partial, included everywhere

Create `internal/web/templates/partials/profile-switcher.html`:

```html
{{/*
  Profile switcher.
  Inputs (map[string]interface{}):
    - Profiles         []config.Profile
    - SelectedProfile  string     // current ?profile= query param
    - AllowAll         bool       // true when "All profiles" is meaningful
  Outputs:
    hx-get target updates the surrounding block when selection changes.
*/}}
{{ if gt (len .Profiles) 1 }}
<div class="mb-4 flex items-center gap-2">
  <label for="profile-switcher" class="text-sm font-medium text-gray-700">
    Profile:
  </label>
  <select
    id="profile-switcher"
    name="profile"
    hx-get=""
    hx-trigger="change"
    hx-target="body"
    hx-push-url="true"
    hx-include="[name='search'],[name='category'],[name='region'],[name='status']"
    class="rounded border-gray-300 text-sm">
    {{ if .AllowAll }}
    <option value="" {{ if eq .SelectedProfile "" }}selected{{ end }}>
      All profiles ({{ len .Profiles }})
    </option>
    {{ end }}
    {{ range .Profiles }}
    <option value="{{ .ID }}" {{ if eq $.SelectedProfile .ID }}selected{{ end }}>
      {{ .FirstName }} {{ .LastName }} ({{ .ID }})
    </option>
    {{ end }}
  </select>
</div>
{{ end }}
```

The `{{ if gt (len .Profiles) 1 }}` gate means single-profile installs see
no change — the switcher only appears when there's something to switch to.

Include it at the top of the 8 page-level templates that render
broker/history/pipeline data:

* `dashboard.html`
* `brokers.html`
* `history.html`
* `pipeline.html`
* `tasks.html`
* `forms.html`
* `task-detail.html`
* `task-helper.html`

```go-template
{{ template "partials/profile-switcher.html" . }}
```

---

## 2. Server-side: read `?profile=` and pass it to handlers

Add a small helper in `server.go`:

```go
// currentProfile resolves the ?profile= query param against the configured
// profiles. Returns ("", false) when no selection was made. Returns
// ("<id>", true) when a valid ID was selected. Returns ("", false) with a
// logged warning when an unknown ID was supplied — callers fall back to the
// "all profiles" view rather than erroring.
func (s *Server) currentProfile(r *http.Request) (string, bool) {
    id := strings.TrimSpace(r.URL.Query().Get("profile"))
    if id == "" {
        return "", false
    }
    if s.config == nil || s.config.FindProfile(id) == nil {
        log.Printf("Unknown profile ID in query: %q", id)
        return "", false
    }
    return id, true
}
```

Thread the selection through the list-building handlers. For example,
`handleDashboard`:

```go
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
    profileID, _ := s.currentProfile(r)

    var stats Stats
    if profileID != "" {
        stats = s.getStatsForProfile(profileID)
    } else {
        stats = s.getStats()
    }

    data := map[string]interface{}{
        "Stats":           stats,
        "Profiles":        s.config.AllProfiles(),
        "SelectedProfile": profileID,
        "AllowAll":        true,
        // ...
    }
    s.renderPage(w, "dashboard.html", data)
}
```

The getStats helpers delegate to `history.Store.GetStatsForProfile(profileID)`
when a profile is selected.

For list endpoints (`getBrokersWithStatus`, `getRecentHistory`,
pipeline listing), add a `profileID string` parameter. When empty, use the
existing "all" implementation; when set, use the `*ForProfile` variant
already added in PR 2:

```go
func (s *Server) getBrokersWithStatus(search, category, region, statusFilter, profileID string) []BrokerWithStatus {
    var brokerStatuses map[string]history.BrokerStatus
    if s.historyStore != nil {
        if profileID != "" {
            brokerStatuses, _ = s.historyStore.GetAllBrokerStatusesForProfile(profileID)
        } else {
            brokerStatuses, _ = s.historyStore.GetAllBrokerStatuses()
        }
    }
    // ... rest unchanged ...
}
```

---

## 3. Multi-profile setup wizard

Add `/setup/profiles` route. The HTML is just a table with "Add profile"
and "Remove" buttons; the backend persists changes via `config.Save(...)`
after calling `cfg.Validate()`.

```go
rootCmd.Handle("/setup/profiles", s.handleSetupProfiles)
rootCmd.Post("/setup/profiles", s.handleSaveProfiles)
```

Handler sketch:

```go
func (s *Server) handleSaveProfiles(w http.ResponseWriter, r *http.Request) {
    if err := r.ParseForm(); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    // Form submits parallel arrays: id[], first_name[], last_name[], emails[].
    ids := r.Form["id"]
    first := r.Form["first_name"]
    last := r.Form["last_name"]
    emails := r.Form["emails"] // semicolon-separated per row

    if len(ids) == 0 {
        http.Error(w, "at least one profile required", http.StatusBadRequest)
        return
    }

    // Index 0 is the primary profile.
    s.config.Profile.ID = ids[0]
    s.config.Profile.FirstName = first[0]
    s.config.Profile.LastName = last[0]
    s.config.Profile.Emails = splitAndTrim(emails[0], ";")
    if len(s.config.Profile.Emails) > 0 {
        s.config.Profile.Email = s.config.Profile.Emails[0]
    }

    s.config.AdditionalProfiles = s.config.AdditionalProfiles[:0]
    for i := 1; i < len(ids); i++ {
        p := config.Profile{
            ID:        ids[i],
            FirstName: first[i],
            LastName:  last[i],
            Emails:    splitAndTrim(emails[i], ";"),
        }
        if len(p.Emails) > 0 {
            p.Email = p.Emails[0]
        }
        s.config.AdditionalProfiles = append(s.config.AdditionalProfiles, p)
    }

    if err := s.config.Validate(); err != nil {
        http.Error(w, fmt.Sprintf("invalid config: %v", err), http.StatusBadRequest)
        return
    }
    if err := config.Save(s.configPath, s.config); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    http.Redirect(w, r, "/setup/profiles", http.StatusSeeOther)
}
```

Template `setup-profiles.html` renders one table row per profile with
delete buttons that submit to the same handler minus one row. Nothing
fancy — just matches the existing setup.html style.

---

## 4. "Send for" multi-select on the send page

`send.html` gets a checkbox list above the broker table:

```html
<fieldset class="mb-4">
  <legend class="font-medium">Send on behalf of:</legend>
  {{ range .Profiles }}
    <label class="mr-4">
      <input type="checkbox" name="profile_ids" value="{{ .ID }}" checked>
      {{ .FirstName }} {{ .LastName }}
    </label>
  {{ end }}
</fieldset>
```

The form posts to `/api/send-all` which already reads `profile_ids` via
PR 5's `handleAPISendAll`. If the user unchecks everything, the handler
rejects with 400.

---

## 5. Tasks / pipeline needs-review lane

Pipeline and tasks pages pick up one new filter: `?needs_review=1`. Rows
where `profile_id = ''` AND `needs_review = 1` go into a banner at the top
of the pipeline page:

```html
{{ if gt .NeedsReviewCount 0 }}
<div class="bg-yellow-50 border border-yellow-300 text-yellow-900 p-3 rounded mb-4">
  <strong>👁 {{ .NeedsReviewCount }} responses need manual review.</strong>
  <a href="/pipeline?needs_review=1" class="underline">Review now →</a>
</div>
{{ end }}
```

The row detail view gets a "Reassign profile" dropdown that calls
`historyStore.UpdateBrokerResponseProfileID(responseID, newProfileID)`
(method added in PR 2).

---

## 6. Test-email picker

On the `/settings/email` page, the "Send test email" form gets a profile
picker next to the recipient field. Default is the primary profile. The
handler passes the resolved profile's email as Reply-To on the test message
so users can verify end-to-end routing works.

---

## 7. `config.example.yaml`

Already shipped in this PR (`output/impl/config.example.yaml`). Shows
`profile:` with `emails:` list and a commented-out `additional_profiles:`
block.

---

## 8. README.md additions

Add a section near the end of the "Configuration" section:

````markdown
### Multi-profile setup

Eraser can run removal campaigns for multiple people — family members,
aliases, a spouse's addresses — from a single shared sending account.

```yaml
profile:
  id: default
  first_name: Alex
  last_name: Doe
  emails:
    - alex@example.com
    - alex.work@example.com

additional_profiles:
  - id: jane
    first_name: Jane
    last_name: Doe
    emails:
      - jane@example.com
  - id: sam
    first_name: Sam
    last_name: Doe
    emails:
      - sam@example.com

email:
  provider: smtp
  from: alex@gmail.com   # single shared sender
  smtp: { ... }
```

All profiles share the `email.from` account for sending. Broker replies
route to each profile's mailbox via the `Reply-To` header, so Jane's
replies arrive at `jane@example.com` even though the mail was sent
through Alex's Gmail.

You can scope CLI commands to one profile:

```
eraser send --profile jane
eraser fill --pending --profile jane
eraser confirm --pending --profile jane
```

Or omit `--profile` to iterate every configured profile. The web UI has
an identical per-page profile switcher.

Inbox monitoring automatically matches incoming replies to the right
profile. The disambiguation rules (single in-flight → body email hit →
FIFO → needs_review) work well in practice; the "Needs review" lane on
the pipeline page surfaces anything the matcher couldn't resolve.
````

---

## 9. `CLAUDE.md` (or equivalent contributor guide)

Add one short section so anyone who touches eraser later knows the invariants:

````markdown
### Multi-profile invariants

* Every history row has a `profile_id`. The column defaults to `"default"`,
  so legacy rows migrate seamlessly.
* Envelope `From` is always `cfg.Email.From`. Per-profile identity flows
  via `Reply-To` — never spoof the MAIL FROM.
* CLI commands that operate on a single identity (`fill`, `confirm`) take
  `--profile <id>`; commands that operate on a population (`send`,
  `monitor`) default to all profiles with `--profile` as an override.
* Dedup is `(profile_id, broker_id)` — never just `broker_id`. Use
  `GetLastRequestForProfileAndBroker`, not `GetLastRequestForBroker`.
* Inbox replies with unresolved `profile_id` are stored with
  `profile_id = ''` + `needs_review = 1`. Don't default them to a real
  profile on write — the UI surfaces them for human review.
* The persistent job file (`pending_job.json`) has a `version` field. Bump
  `currentShapeVersion` in `job.go` whenever the shape changes and add a
  migration path in `Load()`.
````

---

## Ordering note

Land this PR last. It depends on:

* PR 1 — `AdditionalProfiles`, `AllProfiles()`, `FindProfile()`.
* PR 2 — profile-scoped history methods.
* PR 5 — `handleAPISendAll` reading `profile_ids`.
* PR 7 — `needs_review` + empty `profile_id` semantics.

Ship the earlier PRs first and the web UI will be dormant-but-correct in
the meantime (one profile, no switcher visible, existing URLs unchanged).

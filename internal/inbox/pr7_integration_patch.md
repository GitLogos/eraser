# PR 7 — Integration patch: wire `MatchProfile` into the monitor loop

This PR adds the profile disambiguator helper (see `profilematch.go`). Two
callers need to be updated to use it:

1. `runMonitor` in `cmd/eraser/main.go` — the CLI inbox watcher.
2. `Server.handleMonitorInbox` (or equivalent) in `internal/web/server.go` —
   whichever path the web UI's "Scan inbox now" button uses.

Both sites do the same work:

* Classify each incoming email (existing `inbox.ClassifyResponse`).
* **NEW**: Resolve which profile this reply is for via
  `inbox.MatchProfile`.
* Stamp the resolved `ProfileID` on the `history.BrokerResponse` before
  insertion. When the matcher returns empty `ProfileID` + `NeedsReview=true`,
  store the row unchanged — the UI can filter `WHERE profile_id = ''
  AND needs_review = 1` to show the human-review queue.
* Pipeline status updates use `UpdatePipelineStatusForProfile` so one
  broker → profile row exists per (profile, broker) pair.

---

## 1. Build a `[]ProfileEmails` snapshot once per scan

In both `runMonitor` and the web handler, add a helper after config/store
init:

```go
// buildProfileEmails converts the current config into the
// slice shape inbox.MatchProfile wants. Cheap; we can call it once per
// scan and pass the slice down.
func buildProfileEmails(cfg *config.Config) []inbox.ProfileEmails {
    all := cfg.AllProfiles()
    out := make([]inbox.ProfileEmails, len(all))
    for i, p := range all {
        emails := append([]string(nil), p.Emails...)
        if p.Email != "" {
            emails = append(emails, p.Email)
        }
        out[i] = inbox.ProfileEmails{ProfileID: p.ID, Emails: emails}
    }
    return out
}
```

---

## 2. `runMonitor` — replace the per-email classify+store block

In `cmd/eraser/main.go` around line 695 (inside the `for _, email := range emails`
loop), the current code is:

```go
for _, email := range emails {
    classified := inbox.ClassifyResponse(&email)
    responses = append(responses, classified)

    brokerResp := &history.BrokerResponse{
        BrokerID:     email.BrokerID,
        ...
        NeedsReview:  classified.NeedsReview,
        ReceivedAt:   email.ReceivedAt,
    }

    if err := store.AddBrokerResponse(brokerResp); err != nil { ... }

    var pipelineStatus history.PipelineStatus
    switch classified.Type { ... }

    if err := store.UpdatePipelineStatus(email.BrokerID, pipelineStatus); err != nil { ... }

    printClassifiedResponse(classified)
}
```

Replace with:

```go
profileEmails := buildProfileEmails(cfg)

for _, email := range emails {
    classified := inbox.ClassifyResponse(&email)
    responses = append(responses, classified)

    // NEW: resolve which profile this reply is for.
    match := inbox.MatchProfile(&email, profileEmails, store)

    brokerResp := &history.BrokerResponse{
        ProfileID:    match.ProfileID, // may be "" when NeedsReview
        BrokerID:     email.BrokerID,
        BrokerName:   email.BrokerName,
        ResponseType: string(classified.Type),
        EmailFrom:    email.From,
        EmailSubject: email.Subject,
        FormURL:      classified.FormURL,
        ConfirmURL:   classified.ConfirmURL,
        // Combine classifier and matcher review signals: if either one
        // wants a human to look, mark the row for review.
        Confidence:  minFloat(classified.Confidence, match.Confidence),
        NeedsReview: classified.NeedsReview || match.NeedsReview,
        ReceivedAt:  email.ReceivedAt,
    }

    if err := store.AddBrokerResponse(brokerResp); err != nil {
        fmt.Printf("⚠️  Failed to store response: %v\n", err)
    }

    var pipelineStatus history.PipelineStatus
    switch classified.Type {
    case inbox.ResponseSuccess:
        pipelineStatus = history.PipelineConfirmed
    case inbox.ResponseFormRequired:
        pipelineStatus = history.PipelineFormRequired
    case inbox.ResponseConfirmationRequired:
        pipelineStatus = history.PipelineAwaitingConfirmation
    case inbox.ResponseRejected:
        pipelineStatus = history.PipelineRejected
    case inbox.ResponsePending:
        pipelineStatus = history.PipelineAwaitingResponse
    default:
        pipelineStatus = history.PipelineAwaitingResponse
    }

    // Only update the pipeline when we know which profile to update. An
    // unresolved (needs_review) response doesn't advance any pipeline row.
    if match.ProfileID != "" {
        if err := store.UpdatePipelineStatusForProfile(match.ProfileID, email.BrokerID, pipelineStatus); err != nil {
            // ignore — row may not exist yet
        }
    }

    printClassifiedResponseWithProfile(classified, match)
}
```

And update the watch-mode callback (around line 792) with the same three
changes: build-once `profileEmails`, call `MatchProfile`, and stamp
`ProfileID` on the stored response:

```go
err := monitor.WatchForNewEmails(ctx, func(email inbox.Email) {
    classified := inbox.ClassifyResponse(&email)
    match := inbox.MatchProfile(&email, profileEmails, store)

    brokerResp := &history.BrokerResponse{
        ProfileID:    match.ProfileID,
        BrokerID:     email.BrokerID,
        BrokerName:   email.BrokerName,
        ResponseType: string(classified.Type),
        EmailFrom:    email.From,
        EmailSubject: email.Subject,
        FormURL:      classified.FormURL,
        ConfirmURL:   classified.ConfirmURL,
        Confidence:   minFloat(classified.Confidence, match.Confidence),
        NeedsReview:  classified.NeedsReview || match.NeedsReview,
        ReceivedAt:   email.ReceivedAt,
    }
    store.AddBrokerResponse(brokerResp)

    fmt.Println()
    fmt.Printf("📨 New email from %s (%s) → profile %s via %s\n",
        email.BrokerName, email.From, match.ProfileID, match.Rule)
    printClassifiedResponseWithProfile(classified, match)
})
```

---

## 3. New helper: `printClassifiedResponseWithProfile`

Add next to `printClassifiedResponse` (around line 823). Extra line shows
which profile won and by which rule — lets users eyeball matcher accuracy:

```go
func printClassifiedResponseWithProfile(r inbox.ClassifiedResponse, m inbox.ProfileMatch) {
    printClassifiedResponse(r)
    if m.Rule == inbox.RuleDefaultOnly {
        return // no multi-profile context worth showing
    }
    if m.NeedsReview {
        fmt.Printf("   👁  Profile match: NEEDS REVIEW (candidates: %v)\n", m.Candidates)
    } else {
        fmt.Printf("   👤 Matched profile: %s (rule=%s, confidence=%.0f%%)\n",
            m.ProfileID, m.Rule, m.Confidence*100)
    }
}
```

And a tiny helper for the `Confidence` merge:

```go
func minFloat(a, b float64) float64 {
    if a < b {
        return a
    }
    return b
}
```

---

## 4. Web server — mirror the change in the scan handler

Wherever `server.go` processes inbox responses (look for
`inbox.ClassifyResponse` followed by `AddBrokerResponse`), apply the same
three edits:

1. `profileEmails := buildProfileEmails(s.config)` once per scan.
2. `match := inbox.MatchProfile(&email, profileEmails, s.historyStore)`
3. Store `brokerResp.ProfileID = match.ProfileID`, `NeedsReview = ... ||
   match.NeedsReview`, and use `UpdatePipelineStatusForProfile` when
   `match.ProfileID != ""`.

(The web equivalent of `printClassifiedResponse` is rendering a row in
`partials/pipeline.html` — PR 8 will add a profile badge + "needs review"
styling there.)

---

## Design notes

* **Rule confidence values are deliberate anchors.** 0.95 / 0.90 / 0.60
  reflect prior intuition: single-in-flight is almost always right; body
  match is nearly as strong but can still be spoofed by a broker copying
  multiple recipients; FIFO is a best-effort guess. Users can filter on
  confidence thresholds in the UI later if they want stricter review gates.
* **Empty `ProfileID` + `NeedsReview=1` is the review queue signal.** We
  don't store `ProfileID = "unknown"` because that value would be easy to
  confuse with a real profile ID; empty-string makes the SQL filter
  unambiguous.
* **Matcher merges with classifier review flag via OR.** If the classifier
  already said "needs review" (bounce-like, low confidence), the matcher
  can't override that — but it can also raise the flag independently.
* **No retroactive rematch.** If a reply arrives before the matcher can
  resolve it, the row sits in needs_review. The web UI provides a
  "Reassign profile" action (PR 8) that lets users manually fix these rows.
* **MatchProfile is cheap.** One SQL call per email (the in-flight lookup)
  and some string work. Even on a 500-email inbox scan this is milliseconds.

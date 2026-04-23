# PR 5 — Patch for `internal/web/server.go` (multi-profile send jobs + resume)

Companion to the `job.go` rewrite (shape v2 with `RemainingItems`). Updates
every site in `server.go` that touches `PersistentJobState`, `processSendJob`,
or the single-send endpoint so that:

1. Jobs iterate **profiles × brokers**, not just brokers.
2. Dedup is per **(profile, broker)** pair via
   `GetLastRequestForProfileAndBroker`.
3. Outgoing mail carries `ReplyTo = profile.Email` (shared envelope From is
   preserved — SMTP providers reject envelope spoofing).
4. Resume works for both fresh v2 state and migrated legacy state.

All changes are additive where possible (old single-profile usage stays
byte-identical for the default-profile-only case).

---

## 1. `resumePendingJob` — iterate the new `RemainingItems`

Replace the existing `resumePendingJob` (around lines 330–377) with:

```go
// resumePendingJob resumes processing of an incomplete job
func (s *Server) resumePendingJob(state *PersistentJobState) {
	// Wait a moment for the server to fully start
	time.Sleep(2 * time.Second)

	if s.config == nil || s.config.Email.Provider == "" {
		log.Printf("Cannot resume job: email not configured")
		s.jobPersistence.Clear()
		return
	}

	sender, err := email.NewSender(s.config.Email)
	if err != nil {
		log.Printf("Cannot resume job: failed to create email sender: %v", err)
		s.jobPersistence.Clear()
		return
	}

	// Build quick lookups for brokers and profiles. Any item in state whose
	// broker_id or profile_id no longer exists in config is silently dropped —
	// we don't want to wedge a resume because someone removed a profile.
	brokerMap := make(map[string]broker.Broker)
	for _, b := range s.brokerDB.Brokers {
		brokerMap[b.ID] = b
	}
	profileMap := make(map[string]config.Profile)
	for _, p := range s.config.AllProfiles() {
		profileMap[p.ID] = p
	}

	var toSend []sendItem
	for _, ri := range state.RemainingItems {
		b, okB := brokerMap[ri.BrokerID]
		p, okP := profileMap[ri.ProfileID]
		if !okB || !okP {
			log.Printf("Resume: dropping unknown item %s (broker_found=%v profile_found=%v)",
				ri, okB, okP)
			continue
		}
		toSend = append(toSend, sendItem{Profile: p, Broker: b})
	}

	if len(toSend) == 0 {
		log.Printf("No valid items remaining in pending job")
		s.jobPersistence.Clear()
		return
	}

	job := s.jobManager.Create(state.Total)
	job.Sent = state.Sent
	job.Failed = state.Failed
	if state.Total > 0 {
		job.Progress = ((state.Sent + state.Failed) * 100) / state.Total
	}

	fmt.Printf("Resuming send job: %d items remaining across %d profile(s)...\n",
		len(toSend), countProfiles(toSend))

	s.processSendJob(job, toSend, sender)
}

// countProfiles returns the distinct profile count in a send queue; used
// only for the resume banner log line.
func countProfiles(items []sendItem) int {
	seen := make(map[string]struct{})
	for _, it := range items {
		seen[it.Profile.ID] = struct{}{}
	}
	return len(seen)
}
```

Also add a small helper type near `BrokerWithStatus` (around line 1075):

```go
// sendItem is what processSendJob iterates over: a (profile, broker) pair
// queued for sending. Wraps broker.Broker rather than BrokerWithStatus
// because resume paths don't carry status info (we re-check dedup anyway
// before each send).
type sendItem struct {
	Profile config.Profile
	Broker  broker.Broker
}
```

---

## 2. `handleAPISendAll` — build the queue as (profile × broker) pairs

Replace the body of `handleAPISendAll` (around line 823) with:

```go
func (s *Server) handleAPISendAll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if !s.rateLimiter.Allow("send-all") {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"error": "Rate limit exceeded. Please wait before sending another batch."})
		return
	}

	if activeJob := s.jobManager.GetActive(); activeJob != nil {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":  "A send job is already in progress",
			"job_id": activeJob.ID,
		})
		return
	}

	if s.config == nil || s.config.Email.Provider == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Email not configured. Please configure email settings first."})
		return
	}

	search := r.FormValue("search")
	category := r.FormValue("category")
	region := r.FormValue("region")
	status := r.FormValue("status")
	// New optional form field: which profiles to send for. If omitted,
	// defaults to every configured profile. Value is a comma-separated list
	// of profile IDs, e.g. "jane,john".
	profileIDsRaw := strings.TrimSpace(r.FormValue("profile_ids"))

	if status == "" {
		status = "pending"
	}

	// Resolve which profiles to include.
	allProfiles := s.config.AllProfiles()
	var profiles []config.Profile
	if profileIDsRaw == "" {
		profiles = allProfiles
	} else {
		wanted := make(map[string]struct{})
		for _, id := range strings.Split(profileIDsRaw, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				wanted[id] = struct{}{}
			}
		}
		for _, p := range allProfiles {
			if _, ok := wanted[p.ID]; ok {
				profiles = append(profiles, p)
			}
		}
		if len(profiles) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "No matching profiles"})
			return
		}
	}

	// Broker list is profile-agnostic for the filter UX; per-profile dedup
	// happens inside processSendJob so we don't have to build separate
	// lists per profile here. This also keeps the UI simple.
	brokers := s.getBrokersWithStatus(search, category, region, status)
	if len(brokers) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "No pending brokers to send to."})
		return
	}

	sender, err := email.NewSender(s.config.Email)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Flatten to the (profile, broker) queue the send loop wants.
	var toSend []sendItem
	for _, p := range profiles {
		for _, b := range brokers {
			toSend = append(toSend, sendItem{Profile: p, Broker: b.Broker})
		}
	}

	job := s.jobManager.Create(len(toSend))

	// Persist initial state so resume can pick up if the server restarts.
	items := make([]RemainingItem, len(toSend))
	profileIDs := make([]string, 0, len(profiles))
	for _, p := range profiles {
		profileIDs = append(profileIDs, p.ID)
	}
	for i, it := range toSend {
		items[i] = RemainingItem{ProfileID: it.Profile.ID, BrokerID: it.Broker.ID}
	}

	jobState := &PersistentJobState{
		ID:             job.ID,
		Status:         job.Status,
		Sent:           0,
		Failed:         0,
		Total:          len(toSend),
		StartedAt:      job.StartedAt,
		RemainingItems: items,
		Search:         search,
		Category:       category,
		Region:         region,
		StatusFilter:   status,
		ProfileIDs:     profileIDs,
	}
	if err := s.jobPersistence.Save(jobState); err != nil {
		log.Printf("Warning: failed to save job state: %v", err)
	}

	go s.processSendJob(job, toSend, sender)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"job_id":       job.ID,
		"total":        len(toSend),
		"profile_ids":  profileIDs,
		"broker_count": len(brokers),
	})
}
```

---

## 3. `processSendJob` — per-(profile, broker) dedup + Reply-To + remaining items

Replace `processSendJob` (around line 921) with:

```go
// processSendJob runs in a background goroutine to send emails. The queue
// is a flat list of (profile, broker) pairs; dedup happens on each pair so
// a resumed job won't re-send items that landed in the DB during a prior
// partial run.
func (s *Server) processSendJob(job *Job, toSend []sendItem, sender email.Sender) {
	sent := 0
	failed := 0
	rateLimitMs := s.config.Options.RateLimitMs
	if rateLimitMs == 0 {
		rateLimitMs = 2000
	}

	dailyLimit := DailyLimitSMTP
	if s.config.Email.Provider == "sendgrid" {
		dailyLimit = DailyLimitSendGrid
	} else if s.config.Email.Provider == "resend" {
		dailyLimit = DailyLimitResend
	}
	job.DailyLimit = dailyLimit

	// Build the "remaining" slice from the queue. This is what gets
	// persisted after every send so a crash/restart resumes correctly.
	remaining := make([]RemainingItem, len(toSend))
	for i, it := range toSend {
		remaining[i] = RemainingItem{ProfileID: it.Profile.ID, BrokerID: it.Broker.ID}
	}

	for i, it := range toSend {
		if job.IsCancelled() {
			break
		}

		if sent >= dailyLimit {
			job.DaySent = sent
			job.Status = JobStatusPaused
			job.Error = fmt.Sprintf("Daily limit of %d emails reached. Remaining %d items will be sent when you restart tomorrow.", dailyLimit, len(remaining))
			s.saveJobProgress(job, sent, failed, remaining)
			log.Printf("Job paused: daily limit of %d reached, %d remaining", dailyLimit, len(remaining))
			return
		}

		job.Update(sent, failed, it.Broker.Name, it.Profile.ID)

		// Per-(profile, broker) dedup — protects against double-sending when
		// the user hits "Send all" twice or resume overlaps with prior run.
		if s.historyStore != nil {
			if prev, err := s.historyStore.GetLastRequestForProfileAndBroker(it.Profile.ID, it.Broker.ID); err == nil && prev != nil && prev.Status == history.StatusSent {
				log.Printf("Skipping %s: already sent for this profile", RemainingItem{ProfileID: it.Profile.ID, BrokerID: it.Broker.ID})
				remaining = remaining[1:]
				s.saveJobProgress(job, sent, failed, remaining)
				continue
			}
		}

		rendered, err := s.tmplEngine.Render("generic", it.Profile, it.Broker)
		if err != nil {
			failed++
			job.Update(sent, failed, it.Broker.Name, it.Profile.ID)
			remaining = remaining[1:]
			s.saveJobProgress(job, sent, failed, remaining)
			continue
		}

		msg := email.Message{
			To:      it.Broker.Email,
			From:    s.config.Email.From,
			ReplyTo: it.Profile.Email, // route broker replies to the profile's mailbox
			Subject: rendered.Subject,
			Body:    rendered.Body,
		}

		ctx, cancel := context.WithTimeout(job.Context(), 30*time.Second)
		result := sender.Send(ctx, msg)
		cancel()

		record := &history.Record{
			ProfileID:  it.Profile.ID,
			BrokerID:   it.Broker.ID,
			BrokerName: it.Broker.Name,
			Email:      it.Broker.Email,
			Template:   "generic",
			SentAt:     time.Now(),
		}

		if result.Success {
			record.Status = history.StatusSent
			record.MessageID = result.MessageID
			sent++
			job.ResetAuthFailures()
		} else {
			record.Status = history.StatusFailed
			errMsg := ""
			if result.Error != nil {
				errMsg = result.Error.Error()
				record.Error = errMsg
			}
			failed++

			if strings.Contains(strings.ToLower(errMsg), "auth") {
				if job.RecordAuthFailure() {
					if s.historyStore != nil {
						s.historyStore.Add(record)
					}
					remaining = remaining[1:]
					s.saveJobProgress(job, sent, failed, remaining)
					job.StopWithError("auth", "Stopped due to repeated authentication failures. Your email provider may have rate-limited or blocked your account. Please check your email settings and try again later.")
					log.Printf("Job stopped: repeated auth failures after %d sent, %d failed", sent, failed)
					return
				}
			}
		}

		if s.historyStore != nil {
			s.historyStore.Add(record)
		}

		job.Update(sent, failed, it.Broker.Name, it.Profile.ID)

		remaining = remaining[1:]
		s.saveJobProgress(job, sent, failed, remaining)

		if i < len(toSend)-1 && !job.IsCancelled() {
			time.Sleep(time.Duration(rateLimitMs) * time.Millisecond)
		}
	}

	job.Complete()
	if err := s.jobPersistence.Clear(); err != nil {
		log.Printf("Warning: failed to clear job state: %v", err)
	}
}
```

---

## 4. `saveJobProgress` — take `[]RemainingItem`

Replace `saveJobProgress` (around line 1050) with:

```go
func (s *Server) saveJobProgress(job *Job, sent, failed int, remaining []RemainingItem) {
	state := &PersistentJobState{
		ID:             job.ID,
		Status:         job.Status,
		Sent:           sent,
		Failed:         failed,
		Total:          job.Total,
		StartedAt:      job.StartedAt,
		RemainingItems: remaining,
	}
	if err := s.jobPersistence.Save(state); err != nil {
		log.Printf("Warning: failed to save job progress: %v", err)
	}
}
```

---

## 5. `handleAPISendOne` — profile override + per-profile dedup + Reply-To

The per-broker send endpoint also needs profile awareness. Replace the
template-render + message-build block (around lines 770–810) with:

```go
	// Pick the profile: the client may pass ?profile=<id> to target one
	// profile; default is the primary.
	profileID := r.URL.Query().Get("profile")
	profile := s.config.Profile
	if profileID != "" {
		if p := s.config.FindProfile(profileID); p != nil {
			profile = *p
		} else {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`<span class="text-red-600">Unknown profile</span>`))
			return
		}
	}

	// Per-(profile, broker) dedup — honour the same invariant as the
	// batch loop.
	if s.historyStore != nil {
		if prev, err := s.historyStore.GetLastRequestForProfileAndBroker(profile.ID, br.ID); err == nil && prev != nil && prev.Status == history.StatusSent {
			w.Write([]byte(fmt.Sprintf(`<span class="text-gray-500">Already sent on %s</span>`, prev.SentAt.Format("2006-01-02"))))
			return
		}
	}

	rendered, err := s.tmplEngine.Render("generic", profile, *br)
	if err != nil {
		w.Write([]byte(fmt.Sprintf(`<span class="text-red-600">Template error: %s</span>`, template.HTMLEscapeString(err.Error()))))
		return
	}

	msg := email.Message{
		To:      br.Email,
		From:    s.config.Email.From,
		ReplyTo: profile.Email,
		Subject: rendered.Subject,
		Body:    rendered.Body,
	}
```

And downstream, when building the history record, attribute it to `profile.ID`:

```go
	record := &history.Record{
		ProfileID:  profile.ID,
		BrokerID:   br.ID,
		BrokerName: br.Name,
		Email:      br.Email,
		Template:   "generic",
		SentAt:     time.Now(),
	}
```

---

## Legacy resume behaviour

If a user upgrades while a `pending_job.json` from the previous version
exists, `JobPersistence.Load()` handles this automatically:

* Detects missing `version` field.
* Unmarshals into the legacy struct shape.
* Maps every `remaining_brokers` entry to
  `(ProfileID: history.DefaultProfileID, BrokerID: id)`.
* Logs the migration.
* Returns the upgraded in-memory state; `Save()` writes v2 on the next tick.

No user intervention needed. If the file is completely unparseable it gets
deleted rather than blocking server startup.

---

## What this does NOT change in server.go

* `testEmailCmd` around line 1447 — test email sends from the primary
  profile only, which is fine. Will get a profile picker in PR 8's web UI.
* Broker-status queries (`getBrokersWithStatus`) — they still use the
  global `GetAllBrokerStatuses` and show "has anyone sent to this broker?".
  PR 8 adds a UI profile filter that calls
  `GetAllBrokerStatusesForProfile`.

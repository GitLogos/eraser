// Phase 3 - Server.go Updates
// Changes: Add support for multi-profile sending via ?profile_id query param
// Default behavior: Send for ALL configured profiles
// With profile selection: Send for only specified profile(s)
// Track ProfileID in all history records
// Add ReplyTo headers for per-profile routing

// Updated handleAPISendOne to support profile selection via query param
func (s *Server) handleAPISendOne(w http.ResponseWriter, r *http.Request) {
	// Rate limiting - prevent abuse of email sending
	if !s.rateLimiter.Allow("send") {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`<span class="text-yellow-600">Rate limit exceeded. Please wait a moment before sending more emails.</span>`))
		return
	}

	brokerID := chi.URLParam(r, "brokerID")
	br := s.brokerDB.FindByID(brokerID)
	if br == nil {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`<span class="text-red-600">Broker not found</span>`))
		return
	}

	if s.config == nil || s.config.Email.Provider == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<span class="text-red-600">Email not configured. <a href="/setup" class="underline">Configure now</a></span>`))
		return
	}

	// Get profile selection from query param (default to all profiles)
	profileID := r.URL.Query().Get("profile_id")
	var profiles []config.Profile

	if profileID != "" {
		// Single profile specified
		allProfiles := s.config.AllProfiles()
		for _, p := range allProfiles {
			if p.ID == profileID {
				profiles = []config.Profile{p}
				break
			}
		}
		if len(profiles) == 0 {
			// Fallback to primary profile if no matching profile found
			profiles = []config.Profile{s.config.Profile}
		}
	} else {
		// Default: send for ALL configured profiles (multi-profile behavior)
		profiles = s.config.AllProfiles()
	}

	// Create email sender
	sender, err := email.NewSender(s.config.Email)
	if err != nil {
		w.Write([]byte(fmt.Sprintf(`<span class="text-red-600">Error: %s</span>`, template.HTMLEscapeString(err.Error()))))
		return
	}

	// Send for each profile
	for _, p := range profiles {
		// Generate email content using template engine
		rendered, err := s.tmplEngine.Render("generic", p, *br)
		if err != nil {
			w.Write([]byte(fmt.Sprintf(`<span class="text-red-600">Template error: %s</span>`, template.HTMLEscapeString(err.Error()))))
			return
		}

		msg := email.Message{
			To:      br.Email,
			From:    s.config.Email.From,
			// NEW: Add Reply-To header for per-profile routing
			ReplyTo:  p.Emails[0],
			Subject:  rendered.Subject,
			Body:     rendered.Body,
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		result := sender.Send(ctx, msg)

		// Record in history with ProfileID
		record := &history.Record{
			BrokerID:   br.ID,
			BrokerName: br.Name,
			Email:      br.Email,
			Template:   "generic",
			SentAt:     time.Now(),
			ProfileID:  resolveProfileID(profileID), // NEW: Track which profile sent
		}

		if result.Success {
			record.Status = history.StatusSent
			record.MessageID = result.MessageID
		} else {
			record.Status = history.StatusFailed
			if result.Error != nil {
				record.Error = result.Error.Error()
			}
		}

		if s.historyStore != nil {
			s.historyStore.Add(record)
		}

		if result.Success {
			w.Write([]byte(`<span class="px-2 inline-flex text-xs leading-5 font-semibold rounded-full bg-green-100 text-green-800">Sent (profile: %s)</span>`, template.HTML(p.ID)))
		} else {
			errMsg := "Unknown error"
			if result.Error != nil {
				errMsg = result.Error.Error()
			}
			w.Write([]byte(fmt.Sprintf(`<span class="text-red-600" title="%s">Failed (profile: %s)</span>`, template.HTMLEscapeString(errMsg), template.HTML(p.ID))))
		}

		// Small delay between profiles to avoid overwhelming broker
		time.Sleep(1 * time.Second)
	}

	// Return success for the first profile sent
	if len(profiles) > 0 {
		w.WriteHeader(http.StatusOK)
	}
}

// Updated handleAPISendAll to support multi-profile sending
func (s *Server) handleAPISendAll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Rate limiting - prevent abuse of bulk email sending
	if !s.rateLimiter.Allow("send-all") {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"error": "Rate limit exceeded. Please wait before sending another batch."})
		return
	}

	// Check if a job is already running
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

	// Get filter parameters from form
	search := r.FormValue("search")
	category := r.FormValue("category")
	region := r.FormValue("region")
	status := r.FormValue("status")

	// NEW: Get profile selection from form
	selectedProfileIDs := r.FormValue("profiles")
	var selectedProfiles []config.Profile

	if selectedProfileIDs != "" {
		// Multiple profiles selected (comma-separated)
		idList := strings.Split(selectedProfileIDs, ",")
		allProfiles := s.config.AllProfiles()
		for _, id := range idList {
			id = strings.TrimSpace(id)
			for _, p := range allProfiles {
				if p.ID == id {
					selectedProfiles = append(selectedProfiles, p)
				}
			}
		}
	} else {
		// Default: send for ALL configured profiles (multi-profile behavior)
		selectedProfiles = s.config.AllProfiles()
	}

	// If no status filter specified, default to pending (never sent)
	if status == "" {
		status = "pending"
	}

	toSend := s.getBrokersWithStatus(search, category, region, status)

	if len(toSend) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "No pending brokers to send to."})
		return
	}

	// Create email sender (validate config before starting job)
	sender, err := email.NewSender(s.config.Email)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Create a new job
	job := s.jobManager.Create(len(toSend))

	// Extract broker IDs for persistence
	brokerIDs := make([]string, len(toSend))
	for i, b := range toSend {
		brokerIDs[i] = b.ID
	}

	// Save initial job state
	jobState := &PersistentJobState{
		ID:               job.ID,
		Status:           job.Status,
		Sent:             0,
		Failed:           0,
		Total:            len(toSend),
		StartedAt:        job.StartedAt,
		RemainingBrokers: brokerIDs,
		SelectedProfiles: selectedProfiles,  // NEW: Store selected profiles
		Search:           search,
		Category:         category,
		Region:           region,
		StatusFilter:     status,
	}
	if err := s.jobPersistence.Save(jobState); err != nil {
		log.Printf("Warning: failed to save job state: %v", err)
	}

	// Start background goroutine to process emails
	go s.processSendJob(job, toSend, sender, selectedProfiles)

	// Return job ID immediately
	json.NewEncoder(w).Encode(map[string]interface{}{
		"job_id": job.ID,
		"total":  len(toSend),
	})
}

// Updated processSendJob to loop over multiple profiles
func (s *Server) processSendJob(job *Job, toSend []BrokerWithStatus, sender email.Sender, selectedProfiles []config.Profile) {
	sent := 0
	failed := 0
	rateLimitMs := s.config.Options.RateLimitMs
	if rateLimitMs == 0 {
		rateLimitMs = 2000 // Default 2 second delay
	}

	// Set daily limit based on provider
	dailyLimit := DailyLimitSMTP // Default for SMTP
	if s.config.Email.Provider == "sendgrid" {
		dailyLimit = DailyLimitSendGrid
	} else if s.config.Email.Provider == "resend" {
		dailyLimit = DailyLimitResend
	}
	job.DailyLimit = dailyLimit

	// Track remaining brokers for persistence
	remaining := make([]string, len(toSend))
	for i, b := range toSend {
		remaining[i] = b.ID
	}

	// NEW: Track sent emails per profile for quota calculation
	sentPerProfile := make(map[string]int)

	for _, b := range toSend {
		// Check if job was cancelled
		if job.IsCancelled() {
			break
		}

		// Check daily limit
		if sent >= dailyLimit {
			job.DaySent = sent
			job.Status = JobStatusPaused
			job.Error = fmt.Sprintf("Daily limit of %d emails reached. Remaining %d profiles × brokers will be sent when you restart tomorrow.", dailyLimit, len(remaining))
			s.saveJobProgress(job, sent, failed, remaining, sentPerProfile)
			log.Printf("Job paused: daily limit of %d reached, %d remaining", dailyLimit, len(remaining))
			return
		}

		// Update current broker
		job.Update(sent, failed, b.Name)

		// NEW: Loop over each selected profile
		for _, p := range selectedProfiles {
			// Skip if this profile has already sent enough (rate limiting per profile)
			maxPerProfile := dailyLimit / len(selectedProfiles) + 1
			if sentPerProfile[p.ID] >= maxPerProfile {
				continue
			}

			// Generate email
			rendered, err := s.tmplEngine.Render("generic", p, b.Broker)
			if err != nil {
				failed++
				sentPerProfile[p.ID]++
				job.Update(sent, failed, b.Name)
				// Remove from remaining even on failure
				remaining = remaining[1:]
				s.saveJobProgress(job, sent, failed, remaining, sentPerProfile)
				continue
			}

			msg := email.Message{
				To:      b.Email,
				From:    s.config.Email.From,
				// NEW: Add Reply-To header for per-profile routing
				ReplyTo:  p.Emails[0],
				Subject:  rendered.Subject,
				Body:     rendered.Body,
			}

			// Use job's context with timeout for cancellation support
			ctx, cancel := context.WithTimeout(job.Context(), 30*time.Second)
			result := sender.Send(ctx, msg)
			cancel()

			// Record in history with ProfileID
			record := &history.Record{
				BrokerID:   b.ID,
				BrokerName: b.Name,
				Email:      b.Email,
				Template:   "generic",
				SentAt:     time.Now(),
				ProfileID:  p.ID,  // NEW: Track which profile sent
			}

			if result.Success {
				record.Status = history.StatusSent
				record.MessageID = result.MessageID
				sentPerProfile[p.ID]++
				sent++
				job.ResetAuthFailures() // Reset on success
			} else {
				record.Status = history.StatusFailed
				errMsg := ""
				if result.Error != nil {
					errMsg = result.Error.Error()
					record.Error = errMsg
				}
				failed++

				// Check for auth failures and stop if too many consecutive
				if strings.Contains(strings.ToLower(errMsg), "auth") {
					if job.RecordAuthFailure() {
						// Stop job due to auth errors
						if s.historyStore != nil {
							s.historyStore.Add(record)
						}
						remaining = remaining[1:]
						s.saveJobProgress(job, sent, failed, remaining, sentPerProfile)
						job.StopWithError("auth", "Stopped due to repeated authentication failures. Your email provider may have rate-limited or blocked your account. Please check your email settings and try again later.")
						log.Printf("Job stopped: repeated auth failures after %d sent, %d failed", sent, failed)
						return
					}
				}
			}

			if s.historyStore != nil {
				s.historyStore.Add(record)
			}

			// Update job progress
			job.Update(sent, failed, b.Name)

			// Remove processed broker from remaining and save state
			remaining = remaining[1:]
			s.saveJobProgress(job, sent, failed, remaining, sentPerProfile)

			// Rate limit delay (skip on last item)
			if len(remaining) > 0 && !job.IsCancelled() {
				time.Sleep(time.Duration(rateLimitMs) * time.Millisecond)
			}
		}
	}

	// Mark job as complete and clear persisted state
	job.Complete()
	if err := s.jobPersistence.Clear(); err != nil {
		log.Printf("Warning: failed to clear job state: %v", err)
	}
}

// Updated saveJobProgress to handle profile-scoped data
func (s *Server) saveJobProgress(job *Job, sent, failed int, remaining []string, sentPerProfile map[string]int) {
	state := &PersistentJobState{
		ID:               job.ID,
		Status:           job.Status,
		Sent:             sent,
		Failed:           failed,
		Total:            job.Total,
		StartedAt:        job.StartedAt,
		RemainingBrokers: remaining,
		SentPerProfile:   sentPerProfile, // NEW: Store per-profile sent counts
	}
	if err := s.jobPersistence.Save(state); err != nil {
		log.Printf("Warning: failed to save job progress: %v", err)
	}
}

// PersistentJobState with profile support
type PersistentJobState struct {
	ID                 string             `json:"id"`
	Status             JobStatus          `json:"status"`
	Sent               int                `json:"sent"`
	Failed             int                `json:"failed"`
	Total              int                `json:"total"`
	StartedAt          time.Time          `json:"started_at"`
	RemainingBrokers   []string           `json:"remaining_brokers"`
	SelectedProfiles   []config.Profile   `json:"selected_profiles,omitempty"`
	SentPerProfile     map[string]int     `json:"sent_per_profile,omitempty"`
	Search             string             `json:"search,omitempty"`
	Category           string             `json:"category,omitempty"`
	Region             string             `json:"region,omitempty"`
	StatusFilter       string             `json:"status_filter,omitempty"`
}
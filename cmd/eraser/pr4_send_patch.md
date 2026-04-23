# PR 4 — Patch for `cmd/eraser/main.go` (send loop + Reply-To + --profile)

This patch adds multi-profile support to the CLI `send` command. It is the
keystone runtime change: for each profile, for each broker, send — with per-
profile dedup (via `GetLastRequestForProfileAndBroker`) and the profile's
primary email wired to the Reply-To header.

All other commands (`monitor`, `pipeline`, `fill`, `confirm`, …) continue to
use `cfg.Profile` (the primary) in this PR and will get their own `--profile`
flags in PR 6.

---

## 1. Add a `--profile` flag on `sendCmd()`

Replace the existing `sendCmd()` (currently around lines 95–108) with:

```go
var sendProfileID string

func sendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send removal requests to data brokers",
		Long: `Send data removal requests to all configured data brokers.

By default this sends on behalf of every configured profile (the primary
profile plus everyone in additional_profiles). Use --profile <id> to scope
the run to a single profile.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSend(sendProfileID)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview emails without sending")
	cmd.Flags().StringVar(&sendProfileID, "profile", "", "Only send for this profile ID (default: all profiles)")

	return cmd
}
```

---

## 2. Replace `runSend()` (lines 242–371) with the nested version

```go
func runSend(profileID string) error {
	cfg, err := config.Load(resolveConfigPath())
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	// Override dry-run from command line
	if dryRun {
		cfg.Options.DryRun = true
	}

	// Resolve which profiles to send on behalf of.
	var profiles []config.Profile
	if profileID != "" {
		p := cfg.FindProfile(profileID)
		if p == nil {
			return fmt.Errorf("profile %q not found (known IDs: %v)", profileID, profileIDs(cfg))
		}
		profiles = []config.Profile{*p}
	} else {
		profiles = cfg.AllProfiles()
	}

	brokerDB, err := broker.LoadFromFile(resolveBrokerPath())
	if err != nil {
		return fmt.Errorf("failed to load brokers: %w", err)
	}

	brokers := brokerDB.Filter(cfg.Options.Regions, cfg.Options.ExcludedBrokers)
	if len(brokers) == 0 {
		fmt.Println("No brokers to process.")
		return nil
	}

	tmplEngine, err := template.NewEngine()
	if err != nil {
		return fmt.Errorf("failed to initialize templates: %w", err)
	}

	var sender email.Sender
	if !cfg.Options.DryRun {
		sender, err = email.NewSender(cfg.Email)
		if err != nil {
			return fmt.Errorf("failed to initialize email sender: %w", err)
		}
	}

	store, err := history.NewStore(history.DefaultDBPath())
	if err != nil {
		return fmt.Errorf("failed to initialize history: %w", err)
	}
	defer store.Close()

	if cfg.Options.DryRun {
		fmt.Println("🔍 DRY RUN MODE - No emails will be sent")
		fmt.Println()
	}

	totalAttempts := len(profiles) * len(brokers)
	fmt.Printf("📤 Processing %d broker(s) × %d profile(s) = %d request(s)...\n",
		len(brokers), len(profiles), totalAttempts)
	fmt.Println()

	successCount := 0
	failCount := 0
	skippedCount := 0
	sequence := 0

	for _, p := range profiles {
		fmt.Printf("👤 Profile: %s (%s %s)\n", p.ID, p.FirstName, p.LastName)

		for i, b := range brokers {
			fmt.Printf("  [%d/%d] %s (%s)\n", i+1, len(brokers), b.Name, b.Email)

			// Per-(profile, broker) dedup: skip if already sent successfully
			// for THIS profile to THIS broker. A send for profile A must not
			// suppress profile B to the same broker.
			if prev, err := store.GetLastRequestForProfileAndBroker(p.ID, b.ID); err == nil && prev != nil && prev.Status == history.StatusSent {
				fmt.Printf("    ⏭  Already sent on %s — skipping\n", prev.SentAt.Format("2006-01-02"))
				skippedCount++
				continue
			}

			// Render email for this (profile, broker) pair.
			emailMsg, err := tmplEngine.Render(cfg.Options.Template, p, b)
			if err != nil {
				fmt.Printf("    ❌ Failed to render template: %v\n", err)
				failCount++
				continue
			}

			if cfg.Options.DryRun {
				fmt.Printf("    📧 Would send: %s\n", emailMsg.Subject)
				fmt.Printf("    📍 To: %s | Reply-To: %s\n", b.Email, p.Email)
				successCount++
				continue
			}

			// Build the outgoing message. Envelope sender (and From header)
			// stay as the shared cfg.Email.From so SMTP providers accept the
			// mail; Reply-To routes broker replies to the profile's mailbox.
			msg := email.Message{
				To:      b.Email,
				From:    cfg.Email.From,
				ReplyTo: p.Email,
				Subject: emailMsg.Subject,
				Body:    emailMsg.Body,
			}

			sequence++
			ctx := context.WithValue(context.Background(), "sequence", sequence)
			result := sender.Send(ctx, msg)

			record := &history.Record{
				ProfileID:  p.ID,
				BrokerID:   b.ID,
				BrokerName: b.Name,
				Email:      b.Email,
				Template:   cfg.Options.Template,
				SentAt:     time.Now(),
			}

			if result.Success {
				record.Status = history.StatusSent
				record.MessageID = result.MessageID
				fmt.Printf("    ✅ Sent successfully\n")
				successCount++
			} else {
				record.Status = history.StatusFailed
				record.Error = result.Error.Error()
				fmt.Printf("    ❌ Failed: %v\n", result.Error)
				failCount++
			}

			if err := store.Add(record); err != nil {
				fmt.Printf("    ⚠️  Failed to record history: %v\n", err)
			}

			// Rate limit between any two sends, not just within a profile.
			// The last (profile, broker) pair doesn't sleep.
			if !(p.ID == profiles[len(profiles)-1].ID && i == len(brokers)-1) {
				time.Sleep(time.Duration(cfg.Options.RateLimitMs) * time.Millisecond)
			}
		}
		fmt.Println()
	}

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	if cfg.Options.DryRun {
		fmt.Printf("📊 Dry run complete: %d request(s) would be sent (%d skipped as duplicates)\n",
			successCount, skippedCount)
	} else {
		fmt.Printf("📊 Complete: %d sent, %d failed, %d skipped\n", successCount, failCount, skippedCount)
	}

	return nil
}

// profileIDs returns the list of known profile IDs. Used for error messages
// so users get an actionable hint when --profile resolves to nothing.
func profileIDs(cfg *config.Config) []string {
	all := cfg.AllProfiles()
	ids := make([]string, len(all))
	for i, p := range all {
		ids[i] = p.ID
	}
	return ids
}
```

---

## Design notes

* **Envelope sender stays shared.** We do NOT set `msg.From = p.Email`. Many
  SMTP providers (Gmail especially) reject a `MAIL FROM` that isn't the
  authenticated account. Reply-To is the standards-compliant way to route
  replies to a different mailbox.
* **Dedup is profile-scoped.** `GetLastRequestForProfileAndBroker` means
  profile B can still send to broker X even if profile A already did.
* **Order is profile-outer, broker-inner.** This matters for the inbox
  disambiguator's FIFO rule 3 — requests for the same broker land in the DB
  with distinct `sent_at` values.
* **Sequence counter is global.** The `sequence` context value used in
  `MessageID` is incremented across profiles so each message gets a unique ID.
* **`runSend` signature now takes profileID.** If `""`, send for all
  profiles; if non-empty, scope to that one.

## What this patch does NOT change

* The envelope `From` / SMTP auth — still the single shared sender.
* `runFill`, `runConfirm`, `runMonitor` — those get `--profile` in PR 6/7.
* Web UI `/api/send` — that's PR 5/8.

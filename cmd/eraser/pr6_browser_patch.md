# PR 6 — Patch for `cmd/eraser/main.go` (browser commands: --profile on fill/confirm)

`browser.New(cfg, &profile)` already takes a single `*config.Profile`, so the
browser package itself needs no changes for this PR. All work is in the CLI
command wrappers and the DB-lookup paths, which need to be scoped to the
chosen profile.

Each fill/confirm invocation operates on behalf of exactly **one** profile
(the form-filler or link-clicker can only use one identity at a time), so the
`--profile` flag is scalar, not plural.

---

## 1. `fillCmd()` — add `--profile <id>`

Replace the existing `fillCmd()` (around lines 920–969) with:

```go
func fillCmd() *cobra.Command {
	var brokerID string
	var formURL string
	var headless bool
	var autoSubmit bool
	var screenshotDir string
	var pending bool
	var waitForCaptcha bool
	var profileID string

	cmd := &cobra.Command{
		Use:   "fill",
		Short: "Fill opt-out forms using browser automation",
		Long: `Navigate to data broker opt-out forms and automatically fill them using your profile data.

This command uses headless Chrome to:
- Navigate to opt-out form URLs
- Detect and fill form fields with your personal information
- Detect CAPTCHAs (creates tasks for manual solving)
- Optionally submit the form

A single fill run uses ONE profile's data. Pass --profile <id> to pick a
non-default profile; omit it to use the primary profile.

Examples:
  # Fill a specific form URL with the primary profile
  eraser fill --url "https://example.com/optout"

  # Fill for broker spokeo using a specific profile
  eraser fill --broker spokeo --profile jane

  # Fill every pending form for one profile
  eraser fill --pending --profile john

  # Fill with visible browser for debugging
  eraser fill --url "https://example.com/optout" --headless=false

  # Fill form and wait for you to solve CAPTCHA, then auto-submit
  eraser fill --url "https://example.com/optout" --headless=false --wait --submit`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFill(brokerID, formURL, headless, autoSubmit, screenshotDir, pending, waitForCaptcha, profileID)
		},
	}

	cmd.Flags().StringVar(&brokerID, "broker", "", "Broker ID to fill form for (uses URL from pipeline)")
	cmd.Flags().StringVar(&formURL, "url", "", "Direct URL to the opt-out form")
	cmd.Flags().BoolVar(&headless, "headless", true, "Run browser in headless mode")
	cmd.Flags().BoolVar(&autoSubmit, "submit", false, "Automatically submit the form after filling")
	cmd.Flags().StringVar(&screenshotDir, "screenshots", "", "Directory to save screenshots (default: ~/.eraser/screenshots)")
	cmd.Flags().BoolVar(&pending, "pending", false, "Fill all pending forms from the pipeline")
	cmd.Flags().BoolVar(&waitForCaptcha, "wait", false, "Wait for user to solve CAPTCHA before continuing (use with --headless=false)")
	cmd.Flags().StringVar(&profileID, "profile", "", "Profile ID to use (default: primary profile)")

	return cmd
}
```

---

## 2. `runFill` — resolve profile, scope DB lookups, tag tasks

Replace `runFill` (around lines 971–1173). The meaningful diffs vs the
current body:

* New `profileID` parameter.
* Resolve profile via `cfg.FindProfile` (or fall back to primary).
* Use that profile's data for `browser.New` and the CAPTCHA helper JSON.
* Pipeline queries switch to the profile-scoped variants:
  `GetBrokerResponsesForProfile`, `UpdatePipelineStatusForProfile`.
* Pending CAPTCHA tasks are stamped with `ProfileID`.

```go
func runFill(brokerID, formURL string, headless, autoSubmit bool, screenshotDir string, pending, waitForCaptcha bool, profileID string) error {
	cfg, err := config.Load(resolveConfigPath())
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Resolve the profile this run operates under.
	profile := cfg.Profile
	if profileID != "" {
		p := cfg.FindProfile(profileID)
		if p == nil {
			return fmt.Errorf("profile %q not found (known: %v)", profileID, profileIDs(cfg))
		}
		profile = *p
	}
	if profile.ID == "" {
		profile.ID = config.DefaultProfileID
	}

	if screenshotDir == "" {
		home, _ := os.UserHomeDir()
		screenshotDir = filepath.Join(home, ".eraser", "screenshots")
	}

	store, err := history.NewStore(history.DefaultDBPath())
	if err != nil {
		return fmt.Errorf("failed to initialize history: %w", err)
	}
	defer store.Close()

	browserCfg := browser.DefaultConfig()
	browserCfg.Headless = headless
	browserCfg.ScreenshotDir = screenshotDir
	if cfg.Pipeline.BrowserTimeoutSec > 0 {
		browserCfg.Timeout = time.Duration(cfg.Pipeline.BrowserTimeoutSec) * time.Second
	}

	if waitForCaptcha {
		if headless {
			fmt.Println("⚠️  Warning: --wait requires --headless=false to be useful")
		}
		browserCfg.WaitForUser = true
		browserCfg.Timeout = 5 * time.Minute
		browserCfg.WaitCallback = func() error {
			fmt.Println()
			fmt.Println("       ⏸️  CAPTCHA detected! Solve it in the browser window.")
			fmt.Println("       Press ENTER when done (or Ctrl+C to cancel)...")
			fmt.Println()
			reader := bufio.NewReader(os.Stdin)
			_, err := reader.ReadString('\n')
			return err
		}
	}

	// Browser takes the profile pointer — this is the identity that will be
	// typed into form fields.
	b, err := browser.New(browserCfg, &profile)
	if err != nil {
		return fmt.Errorf("failed to create browser: %w", err)
	}
	defer b.Close()

	fmt.Println("🌐 Browser Automation")
	fmt.Printf("👤 Profile: %s (%s %s)\n", profile.ID, profile.FirstName, profile.LastName)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// Build the queue. The pipeline lookups are now profile-scoped so the
	// same broker can appear in multiple profiles' queues without overlap.
	type formJob struct {
		BrokerID string
		URL      string
	}
	var formsToFill []formJob

	switch {
	case formURL != "":
		formsToFill = append(formsToFill, formJob{BrokerID: brokerID, URL: formURL})

	case brokerID != "":
		responses, err := store.GetBrokerResponsesForProfile(profile.ID, "form_required", false, 100)
		if err != nil {
			return fmt.Errorf("failed to get broker responses: %w", err)
		}
		found := false
		for _, resp := range responses {
			if resp.BrokerID == brokerID && resp.FormURL != "" {
				formsToFill = append(formsToFill, formJob{BrokerID: resp.BrokerID, URL: resp.FormURL})
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("no form URL found for broker %q under profile %q", brokerID, profile.ID)
		}

	case pending:
		responses, err := store.GetBrokerResponsesForProfile(profile.ID, "form_required", false, 100)
		if err != nil {
			return fmt.Errorf("failed to get broker responses: %w", err)
		}
		for _, resp := range responses {
			if resp.FormURL != "" {
				formsToFill = append(formsToFill, formJob{BrokerID: resp.BrokerID, URL: resp.FormURL})
			}
		}
		if len(formsToFill) == 0 {
			fmt.Printf("✅ No pending forms to fill for profile %q\n", profile.ID)
			return nil
		}

	default:
		return fmt.Errorf("please specify --url, --broker, or --pending")
	}

	fmt.Printf("📋 Forms to process: %d\n", len(formsToFill))
	fmt.Println()

	for i, form := range formsToFill {
		fmt.Printf("[%d/%d] Processing %s\n", i+1, len(formsToFill), form.URL)
		if form.BrokerID != "" {
			fmt.Printf("       Broker: %s\n", form.BrokerID)
		}

		result, err := b.NavigateAndFill(form.URL, form.BrokerID, autoSubmit)
		if err != nil {
			fmt.Printf("       ❌ Error: %v\n", err)
			continue
		}

		if len(result.FieldsFilled) > 0 {
			fmt.Printf("       ✅ Filled fields: %s\n", strings.Join(result.FieldsFilled, ", "))
		}
		if len(result.FieldsMissing) > 0 {
			fmt.Printf("       ⚠️  Missing profile data for: %s\n", strings.Join(result.FieldsMissing, ", "))
		}

		if result.CaptchaFound {
			fmt.Printf("       🤖 CAPTCHA detected: %s\n", result.CaptchaType)

			// Helper-page data is scoped to this profile.
			profileData := map[string]string{
				"email":     profile.Email,
				"firstName": profile.FirstName,
				"lastName":  profile.LastName,
				"phone":     profile.Phone,
				"address":   profile.Address,
				"city":      profile.City,
				"state":     profile.State,
				"zipCode":   profile.ZipCode,
				"country":   profile.Country,
			}
			profileJSON, _ := json.Marshal(profileData)

			task := &history.PendingTask{
				ProfileID:    profile.ID,
				BrokerID:     form.BrokerID,
				BrokerName:   form.BrokerID, // TODO: broker-name lookup
				TaskType:     history.TaskCaptcha,
				FormURL:      form.URL,
				BrowserState: string(profileJSON),
				Status:       "pending",
			}
			if result.ScreenshotPath != "" {
				task.ScreenshotPath = result.ScreenshotPath
			}

			if err := store.AddPendingTask(task); err != nil {
				fmt.Printf("       ⚠️  Failed to create task: %v\n", err)
			} else {
				fmt.Printf("       📝 Created CAPTCHA task for manual solving\n")
			}

			store.UpdatePipelineStatusForProfile(profile.ID, form.BrokerID, history.PipelineAwaitingCaptcha)
		} else if result.SubmitAttempted {
			fmt.Printf("       📨 Form submitted!\n")
			store.UpdatePipelineStatusForProfile(profile.ID, form.BrokerID, history.PipelineFormFilled)
		} else if result.Success {
			fmt.Printf("       ✅ Form filled (not submitted)\n")
			store.UpdatePipelineStatusForProfile(profile.ID, form.BrokerID, history.PipelineFormFilled)
		}

		if result.ScreenshotPath != "" {
			fmt.Printf("       📸 Screenshot: %s\n", result.ScreenshotPath)
		}

		fmt.Println()
		if i < len(formsToFill)-1 {
			time.Sleep(2 * time.Second)
		}
	}

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("✅ Processed %d forms for profile %q\n", len(formsToFill), profile.ID)
	return nil
}
```

---

## 3. `confirmCmd()` — add `--profile <id>`

Replace `confirmCmd()` (around lines 1175–1219) with:

```go
func confirmCmd() *cobra.Command {
	var confirmURL string
	var brokerID string
	var pending bool
	var validateDomain bool
	var dryRun bool
	var profileID string

	cmd := &cobra.Command{
		Use:   "confirm",
		Short: "Click confirmation links from broker emails",
		Long: `Automatically click confirmation links received from data brokers.

This command makes HTTP GET requests to confirmation URLs to complete the opt-out process.
It follows redirects and verifies success based on the response content.

Like 'fill', confirm is a single-profile operation. Pass --profile <id> to
scope to a non-default profile.

Examples:
  # Confirm a specific URL
  eraser confirm --url "https://broker.com/confirm?token=abc123"

  # Confirm for broker spokeo under profile jane
  eraser confirm --broker spokeo --profile jane

  # Confirm all pending confirmation links for a profile
  eraser confirm --pending --profile john

  # Preview without actually clicking
  eraser confirm --pending --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfirm(confirmURL, brokerID, pending, validateDomain, dryRun, profileID)
		},
	}

	cmd.Flags().StringVar(&confirmURL, "url", "", "Direct confirmation URL to click")
	cmd.Flags().StringVar(&brokerID, "broker", "", "Broker ID to confirm for (uses URL from pipeline)")
	cmd.Flags().BoolVar(&pending, "pending", false, "Confirm all pending confirmation links")
	cmd.Flags().BoolVar(&validateDomain, "validate-domain", true, "Validate URL domain against known brokers")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview links without clicking them")
	cmd.Flags().StringVar(&profileID, "profile", "", "Profile ID to use (default: primary profile)")

	return cmd
}
```

---

## 4. `runConfirm` — scope lookups + pipeline updates

Update `runConfirm` signature and body. The changes are analogous to
`runFill`:

* New `profileID` parameter, resolved against cfg.
* Config is loaded (the current body only loads brokers — we need profiles too).
* Pipeline queries switch to `GetBrokerResponsesForProfile`.
* Pipeline status writes switch to `UpdatePipelineStatusForProfile`.

Replace the signature and the pipeline-lookup + pipeline-update blocks
(around lines 1221–1413). Minimal delta — only lines that touch pipeline
state. Everything else (HTTP handler, redirect printing, dry-run output)
stays identical:

```go
func runConfirm(confirmURL, brokerID string, pending, validateDomain, dryRun bool, profileID string) error {
	// Load config so we can resolve the profile for scoping.
	cfg, err := config.Load(resolveConfigPath())
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	profile := cfg.Profile
	if profileID != "" {
		p := cfg.FindProfile(profileID)
		if p == nil {
			return fmt.Errorf("profile %q not found (known: %v)", profileID, profileIDs(cfg))
		}
		profile = *p
	}
	if profile.ID == "" {
		profile.ID = config.DefaultProfileID
	}

	brokerDB, err := broker.LoadFromFile(resolveBrokerPath())
	if err != nil {
		return fmt.Errorf("failed to load brokers: %w", err)
	}

	store, err := history.NewStore(history.DefaultDBPath())
	if err != nil {
		return fmt.Errorf("failed to initialize history: %w", err)
	}
	defer store.Close()

	// … domain list unchanged …

	handler := browser.NewConfirmationHandler(brokerDomains)

	fmt.Println("🔗 Confirmation Link Handler")
	fmt.Printf("👤 Profile: %s\n", profile.ID)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// ---- queue build (only the lookup call changes) ----
	var linksToConfirm []struct {
		BrokerID string
		URL      string
	}

	if confirmURL != "" {
		linksToConfirm = append(linksToConfirm, struct {
			BrokerID string
			URL      string
		}{BrokerID: brokerID, URL: confirmURL})
	} else if brokerID != "" {
		responses, err := store.GetBrokerResponsesForProfile(profile.ID, "confirmation_required", false, 100)
		if err != nil {
			return fmt.Errorf("failed to get broker responses: %w", err)
		}
		found := false
		for _, resp := range responses {
			if resp.BrokerID == brokerID && resp.ConfirmURL != "" {
				linksToConfirm = append(linksToConfirm, struct {
					BrokerID string
					URL      string
				}{BrokerID: resp.BrokerID, URL: resp.ConfirmURL})
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("no confirmation URL found for broker %q under profile %q", brokerID, profile.ID)
		}
	} else if pending {
		responses, err := store.GetBrokerResponsesForProfile(profile.ID, "confirmation_required", false, 100)
		if err != nil {
			return fmt.Errorf("failed to get broker responses: %w", err)
		}
		for _, resp := range responses {
			if resp.ConfirmURL != "" {
				linksToConfirm = append(linksToConfirm, struct {
					BrokerID string
					URL      string
				}{BrokerID: resp.BrokerID, URL: resp.ConfirmURL})
			}
		}
		if len(linksToConfirm) == 0 {
			fmt.Printf("✅ No pending confirmation links for profile %q\n", profile.ID)
			return nil
		}
	} else {
		return fmt.Errorf("please specify --url, --broker, or --pending")
	}

	// ---- HTTP handling unchanged through line 1367 ----
	// Only the two UpdatePipelineStatus calls near the end need to become
	// UpdatePipelineStatusForProfile:

	//   Success branch:
	if link.BrokerID != "" {
		store.UpdatePipelineStatusForProfile(profile.ID, link.BrokerID, history.PipelineConfirmed)
	}

	//   Failure branch:
	if link.BrokerID != "" {
		store.UpdatePipelineStatusForProfile(profile.ID, link.BrokerID, history.PipelineFailed)
	}

	// ---- summary unchanged ----
	return nil
}
```

---

## Design notes

* **Single profile per invocation** matches the semantics of a single browser
  tab typing a single identity into a form. Running for all profiles would
  require launching N browsers or serialising, either of which is expensive
  and error-prone; `--profile jane && --profile john` (two commands) is the
  clean answer.
* **Fall back to primary on empty `--profile`.** Legacy scripts without
  `--profile` keep working exactly as before — `profile = cfg.Profile`, whose
  `ID` defaults to `"default"` via `normaliseProfiles()` in PR 1.
* **Profile-scoped pipeline updates prevent cross-profile bleed.** If Jane's
  Spokeo confirm succeeds, John's Spokeo status is unaffected, because both
  rows have distinct (profile_id, broker_id) keys.
* **No browser package changes.** `browser.New(cfg, *config.Profile)` already
  scopes at the right level — we just give it the right profile pointer.

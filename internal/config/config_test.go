package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempConfig writes yaml content to a temp file with 0600 perms and
// returns its path. Restricted perms match what Save() would produce, so the
// permissions check in Load() doesn't print a warning to stderr.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

// TestLoad_LegacySingleProfile verifies that an existing single-profile config
// (the shape every current user has) loads unchanged and normalises cleanly.
// This is the backward-compatibility contract.
func TestLoad_LegacySingleProfile(t *testing.T) {
	yaml := `
profile:
  first_name: Jane
  last_name: Doe
  email: jane@example.com
  address: 123 Main St
  city: San Francisco

email:
  provider: smtp
  from: jane@gmail.com
  smtp:
    host: smtp.gmail.com
    port: 465
    username: jane@gmail.com
    password: app-password
    use_tls: true

options:
  template: generic
`
	cfg, err := Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Profile.FirstName != "Jane" || cfg.Profile.LastName != "Doe" {
		t.Errorf("primary profile name not preserved")
	}
	if cfg.Profile.ID != DefaultProfileID {
		t.Errorf("legacy primary profile must get ID %q, got %q", DefaultProfileID, cfg.Profile.ID)
	}
	if len(cfg.Profile.Emails) != 1 || cfg.Profile.Emails[0] != "jane@example.com" {
		t.Errorf("legacy Email should backfill Emails=[Email], got %v", cfg.Profile.Emails)
	}
	if cfg.Profile.Email != "jane@example.com" {
		t.Errorf("legacy Email field must be preserved")
	}
	if len(cfg.AdditionalProfiles) != 0 {
		t.Errorf("legacy config should not gain additional profiles, got %d", len(cfg.AdditionalProfiles))
	}
	if len(cfg.AllProfiles()) != 1 {
		t.Errorf("AllProfiles() should return 1 for legacy config, got %d", len(cfg.AllProfiles()))
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("legacy config should validate, got %v", err)
	}
}

// TestLoad_MultiProfile verifies the new shape: primary + additional_profiles,
// each with optional emails[].
func TestLoad_MultiProfile(t *testing.T) {
	yaml := `
profile:
  id: jane
  first_name: Jane
  last_name: Doe
  emails:
    - jane@gmail.com
    - jane.doe@work.com

additional_profiles:
  - first_name: John
    last_name: Doe
    emails:
      - john@gmail.com
      - jdoe@oldjob.com
  - id: kid1
    first_name: Alex
    last_name: Doe
    email: alex@example.com

email:
  provider: smtp
  from: household@gmail.com
  smtp:
    host: smtp.gmail.com
    port: 465
    username: household@gmail.com
    password: app-password
    use_tls: true
`
	cfg, err := Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	profiles := cfg.AllProfiles()
	if len(profiles) != 3 {
		t.Fatalf("expected 3 profiles, got %d", len(profiles))
	}

	// Primary explicitly set its ID to "jane" — that wins over DefaultProfileID.
	if profiles[0].ID != "jane" {
		t.Errorf("primary profile ID should be %q, got %q", "jane", profiles[0].ID)
	}
	// Primary's Email field should backfill from Emails[0].
	if profiles[0].Email != "jane@gmail.com" {
		t.Errorf("primary Email should backfill to Emails[0], got %q", profiles[0].Email)
	}

	// John has no ID set → slugified from name.
	if profiles[1].ID != "john-doe" {
		t.Errorf("john's ID should slugify to %q, got %q", "john-doe", profiles[1].ID)
	}
	if profiles[1].Email != "john@gmail.com" {
		t.Errorf("john's Email should backfill from Emails[0], got %q", profiles[1].Email)
	}

	// Kid1 set ID explicitly; Email set, Emails should backfill.
	if profiles[2].ID != "kid1" {
		t.Errorf("kid1 ID should be preserved, got %q", profiles[2].ID)
	}
	if len(profiles[2].Emails) != 1 || profiles[2].Emails[0] != "alex@example.com" {
		t.Errorf("kid1 Emails should backfill from Email, got %v", profiles[2].Emails)
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("multi-profile config should validate, got %v", err)
	}
}

// TestLoad_IDCollision verifies that a user-assigned ID colliding with a
// slugified name gets disambiguated with -2.
func TestLoad_IDCollision(t *testing.T) {
	yaml := `
profile:
  id: john-doe
  first_name: John
  last_name: Doe
  email: john1@example.com

additional_profiles:
  - first_name: John
    last_name: Doe
    email: john2@example.com

email:
  provider: smtp
  from: shared@example.com
  smtp:
    host: smtp.example.com
    port: 465
    username: shared@example.com
    password: pw
    use_tls: true
`
	cfg, err := Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Profile.ID != "john-doe" {
		t.Errorf("primary ID should be preserved as %q", "john-doe")
	}
	if cfg.AdditionalProfiles[0].ID != "john-doe-2" {
		t.Errorf("collided ID should disambiguate to %q, got %q", "john-doe-2", cfg.AdditionalProfiles[0].ID)
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("config with disambiguated IDs should validate, got %v", err)
	}
}

func TestValidate_MissingEmail(t *testing.T) {
	yaml := `
profile:
  first_name: Jane
  last_name: Doe

additional_profiles:
  - first_name: John
    last_name: Doe
    email: john@example.com

email:
  provider: smtp
  from: shared@example.com
  smtp:
    host: smtp.example.com
    port: 465
    username: shared@example.com
    password: pw
    use_tls: true
`
	cfg, err := Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("Validate should reject a profile with no emails")
	}
	if !strings.Contains(err.Error(), "at least one email") {
		t.Errorf("error should mention missing email, got %v", err)
	}
}

func TestValidate_MissingName(t *testing.T) {
	yaml := `
profile:
  first_name: Jane
  email: jane@example.com

email:
  provider: smtp
  from: shared@example.com
  smtp:
    host: smtp.example.com
    port: 465
    username: shared@example.com
    password: pw
    use_tls: true
`
	cfg, err := Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("Validate should reject a profile missing last_name")
	}
	if !strings.Contains(err.Error(), "first_name and last_name") {
		t.Errorf("error should mention name fields, got %v", err)
	}
}

func TestAllEmails_EmptyProfile(t *testing.T) {
	var p Profile
	if got := p.AllEmails(); got != nil {
		t.Errorf("empty profile should return nil emails, got %v", got)
	}
}

func TestFindProfile(t *testing.T) {
	cfg := &Config{
		Profile: Profile{ID: "jane", FirstName: "Jane", LastName: "Doe"},
		AdditionalProfiles: []Profile{
			{ID: "john", FirstName: "John", LastName: "Doe"},
		},
	}
	if p := cfg.FindProfile("jane"); p == nil || p.FirstName != "Jane" {
		t.Errorf("FindProfile(\"jane\") should return primary profile")
	}
	if p := cfg.FindProfile("john"); p == nil || p.FirstName != "John" {
		t.Errorf("FindProfile(\"john\") should return additional profile")
	}
	if p := cfg.FindProfile("nobody"); p != nil {
		t.Errorf("FindProfile for unknown ID should return nil")
	}
}

// TestLoad_RoundTrip verifies Save() followed by Load() preserves structure
// for a multi-profile config. This catches YAML tag issues.
func TestLoad_RoundTrip(t *testing.T) {
	original := &Config{
		Profile: Profile{
			ID:        "jane",
			FirstName: "Jane",
			LastName:  "Doe",
			Email:     "jane@example.com",
			Emails:    []string{"jane@example.com", "jane2@example.com"},
		},
		AdditionalProfiles: []Profile{
			{
				ID:        "john",
				FirstName: "John",
				LastName:  "Doe",
				Email:     "john@example.com",
				Emails:    []string{"john@example.com"},
			},
		},
		Email: EmailConfig{
			Provider: "smtp",
			From:     "shared@example.com",
			SMTP: SMTPConfig{
				Host:     "smtp.example.com",
				Port:     465,
				Username: "shared@example.com",
				Password: "pw",
				UseTLS:   true,
			},
		},
		Options: Options{Template: "generic", RateLimitMs: 2000},
	}

	path := filepath.Join(t.TempDir(), "rt.yaml")
	if err := Save(path, original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Profile.ID != original.Profile.ID {
		t.Errorf("primary ID changed: %q → %q", original.Profile.ID, loaded.Profile.ID)
	}
	if len(loaded.Profile.Emails) != 2 {
		t.Errorf("primary Emails length changed: %d → %d", 2, len(loaded.Profile.Emails))
	}
	if len(loaded.AdditionalProfiles) != 1 || loaded.AdditionalProfiles[0].ID != "john" {
		t.Errorf("additional profile not preserved through round-trip")
	}
}

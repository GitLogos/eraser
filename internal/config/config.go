package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultRateLimitMs = 2000

// DefaultProfileID is the stable ID assigned to the primary profile of a legacy
// (single-profile) config. Existing history rows migrate to this ID via the
// DEFAULT clause on the profile_id columns, so continuity is preserved.
const DefaultProfileID = "default"

func checkFilePermissions(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0077 != 0 {
		return fmt.Errorf("config file %s has insecure permissions %04o; should be 0600", path, perm)
	}
	return nil
}

type Config struct {
	Profile            Profile     `yaml:"profile"`
	AdditionalProfiles []Profile   `yaml:"additional_profiles,omitempty"`
	Email              EmailConfig `yaml:"email"`
	Options            Options     `yaml:"options"`
	Inbox              InboxConfig `yaml:"inbox,omitempty"`
	Pipeline           Pipeline    `yaml:"pipeline,omitempty"`
}

// InboxConfig holds IMAP settings for monitoring broker responses
type InboxConfig struct {
	Enabled       bool   `yaml:"enabled"`
	Provider      string `yaml:"provider"`       // "gmail", "outlook", "imap"
	Server        string `yaml:"server"`         // e.g., "imap.gmail.com"
	Port          int    `yaml:"port"`           // e.g., 993
	Email         string `yaml:"email"`          // Email address to monitor
	Password      string `yaml:"password"`       // App password (not main password)
	Folder        string `yaml:"folder"`         // Folder to monitor (default: "INBOX")
	AutoArchive   bool   `yaml:"auto_archive"`   // Automatically move processed emails to archive folder
	ArchiveFolder string `yaml:"archive_folder"` // Folder to archive emails to (default: "Eraser")
}

// Pipeline holds settings for the automation pipeline
type Pipeline struct {
	AutoConfirm       bool `yaml:"auto_confirm"`        // Auto-click confirmation links
	AutoFillForms     bool `yaml:"auto_fill_forms"`     // Enable browser automation for forms
	BrowserHeadless   bool `yaml:"browser_headless"`    // Run browser in headless mode
	BrowserTimeoutSec int  `yaml:"browser_timeout_sec"` // Browser operation timeout
}

type Profile struct {
	ID          string   `yaml:"id,omitempty"`
	FirstName   string   `yaml:"first_name"`
	LastName    string   `yaml:"last_name"`
	Email       string   `yaml:"email"`
	Emails      []string `yaml:"emails,omitempty"`
	Address     string   `yaml:"address,omitempty"`
	City        string   `yaml:"city,omitempty"`
	State       string   `yaml:"state,omitempty"`
	ZipCode     string   `yaml:"zip_code,omitempty"`
	Country     string   `yaml:"country,omitempty"`
	Phone       string   `yaml:"phone,omitempty"`
	DateOfBirth string   `yaml:"date_of_birth,omitempty"`
}

func (p Profile) FullName() string { return p.FirstName + " " + p.LastName }

// AllEmails returns every email address associated with this profile. It is
// always non-empty after Load() has normalised the config: if Emails is empty
// it contains only the single .Email value.
func (p Profile) AllEmails() []string {
	if len(p.Emails) > 0 {
		return p.Emails
	}
	if p.Email != "" {
		return []string{p.Email}
	}
	return nil
}

type EmailConfig struct {
	Provider string     `yaml:"provider"`
	From     string     `yaml:"from"`
	SMTP     SMTPConfig `yaml:"smtp,omitempty"`
}

type Email = EmailConfig

type SMTPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	UseTLS   bool   `yaml:"use_tls"`
}

type Options struct {
	Template        string   `yaml:"template"`
	DryRun          bool     `yaml:"dry_run"`
	RateLimitMs     int      `yaml:"rate_limit_ms"`
	Regions         []string `yaml:"regions"`
	ExcludedBrokers []string `yaml:"excluded_brokers,omitempty"`
}

func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".eraser", "config.yaml")
}

// AllProfiles returns the primary profile followed by any additional profiles,
// in declaration order. Callers that need to iterate every profile (send loop,
// template rendering, history dedup) should use this helper rather than
// touching Profile/AdditionalProfiles directly.
func (c *Config) AllProfiles() []Profile {
	out := make([]Profile, 0, 1+len(c.AdditionalProfiles))
	out = append(out, c.Profile)
	out = append(out, c.AdditionalProfiles...)
	return out
}

// FindProfile returns the profile with the given ID, or nil if none matches.
func (c *Config) FindProfile(id string) *Profile {
	for i := range c.AdditionalProfiles {
		if c.AdditionalProfiles[i].ID == id {
			return &c.AdditionalProfiles[i]
		}
	}
	if c.Profile.ID == id {
		return &c.Profile
	}
	return nil
}

func Load(path string) (*Config, error) {
	if err := checkFilePermissions(path); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: %v\n", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if cfg.Options.Template == "" {
		cfg.Options.Template = "generic"
	}
	if cfg.Options.RateLimitMs == 0 {
		cfg.Options.RateLimitMs = defaultRateLimitMs
	}

	// Set inbox defaults
	if cfg.Inbox.Folder == "" {
		cfg.Inbox.Folder = "INBOX"
	}
	if cfg.Inbox.ArchiveFolder == "" {
		cfg.Inbox.ArchiveFolder = "Eraser"
	}
	if cfg.Inbox.Provider == "gmail" && cfg.Inbox.Server == "" {
		cfg.Inbox.Server = "imap.gmail.com"
		cfg.Inbox.Port = 993
	}
	if cfg.Inbox.Provider == "outlook" && cfg.Inbox.Server == "" {
		cfg.Inbox.Server = "outlook.office365.com"
		cfg.Inbox.Port = 993
	}

	// Set pipeline defaults
	if cfg.Pipeline.BrowserTimeoutSec == 0 {
		cfg.Pipeline.BrowserTimeoutSec = 30
	}
	cfg.Pipeline.BrowserHeadless = true // Default to headless

	// Normalise all profiles: backfill IDs, sync Email <-> Emails, ensure
	// unique IDs across primary + additional profiles.
	normaliseProfiles(&cfg)

	return &cfg, nil
}

// normaliseProfiles backfills missing fields on every profile and guarantees
// stable, unique IDs. It mutates cfg in place.
//
// Rules (applied in order):
//  1. If Emails is empty but Email is set, Emails = [Email].
//  2. If Email is empty but Emails is non-empty, Email = Emails[0].
//  3. The primary profile, if its ID is empty, gets DefaultProfileID so legacy
//     history rows (which migrated with DEFAULT 'default') remain attributed
//     to the same logical profile.
//  4. Additional profiles with empty ID are slug-assigned from their name.
//  5. Any ID collision across all profiles is disambiguated with -2, -3, ...
func normaliseProfiles(cfg *Config) {
	// Step 1 & 2: email <-> emails reconciliation.
	syncEmails(&cfg.Profile)
	for i := range cfg.AdditionalProfiles {
		syncEmails(&cfg.AdditionalProfiles[i])
	}

	// Step 3: primary gets the default ID if unset.
	if cfg.Profile.ID == "" {
		cfg.Profile.ID = DefaultProfileID
	}

	// Step 4 & 5: assign + dedupe IDs across all profiles.
	seen := make(map[string]bool)
	seen[cfg.Profile.ID] = true
	for i := range cfg.AdditionalProfiles {
		p := &cfg.AdditionalProfiles[i]
		if p.ID == "" {
			p.ID = slugify(p.FirstName + "-" + p.LastName)
			if p.ID == "" || p.ID == "-" {
				p.ID = fmt.Sprintf("profile-%d", i+2)
			}
		}
		p.ID = ensureUnique(p.ID, seen)
		seen[p.ID] = true
	}
}

func syncEmails(p *Profile) {
	if len(p.Emails) == 0 && p.Email != "" {
		p.Emails = []string{p.Email}
		return
	}
	if p.Email == "" && len(p.Emails) > 0 {
		p.Email = p.Emails[0]
	}
}

var slugifyRE = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugifyRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

func ensureUnique(id string, seen map[string]bool) string {
	if !seen[id] {
		return id
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", id, n)
		if !seen[candidate] {
			return candidate
		}
	}
}

func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to serialize config: %w", err)
	}
	return os.WriteFile(path, data, 0600)
}

func (c *Config) Validate() error {
	// Every profile must have a name and at least one email. Email provider
	// configuration is validated once (shared sender).
	profiles := c.AllProfiles()
	if len(profiles) == 0 {
		return fmt.Errorf("profile: at least one profile is required")
	}

	ids := make(map[string]bool)
	for i, p := range profiles {
		label := fmt.Sprintf("profile[%d]", i)
		if p.ID != "" {
			label = fmt.Sprintf("profile %q", p.ID)
		}
		if p.FirstName == "" || p.LastName == "" {
			return fmt.Errorf("%s: first_name and last_name are required", label)
		}
		if len(p.AllEmails()) == 0 {
			return fmt.Errorf("%s: at least one email is required", label)
		}
		if ids[p.ID] {
			return fmt.Errorf("%s: duplicate profile id %q", label, p.ID)
		}
		ids[p.ID] = true
	}

	if c.Email.Provider == "" {
		return fmt.Errorf("email: provider is required")
	}
	if c.Email.From == "" {
		return fmt.Errorf("email: from address is required")
	}
	if c.Email.Provider != "smtp" {
		return fmt.Errorf("email: unknown provider %q (only smtp is supported)", c.Email.Provider)
	}
	if c.Email.SMTP.Host == "" {
		return fmt.Errorf("email.smtp: host is required")
	}
	if c.Email.SMTP.Port == 0 {
		return fmt.Errorf("email.smtp: port is required")
	}

	return nil
}

// ValidateInbox validates inbox configuration (only called when inbox monitoring is used)
func (c *Config) ValidateInbox() error {
	if !c.Inbox.Enabled {
		return fmt.Errorf("inbox: monitoring is not enabled in config")
	}
	if c.Inbox.Email == "" {
		return fmt.Errorf("inbox: email address is required")
	}
	if c.Inbox.Password == "" {
		return fmt.Errorf("inbox: password (app password) is required")
	}
	if c.Inbox.Server == "" {
		return fmt.Errorf("inbox: IMAP server is required")
	}
	if c.Inbox.Port == 0 {
		return fmt.Errorf("inbox: IMAP port is required")
	}
	return nil
}

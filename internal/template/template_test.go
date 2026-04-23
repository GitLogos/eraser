package template

import (
	"strings"
	"testing"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
)

func testBroker() broker.Broker {
	return broker.Broker{
		ID:        "example",
		Name:      "Example Broker",
		Email:     "privacy@example.com",
		Website:   "https://example.com",
		OptOutURL: "https://example.com/optout",
		Region:    "us",
		Category:  "people-search",
	}
}

// TestRender_SingleEmail_BackwardCompatible is the critical invariant: a
// profile with a single email must produce output that uses the legacy
// "- Email Address: <addr>" line, never the bulleted list. Any drift here
// means we've broken existing users.
func TestRender_SingleEmail_BackwardCompatible(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	profile := config.Profile{
		FirstName: "Jane",
		LastName:  "Doe",
		Email:     "jane@example.com",
		// Emails is left empty on purpose — Render() must fall back gracefully.
	}

	for _, tmplName := range []string{"generic", "gdpr", "ccpa"} {
		t.Run(tmplName, func(t *testing.T) {
			result, err := engine.Render(tmplName, profile, testBroker())
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !strings.Contains(result.Body, "- Email Address: jane@example.com") {
				t.Errorf("%s: expected legacy single-email line, got:\n%s", tmplName, result.Body)
			}
			if strings.Contains(result.Body, "- Email Addresses:") {
				t.Errorf("%s: unexpected plural 'Email Addresses' block for single-email profile", tmplName)
			}
		})
	}
}

// TestRender_MultipleEmails verifies the bulleted list appears when a profile
// has more than one email, and that every address is present.
func TestRender_MultipleEmails(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	profile := config.Profile{
		FirstName: "Jane",
		LastName:  "Doe",
		Email:     "jane@example.com",
		Emails:    []string{"jane@example.com", "jane.doe@work.com", "j.doe1990@yahoo.com"},
	}

	for _, tmplName := range []string{"generic", "gdpr", "ccpa"} {
		t.Run(tmplName, func(t *testing.T) {
			result, err := engine.Render(tmplName, profile, testBroker())
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !strings.Contains(result.Body, "- Email Addresses:") {
				t.Errorf("%s: expected 'Email Addresses:' header, got:\n%s", tmplName, result.Body)
			}
			// Legacy single-email line must not appear when the plural block is emitted.
			if strings.Contains(result.Body, "- Email Address: ") {
				t.Errorf("%s: unexpected legacy single-email line for multi-email profile", tmplName)
			}
			for _, addr := range profile.Emails {
				if !strings.Contains(result.Body, "  - "+addr) {
					t.Errorf("%s: expected bulleted email %q in body", tmplName, addr)
				}
			}
		})
	}
}

// TestRender_EmailsBackfillFromEmail verifies that a profile with only .Email
// set (no .Emails) still renders correctly — the Render function has a
// defensive fallback for callers bypassing config.Load.
func TestRender_EmailsBackfillFromEmail(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	profile := config.Profile{
		FirstName: "John",
		LastName:  "Doe",
		Email:     "john@example.com",
	}
	result, err := engine.Render("generic", profile, testBroker())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(result.Body, "- Email Address: john@example.com") {
		t.Errorf("expected single-email line from Email fallback, got:\n%s", result.Body)
	}
}

// TestRender_ContactLineUsesPrimaryEmail verifies that the "contact me at"
// line in every template uses .Email (the primary), not the whole list.
// Brokers need one canonical reply target.
func TestRender_ContactLineUsesPrimaryEmail(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	profile := config.Profile{
		FirstName: "Jane",
		LastName:  "Doe",
		Email:     "jane@example.com",
		Emails:    []string{"jane@example.com", "jane2@example.com"},
	}
	for _, tmplName := range []string{"generic", "gdpr", "ccpa"} {
		t.Run(tmplName, func(t *testing.T) {
			result, err := engine.Render(tmplName, profile, testBroker())
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !strings.Contains(result.Body, "contact me at jane@example.com") {
				t.Errorf("%s: expected 'contact me at <primary email>', got:\n%s", tmplName, result.Body)
			}
		})
	}
}

package inbox

import (
	"testing"
	"time"

	"github.com/eraser-privacy/eraser/internal/history"
)

type fakeStore struct {
	records map[string][]history.Record // brokerID → records
}

func (f *fakeStore) GetInFlightBrokerRequests(brokerID string) ([]history.Record, error) {
	return f.records[brokerID], nil
}

func makeEmail(broker, body string) *Email {
	return &Email{BrokerID: broker, Subject: "Re: Your privacy request", Body: body}
}

func TestMatchProfile_SingleProfile_ShortCircuits(t *testing.T) {
	m := MatchProfile(makeEmail("spokeo", ""), []ProfileEmails{}, &fakeStore{})
	if m.ProfileID != history.DefaultProfileID {
		t.Errorf("expected %q, got %q", history.DefaultProfileID, m.ProfileID)
	}
	if m.Rule != RuleDefaultOnly {
		t.Errorf("expected rule=%s, got %s", RuleDefaultOnly, m.Rule)
	}
}

func TestMatchProfile_SingleInFlight_Wins(t *testing.T) {
	store := &fakeStore{records: map[string][]history.Record{
		"spokeo": {{ProfileID: "jane", BrokerID: "spokeo", SentAt: time.Now()}},
	}}
	profiles := []ProfileEmails{
		{ProfileID: "jane", Emails: []string{"jane@example.com"}},
		{ProfileID: "john", Emails: []string{"john@example.com"}},
	}
	m := MatchProfile(makeEmail("spokeo", "Dear customer..."), profiles, store)
	if m.ProfileID != "jane" {
		t.Errorf("expected jane, got %q", m.ProfileID)
	}
	if m.Rule != RuleSingleInFlight {
		t.Errorf("expected rule=%s, got %s", RuleSingleInFlight, m.Rule)
	}
}

func TestMatchProfile_BodyEmailMatch_Wins(t *testing.T) {
	// Two profiles both in-flight, but body mentions john's email only.
	store := &fakeStore{records: map[string][]history.Record{
		"spokeo": {
			{ProfileID: "jane", BrokerID: "spokeo", SentAt: time.Now().Add(-2 * time.Hour)},
			{ProfileID: "john", BrokerID: "spokeo", SentAt: time.Now().Add(-1 * time.Hour)},
		},
	}}
	profiles := []ProfileEmails{
		{ProfileID: "jane", Emails: []string{"jane@example.com"}},
		{ProfileID: "john", Emails: []string{"john@example.com", "john.work@example.com"}},
	}
	body := "We are processing the removal for john@example.com."
	m := MatchProfile(makeEmail("spokeo", body), profiles, store)
	if m.ProfileID != "john" {
		t.Errorf("expected john, got %q", m.ProfileID)
	}
	if m.Rule != RuleBodyEmailMatch {
		t.Errorf("expected rule=%s, got %s", RuleBodyEmailMatch, m.Rule)
	}
}

func TestMatchProfile_BodyEmailMatch_NotASubstring(t *testing.T) {
	// jane@example.com must NOT match janex@example.com.
	store := &fakeStore{records: map[string][]history.Record{
		"spokeo": {
			{ProfileID: "jane", BrokerID: "spokeo", SentAt: time.Now().Add(-1 * time.Hour)},
			{ProfileID: "john", BrokerID: "spokeo", SentAt: time.Now().Add(-30 * time.Minute)},
		},
	}}
	profiles := []ProfileEmails{
		{ProfileID: "jane", Emails: []string{"jane@example.com"}},
		{ProfileID: "john", Emails: []string{"john@example.com"}},
	}
	body := "Email janex@example.com for support."
	m := MatchProfile(makeEmail("spokeo", body), profiles, store)
	if m.Rule == RuleBodyEmailMatch {
		t.Errorf("should not have matched by body; got rule=%s profile=%s", m.Rule, m.ProfileID)
	}
}

func TestMatchProfile_FIFO_Wins(t *testing.T) {
	// Two in-flight, body mentions nobody → fall to FIFO.
	now := time.Now()
	store := &fakeStore{records: map[string][]history.Record{
		"spokeo": {
			{ProfileID: "jane", BrokerID: "spokeo", SentAt: now.Add(-3 * time.Hour)},
			{ProfileID: "john", BrokerID: "spokeo", SentAt: now.Add(-1 * time.Hour)},
		},
	}}
	profiles := []ProfileEmails{
		{ProfileID: "jane", Emails: []string{"jane@example.com"}},
		{ProfileID: "john", Emails: []string{"john@example.com"}},
	}
	m := MatchProfile(makeEmail("spokeo", "Generic processing update."), profiles, store)
	if m.Rule != RuleFIFO {
		t.Errorf("expected rule=%s, got %s", RuleFIFO, m.Rule)
	}
	if m.ProfileID != "jane" {
		t.Errorf("expected jane (oldest), got %q", m.ProfileID)
	}
}

func TestMatchProfile_NeedsReview_WhenNoSignal(t *testing.T) {
	// Nothing in-flight for this broker, multiple profiles configured,
	// no email hit in body → needs_review.
	store := &fakeStore{records: map[string][]history.Record{}}
	profiles := []ProfileEmails{
		{ProfileID: "jane", Emails: []string{"jane@example.com"}},
		{ProfileID: "john", Emails: []string{"john@example.com"}},
	}
	m := MatchProfile(makeEmail("unknown-broker", "Hi."), profiles, store)
	if m.Rule != RuleNeedsReview {
		t.Errorf("expected rule=%s, got %s", RuleNeedsReview, m.Rule)
	}
	if !m.NeedsReview {
		t.Errorf("expected NeedsReview=true")
	}
	if m.ProfileID != "" {
		t.Errorf("expected empty profile, got %q", m.ProfileID)
	}
}

func TestMatchProfile_BodyMultipleHits_FallsThrough(t *testing.T) {
	// If body mentions BOTH profiles' emails, rule 2 is ambiguous and we
	// should fall through to FIFO.
	now := time.Now()
	store := &fakeStore{records: map[string][]history.Record{
		"spokeo": {
			{ProfileID: "jane", BrokerID: "spokeo", SentAt: now.Add(-2 * time.Hour)},
			{ProfileID: "john", BrokerID: "spokeo", SentAt: now.Add(-1 * time.Hour)},
		},
	}}
	profiles := []ProfileEmails{
		{ProfileID: "jane", Emails: []string{"jane@example.com"}},
		{ProfileID: "john", Emails: []string{"john@example.com"}},
	}
	body := "Copying jane@example.com and john@example.com on this reply."
	m := MatchProfile(makeEmail("spokeo", body), profiles, store)
	if m.Rule != RuleFIFO {
		t.Errorf("expected rule=%s (fallback from ambiguous body), got %s", RuleFIFO, m.Rule)
	}
	if m.ProfileID != "jane" {
		t.Errorf("expected jane (oldest), got %q", m.ProfileID)
	}
}

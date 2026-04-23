package inbox

import (
	"strings"

	"github.com/eraser-privacy/eraser/internal/history"
)

// ProfileMatchRule is the winning rule that resolved a profile for a reply.
// Stored on the result for observability — the web UI can colour-code by
// rule so users see how confident the match is.
type ProfileMatchRule string

const (
	RuleSingleInFlight ProfileMatchRule = "single_in_flight"  // only one profile has an open request to this broker
	RuleBodyEmailMatch ProfileMatchRule = "body_email_match"  // reply body contains one of a profile's emails
	RuleFIFO           ProfileMatchRule = "fifo_oldest"       // multiple candidates, pick oldest in-flight
	RuleNeedsReview    ProfileMatchRule = "needs_review"      // couldn't disambiguate; human must look
	RuleDefaultOnly    ProfileMatchRule = "default_only"      // no profiles configured beyond default
)

// ProfileMatch holds the resolution outcome.
type ProfileMatch struct {
	ProfileID   string
	Rule        ProfileMatchRule
	Candidates  []string // all profile IDs that were considered (for logging/UI)
	NeedsReview bool
	Confidence  float64 // 0.0-1.0, mirrors what ClassifyResponse does
	Reason      string  // human-readable explanation
}

// EmailAddresses is the minimum slice of per-profile data the matcher needs:
// profile ID + every email address that profile declared. Using a small
// struct instead of config.Profile keeps this package free of a circular
// dependency risk with config.
type ProfileEmails struct {
	ProfileID string
	Emails    []string // all addresses (primary + extras), lowercased
}

// HistoryLookup is the read-only interface on history.Store that the matcher
// needs. Defined here so the package can be tested with a fake store.
type HistoryLookup interface {
	GetInFlightBrokerRequests(brokerID string) ([]history.Record, error)
}

// MatchProfile applies the 4-rule disambiguation ladder:
//
//   1. If exactly one profile has an in-flight request to this broker,
//      match it. (Fast path — covers the vast majority of replies.)
//   2. If the reply body or subject contains one of a profile's email
//      addresses, match to that profile.
//   3. Otherwise, match to the profile whose in-flight request is oldest
//      (FIFO). Data brokers tend to process removal emails in order.
//   4. If none of the above apply, flag needs_review.
//
// Rules are applied in order, short-circuiting on the first match.
//
// `email` may be nil only if you're testing classifier-agnostic cases;
// production callers always have a populated Email.
func MatchProfile(email *Email, profiles []ProfileEmails, store HistoryLookup) ProfileMatch {
	// Short-circuit: no multi-profile setup at all.
	if len(profiles) <= 1 {
		id := history.DefaultProfileID
		if len(profiles) == 1 {
			id = profiles[0].ProfileID
		}
		return ProfileMatch{
			ProfileID:  id,
			Rule:       RuleDefaultOnly,
			Candidates: []string{id},
			Confidence: 1.0,
			Reason:     "only one profile configured",
		}
	}

	candidateIDs := make([]string, len(profiles))
	for i, p := range profiles {
		candidateIDs[i] = p.ProfileID
	}

	// --- Rule 1: single in-flight ---
	var inFlight []history.Record
	if email != nil && email.BrokerID != "" {
		recs, err := store.GetInFlightBrokerRequests(email.BrokerID)
		if err == nil {
			inFlight = recs
		}
	}

	// Distinct in-flight profile IDs for this broker.
	inFlightProfiles := uniqueProfileIDs(inFlight)
	if len(inFlightProfiles) == 1 {
		return ProfileMatch{
			ProfileID:  inFlightProfiles[0],
			Rule:       RuleSingleInFlight,
			Candidates: candidateIDs,
			Confidence: 0.95,
			Reason:     "only one profile has an open request to this broker",
		}
	}

	// --- Rule 2: body contains a profile email ---
	if email != nil {
		haystack := strings.ToLower(email.Subject + "\n" + email.Body + "\n" + email.HTMLBody)
		var hits []string
		for _, p := range profiles {
			for _, addr := range p.Emails {
				addr = strings.ToLower(strings.TrimSpace(addr))
				if addr == "" {
					continue
				}
				// Bound each hit to non-alphanumeric boundaries so
				// "jane@x.com" doesn't match "janex@x.com". A plain
				// Contains() is too loose here.
				if containsAsToken(haystack, addr) {
					hits = append(hits, p.ProfileID)
					break // no need to keep scanning this profile's emails
				}
			}
		}
		hits = dedupe(hits)
		if len(hits) == 1 {
			return ProfileMatch{
				ProfileID:  hits[0],
				Rule:       RuleBodyEmailMatch,
				Candidates: candidateIDs,
				Confidence: 0.90,
				Reason:     "reply references this profile's email address",
			}
		}
		// More than one hit → don't use rule 2 (ambiguous).
	}

	// --- Rule 3: FIFO over in-flight requests ---
	if len(inFlightProfiles) > 1 {
		oldest := findOldest(inFlight)
		if oldest != "" {
			return ProfileMatch{
				ProfileID:   oldest,
				Rule:        RuleFIFO,
				Candidates:  candidateIDs,
				Confidence:  0.60, // deliberately lower — FIFO is a heuristic
				Reason:      "multiple open requests; matched to oldest by sent_at",
				NeedsReview: false,
			}
		}
	}

	// --- Rule 4: needs review ---
	return ProfileMatch{
		ProfileID:   "", // caller records with empty ProfileID, UI surfaces it
		Rule:        RuleNeedsReview,
		Candidates:  candidateIDs,
		Confidence:  0.0,
		NeedsReview: true,
		Reason:      "could not disambiguate profile; manual review required",
	}
}

// uniqueProfileIDs returns distinct ProfileIDs preserving first-seen order.
func uniqueProfileIDs(recs []history.Record) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		id := r.ProfileID
		if id == "" {
			id = history.DefaultProfileID
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// findOldest returns the ProfileID of the earliest-sent record in the list.
// GetInFlightBrokerRequests is expected to return records ordered by
// sent_at ASC; we defensively re-scan rather than trust ordering.
func findOldest(recs []history.Record) string {
	if len(recs) == 0 {
		return ""
	}
	oldest := recs[0]
	for _, r := range recs[1:] {
		if r.SentAt.Before(oldest.SentAt) {
			oldest = r
		}
	}
	id := oldest.ProfileID
	if id == "" {
		id = history.DefaultProfileID
	}
	return id
}

// containsAsToken returns true if `needle` appears in `haystack` bounded by
// non-word characters (or string start/end). Keeps "jane@x.com" from
// matching "janex@x.com" while still matching "To: jane@x.com," or
// "<jane@x.com>".
func containsAsToken(haystack, needle string) bool {
	idx := 0
	for {
		hit := strings.Index(haystack[idx:], needle)
		if hit < 0 {
			return false
		}
		hit += idx
		// Check left boundary.
		if hit > 0 {
			prev := haystack[hit-1]
			if isEmailWordChar(prev) {
				idx = hit + 1
				continue
			}
		}
		// Check right boundary.
		end := hit + len(needle)
		if end < len(haystack) {
			next := haystack[end]
			if isEmailWordChar(next) {
				idx = hit + 1
				continue
			}
		}
		return true
	}
}

// isEmailWordChar reports whether a byte is one that could extend an email
// local-part or domain (so finding it adjacent means we didn't actually hit
// the full address). Covers a-z, 0-9, '.', '_', '-', '+' — the classic set.
func isEmailWordChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '.' || b == '_' || b == '-' || b == '+':
		return true
	}
	return false
}

func dedupe(xs []string) []string {
	seen := make(map[string]struct{})
	out := xs[:0]
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}

package history

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultProfileID mirrors config.DefaultProfileID. Duplicated here to avoid
// a cross-package import cycle; they must stay in sync. See migrate().
const DefaultProfileID = "default"

type Status string

const (
	StatusSent    Status = "sent"
	StatusFailed  Status = "failed"
	StatusPending Status = "pending"
)

// PipelineStatus represents the current stage in the removal pipeline
type PipelineStatus string

const (
	PipelineEmailSent            PipelineStatus = "email_sent"
	PipelineAwaitingResponse     PipelineStatus = "awaiting_response"
	PipelineFormRequired         PipelineStatus = "form_required"
	PipelineFormFilled           PipelineStatus = "form_filled"
	PipelineAwaitingCaptcha      PipelineStatus = "awaiting_captcha"
	PipelineCaptchaSolved        PipelineStatus = "captcha_solved"
	PipelineAwaitingConfirmation PipelineStatus = "awaiting_confirmation"
	PipelineConfirmed            PipelineStatus = "confirmed"
	PipelineFailed               PipelineStatus = "failed"
	PipelineRejected             PipelineStatus = "rejected"
)

// TaskType represents the type of pending task
type TaskType string

const (
	TaskCaptcha    TaskType = "captcha"
	TaskManualForm TaskType = "manual_form"
	TaskReview     TaskType = "review"
	TaskConfirm    TaskType = "confirm"
)

type Record struct {
	ID             int64
	ProfileID      string // stable ID of the profile this request was sent on behalf of
	BrokerID       string
	BrokerName     string
	Email          string
	Template       string
	Status         Status
	MessageID      string
	Error          string
	SentAt         time.Time
	CreatedAt      time.Time
	PipelineStatus PipelineStatus // Current stage in pipeline
}

// BrokerResponse stores a classified response from a broker
type BrokerResponse struct {
	ID           int64
	ProfileID    string // profile the response was attributed to (may be "" if ambiguous)
	BrokerID     string
	BrokerName   string
	ResponseType string // form_required, confirmation_required, success, rejected, pending, unknown
	EmailFrom    string
	EmailSubject string
	EmailBody    string // Stored for reclassification
	FormURL      string // Extracted form URL (if any)
	ConfirmURL   string // Extracted confirmation URL (if any)
	Confidence   float64
	NeedsReview  bool
	ReceivedAt   time.Time
	ProcessedAt  time.Time
	CreatedAt    time.Time
}

// PendingTask represents a task that needs human intervention
type PendingTask struct {
	ID             int64
	ProfileID      string
	BrokerID       string
	BrokerName     string
	TaskType       TaskType
	FormURL        string
	ScreenshotPath string
	BrowserState   string // JSON serialized browser state (profile data for helper page)
	Notes          string
	Status         string // pending, completed, skipped
	CreatedAt      time.Time
	OpenedAt       sql.NullTime // When user first opened the helper page
	CompletedAt    sql.NullTime
}

type Store struct {
	db *sql.DB
}

// resolveProfileID returns the given id or DefaultProfileID if empty. Used on
// insert paths so callers can omit the profile ID for legacy code paths and
// still land at a well-defined row value.
func resolveProfileID(id string) string {
	if id == "" {
		return DefaultProfileID
	}
	return id
}

// scanRecord handles nullable columns when scanning a row. The SELECT column
// order is: id, profile_id, broker_id, broker_name, email, template, status,
// message_id, error, sent_at, created_at.
func scanRecord(scanner interface{ Scan(...any) error }) (*Record, error) {
	var r Record
	var sentAt, createdAt sql.NullTime
	var messageID, errStr sql.NullString
	var profileID sql.NullString

	err := scanner.Scan(&r.ID, &profileID, &r.BrokerID, &r.BrokerName, &r.Email, &r.Template,
		&r.Status, &messageID, &errStr, &sentAt, &createdAt)
	if err != nil {
		return nil, err
	}

	r.ProfileID = profileID.String
	if r.ProfileID == "" {
		r.ProfileID = DefaultProfileID
	}
	r.MessageID = messageID.String
	r.Error = errStr.String
	r.SentAt = sentAt.Time
	r.CreatedAt = createdAt.Time
	return &r, nil
}

func NewStore(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, fmt.Errorf("failed to create history directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) migrate() error {
	// Additive migrations for pre-existing databases. SQLite errors on
	// duplicate ADD COLUMN are ignored — this is the idempotent pattern
	// already used in this file.
	s.db.Exec(`ALTER TABLE removal_requests ADD COLUMN pipeline_status TEXT DEFAULT 'email_sent'`)
	s.db.Exec(`ALTER TABLE pending_tasks ADD COLUMN opened_at DATETIME`)

	// Multi-profile migration (additive, safe): every existing row gets
	// profile_id = 'default' so legacy single-profile history stays attributed
	// to the primary profile. New inserts supply an explicit ID.
	s.db.Exec(`ALTER TABLE removal_requests ADD COLUMN profile_id TEXT NOT NULL DEFAULT 'default'`)
	s.db.Exec(`ALTER TABLE broker_responses ADD COLUMN profile_id TEXT NOT NULL DEFAULT 'default'`)
	s.db.Exec(`ALTER TABLE pending_tasks   ADD COLUMN profile_id TEXT NOT NULL DEFAULT 'default'`)

	query := `
	CREATE TABLE IF NOT EXISTS removal_requests (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		profile_id TEXT NOT NULL DEFAULT 'default',
		broker_id TEXT NOT NULL,
		broker_name TEXT NOT NULL,
		email TEXT NOT NULL,
		template TEXT NOT NULL,
		status TEXT NOT NULL,
		message_id TEXT,
		error TEXT,
		sent_at DATETIME,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		pipeline_status TEXT DEFAULT 'email_sent'
	);

	CREATE INDEX IF NOT EXISTS idx_broker_id ON removal_requests(broker_id);
	CREATE INDEX IF NOT EXISTS idx_sent_at ON removal_requests(sent_at);
	CREATE INDEX IF NOT EXISTS idx_status ON removal_requests(status);
	CREATE INDEX IF NOT EXISTS idx_pipeline_status ON removal_requests(pipeline_status);
	CREATE INDEX IF NOT EXISTS idx_profile_broker ON removal_requests(profile_id, broker_id);

	-- Broker responses table (stores classified email responses)
	CREATE TABLE IF NOT EXISTS broker_responses (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		profile_id TEXT NOT NULL DEFAULT 'default',
		broker_id TEXT NOT NULL,
		broker_name TEXT NOT NULL,
		response_type TEXT NOT NULL,
		email_from TEXT,
		email_subject TEXT,
		form_url TEXT,
		confirm_url TEXT,
		confidence REAL,
		needs_review INTEGER DEFAULT 0,
		received_at DATETIME,
		processed_at DATETIME,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_br_broker_id ON broker_responses(broker_id);
	CREATE INDEX IF NOT EXISTS idx_br_response_type ON broker_responses(response_type);
	CREATE INDEX IF NOT EXISTS idx_br_needs_review ON broker_responses(needs_review);
	CREATE INDEX IF NOT EXISTS idx_br_profile_broker ON broker_responses(profile_id, broker_id);

	-- Pending tasks table (for CAPTCHAs, manual forms, etc.)
	CREATE TABLE IF NOT EXISTS pending_tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		profile_id TEXT NOT NULL DEFAULT 'default',
		broker_id TEXT NOT NULL,
		broker_name TEXT NOT NULL,
		task_type TEXT NOT NULL,
		form_url TEXT,
		screenshot_path TEXT,
		browser_state TEXT,
		notes TEXT,
		status TEXT DEFAULT 'pending',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		opened_at DATETIME,
		completed_at DATETIME
	);

	CREATE INDEX IF NOT EXISTS idx_pt_broker_id ON pending_tasks(broker_id);
	CREATE INDEX IF NOT EXISTS idx_pt_task_type ON pending_tasks(task_type);
	CREATE INDEX IF NOT EXISTS idx_pt_status ON pending_tasks(status);
	CREATE INDEX IF NOT EXISTS idx_pt_profile_broker ON pending_tasks(profile_id, broker_id);
	`

	_, err := s.db.Exec(query)
	if err != nil {
		return fmt.Errorf("failed to migrate database: %w", err)
	}

	return nil
}

func (s *Store) Add(record *Record) error {
	query := `
	INSERT INTO removal_requests (profile_id, broker_id, broker_name, email, template, status, message_id, error, sent_at, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	result, err := s.db.Exec(query,
		resolveProfileID(record.ProfileID),
		record.BrokerID,
		record.BrokerName,
		record.Email,
		record.Template,
		record.Status,
		record.MessageID,
		record.Error,
		record.SentAt,
		time.Now(),
	)
	if err != nil {
		return fmt.Errorf("failed to insert record: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("failed to get last insert id: %w", err)
	}

	record.ID = id
	if record.ProfileID == "" {
		record.ProfileID = DefaultProfileID
	}
	return nil
}

// GetLastRequestForBroker returns the most recent request for a broker across
// all profiles. Prefer GetLastRequestForProfileAndBroker when scoped dedup is
// required.
func (s *Store) GetLastRequestForBroker(brokerID string) (*Record, error) {
	query := `
	SELECT id, profile_id, broker_id, broker_name, email, template, status, message_id, error, sent_at, created_at
	FROM removal_requests WHERE broker_id = ? ORDER BY sent_at DESC LIMIT 1`

	record, err := scanRecord(s.db.QueryRow(query, brokerID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query record: %w", err)
	}
	return record, nil
}

// GetLastRequestForProfileAndBroker returns the most recent request scoped to
// a specific (profile, broker) pair. This is the correct dedup key for the
// send loop: sending profile A to broker X must NOT skip profile B to the
// same broker.
func (s *Store) GetLastRequestForProfileAndBroker(profileID, brokerID string) (*Record, error) {
	query := `
	SELECT id, profile_id, broker_id, broker_name, email, template, status, message_id, error, sent_at, created_at
	FROM removal_requests WHERE profile_id = ? AND broker_id = ? ORDER BY sent_at DESC LIMIT 1`

	record, err := scanRecord(s.db.QueryRow(query, resolveProfileID(profileID), brokerID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query record: %w", err)
	}
	return record, nil
}

func (s *Store) GetRecentRequests(limit int) ([]Record, error) {
	query := `
	SELECT id, profile_id, broker_id, broker_name, email, template, status, message_id, error, sent_at, created_at
	FROM removal_requests ORDER BY sent_at DESC LIMIT ?`

	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query records: %w", err)
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan record: %w", err)
		}
		records = append(records, *record)
	}
	return records, rows.Err()
}

// GetRecentRequestsForProfile returns recent requests for one profile.
func (s *Store) GetRecentRequestsForProfile(profileID string, limit int) ([]Record, error) {
	query := `
	SELECT id, profile_id, broker_id, broker_name, email, template, status, message_id, error, sent_at, created_at
	FROM removal_requests WHERE profile_id = ? ORDER BY sent_at DESC LIMIT ?`

	rows, err := s.db.Query(query, resolveProfileID(profileID), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query records: %w", err)
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan record: %w", err)
		}
		records = append(records, *record)
	}
	return records, rows.Err()
}

func (s *Store) GetStats() (total, sent, failed int, err error) {
	query := `SELECT COUNT(*), SUM(CASE WHEN status='sent' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END) FROM removal_requests`

	err = s.db.QueryRow(query).Scan(&total, &sent, &failed)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to get stats: %w", err)
	}
	return
}

// GetStatsForProfile returns the same totals scoped to one profile.
func (s *Store) GetStatsForProfile(profileID string) (total, sent, failed int, err error) {
	query := `SELECT COUNT(*), SUM(CASE WHEN status='sent' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END) FROM removal_requests WHERE profile_id = ?`

	err = s.db.QueryRow(query, resolveProfileID(profileID)).Scan(&total, &sent, &failed)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to get stats: %w", err)
	}
	return
}

func (s *Store) GetMonthlyStats() (sent, failed int, err error) {
	now := time.Now()
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	query := `SELECT SUM(CASE WHEN status='sent' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END) FROM removal_requests WHERE sent_at >= ?`

	var sentNull, failedNull sql.NullInt64
	err = s.db.QueryRow(query, startOfMonth).Scan(&sentNull, &failedNull)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get monthly stats: %w", err)
	}
	return int(sentNull.Int64), int(failedNull.Int64), nil
}

func (s *Store) Close() error { return s.db.Close() }

type BrokerStatus struct {
	BrokerID  string
	LastSent  time.Time
	Status    Status
	TotalSent int
}

// GetAllBrokerStatuses returns per-broker status aggregated across all
// profiles. Use when the caller wants "has anyone sent to this broker?"
// semantics (e.g., global dashboard stats). For profile-scoped dedup in the
// send loop, use GetAllBrokerStatusesForProfile instead.
func (s *Store) GetAllBrokerStatuses() (map[string]BrokerStatus, error) {
	query := `SELECT broker_id, MAX(sent_at) as last_sent,
		(SELECT status FROM removal_requests r2 WHERE r2.broker_id = r.broker_id ORDER BY sent_at DESC LIMIT 1),
		COUNT(*) FROM removal_requests r GROUP BY broker_id`

	return s.queryBrokerStatuses(query)
}

// GetAllBrokerStatusesForProfile returns per-broker status scoped to one
// profile. This is what the web UI and CLI send loop should use when
// showing "what's left to send for this person".
func (s *Store) GetAllBrokerStatusesForProfile(profileID string) (map[string]BrokerStatus, error) {
	query := `SELECT broker_id, MAX(sent_at) as last_sent,
		(SELECT status FROM removal_requests r2 WHERE r2.broker_id = r.broker_id AND r2.profile_id = r.profile_id ORDER BY sent_at DESC LIMIT 1),
		COUNT(*) FROM removal_requests r WHERE profile_id = ? GROUP BY broker_id`

	return s.queryBrokerStatuses(query, resolveProfileID(profileID))
}

func (s *Store) queryBrokerStatuses(query string, args ...any) (map[string]BrokerStatus, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query broker statuses: %w", err)
	}
	defer rows.Close()

	statuses := make(map[string]BrokerStatus)
	for rows.Next() {
		var bs BrokerStatus
		var lastSent sql.NullTime
		var status string

		if err := rows.Scan(&bs.BrokerID, &lastSent, &status, &bs.TotalSent); err != nil {
			return nil, fmt.Errorf("failed to scan broker status: %w", err)
		}
		bs.LastSent = lastSent.Time
		bs.Status = Status(status)
		statuses[bs.BrokerID] = bs
	}
	return statuses, rows.Err()
}

// DeleteByStatus deletes all records with the given status
func (s *Store) DeleteByStatus(status Status) (int64, error) {
	result, err := s.db.Exec(`DELETE FROM removal_requests WHERE status = ?`, string(status))
	if err != nil {
		return 0, fmt.Errorf("failed to delete records: %w", err)
	}
	return result.RowsAffected()
}

func DefaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "eraser_history.db"
	}
	return filepath.Join(home, ".eraser", "history.db")
}

// ==================== Broker Response Methods ====================

// AddBrokerResponse stores a classified response from a broker
func (s *Store) AddBrokerResponse(resp *BrokerResponse) error {
	query := `
	INSERT INTO broker_responses (profile_id, broker_id, broker_name, response_type, email_from, email_subject, email_body,
		form_url, confirm_url, confidence, needs_review, received_at, processed_at, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	needsReview := 0
	if resp.NeedsReview {
		needsReview = 1
	}

	result, err := s.db.Exec(query,
		resolveProfileID(resp.ProfileID),
		resp.BrokerID, resp.BrokerName, resp.ResponseType, resp.EmailFrom, resp.EmailSubject, resp.EmailBody,
		resp.FormURL, resp.ConfirmURL, resp.Confidence, needsReview,
		resp.ReceivedAt, time.Now(), time.Now(),
	)
	if err != nil {
		return fmt.Errorf("failed to insert broker response: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("failed to get last insert id: %w", err)
	}
	resp.ID = id
	if resp.ProfileID == "" {
		resp.ProfileID = DefaultProfileID
	}
	return nil
}

// FindBrokerResponseBySubject finds an existing response by broker_id and email_subject
func (s *Store) FindBrokerResponseBySubject(brokerID, subject string) (*BrokerResponse, error) {
	query := `SELECT id, profile_id, broker_id, broker_name, response_type, email_from, email_subject,
		form_url, confirm_url, confidence, needs_review, received_at, processed_at, created_at
		FROM broker_responses WHERE broker_id = ? AND email_subject = ? LIMIT 1`

	var r BrokerResponse
	var profileID sql.NullString
	var needsReviewInt int
	var receivedAtStr, processedAtStr, createdAtStr sql.NullString
	var formURL, confirmURL sql.NullString

	err := s.db.QueryRow(query, brokerID, subject).Scan(
		&r.ID, &profileID, &r.BrokerID, &r.BrokerName, &r.ResponseType, &r.EmailFrom, &r.EmailSubject,
		&formURL, &confirmURL, &r.Confidence, &needsReviewInt, &receivedAtStr, &processedAtStr, &createdAtStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to find broker response: %w", err)
	}

	r.ProfileID = profileID.String
	if r.ProfileID == "" {
		r.ProfileID = DefaultProfileID
	}
	r.FormURL = formURL.String
	r.ConfirmURL = confirmURL.String
	r.NeedsReview = needsReviewInt == 1

	return &r, nil
}

// UpdateBrokerResponseClassification updates the classification fields of a response
func (s *Store) UpdateBrokerResponseClassification(id int64, responseType string, formURL, confirmURL string, confidence float64, needsReview bool) error {
	query := `UPDATE broker_responses SET response_type = ?, form_url = ?, confirm_url = ?,
		confidence = ?, needs_review = ?, processed_at = ? WHERE id = ?`

	needsReviewInt := 0
	if needsReview {
		needsReviewInt = 1
	}

	_, err := s.db.Exec(query, responseType, formURL, confirmURL, confidence, needsReviewInt, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to update broker response: %w", err)
	}
	return nil
}

// UpdateBrokerResponseBody updates the email body for an existing response
func (s *Store) UpdateBrokerResponseBody(id int64, body string) error {
	query := `UPDATE broker_responses SET email_body = ? WHERE id = ?`
	_, err := s.db.Exec(query, body, id)
	if err != nil {
		return fmt.Errorf("failed to update broker response body: %w", err)
	}
	return nil
}

// UpdateBrokerResponseProfileID sets the profile attribution for a response
// after the fact (used when the inbox disambiguator revises its guess, e.g.,
// a later reply clarifies ownership).
func (s *Store) UpdateBrokerResponseProfileID(id int64, profileID string) error {
	_, err := s.db.Exec(`UPDATE broker_responses SET profile_id = ? WHERE id = ?`, resolveProfileID(profileID), id)
	if err != nil {
		return fmt.Errorf("failed to update broker response profile_id: %w", err)
	}
	return nil
}

// ClearBrokerResponses removes all broker responses (for full re-scan)
func (s *Store) ClearBrokerResponses() error {
	_, err := s.db.Exec("DELETE FROM broker_responses")
	if err != nil {
		return fmt.Errorf("failed to clear broker responses: %w", err)
	}
	return nil
}

// scanBrokerResponse is shared between the all/filtered/range query paths.
// Column order: id, profile_id, broker_id, broker_name, response_type,
// email_from, email_subject, [email_body,] form_url, confirm_url, confidence,
// needs_review, received_at, processed_at, created_at. The email_body column
// is opt-in — callers pass includeBody=true to fetch it.
func scanBrokerResponse(scanner interface{ Scan(...any) error }, includeBody bool) (*BrokerResponse, error) {
	var r BrokerResponse
	var profileID sql.NullString
	var needsReviewInt int
	var receivedAtStr, processedAtStr, createdAtStr sql.NullString
	var formURL, confirmURL sql.NullString
	var emailBody sql.NullString

	var err error
	if includeBody {
		err = scanner.Scan(&r.ID, &profileID, &r.BrokerID, &r.BrokerName, &r.ResponseType, &r.EmailFrom, &r.EmailSubject, &emailBody,
			&formURL, &confirmURL, &r.Confidence, &needsReviewInt, &receivedAtStr, &processedAtStr, &createdAtStr)
	} else {
		err = scanner.Scan(&r.ID, &profileID, &r.BrokerID, &r.BrokerName, &r.ResponseType, &r.EmailFrom, &r.EmailSubject,
			&formURL, &confirmURL, &r.Confidence, &needsReviewInt, &receivedAtStr, &processedAtStr, &createdAtStr)
	}
	if err != nil {
		return nil, err
	}

	r.ProfileID = profileID.String
	if r.ProfileID == "" {
		r.ProfileID = DefaultProfileID
	}
	if includeBody {
		r.EmailBody = emailBody.String
	}
	r.FormURL = formURL.String
	r.ConfirmURL = confirmURL.String
	r.NeedsReview = needsReviewInt == 1

	if receivedAtStr.Valid {
		r.ReceivedAt, _ = time.Parse(time.RFC3339, receivedAtStr.String)
		if r.ReceivedAt.IsZero() {
			r.ReceivedAt, _ = time.Parse("2006-01-02 15:04:05", receivedAtStr.String)
		}
	}
	if processedAtStr.Valid {
		r.ProcessedAt, _ = time.Parse(time.RFC3339, processedAtStr.String)
		if r.ProcessedAt.IsZero() {
			r.ProcessedAt, _ = time.Parse("2006-01-02 15:04:05", processedAtStr.String)
		}
	}
	if createdAtStr.Valid {
		r.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr.String)
		if r.CreatedAt.IsZero() {
			r.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAtStr.String)
		}
	}
	return &r, nil
}

// GetAllBrokerResponses retrieves all broker responses (for reclassification)
func (s *Store) GetAllBrokerResponses() ([]BrokerResponse, error) {
	query := `SELECT id, profile_id, broker_id, broker_name, response_type, email_from, email_subject, email_body,
		form_url, confirm_url, confidence, needs_review, received_at, processed_at, created_at
		FROM broker_responses ORDER BY created_at DESC`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to query all broker responses: %w", err)
	}
	defer rows.Close()

	var responses []BrokerResponse
	for rows.Next() {
		r, err := scanBrokerResponse(rows, true)
		if err != nil {
			return nil, fmt.Errorf("failed to scan broker response: %w", err)
		}
		responses = append(responses, *r)
	}

	return responses, rows.Err()
}

// GetBrokerResponses retrieves broker responses with optional filtering
func (s *Store) GetBrokerResponses(responseType string, needsReview bool, limit int) ([]BrokerResponse, error) {
	return s.getBrokerResponsesScoped("", responseType, needsReview, limit)
}

// GetBrokerResponsesForProfile is the profile-scoped variant of GetBrokerResponses.
func (s *Store) GetBrokerResponsesForProfile(profileID, responseType string, needsReview bool, limit int) ([]BrokerResponse, error) {
	return s.getBrokerResponsesScoped(resolveProfileID(profileID), responseType, needsReview, limit)
}

func (s *Store) getBrokerResponsesScoped(profileID, responseType string, needsReview bool, limit int) ([]BrokerResponse, error) {
	baseCols := `id, profile_id, broker_id, broker_name, response_type, email_from, email_subject,
		form_url, confirm_url, confidence, needs_review, received_at, processed_at, created_at`

	var whereParts []string
	var args []interface{}
	if profileID != "" {
		whereParts = append(whereParts, "profile_id = ?")
		args = append(args, profileID)
	}
	if responseType != "" {
		whereParts = append(whereParts, "response_type = ?")
		args = append(args, responseType)
	}
	if needsReview {
		whereParts = append(whereParts, "needs_review = 1")
	}

	query := "SELECT " + baseCols + " FROM broker_responses"
	if len(whereParts) > 0 {
		query += " WHERE "
		for i, part := range whereParts {
			if i > 0 {
				query += " AND "
			}
			query += part
		}
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query broker responses: %w", err)
	}
	defer rows.Close()

	var responses []BrokerResponse
	for rows.Next() {
		r, err := scanBrokerResponse(rows, false)
		if err != nil {
			return nil, fmt.Errorf("failed to scan broker response: %w", err)
		}
		responses = append(responses, *r)
	}
	return responses, rows.Err()
}

// GetInFlightBrokerRequests returns the profiles that currently have an
// unresolved (pipeline_status != 'confirmed' / 'rejected' / 'failed') request
// for the given broker, ordered by sent_at ASC (oldest first). Used by the
// inbox disambiguator's rule 3 (FIFO fallback).
func (s *Store) GetInFlightBrokerRequests(brokerID string) ([]Record, error) {
	query := `
	SELECT id, profile_id, broker_id, broker_name, email, template, status, message_id, error, sent_at, created_at
	FROM removal_requests
	WHERE broker_id = ?
	  AND status = 'sent'
	  AND (pipeline_status IS NULL OR pipeline_status NOT IN ('confirmed','rejected','failed'))
	ORDER BY sent_at ASC`

	rows, err := s.db.Query(query, brokerID)
	if err != nil {
		return nil, fmt.Errorf("failed to query in-flight requests: %w", err)
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan record: %w", err)
		}
		records = append(records, *record)
	}
	return records, rows.Err()
}

// GetResponseStats returns counts of response types
func (s *Store) GetResponseStats() (map[string]int, error) {
	query := `SELECT response_type, COUNT(*) FROM broker_responses GROUP BY response_type`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to query response stats: %w", err)
	}
	defer rows.Close()

	stats := make(map[string]int)
	for rows.Next() {
		var responseType string
		var count int
		if err := rows.Scan(&responseType, &count); err != nil {
			return nil, fmt.Errorf("failed to scan response stat: %w", err)
		}
		stats[responseType] = count
	}
	return stats, rows.Err()
}

// FormWithStatus represents a form detected from email with its current fill status
type FormWithStatus struct {
	BrokerID       string
	ProfileID      string
	BrokerName     string
	FormURL        string
	EmailSubject   string
	DetectedAt     time.Time
	Status         string // pending, filled, captcha, failed, skipped
	TaskID         int64  // If there's a pending task
	PipelineStatus PipelineStatus
}

// GetFormsWithStatus returns all detected forms with their current status
func (s *Store) GetFormsWithStatus() ([]FormWithStatus, error) {
	// Get all broker_responses with form_url, joined with pending_tasks and removal_requests.
	// Joins are scoped by profile_id so a form for profile A does not accidentally
	// pick up a pending_task for profile B on the same broker.
	query := `
	SELECT
		br.broker_id,
		br.profile_id,
		br.broker_name,
		br.form_url,
		br.email_subject,
		br.created_at as detected_at,
		COALESCE(pt.id, 0) as task_id,
		COALESCE(pt.status, '') as task_status,
		COALESCE(rr.pipeline_status, '') as pipeline_status
	FROM broker_responses br
	LEFT JOIN pending_tasks pt
		ON br.broker_id = pt.broker_id
		AND br.profile_id = pt.profile_id
		AND pt.task_type IN ('captcha', 'manual_form')
	LEFT JOIN (
		SELECT broker_id, profile_id, pipeline_status
		FROM removal_requests
		WHERE id IN (SELECT MAX(id) FROM removal_requests GROUP BY profile_id, broker_id)
	) rr ON br.broker_id = rr.broker_id AND br.profile_id = rr.profile_id
	WHERE br.form_url IS NOT NULL AND br.form_url != ''
	ORDER BY br.created_at DESC
	`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to query forms: %w", err)
	}
	defer rows.Close()

	var forms []FormWithStatus
	seen := make(map[string]bool) // Dedupe by (profile_id, broker_id)

	for rows.Next() {
		var f FormWithStatus
		var taskStatus, pipelineStatus string

		if err := rows.Scan(&f.BrokerID, &f.ProfileID, &f.BrokerName, &f.FormURL, &f.EmailSubject,
			&f.DetectedAt, &f.TaskID, &taskStatus, &pipelineStatus); err != nil {
			return nil, fmt.Errorf("failed to scan form: %w", err)
		}

		key := f.ProfileID + "|" + f.BrokerID
		if seen[key] {
			continue
		}
		seen[key] = true

		f.PipelineStatus = PipelineStatus(pipelineStatus)

		// Determine status based on task and pipeline status
		if taskStatus == "completed" {
			f.Status = "filled"
		} else if taskStatus == "skipped" {
			f.Status = "skipped"
		} else if taskStatus == "pending" && f.TaskID > 0 {
			f.Status = "captcha"
		} else if pipelineStatus == string(PipelineFormFilled) || pipelineStatus == string(PipelineConfirmed) {
			f.Status = "filled"
		} else if pipelineStatus == string(PipelineFailed) {
			f.Status = "failed"
		} else if pipelineStatus == string(PipelineRejected) {
			f.Status = "skipped"
		} else {
			f.Status = "pending"
		}

		forms = append(forms, f)
	}

	return forms, rows.Err()
}

// GetFormStats returns counts of forms by status
func (s *Store) GetFormStats() (pending, filled, captcha, failed, skipped int, err error) {
	forms, err := s.GetFormsWithStatus()
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}

	for _, f := range forms {
		switch f.Status {
		case "pending":
			pending++
		case "filled":
			filled++
		case "captcha":
			captcha++
		case "failed":
			failed++
		case "skipped":
			skipped++
		}
	}
	return
}

// ==================== Pending Task Methods ====================

// AddPendingTask creates a new pending task for human intervention
func (s *Store) AddPendingTask(task *PendingTask) error {
	query := `
	INSERT INTO pending_tasks (profile_id, broker_id, broker_name, task_type, form_url, screenshot_path,
		browser_state, notes, status, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	result, err := s.db.Exec(query,
		resolveProfileID(task.ProfileID),
		task.BrokerID, task.BrokerName, task.TaskType, task.FormURL, task.ScreenshotPath,
		task.BrowserState, task.Notes, "pending", time.Now(),
	)
	if err != nil {
		return fmt.Errorf("failed to insert pending task: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("failed to get last insert id: %w", err)
	}
	task.ID = id
	if task.ProfileID == "" {
		task.ProfileID = DefaultProfileID
	}
	return nil
}

// scanPendingTask scans a single pending_tasks row. Column order:
// id, profile_id, broker_id, broker_name, task_type, form_url, screenshot_path,
// browser_state, notes, status, created_at, opened_at, completed_at.
func scanPendingTask(scanner interface{ Scan(...any) error }) (*PendingTask, error) {
	var t PendingTask
	var profileID sql.NullString
	var createdAt sql.NullTime
	var formURL, screenshotPath, browserState, notes sql.NullString

	err := scanner.Scan(&t.ID, &profileID, &t.BrokerID, &t.BrokerName, &t.TaskType,
		&formURL, &screenshotPath, &browserState, &notes, &t.Status,
		&createdAt, &t.OpenedAt, &t.CompletedAt)
	if err != nil {
		return nil, err
	}

	t.ProfileID = profileID.String
	if t.ProfileID == "" {
		t.ProfileID = DefaultProfileID
	}
	t.FormURL = formURL.String
	t.ScreenshotPath = screenshotPath.String
	t.BrowserState = browserState.String
	t.Notes = notes.String
	t.CreatedAt = createdAt.Time
	return &t, nil
}

// GetPendingTasks retrieves pending tasks with optional filtering
func (s *Store) GetPendingTasks(taskType TaskType, status string) ([]PendingTask, error) {
	return s.getPendingTasksScoped("", taskType, status)
}

// GetPendingTasksForProfile is the profile-scoped variant of GetPendingTasks.
func (s *Store) GetPendingTasksForProfile(profileID string, taskType TaskType, status string) ([]PendingTask, error) {
	return s.getPendingTasksScoped(resolveProfileID(profileID), taskType, status)
}

func (s *Store) getPendingTasksScoped(profileID string, taskType TaskType, status string) ([]PendingTask, error) {
	baseCols := `id, profile_id, broker_id, broker_name, task_type, form_url, screenshot_path,
		browser_state, notes, status, created_at, opened_at, completed_at`

	var whereParts []string
	var args []interface{}
	if profileID != "" {
		whereParts = append(whereParts, "profile_id = ?")
		args = append(args, profileID)
	}
	if taskType != "" {
		whereParts = append(whereParts, "task_type = ?")
		args = append(args, taskType)
	}
	if status != "" {
		whereParts = append(whereParts, "status = ?")
		args = append(args, status)
	}

	query := "SELECT " + baseCols + " FROM pending_tasks"
	if len(whereParts) > 0 {
		query += " WHERE "
		for i, part := range whereParts {
			if i > 0 {
				query += " AND "
			}
			query += part
		}
	}
	query += " ORDER BY created_at DESC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query pending tasks: %w", err)
	}
	defer rows.Close()

	var tasks []PendingTask
	for rows.Next() {
		t, err := scanPendingTask(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan pending task: %w", err)
		}
		tasks = append(tasks, *t)
	}

	return tasks, rows.Err()
}

// GetPendingTaskByID retrieves a specific pending task
func (s *Store) GetPendingTaskByID(id int64) (*PendingTask, error) {
	query := `SELECT id, profile_id, broker_id, broker_name, task_type, form_url, screenshot_path,
		browser_state, notes, status, created_at, opened_at, completed_at
		FROM pending_tasks WHERE id = ?`

	t, err := scanPendingTask(s.db.QueryRow(query, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query pending task: %w", err)
	}
	return t, nil
}

// GetPendingTaskForProfileAndBroker retrieves the most recent pending task
// scoped to (profile, broker). Callers that act on a task (fill-form, confirm)
// should use this to avoid acting on another profile's task by accident.
func (s *Store) GetPendingTaskForProfileAndBroker(profileID, brokerID string) (*PendingTask, error) {
	query := `SELECT id, profile_id, broker_id, broker_name, task_type, form_url, screenshot_path,
		browser_state, notes, status, created_at, opened_at, completed_at
		FROM pending_tasks WHERE profile_id = ? AND broker_id = ? ORDER BY created_at DESC LIMIT 1`

	t, err := scanPendingTask(s.db.QueryRow(query, resolveProfileID(profileID), brokerID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query pending task: %w", err)
	}
	return t, nil
}

// CompletePendingTask marks a task as completed
func (s *Store) CompletePendingTask(id int64, status string) error {
	query := `UPDATE pending_tasks SET status = ?, completed_at = ? WHERE id = ?`
	_, err := s.db.Exec(query, status, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to complete pending task: %w", err)
	}
	return nil
}

// MarkTaskOpened sets the opened_at timestamp (only if not already set)
func (s *Store) MarkTaskOpened(id int64) error {
	query := `UPDATE pending_tasks SET opened_at = ? WHERE id = ? AND opened_at IS NULL`
	_, err := s.db.Exec(query, time.Now(), id)
	if err != nil {
		return fmt.Errorf("failed to mark task opened: %w", err)
	}
	return nil
}

// GetPendingTaskStats returns counts of pending tasks by type and status
func (s *Store) GetPendingTaskStats() (pending, completed, skipped int, err error) {
	query := `SELECT
		SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status='completed' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status='skipped' THEN 1 ELSE 0 END)
		FROM pending_tasks`

	var pendingNull, completedNull, skippedNull sql.NullInt64
	err = s.db.QueryRow(query).Scan(&pendingNull, &completedNull, &skippedNull)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to get task stats: %w", err)
	}
	return int(pendingNull.Int64), int(completedNull.Int64), int(skippedNull.Int64), nil
}

// ==================== Pipeline Status Methods ====================

// UpdatePipelineStatus updates the pipeline status for a broker. Without a
// profile scope, this updates the latest request across any profile for that
// broker. Prefer UpdatePipelineStatusForProfile when a profile is known.
func (s *Store) UpdatePipelineStatus(brokerID string, status PipelineStatus) error {
	query := `UPDATE removal_requests SET pipeline_status = ? WHERE broker_id = ? AND id = (
		SELECT id FROM removal_requests WHERE broker_id = ? ORDER BY sent_at DESC LIMIT 1
	)`
	_, err := s.db.Exec(query, status, brokerID, brokerID)
	if err != nil {
		return fmt.Errorf("failed to update pipeline status: %w", err)
	}
	return nil
}

// UpdatePipelineStatusForProfile is the profile-scoped variant.
func (s *Store) UpdatePipelineStatusForProfile(profileID, brokerID string, status PipelineStatus) error {
	pid := resolveProfileID(profileID)
	query := `UPDATE removal_requests SET pipeline_status = ? WHERE profile_id = ? AND broker_id = ? AND id = (
		SELECT id FROM removal_requests WHERE profile_id = ? AND broker_id = ? ORDER BY sent_at DESC LIMIT 1
	)`
	_, err := s.db.Exec(query, status, pid, brokerID, pid, brokerID)
	if err != nil {
		return fmt.Errorf("failed to update pipeline status: %w", err)
	}
	return nil
}

// GetPipelineStats returns counts by pipeline status
func (s *Store) GetPipelineStats() (map[PipelineStatus]int, error) {
	query := `SELECT pipeline_status, COUNT(*) FROM removal_requests
		WHERE id IN (SELECT MAX(id) FROM removal_requests GROUP BY profile_id, broker_id)
		GROUP BY pipeline_status`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to query pipeline stats: %w", err)
	}
	defer rows.Close()

	stats := make(map[PipelineStatus]int)
	for rows.Next() {
		var status sql.NullString
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("failed to scan pipeline stat: %w", err)
		}
		if status.Valid {
			stats[PipelineStatus(status.String)] = count
		} else {
			stats[PipelineEmailSent] = count
		}
	}
	return stats, rows.Err()
}

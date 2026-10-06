package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB *sql.DB
	// folderRoleInsertHook is a package-private failure seam used by the
	// transaction rollback test. Production stores leave it nil.
	folderRoleInsertHook func() error
}

var (
	ErrSetupCompleted        = errors.New("initial setup has already been completed")
	ErrAccountDeleting       = errors.New("account deletion is already in progress")
	ErrAccountNotFound       = errors.New("account not found")
	ErrAccountConfigConflict = errors.New("account configuration version conflict")
	ErrInvalidAccountConfig  = errors.New("invalid account configuration")
	ErrInvalidFolderRole     = errors.New("invalid folder role configuration")
)

const sqliteBusyTimeout = 5000

// sqliteDSN keeps connection-local SQLite policy in the DSN. database/sql may
// create a fresh driver connection after the first connection is closed, so
// setting these PRAGMAs only once through Migrate would not be sufficient.
func sqliteDSN(path string, readOnly bool) string {
	params := []string{
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(1)",
	}
	if readOnly {
		params = append(params, "mode=ro", "_pragma=query_only(1)")
	} else {
		params = append(params, "_pragma=journal_mode(WAL)")
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: strings.Join(params, "&")}).String()
}

func openDB(path string, readOnly bool) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path, readOnly))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

func Open(path string) (*Store, error) {
	db, err := openDB(path, false)
	if err != nil {
		return nil, err
	}
	s := &Store{DB: db}
	if err := s.Migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an existing database without running migrations or
// enabling SQLite's write path. It is intended for short-lived helpers such
// as voxmail-secret, which must never create or mutate the application DB.
func OpenReadOnly(path string) (*Store, error) {
	db, err := openDB(path, true)
	if err != nil {
		return nil, err
	}
	s := &Store{DB: db}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open read-only database: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) Healthy(ctx context.Context) error { return s.DB.PingContext(ctx) }

type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	PINHash      string `json:"-"`
	Role         string `json:"role"`
	Enabled      bool   `json:"enabled"`
	TOTPSecret   string `json:"-"`
	BackupCodes  string `json:"-"`
}

type Account struct {
	ID                            string  `json:"id"`
	UserID                        string  `json:"user_id"`
	CanonicalName                 string  `json:"canonical_name"`
	Email                         string  `json:"email"`
	SenderName                    string  `json:"sender_name"`
	IMAPHost                      string  `json:"imap_host"`
	IMAPUser                      string  `json:"imap_user"`
	SMTPHost                      string  `json:"smtp_host"`
	SMTPUser                      string  `json:"smtp_user"`
	IMAPPort                      int     `json:"imap_port"`
	IMAPSecurity                  string  `json:"imap_security"`
	SMTPPort                      int     `json:"smtp_port"`
	SMTPSecurity                  string  `json:"smtp_security"`
	IMAPPassword                  string  `json:"-"`
	SMTPPassword                  string  `json:"-"`
	FolderMap                     string  `json:"folder_map"`
	AlertFolders                  string  `json:"alert_folders"`
	SyncIntervalMinutes           int     `json:"sync_interval_minutes"`
	ReconciliationIntervalMinutes int     `json:"reconciliation_interval_minutes"`
	DisplayOrder                  int     `json:"display_order"`
	InitialCutoff                 *string `json:"initial_cutoff,omitempty"`
	RetentionDays                 *int    `json:"retention_days,omitempty"`
	CallAlertEnabled              bool    `json:"call_alert_enabled"`
	ConfigVersion                 int     `json:"config_version"`
	ConfigVersionSet              bool    `json:"-"`
}

// CleanupJob records owner-scoped filesystem paths that must be removed after
// an account's database row is hidden/deleted. It intentionally has no
// foreign-key dependency on the account so a failed filesystem cleanup can be
// retried after metadata removal.
type CleanupJob struct {
	ID        string
	UserID    string
	AccountID string
	Paths     []string
}

// VoiceInstallJob is the durable status of an administrator-triggered voice
// installation. A queued/running row is deliberately retained across process
// death so startup can expose it as interrupted instead of silently returning
// a 404 for an accepted job.
type VoiceInstallJob struct {
	ID        string
	UserID    string
	Voice     string
	Stage     string
	Status    string
	Progress  int
	Error     string
	CreatedAt string
	UpdatedAt string
}

type FolderRole struct {
	AccountID  string `json:"account_id"`
	Role       string `json:"role"`
	RemotePath string `json:"remote_path"`
	LocalPath  string `json:"local_path"`
}

type DraftAttachment struct {
	Filename    string
	ContentType string
	Path        string
	Size        int64
}

type DraftRecord struct {
	ID                string
	UserID            string
	AccountID         string
	Subject           string
	Body              string
	ForwardMode       string
	OriginalMessageID *int64
	To                []string
	Cc                []string
	Bcc               []string
	Attachments       []DraftAttachment
	CreatedAt         string
	UpdatedAt         string
}

type SyncRun struct {
	ID         int64  `json:"id"`
	AccountID  string `json:"account_id"`
	Kind       string `json:"kind"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Success    bool   `json:"success"`
	Changed    bool   `json:"changed"`
	Error      string `json:"error,omitempty"`
}

type MessageMutation struct {
	ID         int64  `json:"id"`
	MessageID  int64  `json:"message_id"`
	AccountID  string `json:"account_id"`
	Operation  string `json:"operation"`
	FromFolder string `json:"from_folder"`
	ToFolder   string `json:"to_folder"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// OutboundSubmission records the durable identity and last known SMTP outcome
// of one message submission attempt. A pending or uncertain row is evidence
// that its Message-ID must be reconciled before a caller retries.
type OutboundSubmission struct {
	MessageID  string
	UserID     string
	AccountID  string
	DraftID    string
	Status     string
	Error      string
	CreatedAt  string
	UpdatedAt  string
	AcceptedAt string
}

// HistoryRetentionPolicy bounds durable historical rows without deleting
// security records or work that still needs reconciliation. MaxRows applies
// independently to each history table's terminal rows.
type HistoryRetentionPolicy struct {
	MaxAge  time.Duration
	MaxRows int
}

type Contact struct {
	ID           int64  `json:"id"`
	UserID       string `json:"user_id"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	DisplayOrder int    `json:"display_order"`
}

type WhitelistEntry struct {
	ID     int64  `json:"id"`
	UserID string `json:"user_id"`
	Phone  string `json:"phone"`
}

type AlertNumber struct {
	ID        int64  `json:"id"`
	UserID    string `json:"user_id"`
	Number    string `json:"number"`
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at,omitempty"`
}

type AttachmentMetadata struct {
	PartPath    string
	Filename    string
	ContentType string
	Size        int64
	ContentID   string
	Disposition string
	Playable    bool
}

// SIPSettings is the single deployment-wide baresip account. Password holds
// the sealed ciphertext as read from the database; it is never serialized.
type SIPSettings struct {
	Domain   string `json:"domain"`
	Username string `json:"username"`
	Password string `json:"-"`
	// Port is retained as a compatibility alias for the old coupled setting.
	// New callers should use LocalPort and RegistrarPort explicitly.
	Port          int    `json:"port,omitempty"`
	LocalPort     int    `json:"local_port"`
	RegistrarPort int    `json:"registrar_port"`
	Transport     string `json:"transport"`
	RegInterval   int    `json:"reg_interval"`
	Enabled       bool   `json:"enabled"`
}

type MailSummary struct {
	ID                                                                        int64
	AccountID, Folder, Path, MessageID, Sender, Recipients, Cc, Subject, Date string
	DateUTC                                                                   *int64
	Read                                                                      bool
	Attachments                                                               int
	UID                                                                       uint32
	UIDValidity                                                               uint32
	Flags                                                                     string
}

type MailCursor struct {
	DateUTC *int64
	ID      int64
}

type MailPage struct {
	Messages []MailSummary
	Next     *MailCursor
}

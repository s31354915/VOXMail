package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CallerRoute is one account a whitelisted phone may reach. The admission
// flow reads all routes for the calling number; more than one route turns the
// greeting into an account-choice menu ordered by first-added binding.
type CallerRoute struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	AccountID   string `json:"account_id"`
	AccountName string `json:"account_name"`
	Position    int    `json:"position"`
}

// BoundAccount is one account a whitelist entry covers.
type BoundAccount struct {
	AccountID   string `json:"account_id"`
	AccountName string `json:"account_name"`
	Position    int    `json:"position"`
}

// WhitelistDetail is the web-facing shape of one whitelist entry with the
// accounts the number can reach.
type WhitelistDetail struct {
	ID       int64          `json:"id"`
	UserID   string         `json:"user_id"`
	Phone    string         `json:"phone"`
	Verified bool           `json:"verified"`
	Accounts []BoundAccount `json:"accounts"`
}

// CallerVerification is a pending phone-ownership proof. Incoming verifications
// are answered by the call service; outgoing ones are dialed by it.
type CallerVerification struct {
	ID         int64  `json:"id"`
	UserID     string `json:"user_id"`
	Phone      string `json:"phone"`
	Code       string `json:"-"`
	Method     string `json:"method"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
	ConsumedAt string `json:"consumed_at"`
}

// VerificationSettings is the administrator policy for number verification.
// Both directions may be offered at once, in which case the user chooses in
// the web UI. The dial-in number is shown to users starting an incoming
// verification when the system cannot derive one from its own configuration.
type VerificationSettings struct {
	Incoming bool   `json:"verification_incoming"`
	Outgoing bool   `json:"verification_outgoing"`
	DialIn   string `json:"verification_dialin"`
}

var (
	// ErrVerificationInvalid reports a wrong, expired, or already used code.
	ErrVerificationInvalid = errors.New("verification code is invalid or expired")
	// ErrVerificationUnavailable reports that no verification direction is
	// enabled, so a number cannot be verified right now.
	ErrVerificationUnavailable = errors.New("number verification is disabled")
)

// AddWhitelist stores a caller number. Verified marks numbers added directly
// by an administrator or proven through a verification call; user-submitted
// numbers start unverified and flip only after a consumed verification.
func (s *Store) AddWhitelist(ctx context.Context, entry WhitelistEntry) error {
	if entry.UserID == "" || strings.TrimSpace(entry.Phone) == "" {
		return fmt.Errorf("user and alert number are required")
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO caller_whitelist(user_id, phone, verified, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, phone) DO UPDATE SET verified = MAX(COALESCE(caller_whitelist.verified,0), excluded.verified)`,
		entry.UserID, strings.TrimSpace(entry.Phone), boolToInt(entry.Verified), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// ListWhitelist keeps the original flat shape for existing callers.
func (s *Store) ListWhitelist(ctx context.Context, userID string) ([]WhitelistEntry, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, user_id, phone, verified FROM caller_whitelist WHERE user_id = ? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []WhitelistEntry
	for rows.Next() {
		var entry WhitelistEntry
		var verified int
		if err := rows.Scan(&entry.ID, &entry.UserID, &entry.Phone, &verified); err != nil {
			return nil, err
		}
		entry.Verified = verified != 0
		result = append(result, entry)
	}
	return result, rows.Err()
}

// ListWhitelistDetail returns entries with their bound accounts for the console.
func (s *Store) ListWhitelistDetail(ctx context.Context, userID string) ([]WhitelistDetail, error) {
	entries, err := s.ListWhitelist(ctx, userID)
	if err != nil {
		return nil, err
	}
	details := make([]WhitelistDetail, 0, len(entries))
	for _, entry := range entries {
		detail := WhitelistDetail{ID: entry.ID, UserID: entry.UserID, Phone: entry.Phone, Verified: entry.Verified, Accounts: []BoundAccount{}}
		details = append(details, detail)
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT phone, account_id, position FROM caller_number_accounts WHERE user_id = ? ORDER BY phone, position, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byPhone := make(map[string]int, len(details))
	for i, detail := range details {
		byPhone[detail.Phone] = i
	}
	for rows.Next() {
		var number, accountID string
		var position int
		if err := rows.Scan(&number, &accountID, &position); err != nil {
			return nil, err
		}
		idx, ok := byPhone[number]
		if !ok {
			continue
		}
		details[idx].Accounts = append(details[idx].Accounts, BoundAccount{AccountID: accountID, Position: position})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Resolve display names in one pass per distinct account.
	for i := range details {
		for j := range details[i].Accounts {
			name, err := s.accountName(ctx, details[i].Accounts[j].AccountID)
			if err != nil {
				continue
			}
			details[i].Accounts[j].AccountName = name
		}
	}
	return details, nil
}

func (s *Store) accountName(ctx context.Context, accountID string) (string, error) {
	var name string
	err := s.DB.QueryRowContext(ctx, `SELECT canonical_name FROM accounts WHERE id = ?`, accountID).Scan(&name)
	return name, err
}

// DeleteWhitelist removes the entry and every account binding behind it.
func (s *Store) DeleteWhitelist(ctx context.Context, userID string, id int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var phone string
	if err := tx.QueryRowContext(ctx, `SELECT phone FROM caller_whitelist WHERE user_id = ? AND id = ?`, userID, id).Scan(&phone); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM caller_number_accounts WHERE user_id = ? AND phone = ?`, userID, phone); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM caller_whitelist WHERE user_id = ? AND id = ?`, userID, id); err != nil {
		return err
	}
	return tx.Commit()
}

// nextCallerPosition allocates the next menu slot for a user's number.
func (s *Store) nextCallerPosition(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, userID, number string) (int, error) {
	var maxPos sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MAX(position) FROM caller_number_accounts WHERE user_id = ? AND phone = ?`, userID, number).Scan(&maxPos); err != nil {
		return 0, err
	}
	return int(maxPos.Int64) + 1, nil
}

// AddCallerAccount binds a whitelist number to one account. The whitelist row
// is created (or upgraded) with the given verification state so an
// administrator add is immediately usable.
func (s *Store) AddCallerAccount(ctx context.Context, userID, number, accountID string, verified bool) error {
	number = strings.TrimSpace(number)
	if userID == "" || number == "" || accountID == "" {
		return fmt.Errorf("user, number, and account are required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner string
	err = tx.QueryRowContext(ctx, `SELECT user_id FROM accounts WHERE id = ? AND COALESCE(deleting,0) = 0`, accountID).Scan(&owner)
	if err == sql.ErrNoRows {
		return ErrAccountNotFound
	}
	if err != nil {
		return err
	}
	if owner != userID {
		return fmt.Errorf("account belongs to another user")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO caller_whitelist(user_id, phone, verified, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, phone) DO UPDATE SET verified = MAX(COALESCE(caller_whitelist.verified,0), excluded.verified)`,
		userID, number, boolToInt(verified), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	position, err := nextCallerPositionT(ctx, tx, userID, number)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO caller_number_accounts(user_id, account_id, phone, position, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id, phone, account_id) DO NOTHING`,
		userID, accountID, number, position, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveCallerAccount unbinds one account from a number. The whitelist entry
// itself stays: it is the verified phone record, not the binding.
func (s *Store) RemoveCallerAccount(ctx context.Context, userID, number, accountID string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM caller_number_accounts WHERE user_id = ? AND phone = ? AND account_id = ?`, userID, number, accountID)
	return err
}

// ReorderCallerAccounts assigns consecutive menu positions to the given
// account ids for one user's number. Account ids not in the list keep their
// relative order behind the reordered ones.
func (s *Store) ReorderCallerAccounts(ctx context.Context, userID, number string, orderedAccountIDs []string) error {
	number = strings.TrimSpace(number)
	if userID == "" || number == "" {
		return fmt.Errorf("user and number are required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT account_id FROM caller_number_accounts WHERE user_id = ? AND phone = ? ORDER BY position, id`, userID, number)
	if err != nil {
		return err
	}
	var existing []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		existing = append(existing, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	index := make(map[string]int, len(orderedAccountIDs))
	for i, id := range orderedAccountIDs {
		index[id] = i + 1
	}
	ordered := make([]string, 0, len(existing))
	for _, id := range orderedAccountIDs {
		for _, candidate := range existing {
			if candidate == id {
				ordered = append(ordered, id)
				break
			}
		}
	}
	for _, id := range existing {
		if _, seen := index[id]; !seen {
			ordered = append(ordered, id)
		}
	}
	position := 1
	for _, id := range ordered {
		if _, err := tx.ExecContext(ctx, `UPDATE caller_number_accounts SET position = ? WHERE user_id = ? AND phone = ? AND account_id = ?`, position, userID, number, id); err != nil {
			return err
		}
		position++
	}
	return tx.Commit()
}

// AccountsForPhone resolves every account a calling number can reach, ordered
// for the admission menu. Disabled users and deleting accounts are skipped.
func (s *Store) AccountsForPhone(ctx context.Context, number string) ([]CallerRoute, error) {
	number = strings.TrimSpace(number)
	rows, err := s.DB.QueryContext(ctx, `
		SELECT b.user_id, u.username, b.account_id, a.canonical_name, b.position
		FROM caller_number_accounts b
		JOIN users u ON u.id = b.user_id
		JOIN accounts a ON a.id = b.account_id
		WHERE b.phone = ? AND u.enabled = 1 AND COALESCE(a.deleting,0) = 0
		ORDER BY b.position, b.id`, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var routes []CallerRoute
	for rows.Next() {
		var route CallerRoute
		if err := rows.Scan(&route.UserID, &route.Username, &route.AccountID, &route.AccountName, &route.Position); err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, rows.Err()
}

// CreateCallerVerification stores a fresh proof code for a user's number,
// replacing any earlier pending one so only the newest code can verify.
func (s *Store) CreateCallerVerification(ctx context.Context, userID, number, method, code string, ttl time.Duration) error {
	number = strings.TrimSpace(number)
	if userID == "" || number == "" || code == "" {
		return fmt.Errorf("user, number, and code are required")
	}
	now := time.Now().UTC()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE caller_verifications SET consumed_at = ? WHERE user_id = ? AND phone = ? AND consumed_at IS NULL`, now.Format(time.RFC3339Nano), userID, number); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO caller_verifications(user_id, phone, code, method, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		userID, number, code, method, now.Format(time.RFC3339Nano), now.Add(ttl).Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

// PendingIncomingVerification returns the freshest unconsumed incoming
// verification for a number, used when that number calls in to hear its code.
func (s *Store) PendingIncomingVerification(ctx context.Context, number string) (CallerVerification, error) {
	var v CallerVerification
	row := s.DB.QueryRowContext(ctx, `SELECT id, user_id, phone, code, method, created_at, expires_at, COALESCE(consumed_at,'') FROM caller_verifications
		WHERE phone = ? AND method = 'incoming' AND consumed_at IS NULL ORDER BY id DESC LIMIT 1`, strings.TrimSpace(number))
	err := row.Scan(&v.ID, &v.UserID, &v.Phone, &v.Code, &v.Method, &v.CreatedAt, &v.ExpiresAt, &v.ConsumedAt)
	if err != nil {
		return CallerVerification{}, err
	}
	expires, parseErr := time.Parse(time.RFC3339Nano, v.ExpiresAt)
	if parseErr != nil || time.Now().UTC().After(expires) {
		return CallerVerification{}, sql.ErrNoRows
	}
	return v, nil
}

// ConsumeCallerVerification checks a submitted code and, when it matches, the
// pending verification is consumed and the number is marked verified.
func (s *Store) ConsumeCallerVerification(ctx context.Context, userID, number, code string) error {
	number, code = strings.TrimSpace(number), strings.TrimSpace(code)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM caller_verifications
		WHERE user_id = ? AND phone = ? AND code = ? AND method IN ('incoming','outgoing') AND consumed_at IS NULL AND expires_at > ?
		ORDER BY id DESC LIMIT 1`, userID, number, code, now.Format(time.RFC3339Nano)).Scan(&id)
	if err == sql.ErrNoRows {
		return ErrVerificationInvalid
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE caller_verifications SET consumed_at = ? WHERE id = ?`, now.Format(time.RFC3339Nano), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE caller_whitelist SET verified = 1 WHERE user_id = ? AND phone = ?`, userID, number); err != nil {
		return err
	}
	return tx.Commit()
}

// VerificationSettings reads the administrator verification policy.
func (s *Store) VerificationSettings(ctx context.Context) (VerificationSettings, error) {
	var incoming, outgoing int
	var dialin string
	err := s.DB.QueryRowContext(ctx, `SELECT verification_incoming, verification_outgoing, verification_dialin FROM system_settings WHERE id = 1`).Scan(&incoming, &outgoing, &dialin)
	if err != nil {
		return VerificationSettings{}, err
	}
	return VerificationSettings{Incoming: incoming != 0, Outgoing: outgoing != 0, DialIn: strings.TrimSpace(dialin)}, nil
}

// SaveVerificationSettings persists the administrator verification policy.
func (s *Store) SaveVerificationSettings(ctx context.Context, settings VerificationSettings) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO system_settings(id, verification_incoming, verification_outgoing, verification_dialin) VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET verification_incoming = excluded.verification_incoming,
		verification_outgoing = excluded.verification_outgoing, verification_dialin = excluded.verification_dialin`,
		boolToInt(settings.Incoming), boolToInt(settings.Outgoing), strings.TrimSpace(settings.DialIn))
	return err
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// nextCallerPositionT is nextCallerPosition bound to a transaction.
func nextCallerPositionT(ctx context.Context, tx *sql.Tx, userID, number string) (int, error) {
	var maxPos sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(position) FROM caller_number_accounts WHERE user_id = ? AND phone = ?`, userID, number).Scan(&maxPos); err != nil {
		return 0, err
	}
	return int(maxPos.Int64) + 1, nil
}

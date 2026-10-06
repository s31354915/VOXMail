package store

import "context"

func (s *Store) IMAPPassword(ctx context.Context, accountID string) (string, error) {
	var value string
	err := s.DB.QueryRowContext(ctx, `SELECT imap_password FROM accounts WHERE id = ?`, accountID).Scan(&value)
	return value, err
}

func (s *Store) SMTPPassword(ctx context.Context, accountID string) (string, error) {
	var value string
	err := s.DB.QueryRowContext(ctx, `SELECT smtp_password FROM accounts WHERE id = ?`, accountID).Scan(&value)
	return value, err
}

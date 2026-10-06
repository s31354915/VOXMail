package store

import (
	"context"
	"database/sql"
)

func (s *Store) AddContact(ctx context.Context, contact Contact) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO contacts(user_id,name,email,display_order) VALUES (?,?,?,?)`, contact.UserID, contact.Name, contact.Email, contact.DisplayOrder)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
func (s *Store) UpdateContact(ctx context.Context, contact Contact) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE contacts SET name=?, email=?, display_order=? WHERE id=? AND user_id=?`, contact.Name, contact.Email, contact.DisplayOrder, contact.ID, contact.UserID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) ListContacts(ctx context.Context, userID string) ([]Contact, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user_id,name,email,display_order FROM contacts WHERE user_id=? ORDER BY display_order,name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Contact
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ID, &c.UserID, &c.Name, &c.Email, &c.DisplayOrder); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) DeleteContact(ctx context.Context, userID string, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM contacts WHERE user_id=? AND id=?`, userID, id)
	return err
}

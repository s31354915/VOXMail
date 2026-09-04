package mailindex

import (
	"context"
	"os"
	"time"

	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/store"
)

type Indexer struct{ Store *store.Store }

func (i Indexer) Index(ctx context.Context, accountID, root string) error {
	messages, err := mailparse.Scan(root)
	if err != nil {
		return err
	}
	for _, message := range messages {
		info, err := os.Stat(message.Path)
		if err != nil {
			continue
		}
		_, err = i.Store.DB.ExecContext(ctx, `INSERT INTO mail_messages(account_id,folder,path,message_id,sender,recipients,subject,message_date,is_read,attachment_count,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET folder=excluded.folder,message_id=excluded.message_id,sender=excluded.sender,recipients=excluded.recipients,subject=excluded.subject,message_date=excluded.message_date,is_read=excluded.is_read,attachment_count=excluded.attachment_count,updated_at=excluded.updated_at`, accountID, message.Folder, message.Path, message.MessageID, message.From, message.To, message.Subject, message.Date, message.Read, len(message.Attachments), info.ModTime().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	return nil
}

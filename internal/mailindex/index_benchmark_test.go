package mailindex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/store"
)

// Timing records store API latency, including SQL, transactions and connection
// acquisition. Store calls are not SQL statement counts: upsert performs two
// statements and attachment replacement owns a transaction.
type measuredIndexStore struct {
	*store.Store
	latency                                time.Duration
	find, seen, upsert, attachments, purge int
}

func (s *measuredIndexStore) FindMailIndex(ctx context.Context, account, path string) (store.MailIndexRecord, error) {
	started := time.Now()
	defer func() { s.latency += time.Since(started); s.find++ }()
	return s.Store.FindMailIndex(ctx, account, path)
}
func (s *measuredIndexStore) MarkMailIndexSeen(ctx context.Context, account, path, scan string) error {
	started := time.Now()
	defer func() { s.latency += time.Since(started); s.seen++ }()
	return s.Store.MarkMailIndexSeen(ctx, account, path, scan)
}
func (s *measuredIndexStore) UpsertMailIndex(ctx context.Context, input store.MailIndexInput) (int64, error) {
	started := time.Now()
	defer func() { s.latency += time.Since(started); s.upsert++ }()
	return s.Store.UpsertMailIndex(ctx, input)
}
func (s *measuredIndexStore) ReplaceAttachmentMetadata(ctx context.Context, id int64, input []store.AttachmentMetadata) error {
	started := time.Now()
	defer func() { s.latency += time.Since(started); s.attachments++ }()
	return s.Store.ReplaceAttachmentMetadata(ctx, id, input)
}
func (s *measuredIndexStore) PurgeUnseenMailIndex(ctx context.Context, account, scan, pattern string) error {
	started := time.Now()
	defer func() { s.latency += time.Since(started); s.purge++ }()
	return s.Store.PurgeUnseenMailIndex(ctx, account, scan, pattern)
}

func benchmarkMaildir(b *testing.B, count, attachments int) string {
	b.Helper()
	root := b.TempDir()
	newDir := filepath.Join(root, "Inbox", "new")
	if err := os.MkdirAll(newDir, 0700); err != nil {
		b.Fatal(err)
	}
	for n := 0; n < count; n++ {
		body := "Content-Type: text/plain\r\n\r\n" + strings.Repeat("body ", 400) + "\r\n"
		if attachments > 0 {
			body = "Content-Type: multipart/mixed; boundary=fixture\r\n\r\n--fixture\r\n" + body
			for a := 0; a < attachments; a++ {
				body += fmt.Sprintf("--fixture\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=file-%d.bin\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s\r\n", a, strings.Repeat("x", 4096))
			}
			body += "--fixture--\r\n"
		}
		message := fmt.Sprintf("From: sender@example.test\r\nTo: user@example.test\r\nSubject: Message %d\r\nMessage-ID: <message-%d@example.test>\r\nDate: Thu, 01 Jan 2026 00:00:00 +0000\r\n%s", n, n, body)
		if err := os.WriteFile(filepath.Join(newDir, fmt.Sprintf("message-%06d", n)), []byte(message), 0600); err != nil {
			b.Fatal(err)
		}
	}
	return root
}

func BenchmarkIndexDatabaseOperations(b *testing.B) {
	for _, count := range []int{128, 1024} {
		for _, attachments := range []int{0, 1} {
			for _, mode := range []string{"new", "unchanged", "reparse"} {
				b.Run(fmt.Sprintf("%d_messages/%d_attachments/%s", count, attachments, mode), func(b *testing.B) {
					ctx := context.Background()
					root := benchmarkMaildir(b, count, attachments)
					db, err := store.Open(filepath.Join(b.TempDir(), "index.db"))
					if err != nil {
						b.Fatal(err)
					}
					defer db.Close()
					if _, err := db.DB.Exec(`INSERT INTO users(id,username,password_hash,pin_hash,role,created_at) VALUES('u','u','p','p','user','now')`); err != nil {
						b.Fatal(err)
					}
					if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','u','Mail','u@example.test','Mail','imap','u','p','smtp','u','p','{}','now')`); err != nil {
						b.Fatal(err)
					}
					if mode != "new" {
						if err := (Indexer{Store: db}).Index(ctx, "a", root); err != nil {
							b.Fatal(err)
						}
					}
					measured := &measuredIndexStore{Store: db}
					indexer := Indexer{Store: measured}
					b.ReportAllocs()
					b.ResetTimer()
					for n := 0; n < b.N; n++ {
						b.StopTimer()
						// Reset persistent state outside timing. Reparse explicitly
						// invalidates cached mtimes; filesystem contents stay fixed.
						if mode == "new" {
							if _, err := db.DB.Exec(`DELETE FROM mail_messages WHERE account_id='a'`); err != nil {
								b.Fatal(err)
							}
						}
						if mode == "reparse" {
							if _, err := db.DB.Exec(`UPDATE mail_messages SET updated_at='invalidated' WHERE account_id='a'`); err != nil {
								b.Fatal(err)
							}
						}
						b.StartTimer()
						if err := indexer.Index(ctx, "a", root); err != nil {
							b.Fatal(err)
						}
						b.StopTimer()
						var messages, parts, invalid int
						if err := db.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(source_size<=0 OR last_seen_scan=''),0) FROM mail_messages WHERE account_id='a'`).Scan(&messages, &invalid); err != nil {
							b.Fatal(err)
						}
						if err := db.DB.QueryRow(`SELECT COUNT(*) FROM attachment_metadata`).Scan(&parts); err != nil {
							b.Fatal(err)
						}
						if messages != count || parts != count*attachments || invalid != 0 {
							b.Fatalf("incomplete index: messages=%d parts=%d invalid=%d", messages, parts, invalid)
						}
						b.StartTimer()
					}
					b.StopTimer()
					total := count * b.N
					if measured.find != total || measured.purge != b.N {
						b.Fatalf("missing scan operations: %+v", measured)
					}
					if mode == "unchanged" {
						if measured.seen != total || measured.upsert != 0 || measured.attachments != 0 {
							b.Fatal("unchanged scan reparsed files")
						}
					} else if measured.upsert != total || measured.attachments != total || measured.seen != 0 {
						b.Fatal("new/reparse scan skipped imports")
					}
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(total)/1e3, "index-us/msg")
					b.ReportMetric(float64(measured.latency.Nanoseconds())/float64(total)/1e3, "store-us/msg")
					b.ReportMetric(100*float64(measured.latency)/float64(b.Elapsed()), "store-percent")
					b.ReportMetric(float64(measured.find+measured.seen+measured.upsert+measured.attachments+measured.purge)/float64(total), "store-calls/msg")
				})
			}
		}
	}
}

func BenchmarkIndexParsingOnly(b *testing.B) {
	for _, count := range []int{128, 1024} {
		for _, attachments := range []int{0, 1} {
			b.Run(fmt.Sprintf("%d_messages/%d_attachments", count, attachments), func(b *testing.B) {
				root := benchmarkMaildir(b, count, attachments)
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					seen := 0
					err := mailparse.ScanMetadataEachContext(context.Background(), root, func(message mailparse.MaildirMessage) error {
						seen++
						if len(message.Attachments) != attachments {
							return fmt.Errorf("attachment count %d", len(message.Attachments))
						}
						for _, part := range message.Attachments {
							if part.Size != 4096 {
								return fmt.Errorf("attachment size %d", part.Size)
							}
						}
						return nil
					})
					if err != nil || seen != count {
						b.Fatalf("scan count=%d err=%v", seen, err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(count*b.N)/1e3, "parse-us/msg")
			})
		}
	}
}

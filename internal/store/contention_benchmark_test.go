package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// BenchmarkSQLiteConnectionContention uses production connection policy and
// real page reads/upsert/attachment transactions. Each burst starts bounded
// workers together and joins them; no benchmark changes the connection count.
func BenchmarkSQLiteConnectionContention(b *testing.B) {
	const readsPerWorker, writesPerBurst = 32, 16
	for _, readers := range []int{1, 4} {
		for _, writerParts := range []int{-1, 0, 16} {
			name := "reads_only"
			if writerParts >= 0 {
				name = fmt.Sprintf("writer_%d_attachments", writerParts)
			}
			b.Run(fmt.Sprintf("%d_readers/%s", readers, name), func(b *testing.B) {
				ctx := context.Background()
				db, err := Open(filepath.Join(b.TempDir(), "contention.db"))
				if err != nil {
					b.Fatal(err)
				}
				defer db.Close()
				if err := db.CreateUser(ctx, User{ID: "u", Username: "u", PasswordHash: "p", PINHash: "p", Role: "user", Enabled: true}); err != nil {
					b.Fatal(err)
				}
				if _, err := db.DB.Exec(`INSERT INTO accounts(id,user_id,canonical_name,email,sender_name,imap_host,imap_user,imap_password,smtp_host,smtp_user,smtp_password,folder_map,created_at) VALUES('a','u','Mail','u@example.test','Mail','imap','u','p','smtp','u','p','{}','now')`); err != nil {
					b.Fatal(err)
				}
				var writerID int64
				for i := 0; i < 64; i++ {
					id, err := db.UpsertMailIndex(ctx, MailIndexInput{AccountID: "a", Folder: "Inbox", Path: fmt.Sprintf("/fixture/%d", i), MessageID: fmt.Sprintf("<%d@example.test>", i), Subject: fmt.Sprintf("Message %d", i), SourceSize: 100, ScanID: "fixture", UpdatedAt: "fixture"})
					if err != nil {
						b.Fatal(err)
					}
					if i == 0 {
						writerID = id
					}
				}
				parts := make([]AttachmentMetadata, 0)
				for i := 0; i < writerParts; i++ {
					parts = append(parts, AttachmentMetadata{PartPath: fmt.Sprintf("part-%d", i), Filename: fmt.Sprintf("file-%d.bin", i), ContentType: "application/octet-stream", Size: 4096})
				}
				if db.DB.Stats().MaxOpenConnections != 1 {
					b.Fatal("benchmark must retain production single connection")
				}
				var readTotal time.Duration
				var readP95Total time.Duration
				var waits int64
				var waitTime time.Duration
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					before := db.DB.Stats()
					start := make(chan struct{})
					results := make(chan struct {
						latencies []time.Duration
						err       error
					}, readers+1)
					var wg sync.WaitGroup
					for r := 0; r < readers; r++ {
						wg.Add(1)
						go func() {
							defer wg.Done()
							latencies := make([]time.Duration, 0, readsPerWorker)
							<-start
							for step := 0; step < readsPerWorker; step++ {
								started := time.Now()
								page, err := db.ListMailPage(ctx, "u", "a", "Inbox", false, nil, 50)
								latencies = append(latencies, time.Since(started))
								if err != nil || len(page.Messages) != 50 || page.Next == nil {
									results <- struct {
										latencies []time.Duration
										err       error
									}{latencies, fmt.Errorf("page count=%d next=%v error=%v", len(page.Messages), page.Next, err)}
									return
								}
							}
							results <- struct {
								latencies []time.Duration
								err       error
							}{latencies, nil}
						}()
					}
					if writerParts >= 0 {
						wg.Add(1)
						go func() {
							defer wg.Done()
							<-start
							var err error
							for step := 0; step < writesPerBurst; step++ {
								_, err = db.UpsertMailIndex(ctx, MailIndexInput{AccountID: "a", Folder: "Inbox", Path: "/fixture/0", MessageID: "<0@example.test>", Subject: "Writer", SourceSize: 100, ScanID: "writer", UpdatedAt: "writer", Attachments: len(parts)})
								if err != nil {
									break
								}
								if err = db.ReplaceAttachmentMetadata(ctx, writerID, parts); err != nil {
									break
								}
							}
							results <- struct {
								latencies []time.Duration
								err       error
							}{nil, err}
						}()
					}
					close(start)
					wg.Wait()
					close(results)
					b.StopTimer()
					var latencies []time.Duration
					for result := range results {
						if result.err != nil {
							b.Fatal(result.err)
						}
						latencies = append(latencies, result.latencies...)
					}
					if len(latencies) != readers*readsPerWorker {
						b.Fatal("incomplete reader burst")
					}
					for _, latency := range latencies {
						readTotal += latency
					}
					sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
					readP95Total += latencies[(95*len(latencies)+99)/100-1]
					after := db.DB.Stats()
					waits += after.WaitCount - before.WaitCount
					waitTime += after.WaitDuration - before.WaitDuration
					var rowCount, partCount int
					if err := db.DB.QueryRow(`SELECT COUNT(*) FROM mail_messages`).Scan(&rowCount); err != nil {
						b.Fatal(err)
					}
					if err := db.DB.QueryRow(`SELECT COUNT(*) FROM attachment_metadata`).Scan(&partCount); err != nil {
						b.Fatal(err)
					}
					if rowCount != 64 || partCount != len(parts) {
						b.Fatalf("writer lost metadata: rows=%d parts=%d", rowCount, partCount)
					}
					b.StartTimer()
				}
				b.StopTimer()
				b.ReportMetric(float64(readTotal.Nanoseconds())/float64(b.N*readers*readsPerWorker)/1e3, "read-mean-us/op")
				b.ReportMetric(float64(readP95Total.Nanoseconds())/float64(b.N)/1e3, "read-p95-us/burst")
				b.ReportMetric(float64(waits)/float64(b.N), "pool-waits/burst")
				b.ReportMetric(float64(waitTime.Nanoseconds())/float64(b.N)/1e6, "pool-wait-ms/burst")
			})
		}
	}
}

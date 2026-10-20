package speech

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSpeechBenchmarkHelper is an isolated CPU-bound child implementing the
// Piper output contract. It does not load neural models or contact providers.
func TestSpeechBenchmarkHelper(t *testing.T) {
	if os.Getenv("VOXMAIL_SPEECH_BENCH_HELPER") != "1" {
		return
	}
	var output string
	for i, arg := range os.Args {
		if arg == "--output_file" && i+1 < len(os.Args) {
			output = os.Args[i+1]
		}
	}
	if output == "" {
		os.Exit(90)
	}
	value := sha256.Sum256([]byte("controlled speech CPU workload"))
	for i := 0; i < 400000; i++ {
		value = sha256.Sum256(value[:])
	}
	if err := writeTestWAV(output); err != nil {
		os.Exit(91)
	}
	// Use the computed value as the PCM sample; create no extra artifacts.
	file, err := os.OpenFile(output, os.O_WRONLY, 0)
	if err != nil {
		os.Exit(92)
	}
	if _, err := file.WriteAt(value[:2], 44); err != nil {
		os.Exit(92)
	}
	if err := file.Close(); err != nil {
		os.Exit(92)
	}
	os.Exit(0)
}

func BenchmarkCallPreviewSpeechContention(b *testing.B) {
	for _, mode := range []struct {
		name   string
		calls  int
		shared bool
	}{
		{"one_call", 1, true}, {"four_calls_shared_voice_speed", 4, true}, {"four_calls_distinct_speeds", 4, false},
	} {
		for _, previews := range []int{0, 4} {
			b.Run(fmt.Sprintf("%s/%d_previews", mode.name, previews), func(b *testing.B) {
				root := b.TempDir()
				binary, err := os.Executable()
				if err != nil {
					b.Fatal(err)
				}
				wrapper := filepath.Join(root, "piper-fixture")
				quoted := "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "'"
				// Suppress race-runtime exit delay in helper processes only.
				script := "#!/bin/sh\nVOXMAIL_SPEECH_BENCH_HELPER=1 GORACE=atexit_sleep_ms=0 exec " + quoted + " -test.run '^TestSpeechBenchmarkHelper$' -- \"$@\"\n"
				if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
					b.Fatal(err)
				}
				model := filepath.Join(root, "fixture.onnx")
				if err := os.WriteFile(model, []byte("fixture model; not a neural network"), 0600); err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(model+".json", []byte(`{"audio":{"sample_rate":8000}}`), 0600); err != nil {
					b.Fatal(err)
				}
				pool := NewRuntimePool(wrapper, root, filepath.Join(root, "missing-whisper"), "unused", filepath.Join(root, "runtime"), "python3", false)
				runtimes := make([]*Runtime, mode.calls)
				leases := make([]*Lease, mode.calls)
				defer func() {
					for _, lease := range leases {
						lease.Release()
					}
				}()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				for i := range runtimes {
					speed := 3
					if !mode.shared {
						speed = i + 1
					}
					runtimes[i], leases[i] = pool.Activate("fixture", speed)
				}
				for _, runtime := range runtimes {
					if err := runtime.WaitTTS(ctx); err != nil {
						b.Fatal(err)
					}
				}
				for i := 1; i < len(runtimes); i++ {
					if (runtimes[i] == runtimes[0]) != mode.shared {
						b.Fatal("incorrect runtime sharing")
					}
				}
				var callTotal, callMaxTotal, previewTotal time.Duration
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					start := make(chan struct{})
					type result struct {
						preview bool
						latency time.Duration
						output  string
						err     error
					}
					results := make(chan result, mode.calls+previews)
					var wg sync.WaitGroup
					for i := 0; i < mode.calls+previews; i++ {
						wg.Add(1)
						go func(i int) {
							defer wg.Done()
							<-start
							output := filepath.Join(root, fmt.Sprintf("sample-%d-%d.wav", n, i))
							started := time.Now()
							var err error
							preview := i >= mode.calls
							if preview {
								err = (Piper{Binary: wrapper, Model: model, Extra: SpeedExtra(3)}).Synthesize(ctx, "This is a VOXMail voice preview.", output)
							} else {
								err = runtimes[i].Synthesize(ctx, "For Inbox press one. For Drafts press two.", output)
							}
							results <- result{preview, time.Since(started), output, err}
						}(i)
					}
					close(start)
					wg.Wait()
					close(results)
					b.StopTimer()
					var maxCall time.Duration
					for r := range results {
						if r.err != nil {
							b.Fatal(r.err)
						}
						if !ValidWAV(r.output) {
							b.Fatal("invalid synthesis output")
						}
						if r.preview {
							previewTotal += r.latency
						} else {
							callTotal += r.latency
							if r.latency > maxCall {
								maxCall = r.latency
							}
						}
						if err := os.Remove(r.output); err != nil {
							b.Fatal(err)
						}
					}
					callMaxTotal += maxCall
					b.StartTimer()
				}
				b.StopTimer()
				b.ReportMetric(float64(callTotal.Nanoseconds())/float64(b.N*mode.calls)/1e6, "call-mean-ms/op")
				b.ReportMetric(float64(callMaxTotal.Nanoseconds())/float64(b.N)/1e6, "call-max-ms/burst")
				if previews > 0 {
					b.ReportMetric(float64(previewTotal.Nanoseconds())/float64(b.N*previews)/1e6, "preview-mean-ms/op")
				}
			})
		}
	}
}

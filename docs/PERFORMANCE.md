# Performance measurements

These controlled benchmarks measure specific sources of contention. They do not
estimate real-provider throughput or establish a concurrency setting for every
deployment. Run them on the deployment hardware before changing worker counts.

## Sequential account synchronization

Command (2026-10-08, linux/amd64 codespace; runtime/CPU in the raw result):

```sh
go test ./internal/mailsync -run '^$' -bench '^BenchmarkSequentialAccountDelay$' -benchtime=3x -count=3 -benchmem
```

The benchmark uses three ordered accounts, real SQLite run records, generated
mbsync configuration and the normal bounded child process runner. A disposable
shell child waits 0, 50 or 200 ms on the first/last account before returning exit
status 7. All accounts follow the failed-provider completion path; no provider
connection, real account or production mail is used. Every measured sweep must
create three run rows and return the expected child exit status. The periodic
scheduler's clock advances past failure backoff so repeated samples remain due.
The middle-account metric measures from sweep admission to its endpoint check.
Setup and result validation are outside the measurement window.

Median of three runs, three sweeps per run:

| Scheduler / injected delay | Sweep duration (ms) | Middle account starts (ms) |
| --- | ---: | ---: |
| startup/baseline | 8.54 | 3.55 |
| startup/slow_first_50ms | 60.60 | 55.55 |
| startup/slow_last_50ms | 60.32 | 3.87 |
| startup/slow_first_200ms | 210.64 | 205.60 |
| startup/slow_last_200ms | 210.22 | 3.67 |
| due/baseline | 8.07 | 3.50 |
| due/slow_first_50ms | 61.09 | 55.92 |
| due/slow_last_50ms | 60.44 | 3.93 |
| due/slow_first_200ms | 210.65 | 205.80 |
| due/slow_last_200ms | 211.05 | 4.33 |

Both startup and periodic sweeps delay later accounts by approximately the full
slow-account duration. Moving the same delay behind the middle account removes
that wait without reducing total sweep time. This isolates account ordering
from ordinary configuration/database/process overhead. A slow failing account
can therefore consume its full sync budget before later due accounts start.

The current sequential scheduled policy remains in place. These measurements
establish the scheduling bottleneck, but do not measure successful-provider
indexing, database contention, memory, speech pressure or a candidate worker
pool's deletion/cancellation/ownership behavior. P02–P04 cover the remaining
measurements; a scheduling concurrency change must include those acceptance
checks rather than deriving a pool size from injected latency alone.

[Raw benchmark output](benchmarks/scheduling.txt) records all samples and
allocation statistics. Re-run with more samples on target hardware when choosing
a scheduling policy or comparing an implementation change.


## Per-message indexing

```sh
go test ./internal/mailindex -run '^$' -bench '^BenchmarkIndex(DatabaseOperations|ParsingOnly)$' -benchtime=3x -count=3 -benchmem
```

The fixture uses 128 or 1,024 real Maildir files with a 2 KB plain body and zero
or one 4 KB attachment. Each full-index sample uses the production SQLite store,
normal schema/WAL/connection settings, and unmodified indexing paths. `new`
removes prior rows before the timed scan; `unchanged` pre-indexes files and retains
their cache metadata; `reparse` invalidates cached modification times before each
scan while keeping file contents fixed. State resets and validation queries are
outside timing. Parsing-only cases scan the same files and verify attachment
counts and sizes without a database.

A timing wrapper records store API calls and their elapsed time. This includes
SQLite/driver work, transaction commits and connection acquisition; it is not a
count of SQL statements or a split between filesystem and driver costs. The
wrapper's clock overhead is included. The benchmark checks message count,
attachment row count, source size and scan identity after every indexed sample,
and checks that unchanged files take the seen-marker path rather than reparsing.

New/reparsed messages call lookup, upsert and attachment replacement once each,
plus one stale-row purge per sweep. Upsert executes both a write and an ID lookup;
attachment replacement performs a delete plus each attachment insert in its own
transaction, even with zero attachments. An unchanged message still performs a
lookup and a seen-marker write, plus the final sweep purge. These operations
explain why avoiding MIME parsing alone does not eliminate database work.

Median of three samples, three scans per sample (microseconds per message):

| Messages / attachments / scan | Total index | Store calls/msg | Store time | Store share |
| --- | ---: | ---: | ---: | ---: |
| 128_messages/0_attachments/new | 722.3 | 3.008 | 660.2 | 91.41% |
| 128_messages/0_attachments/unchanged | 418.2 | 2.008 | 407.2 | 97.37% |
| 128_messages/0_attachments/reparse | 705.5 | 3.008 | 640.9 | 90.85% |
| 128_messages/1_attachments/new | 1099.0 | 3.008 | 1020.0 | 92.78% |
| 128_messages/1_attachments/unchanged | 340.4 | 2.008 | 329.7 | 96.85% |
| 128_messages/1_attachments/reparse | 1009.0 | 3.008 | 926.7 | 92.16% |
| 1024_messages/0_attachments/new | 697.2 | 3.001 | 634.8 | 91.04% |
| 1024_messages/0_attachments/unchanged | 316.7 | 2.001 | 307.0 | 96.92% |
| 1024_messages/0_attachments/reparse | 656.9 | 3.001 | 593.6 | 90.50% |
| 1024_messages/1_attachments/new | 957.2 | 3.001 | 881.2 | 92.05% |
| 1024_messages/1_attachments/unchanged | 351.8 | 2.001 | 341.7 | 97.08% |
| 1024_messages/1_attachments/reparse | 976.5 | 3.001 | 898.0 | 91.96% |

| Messages / attachments | Parsing-only time (µs/msg) |
| --- | ---: |
| 128_messages/0_attachments | 29.0 |
| 128_messages/1_attachments | 37.3 |
| 1024_messages/0_attachments | 27.0 |
| 1024_messages/1_attachments | 37.2 |

In this fixture, store operations consume roughly 90–97% of indexed scan time.
The unchanged path reduces work to two store calls per message, but each seen
marker still performs a separate write. These observations make bounded batches
of seen-marker writes and atomic message/attachment import candidates for a
future comparison. They do not establish a batch size, safe transaction lifetime,
or performance on a different filesystem. Large transactions could delay callers
sharing the single connection; P03 measures that contention before introducing
batching or extra connections. Production indexing/transaction boundaries remain
unchanged. Preserve cancellation, partial-sync no-purge behavior, malformed-file
retention, attachment consistency and account deletion when testing a candidate.

[Raw indexing benchmark output](benchmarks/indexing.txt) includes all samples and
allocation statistics. These runs use warm fixture files and a local codespace
volume; they are not cold-disk or production-scale throughput claims.


## Single SQLite connection contention

```sh
go test ./internal/store -run '^$' -bench '^BenchmarkSQLiteConnectionContention$' -benchtime=5x -count=3 -benchmem
go test -race ./internal/store -run '^$' -bench '^BenchmarkSQLiteConnectionContention$' -benchtime=1x -count=1
```

The production schema, WAL policy and `MaxOpenConns(1)` stay unchanged. Each
burst admits one or four bounded reader goroutines and optionally one writer
at a common barrier. Each reader fetches 32 real keyset pages of 50 messages
from a 64-message mailbox. The writer performs 16 message upsert/attachment
replacement sequences, with zero or 16 attachment metadata rows per transaction.
The fixture uses no network, attachment blobs, artificial transaction sleep or
production data. Every burst joins all workers, validates every page's count and
next cursor, and checks final message/attachment row counts. Setup, validation,
percentile sorting and metric aggregation are outside timing.

Read latency includes pool acquisition, SQL/driver work and row decoding. Pool
wait count/duration come from `database/sql.DB.Stats` deltas around the burst.
Their duration sums all waiting goroutines and can exceed elapsed wall time.
The reported p95 is the nearest-rank percentile within each burst, averaged
across that sample's five bursts, rather than a pooled percentile across samples.
The table reports medians of three samples; race-instrumented timing is excluded.

| Readers / writer | Burst (ms) | Mean read (µs) | Mean burst read p95 (µs) | Pool waits/burst | Aggregate pool wait (ms/burst) |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1_readers/reads_only | 13.69 | 426.9 | 497.3 | 0.0 | 0.00 |
| 1_readers/writer_0_attachments | 25.83 | 708.2 | 1138.0 | 64.0 | 22.81 |
| 1_readers/writer_16_attachments | 36.06 | 911.9 | 1322.0 | 64.0 | 29.33 |
| 4_readers/reads_only | 58.64 | 1774.0 | 4094.0 | 125.8 | 168.80 |
| 4_readers/writer_0_attachments | 69.47 | 1945.0 | 4640.0 | 162.4 | 249.20 |
| 4_readers/writer_16_attachments | 82.11 | 2197.0 | 5334.0 | 164.6 | 281.50 |

One reader has no pool waits alone; concurrent readers and a writer generate
substantial acquisition waits. The 16-attachment writer approximately doubles
single-reader latency compared with reading alone in this fixture. Four readers
already contend with each other, and attachment writes add further delay. This
confirms a connection-sharing bottleneck distinct from provider latency or MIME
parsing. It does not prove that adding read connections would improve every
workload: this test does not compare a separate read pool, long-lived snapshots,
checkpoint pressure, schema migration or production storage.

Retain the current connection policy for this review. A future read-pool or
batching candidate must compare these same workloads and preserve per-connection
foreign-key/query-only/busy-timeout policy, account ownership, deletion ordering,
cancellation and backup/restore behavior. Never increase connections solely from
aggregate wait duration, which counts overlapping waiters.

[Raw connection contention output](benchmarks/sqlite-contention.txt) preserves
all samples and allocation statistics. The race execution passed all six cases
with the concurrent readers/writer active.


## Call/preview speech contention

```sh
go test ./internal/speech -run '^$' -bench '^BenchmarkCallPreviewSpeechContention$' -benchtime=3x -count=3 -benchmem
```

Six cases compare one call, four calls sharing a voice/speed runtime, and four
calls with distinct speeds, each with zero or four concurrent preview synthesis
requests. Calls acquire leases through the normal runtime pool and synthesize
through its serialized Piper worker. Preview synthesis uses the direct one-shot
Piper path used by the web preview service, with no runtime serialization. Runtime
TTS warmup occurs outside timing. Every timed burst starts its bounded requests
at a common barrier and joins them, then validates and removes WAV outputs outside
timing. No production worker, call or preview admission limit is increased.

The child fixture performs 400,000 dependent SHA-256 operations and emits one
valid PCM sample using the computed bytes. The normal process runner, input
normalization, WAV validation and atomic publication are exercised. This is a
CPU contention experiment, not a neural-model speed benchmark: it excludes actual
Piper/ONNX loading, model memory, Whisper recognition, RTP playback, dynamic prompt
cache hits and HTTP polling. Real-model/exact-image acceptance remains R11.
The fixture is local and uses no provider accounts or network access. Results
include process startup and worker wait as part of user-visible synthesis latency.

Median of three samples, three bursts per sample:

| Calls / previews | Mean call synthesis (ms) | Mean slowest call per burst (ms) | Mean preview synthesis (ms) |
| --- | ---: | ---: | ---: |
| one_call/0_previews | 42.10 | 42.10 | — |
| one_call/4_previews | 115.80 | 115.80 | 117.90 |
| four_calls_shared_voice_speed/0_previews | 105.60 | 169.60 | — |
| four_calls_shared_voice_speed/4_previews | 183.30 | 247.40 | 121.60 |
| four_calls_distinct_speeds/0_previews | 96.97 | 103.20 | — |
| four_calls_distinct_speeds/4_previews | 203.10 | 213.50 | 198.50 |

Four CPU-bound previews increased the single call's median synthesis latency
from 42.10 to 115.80 ms on this two-CPU codespace. Shared voice/speed calls also
wait behind the runtime's single worker; distinct speeds use distinct workers
and contend on CPU instead. Separate preview processes do not enter a call's
worker lock, but still consume the same machine's CPU. Dynamic prompt cache hits
and fixed prompt playback bypass much of this work; this experiment specifically
models cache misses. The latency distributions and model memory on target
hardware are required before choosing a worker count or preview priority policy.

Keep existing runtime serialization and preview admission limits. Raising counts
can increase call latency under CPU pressure, so these results support measuring
a candidate scheduling policy rather than raising limits without evidence. No
production concurrency change was made. Race-instrumented executions of all six
benchmark cases passed, as did focused tests for runtime release, Piper/voice
operation cancellation, deletion canceling call work, preview ownership and
preview cancellation. Race timings are excluded from the performance table.

[Raw speech contention output](benchmarks/speech-contention.txt) records samples
and parent-process allocations; it does not report neural-model or child-process
RSS. Real-model and exact-digest runtime validation remains a separate acceptance
gate.

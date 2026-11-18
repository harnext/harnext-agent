# Harnext agent performance

What the agent costs on a developer's computer, measured on one machine, with a way to repeat every number. A Turkish version is in [PERFORMANS.md](PERFORMANS.md). For what the agent reads and sends, see [PRIVACY.md](PRIVACY.md).

## Summary

| | Result |
|---|---|
| First scan of 18 transcripts, 110 MB (last 30 days) | about 1.3 s until the agent is quiet, 1.2 s of CPU time |
| Idle | 0.05 to 0.17 % of one CPU core |
| Memory | 17 to 27 MB (what macOS charges to the process, its "footprint"); `ps` shows 68 to 98 MB RSS for the same process, see below |
| Disk | 0.1 MB (local database, state, status) |
| Download | 5.8 MB (macOS arm64), 14.1 MB unpacked |

The agent reads only what was appended since the last time (it keeps an offset per file), so the first scan is the expensive moment; after that it handles new lines as they arrive.

## How it was measured

One computer: MacBook Pro, Apple M1 Pro (10 cores), macOS, arm64, Go 1.27.3. The input is the developer's own Claude Code history: 18 transcript files, 110.4 MB, 26,938 handled lines (11 sessions), the last 30 days, which is what the agent reads at start. Files were in the operating system's cache, so disk speed is not part of these numbers. Nothing here was measured on Linux or Windows.

**1. The real binary** ([scripts/measure-agent.sh](../scripts/measure-agent.sh)). Runs the agent against a local dummy server with a throwaway agent directory, samples CPU time and memory with `ps`, and reports the first scan and 30 to 60 seconds of idle. Nothing is uploaded and no real agent state is touched. It runs without friction analysis, because the dummy server provides no signed configuration.

| Build | Cold start until quiet | CPU time | Footprint | RSS (`ps`) | Idle CPU |
|---|---|---|---|---|---|
| Development build | ~1.3 s | 1.17 to 1.22 s | 17 to 26 MB | 68 to 80 MB | 0.05 to 0.13 % |
| Release 0.2.3 (obfuscated build, no longer shipped) | ~1.3 s | 1.26 to 1.30 s | 21 to 27 MB | 82 to 100 MB | 0.10 to 0.17 % |

Since 0.2.4 the release is a plain trimmed Go build (the agent is open source), 13.5 MiB instead of 29.2 MiB. The table above was measured before that change and has not been repeated on the same data; the size figures in the summary are for the new build.

The spread is the range of 3 to 5 runs each. Two memory figures are shown because they differ a lot on macOS: RSS also counts pages that Go has freed and macOS has not taken back yet (it only does so under memory pressure), so it reads three to four times higher than the footprint. The footprint is the number to quote; RSS is shown for completeness. The footprint was added to the script later, so it comes from the last 3 runs of each build.

**2. The scan, in one process, with friction analysis** (`TestMeasureScanPlain` and `TestMeasureScanAnalyzed` in `internal/watcher`). Walks the same files, parses every line and aggregates, without the network and the database.

| | Wall time | CPU time | Throughput | Peak RSS |
|---|---|---|---|---|
| Without analysis | 0.49 to 0.53 s | 0.51 to 0.53 s | 207 to 226 MB/s | 38 to 39 MB |
| With analysis (production default configuration) | 0.78 to 0.82 s | 0.82 to 0.84 s | 135 to 141 MB/s | 41 to 42 MB |

Peak RSS includes about 12 to 14 MB that the test process uses before it starts scanning. It is a different measure from the footprint above (RSS, in one process without the database and the network), so the two tables are not meant to be added up or compared one to one.

**3. The hot path on a synthetic fixture.** `BenchmarkParseAndAdd` parses one small transcript and folds it into the aggregator: about 29 µs, about 120 MB/s, 119 allocations per pass.

## Repeat it

```sh
make build && ./scripts/measure-agent.sh                         # real binary; add a path to measure another build
go test -run '^$' -bench ParseAndAdd -benchmem ./internal/aggregate
go test -tags realdata -count=1 -run TestMeasureScanPlain -v ./internal/watcher
HARNEXT_REALDATA_CONFIG=cfg.json go test -tags realdata -count=1 -run TestMeasureScanAnalyzed -v ./internal/watcher
```

`cfg.json` is the JSON of the server's default agent configuration. Run each real-data test in its own process, because peak memory is a per-process maximum. The tests print sizes, times and counts, never paths or content.

## Technical Q&A

**Does it slow down my editor or my AI tool?** The agent is not in the path of your tool. It does not proxy network traffic; it reads the log files your tool writes, after the fact. Idle, it uses well under a fifth of a percent of one core.

**Why does the first start take a second of CPU?** It reads the last 30 days of history once, newest first, so the most recent sessions are uploaded first. After that, only new lines are read.

**How much memory does it use?** About 17 to 27 MB as the operating system counts it (footprint) on the measured machine, while scanning and idle. `ps` shows 68 to 98 MB RSS for the same process because it includes memory Go has freed that macOS has not yet taken back. The split inside the agent (Go runtime, embedded SQLite, watchers, aggregated state) has not been profiled. Linux and Windows were not measured, and they account for memory differently.

**What happens when the server is not reachable?** Uploads retry with a growing wait; counters are kept in the local database and re-sent when the server answers. Nothing is lost and nothing is queued as content, only numbers.

**What if there is no signed configuration?** The friction scores stay empty (the agent sends no score, not a zero). Token, cost and topic data are still collected.

**Which systems are supported?** macOS (arm64, amd64), Linux (amd64, arm64) and Windows (amd64), with launchd, systemd `--user` and Task Scheduler services. Performance was measured on macOS arm64 only.

**How is the binary protected, and how does it update?** The release build is not obfuscated; the source is public (below). Release packages are checked by SHA-256, and the update notice and the agent configuration are signed with Ed25519; the agent accepts only newer versions signed with the key it trusts. Updating can be turned off with `HARNEXT_NO_AUTO_UPDATE=1`.

**Is the agent open source?** Yes, Apache 2.0: github.com/harnext/harnext-agent. The Harnext server and dashboard are not.

**What does it send, and how can I check?** See [PRIVACY.md](PRIVACY.md). `harnext-agent preview` shows the exact data before it is sent.

# Harnext agent

The Harnext agent runs on a developer's computer and reads the local logs of AI coding tools (Claude Code and VS Code Copilot). It sends token, cost and session-friction counters to [Harnext](https://app.harnext.ai). It never sends prompts, replies or source code.

This repository holds the agent and the wire contracts it speaks. The Harnext server, dashboard and database are not open source.

## What it reads and what it sends

- **Reads (read-only):** Claude Code transcripts under `~/.claude/projects`, the Copilot OpenTelemetry file, and VS Code Copilot chat session files. It does not open your project files, `.env` or `appsettings.json`.
- **Sends:** numbers and labels only: token counts, model, tool, session ID and times, turn count, topic label, project name (`owner/repo` from the git remote), and quality counters. The full table and an example request are in [docs/PRIVACY.md](docs/PRIVACY.md) (Turkish: [docs/GIZLILIK.md](docs/GIZLILIK.md)).
- **Never sends:** prompt text, reply text, source code, file contents, file names, full paths, your user name, branch names.

## Check it yourself

```sh
harnext-agent preview              # prints the exact JSON the agent would send, sends nothing
go test ./internal/service -run TestNoFakeSecretLeavesTheMachine
```

The test writes fake secrets into prompts, replies, `.env` and `appsettings.json` and fails if any of them reaches the upload, the status file or the log. A static audit (`internal/fsaudit`) lists every place the agent opens a file, lists a directory or starts a program; an unlisted call breaks the build. The request schema is [contracts/ingest.v1.schema.json](contracts/ingest.v1.schema.json) and allows no other fields.

## Install

Open the Setup page in the Harnext dashboard and run the command it shows. It downloads a release from `dl.harnext.ai`, verifies its SHA-256 against a manifest served by `app.harnext.ai`, and installs a user service (launchd, systemd `--user` or a Windows scheduled task). The agent updates itself from a signed release notice; `HARNEXT_NO_AUTO_UPDATE=1` turns that off.

Commands: `init`, `run`, `status`, `stats`, `stop`, `preview`, `service install|uninstall`, `copilot --apply|--revert`.

## Build

Needs the Go version in `go.mod`.

```sh
make check                         # gofmt, vet, race tests
make build                         # bin/harnext-agent
```

A build from source has no trusted public key and no default server: it runs without friction scores and without self-update, and `init` needs `--server`. Release builds embed the Harnext public key and server address with `-ldflags`; see the `release` target in the [Makefile](Makefile). Friction patterns and weights are not in this repository: they arrive as a signed remote configuration that the agent verifies before use.

## Performance

[docs/PERFORMANCE.md](docs/PERFORMANCE.md) lists CPU, memory and scan time, measured on one machine, with the script to repeat it.

## Security

Report vulnerabilities privately: see [SECURITY.md](SECURITY.md).

## License

Apache License 2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE).

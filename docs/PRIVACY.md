# Harnext privacy: what the agent reads, what it sends

This page describes the Harnext agent, the program that runs on a developer's computer. Every claim here can be checked against the code or by running a command; the last section shows how. A Turkish version is in [GIZLILIK.md](GIZLILIK.md).

## In short

- The agent counts. It reads your AI tool's own log files, turns them into numbers (tokens, counts, a topic label) and sends only those numbers.
- Your prompts, the AI's replies, source code, file contents, file names, branch names and git credentials are never sent and never stored on the server.
- The agent does not open your project files. In a project folder it reads only the git remote, to turn it into `owner/repo`.
- You can look at exactly what would be sent before anything leaves your computer: `harnext-agent preview`.

## What the agent reads on your computer

| What | Where | Why |
|---|---|---|
| Claude Code session logs (transcripts) | `~/.claude/projects/**/*.jsonl` and, if you use a second Claude config folder, `~/.claude*/projects` (folders directly under your home whose name starts with `.claude`) | Token counts, model, tool use counts, friction counters |
| Claude account email | `oauthAccount.emailAddress` inside `~/.claude.json` (or `<config folder>/.claude.json`), read as a stream; nothing else in that file is kept | Telling your accounts apart. The address is turned into a one-way hash at once and never stored or sent in clear |
| Git email of a project | `user.email` in the git config files that apply to the working folder (`.git/config`, `~/.gitconfig`, `~/.config/git/config` and includes); git is not run | Telling Copilot accounts apart (Copilot's own account email is not available locally). Hashed at once, never stored or sent in clear |
| VS Code Copilot chat sessions | VS Code `workspaceStorage/<id>/chatSessions/*.jsonl`, plus `workspace.json` of that workspace | Friction counters, topic, which project folder |
| Copilot telemetry file | The agent's own file (default), or a path you chose | Tokens, model, Copilot credits |
| Git data of a project | `.git`, `.git/commondir` and `.git/config` of the working folder | Only the remote URL, reduced to `owner/repo`. User name, password, token, branch and host are dropped |
| Names of marker files (`package.json`, `go.mod`, ...) | The working folder and its parents | Checked for existence only, never opened. Used to find the project root when there is no git |
| The agent's own files | The agent's directory (token, settings, local database, status) | Operating the agent |
| VS Code `settings.json` | Only when you run `harnext-agent copilot --apply` and confirm | Turns on Copilot telemetry; the file is backed up first and `--revert` restores it |

The agent does not read `.env`, `appsettings.json`, source files or any other file in your projects. It does not read environment variables other than its own (`HARNEXT_TOKEN`, `HARNEXT_HOME`, `HARNEXT_NO_AUTO_UPDATE`) and the standard ones that locate your home and configuration directories. It runs other programs only to install or stop its background service and to start an update that it has already verified.

The log files do contain your prompts, so the agent has to read those lines. It extracts numbers and drops the text. For example, your message is checked in memory against patterns (such as "that is wrong") to count a pushback; the message itself is not kept. For an edit, the agent takes only the folder of the file path inside the project, never the file name.

This list is enforced by a test: see "How you can verify this".

## What the agent sends

Only to your Harnext server, over HTTPS, with your agent token:

| Request | What it carries |
|---|---|
| `POST /api/v1/ingest` | Per session: tool, project identity, topic label, counters, token counts per model and day, timestamps, the session's active time in seconds (`active_seconds`), and a per-folder breakdown (see below) |
| `GET /api/v1/agent-config` | Nothing about you except the token and the agent version. Returns the signed rules used to compute counters |
| `GET /api/v1/agent-identity` | Nothing but the token. Returns the signed list of accounts you connected (one-way hashes and route ids), so the agent knows which sessions to send |
| `POST /api/v1/agent-accounts` | Accounts found on your computer that are not connected yet: a one-way hash, a masked label such as `dev***@gmail.com` (the first few characters of the part before the @ and the full domain), the tool and a session count. Sessions of such accounts are not sent until you connect the account in the panel |
| `GET /api/v1/agent-release` and the release package | Update check. Packages are verified by signature and checksum |

An ingest request has this shape (shortened; the full schema is [contracts/ingest.v1.schema.json](../contracts/ingest.v1.schema.json), a complete example is [contracts/testdata/ingest.v6.vector.json](../contracts/testdata/ingest.v6.vector.json)):

```json
{
  "payload_version": 6,
  "agent_version": "0.3.0",
  "config_version": 7,
  "sessions": [{
    "session_external_id": "claude-multi",
    "tool_name": "claude_code",
    "account_hash": "a1b2c3d4…64 hex characters",
    "project": "acme/app",
    "project_path": "apps/web",
    "topic": "feature",
    "quality": { "exchanges": 10, "friction_exchanges": 1, "tool_calls": 10, "...": "counters only" },
    "turn_count": 10,
    "started_at": "2026-11-01T10:00:00Z",
    "ended_at": "2026-11-01T11:00:00Z",
    "active_seconds": 3000,
    "usage": [{ "model": "claude-sonnet-4", "day": "2026-11-01", "input_tokens": 2, "output_tokens": 1000, "...": "token counts" }],
    "folders": [{ "path": "apps/web", "turn_count": 6, "quality": { "...": "counters" }, "usage": [] }]
  }]
}
```

Everything in it is a number, a date, a model name, a fixed label or a project identity. `account_hash` is a one-way hash of the account's email address; the server uses it only to route the session to the workspace you connected that account to, and does not store it with the session. The email address itself never leaves your computer.

About the project identity: for a git repository it is `owner/repo` and the folder inside the repository (`apps/web`). Outside git it is the working folder with your home directory replaced by `~` (for example `~/work/client-app`). A folder outside your home directory is sent as it is, so avoid sensitive names in such paths, or ask your administrator how to handle it.

## The one-off scan and shareable card

`harnext-agent scan` (also `curl -fsSL app.harnext.ai/scan.sh | sh`) reads the same local files for the last 30 days, installs nothing, writes nothing to disk and sends nothing by itself: it prints a summary on your screen. Only if you answer yes, it prints the exact JSON it would send (token totals per model, session and active-day counts; project names only if you say yes to that separately; the schema is [contracts/card.v1.schema.json](../contracts/card.v1.schema.json)) and asks once more. The server keeps this as a draft behind a private link that expires in 24 hours. Nothing is public until you publish it: you enter your seat price, choose whether project names may be shown (they are deleted from the card if you do not) and confirm. A published card is public at its link and stays until you delete it with the delete link shown once at publishing (there are no accounts: keep that link). Links are random and only their SHA-256 hashes are stored. Network addresses are not stored as such: a keyed hash is kept for rate limiting and removed after two hours. The card shows token counts and an estimate at current list prices, marked as self-reported and not verified; search engines are asked not to index it.

## What is never sent

Prompts and replies, the AI's reasoning, tool input and output (commands, file contents, error text), source code, file names, full paths inside your home directory, your user name and computer name, branch names, git credentials, environment variables, your e-mail address (the agent payload has no e-mail field), screenshots.

## What stays on your computer

The agent keeps a local database with counters and the full working folder of each session. `harnext-agent stats` shows it. It never leaves your computer. The agent's status file holds counts and times only: no paths, no content.

## What the server does with it

The server stores the numbers above, computes cost and scores when you look at them, and keeps the raw rows for the retention period set by your organization (3 to 36 months, 13 by default), then deletes them. Managers see organization totals, not individuals. A group smaller than the minimum group size (3 by default) is not shown. You can export your own data and delete your membership.

## How you can verify this

1. **See the data before it is sent.** `harnext-agent preview` prints the request body for your newest session as JSON. `--sessions 5` shows more, `--all` shows everything. The command reads only the agent's saved state, makes no network call and changes nothing. What is not in that output is not sent.
2. **The fake-secret test.** `go test ./internal/service -run TestNoFakeSecretLeavesTheMachine` (from the repository root) writes fake secrets into a prompt, a reply, a thinking block, a shell command, a tool error, `.env`, `appsettings.json`, a source file, a file name, the git remote with credentials, the branch and the user's directory name, for both Claude Code and Copilot. It runs the whole agent and fails if any of them appears in what is uploaded, in the status file or in the log.
3. **The file-access test.** `go test ./internal/fsaudit` lists every place where the agent's code reads a file, lists a directory, reads the environment, starts a program or opens a network connection, each with a reason. A new one cannot be added without this test failing, so a change that would look at more of your computer has to be written down and reviewed.
4. **The contract.** [contracts/ingest.v1.schema.json](../contracts/ingest.v1.schema.json) is the schema of the request body. It allows no other fields.
5. **The agent's own view.** `harnext-agent status` shows the server it talks to and what it last did.

Limits of these checks: the tests prove the behavior of this code, not of a modified agent. Tests use invented values only; no real prompt, key or customer data is used.

# Security policy

## Reporting a vulnerability

Email **security@harnext.ai**. Please do not open a public issue for a vulnerability.

Include what you found, the agent version (`harnext-agent --version`), your operating system and, if possible, steps to reproduce. Do not send real prompts, source code or tokens.

## Scope

In scope: this repository (the agent and the wire contracts), the release packages and the update mechanism. Anything that makes the agent read files it should not, send data it should not, or accept an unsigned configuration or update is a vulnerability.

Out of scope here: the Harnext server and dashboard. Report those to the same address.

## Supported versions

Only the latest release receives fixes. The agent updates itself, so most installations follow it.

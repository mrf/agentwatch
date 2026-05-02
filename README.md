# agentwatch

A Go library for monitoring multiple local AI coding agents such as Claude Code, OpenAI Codex CLI, and Gemini CLI.

## Overview

`agentwatch` observes local agent session artifacts, aggregates session state, and exposes the result through:

- A pull API: `Monitor.Snapshot()` for synchronous consumers.
- A push API: `monitor.EventSink` for reactive consumers.
- Optional HTTP and WebSocket transport adapters for remote or web frontends.

The library is UI-agnostic. Concepts like racing lanes, leaderboards, and achievements are out of scope — consumers layer those on top.

## Status

Pre-release. API is not yet stable.

## License

MIT

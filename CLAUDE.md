# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

The project rules, commands and architecture are shared by every agent that
works here and live in one place:

@AGENTS.md

## Claude Code specifics

- You lead this project; Codex and Antigravity are reviewers and helpers.
  Delegate research and bounded implementation to sub-agents, cheapest capable
  tier first (haiku, then sonnet, then opus), and give each one a single
  package to own so parallel work does not collide.
- Use `scripts/ask.sh sol|astra|luna` for independent Codex opinions and
  `scripts/review.sh` for diff reviews. Antigravity (`agy`) has a small quota:
  use it only for first-hand Antigravity behaviour or a third opinion.
- When testing the MCP shim against a real session, register it with a
  throwaway `A2P2_HOME` so the user's own node is untouched.

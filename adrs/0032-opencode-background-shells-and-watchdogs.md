# ADR 0032: OpenCode background shells, tool budget and memory stalls

Status: Proposed.
Date: 2026-11-04

## Context

The idle watchdog (ADR 0023) pauses while a tool runs and otherwise fails a run after
`idle_watchdog` of silence. Three production failures did not fit that model.

- **Background shells.** OpenCode 2.x lets the model run a shell with `background:
  true`. The call completes at once with `rawOutput.metadata = {status: "running",
  shellID}`, and OpenCode tells the model to end its response. The ACP prompt then
  answers `end_turn` while the shell runs. `opencode acp` owns the shell through its
  private server. When the shell exits, that server appends a synthetic `<shell
  id=… state=…>` message and resumes the session itself, outside any prompt, so ACP
  streams nothing. The adapter reported success and removed the attempt: checks the
  agent started never finished, and their results never reached it. Verified on
  2.2.5. Upstream's `opencode run` exits the same way.
- **Unbounded tools.** A tool in flight paused the watchdog without limit, up to the
  attempt `timeout`. Upstream bounds it with `MULTICA_AGENT_TOOL_WATCHDOG`, 2 h by
  default.
- **Memory thrash.** An attempt without `attempt_memory_max` that reaches the pool's
  `memory.high` is throttled, not killed (ADR 0020). One attempt stayed at 4 GiB for
  13 minutes with PSI `full` about 90 %. OpenCode froze with the build, sent no model
  request, and the watchdog reported `idle_watchdog`, which Multica treats as an
  agent hang.

## Decision

**Tool budget.** `tool_watchdog` (default `2h`, upstream's) bounds silence while a
tool or a background shell runs. Otherwise `idle_watchdog` applies. Either expiry
fails the run as before.

**Background shells.** On 2.x the ACP client records every shell of the main
session whose completed call reports `metadata.status = running`, and counts it as a
running tool. When the turn ends with such shells, the adapter keeps the agent
process and reads the session every 10 s through `opencode api --standalone GET
/api/session/<id>/message`. That is a short-lived private server on the same
`OPENCODE_DB`; the reads do not disturb the owning server. ACP updates after the
turn are ignored, so the session read is the only source:

- a synthetic `metadata.source = shell` message settles its shell, also when it
  arrived during the turn;
- resumed assistant messages are reported like their ACP stream: text, reasoning,
  tool use and result. Their tokens add to the turn's usage, reported once. Shells
  they move to the background are tracked in turn;
- the attempt ends when no shell runs and an `idle` message follows the last resumed
  message. Six consecutive unreadable reads fail it as the sandbox's failure, not the
  agent's: an out-of-memory read keeps its memory failure, any other is reported as
  `runtime_recovery`, so Multica retries and the retry resumes the session, and the
  last read's error with its redacted stderr is logged as `opencode session
  unreadable`. Refinement (0.2.1): two production runs lost an hour each when the
  reads failed after XDG data had filled the 1 GiB `/workspace` tmpfs (ADR 0015).

Shells of child sessions and background subagents are not followed.

**Memory stalls.** When a watchdog fires or the agent fails without an OOM kill (a thrashing OpenCode loses its own server and answers `ServerUnavailable`), the controller reads the attempt cgroup's
`memory.pressure`, through the pool on the host or inside the attempt. `full avg60`
of at least 50 % makes the failure a `MemoryStallError`: the limit thrashed at (the
attempt ceiling or the pool), the peak and the stall. Like an OOM kill, it is
reported as `agent_error.process_failure`, outside Multica's automatic retries, or
at the pool as `runtime_recovery` (ADR 0020), and counted with
`reason="memory_stall"`. A configured `attempt_memory_max` below the pool's
`memory.high` turns most stalls into OOM kills of the largest process, which the
agent sees as a failed command.

**Usage and diagnosis on every path.** Usage was reported only after the prompt
answered, so a watchdog, stream cut, OOM, stall, cancellation or timeout lost the
tokens already spent; the native daemon reports once at the end on any path. ACP
`usage_update` carries only context size and cost. The adapter now keeps the answered
turn's usage with its resumed turns, and an unanswered turn sums the assistant steps
the session store recorded since the prompt (session API on 2.x, export on 1.x),
read with a 15 s context detached from the attempt. Either total is reported once,
before the final flush, so it precedes the terminal report.

Agent stderr was discarded, so stream cuts, panics and OOMs looked alike. The workload
hands it to the adapter (`execution.StderrStreamer`), which keeps the last 8 KiB from a
line start and, when the attempt fails other than by cancellation, logs it at warn
level with task and attempt after redacting the attempt's credentials, the agent's
custom env values and credential-shaped text. It never reaches Multica. Session API
and export helpers keep discarding theirs.

## Consequences

Background checks finish inside their attempt, and their result reaches Multica with
the resumed turn. An attempt then holds its pool share until the shells end, bounded
by `tool_watchdog` and `timeout`. Session reads depend on OpenCode's undocumented
session API and `--standalone`; `scripts/opencode-release.sh` releases must keep them.

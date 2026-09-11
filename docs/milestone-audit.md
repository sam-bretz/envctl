# Milestone exit-criteria audit

Audit of roadmap milestones M1–M5 and local M7 against the exit checks in `docs/src/content/docs/roadmap.mdx`, as of branch `workflow-runtime` at the preview merge (`4fef238`). M6 (remote sharing) is out of scope by decision. Evidence details, commands and retained state paths are in `docs/implementation-progress.md`.

Evidence labels: **real** is a real VM and/or real agent run; **guest** is a component test in a real guest; **fixture** is a unit or fake-backend test. The only platform exercised is macOS arm64, Lima 2.2.0 (VZ), with an Ubuntu 26.04 guest.

## Summary

| Milestone | Exit checks | Verdict |
| --- | --- | --- |
| M1 Contracts, state, TUI shell | 6 of 6 met | **Deliverable** |
| M2 Dedicated VM, pinned repositories | 7 of 7 met | **Deliverable** for the macOS arm64 support matrix |
| M3 Plugins, executable Plan readiness | 7 of 7 met | **Deliverable** |
| M4 Execution through PR | 5 of 6 met | **Not yet**: a real model resuming after an injected early worker exit |
| M5 Restoration, rewind, review | 7 of 7 met | **Deliverable with a documented gap**: multi-dataset snapshot consistency (work package 1) |
| M7 local Parallel DAGs | 5 of 6 met | **Not yet**: Codex must pass the same harness conformance test (quota resets Sep 17); plugin/provider conformance suites |

## M1: Contracts, durable state, Bubble Tea shell

| Exit check | Verdict | Evidence |
| --- | --- | --- |
| Reject cycles, invalid transitions, missing artifacts | Met | fixture: `TestDAGValidationAndStableOrder`, `TestCheckpointsRequireArtifactsAndBoundReview`, `TestPlanCannotAdvanceFromAgentClaim` |
| Restore a run after process restart | Met | fixture: `TestTransactionalReceiptsSurviveRestart`, `TestServeSingleOwnerAndRestart`; real: coordinator restarts in every integrated run |
| Reject stale revision mutations | Met | fixture: `TestConcurrentClientsCannotOverwrite`, MCP version-fenced replay |
| Navigate/resize the TUI | Met | fixture: `TestDashboardResizeAndReadiness`, `TestSmallTerminalPreservesReviewContentAndDetach` |
| Script commands never launch the TUI | Met | fixture: `TestScriptInvocationsNeverLaunchTheDashboard` (added in pass 19) |
| Plan can't pass on a worker's claim | Met | fixture + real (Plan discovery 97031) |

## M2: Dedicated local VM and pinned repositories

| Exit check | Verdict | Evidence |
| --- | --- | --- |
| Separate daemon identities and data | Met | real: `TestLocalVMIsolation`, child-VM isolation, parallel run (5 distinct daemons) |
| Run A can't mutate run B or the host checkout | Met | real: host-unchanged assertions in integrated runs |
| No host Docker socket or shared writable mount | Met | fixture + real mount/socket checks |
| Upstream branch movement can't change pins | Met | fixture (real Git): `TestSourcePinsSurviveBranchMovement` |
| Guest restart restores the stack | Met | real: `TestRealGuestRestartAndRestoreCrashRecovery` (VM stop, then readiness restarts it; same daemon, healthy stack, volume data intact) |
| Partial provisioning retries never duplicate a VM | Met | fixture + observed real repair |
| Two-repository fixture | Met | real: 84097, 81856 |

Limitation: toolchain and image replay across providers or architectures is not established.

## M3: Invocation plugins and executable Plan readiness

| Exit check | Verdict | Evidence |
| --- | --- | --- |
| Missing requirements hold Plan with no downstream dispatch | Met | real: 81633, 97031, 81856 |
| Attachment affects only its invocation | Met | fixture + real 81856; TUI keys: `TestPluginAttachmentAndRemovalAreRevisionScopedActions` |
| Probe failure can't be overridden by prose | Met | real 81856 |
| Reject conflicting/transitive requirements | Met | fixture |
| Refresh credentials when supported | Met for plugins | real: lease renewal, rotation during pending operations |
| Recover an unavailable service before admission | Met | real: resource repair, Chromium repair |
| Secrets never in locks, artifacts, events | Met | fixture redaction tests; token absent from guest output (pass 18) |

**Credential policy (decided).** Both harnesses accept only long-lived credentials: Claude takes `ANTHROPIC_API_KEY` or a `claude setup-token` token; Codex takes a Codex access token or `OPENAI_API_KEY`. Refreshable logins are refused, and job start removes any login file an earlier version copied into a guest role home. Codex access tokens require a ChatGPT Business or Enterprise workspace. PostgreSQL restore omits roles, ownership and ACLs (documented).

## M4: Durable supervisor/worker execution through PR output

| Exit check | Verdict | Evidence |
| --- | --- | --- |
| Complete a feature with supervisor and worker in the VM | Met | real: six-stage two-repository runs to PR (84097, 81856, Codex); Claude parallel custom DAG through QA |
| Inject early worker exit and failed tests; observe continuation | **Partly met** | Failed tests: real (QA retry 81856; parallel left retry). Early exit: guest fault injection only (80504); no real model resumed after an injected exit |
| Reconnect after closing the TUI | Met | fixture + PTY smoke |
| Coordinator/runner restart without duplicate acceptance or PR | Met | real restarts during Code and parallel work; fixture lost-acknowledgement tests |
| Multi-repository output approved against its complete commit set | Met | real 84097 |
| Human gates explicit and distinct from recovery | Met | fixture; real runs approved by a fixture actor |

Work packages closed in pass 19: structured progress and live steering (real Claude steering test); no-progress stall detection (fixture only); submodule and LFS publication (local Git and git-lfs fixtures only, not real GitHub); opt-in host preview and service endpoints (real preview test on a dedicated VM).

## M5: Checkpoint restoration, rewind, complete TUI review

| Exit check | Verdict | Evidence |
| --- | --- | --- |
| Rewind running Code to Plan; old work checkpoints historically | Met | real 12794 |
| New work restores inputs and revalidates readiness | Met | real 12794 |
| Plugin attachment can't mutate the old worker | Met | fixture + real at Plan (81856) |
| Repeated rewinds can't resurrect superseded queued revisions | Met | fixture (race) |
| Recover a restore crash | Met | real: coordinator killed during a live PostgreSQL restore reconnects to the one job; a killed restore job is replaced by exactly one recovery generation; source restores cut at five points converge |
| Changed commits invalidate QA/approval | Met | fixture |
| Two attached clients | Met | real 12794 |

Open work package gaps: consistent multi-dataset/cross-service snapshots; dataset row and binary artifact comparison; restore-crash coverage of the HTTP fixture is fixture-level only.

## M7 (local): Parallel DAG execution and extensibility

| Exit check | Verdict | Evidence |
| --- | --- | --- |
| Parallel branches join on expected SHAs | Met | real: Claude parallel acceptance, 928.90s |
| One retry can't corrupt another | Met | real: same run (left retried while right stayed live and isolated) |
| Parent changes invalidate correct descendants | Met | fixture: fan-out rewind tests (sibling reused, join invalidated) |
| Capabilities/budgets apply per assignment | Met | fixture: per-node limits and per-child readiness |
| Integration tests don't share mutable fixtures | Met | real: per-branch emulator data, explicit join selection |
| An additional adapter passes the same lifecycle/recovery checks | **Partly met** | Claude passed `TestRealHarnessConformance`; Codex has not run it (quota until Sep 17) |

Open work package gap: plugin and provider conformance suites.

## Remaining before full local delivery

1. Real early-exit continuation: kill a real worker mid-attempt and observe the next attempt resume its session from captured source (M4).
2. Run `TestRealHarnessConformance` for Codex after the quota resets, using a Codex access token or API key (M7).
3. Plugin/provider conformance suites (M7 work package 4).
4. Multi-dataset snapshot consistency; PostgreSQL roles/ownership/ACLs (M5 work package 1, documented limits).
5. Optional, outward-facing: one real GitHub run of LFS/submodule publication (creates LFS objects that cannot be deleted from the repository).
6. Delivery hygiene: destroy `envctl-agent-acceptance` (the leftover `envctl-rev-*` VMs were deleted).

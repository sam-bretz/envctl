# envctl agent guide

Read this before helping someone set up or use envctl. The local Compose commands are stable; the workflow interface below is experimental.

## What it is

envctl gives each git worktree its own isolated docker-compose environment. The local Compose interface renders the repo's compose files into `.envctl/<feature>/compose.yaml` with a per-branch project name, no colliding host ports, and labels, then runs plain `docker compose` on that file. Those lifecycle commands do not require a daemon.

## Experimental workflows

Version 2 workflows use a persistent coordinator, Bubble Tea, and dedicated local VMs. Use `envctl run validate`, `envctl run create --task ...`, `envctl run readiness <run-id> --json`, and `envctl ui`. Keep existing version 1 configuration and lifecycle commands working when helping users migrate.

Before a first run, each harness needs a credential for its VMs; see [Install](/docs/install/#set-up-agent-credentials-workflow-runtime). Claude harnesses accept only `ANTHROPIC_API_KEY` or a `claude setup-token` token, never the user's claude.ai login, whose rotating refresh token would revoke the host login. `claude setup-token` is interactive, so ask the user to run it in their own terminal and save the token to a private file referenced as `credential: file:/absolute/path`. Never ask for the token in chat or print it.

Plan must verify downstream requirements before dependent execution. Attach a local executable package or the bundled Playwright package using `envctl run plugin add <run-id> --file browser.yaml`; attachments are frozen per invocation revision. Use `envctl run fixture init fixtures/http` to scaffold the HTTP dataset emulator, then commit the generated source and seed. PostgreSQL and HTTP fixture datasets retain checksum-verified snapshots outside the VM.

In Bubble Tea, `[` / `]` browse revisions, `,` / `.` select artifacts, `o` opens them, and `p` attaches a plugin reference file. Browsing and detaching do not cancel execution. Historical views are read-only; rewind creates a new revision.

Running attempts expose bounded, redacted `progress` (phase, current check, recent agent activity) in `envctl run show <run-id>` (`--json` for the full object), the Bubble Tea Conversation panel, and MCP run reads. Steer with `envctl run message <run-id> --node <stage> --to worker|supervisor --text ...` or `i` in Bubble Tea. A message reaches a running agent by interrupting its guest job and resuming the same harness session with the message; each message's status (included at start, delivered live, pending, or queued for the next attempt) is shown next to it. Live delivery waits until the harness reports its session, is limited to 8 resumes per role per attempt, and does not apply once an attempt's review has finished. Supervisors see every message for their stage and should reject work that ignores worker steering.

Use `d` to compare a checkpoint against its revision's source pins. To compare checkpoints, select the earlier one, press `b`, navigate to the target (including another revision), then press `d`. `B` resets the base; Escape closes the loaded review. The CLI equivalent is `envctl run diff <run-id> --node code [--revision <revision-id>] [--from <checkpoint-id>] [--json]`. Source comes from retained bundles, with explicit unavailable/truncated results. Comparison includes checkpoint summaries, review/approval/check evidence and artifact/dataset identities; it does not compare database rows or binary artifact contents.

Use `envctl run artifact <digest> --output <new-file>` to export checksum-verified evidence. Existing files are never overwritten. Agent-created temporary reports belong in the attempt's supplied scratch directory; read-only source stages must not add untracked report directories to repositories.

This workflow backend remains experimental. Full rewind, parallel execution, provider expansion, and complete database metadata replay are not all delivered. Read [the fixtures guide](/docs/workflow-fixtures/), [the roadmap](/docs/roadmap/), and `docs/implementation-progress.md` before claiming support or milestone completion.

## Setup checklist

1. `envctl` must be on PATH (`go install ./cmd/envctl` from the envctl repo).
2. The app repo needs `envctl.yaml` at its root:
   ```yaml
   version: 1
   project: mg
   stack:
     files: [deploy/local/docker-compose.yml]
   ports:
     mode: auto
   expose: []
   ```
   `project` must match `^[a-z][a-z0-9-]{0,15}$`. Every `stack.files` path must exist.
3. Add `.envctl/` to `.gitignore`.
4. Install the skill for agents: `envctl agent install` (writes `.claude/skills/envctl/` and `.agents/skills/envctl/`), then add the paragraph from `envctl agent snippet` to CLAUDE.md and AGENTS.md.
5. Optional, for Claude Code worktrees: put `hooks/claude/envctl-worktree-create` and `envctl-worktree-remove` on PATH and merge the output of `envctl hook claude` into `.claude/settings.json`. Both need `jq`.

## Commands

| Command | Use |
| --- | --- |
| `envctl up [--build] [--no-wait]` | Start or converge this branch's environment. |
| `envctl status --json` | Services, endpoints, env lines. Read this to find hosts and ports. |
| `envctl list` | All environments of this project on the host. |
| `envctl logs -f [service]` | Follow logs. |
| `envctl exec <service> -- <cmd>` | Run inside a container. |
| `envctl stop` / `start` | Pause and resume, keep data. |
| `envctl down [-v]` | Remove containers; `-v` also removes volumes and port allocations. |
| `envctl render` | Write the isolated compose file only. |

Global flags: `-C PATH` (operate on another worktree), `--env NAME` (override branch detection), `--port-mode domains|registry`, `--json`.

## How services are reached

- On OrbStack (domains mode): `service.<project>.orb.local` on the container port. No host ports are published.
- Elsewhere (registry mode): `127.0.0.1` on the port in `ENVCTL_PORT_<SERVICE>_<CONTAINERPORT>` from `.envctl/<feature>/env`.

Never assume a fixed port like `localhost:5433`. Always read `envctl status --json` or the env file.

## Common fixes

- `no envctl.yaml found`: the manifest is looked up at the worktree root only. Commit it.
- `docker is not reachable`: start the Docker engine or fix `docker context`.
- OrbStack name does not resolve: enable "Allow access to container domains & IPs" in OrbStack settings.
- `has not been rendered`: run `envctl up` or `envctl render` first.
- To see exactly what compose runs: `docker compose -f .envctl/<feature>/compose.yaml config`.

## CI

`envctl render` does not need Docker. Add to a repo's workflow:

```yaml
jobs:
  validate:
    uses: sam-bretz/envctl/.github/workflows/validate.yml@main
    secrets:
      token: ${{ secrets.ENVCTL_TOKEN }}
```

or install the binary in any job with `uses: sam-bretz/envctl@main`.

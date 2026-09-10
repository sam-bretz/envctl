# envctl agent guide

Read this before helping someone set up or use envctl. It is the whole tool in one page.

## What it is

envctl gives each git worktree its own isolated docker-compose environment. It renders the repo's compose files into `.envctl/<feature>/compose.yaml` with a per-branch project name, no colliding host ports, and labels, then runs plain `docker compose` on that file. There is no server and no database.

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
4. Optional, for Claude Code worktrees: put `hooks/claude/envctl-worktree-create` and `envctl-worktree-remove` on PATH and merge the output of `envctl hook claude` into `.claude/settings.json`. Both need `jq`.

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

Global flags: `-C PATH` (operate on another worktree), `--feature SLUG` (override branch detection), `--port-mode domains|registry`, `--json`.

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

---
name: envctl
description: Start, inspect, and tear down this repository's isolated docker-compose environment with envctl, which gives every git branch and worktree its own stack. Use when a task needs the local services running (database, API, LocalStack), when connecting to Postgres or another service, when docker compose, ports, or localhost:5433 come up, or when working in a git worktree that needs its own environment.
---

# envctl

This repository defines its dev stack in docker-compose and manages it with
`envctl`. Each git worktree gets its own compose project, network, volumes,
and non-colliding ports. Never run `docker compose` directly against the
repo's compose files and never assume a fixed port such as `localhost:5433`.

## Quick start

```bash
envctl up                # start or converge this branch's environment
envctl status --json     # hosts, ports, health; read this before connecting
envctl logs -f api       # follow a service
envctl exec postgres -- psql -U metergraph -c 'select 1'
envctl down -v           # remove containers, volumes, ports when finished
```

`up` is idempotent. Run it again after editing compose files or when unsure
whether the environment is running.

## Find a service

Read `envctl status --json` and use the `env` array or `endpoints` list:

- `ENVCTL_HOST_<SERVICE>` is the host to connect to. On OrbStack it is a DNS
  name like `postgres.mg-feat-x.orb.local` and the port is the container port.
- `ENVCTL_PORT_<SERVICE>_<CONTAINERPORT>` is the host port when one was
  published (registry mode). Absent in domains mode.

Shell pattern:

```bash
eval "$(envctl status --json | jq -r '.env[]' | sed 's/^/export /')"
psql "postgres://metergraph@${ENVCTL_HOST_POSTGRES}:${ENVCTL_PORT_POSTGRES_5432:-5432}/metergraph"
```

The same lines are in `.envctl/<feature>/env`; Makefiles can `-include` it.

## Rules

1. Use `envctl` for lifecycle: `up`, `stop`, `start`, `down`. Do not call
   `docker compose up/down` on the source files; that recreates the shared
   `app` project and collides with other worktrees.
2. Before connecting to any service, read `envctl status --json`.
3. Operating on another worktree: `envctl -C <path> <command>`. The feature
   name follows that worktree's branch.
4. A throwaway environment on the same branch: `envctl --env scratch up`,
   then `envctl --env scratch down -v`. A durable one: `envctl env create
   <name> [--branch B]`; it is kept until `envctl env rm <name>`.
5. Leave nothing behind: `envctl down -v` when a worktree's work is done,
   unless the WorktreeRemove hook is installed (it does this automatically).
6. Rendered files under `.envctl/` are generated; do not edit or commit them.

## When something fails

| Symptom | Do |
| --- | --- |
| `no envctl.yaml found` | You are outside the worktree root, or the manifest is missing. `cd` to the worktree; see REFERENCE.md to create one. |
| `docker is not reachable` | Docker is not running. Tell the user; do not retry in a loop. |
| A port is in use | Run `envctl down -v && envctl up`; envctl reallocates. |
| `has not been rendered` | Run `envctl up` (or `envctl render`) first. |
| Need to see what compose runs | `docker compose -f .envctl/<feature>/compose.yaml config` |

CI: `envctl render` needs no Docker daemon. The reusable workflow
`sam-bretz/envctl/.github/workflows/validate.yml@main` renders on every PR,
and the composite action `sam-bretz/envctl@main` installs the binary in any job.

Details of every command, flag, the manifest, and the env file:
[REFERENCE.md](REFERENCE.md).

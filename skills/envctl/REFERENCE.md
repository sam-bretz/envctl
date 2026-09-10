# envctl reference

## Global flags

```
envctl [-C PATH] [--feature SLUG] [--backend local] [--port-mode domains|registry] [--json] <command>
```

| Flag | Meaning |
| --- | --- |
| `-C PATH` | Operate on the worktree containing PATH. |
| `--feature SLUG` | Override branch detection. Value is slugified. |
| `--port-mode` | Override the manifest's `ports.mode` for this call. |
| `--json` | One JSON document on stdout. Compose progress goes to stderr. |

## Commands

| Command | Effect |
| --- | --- |
| `up [--build] [--no-wait]` | Render then `docker compose up --detach --remove-orphans`; waits for health unless `--no-wait`. Idempotent. |
| `down [-v]` | Remove containers and network. `-v` also removes volumes, releases ports, deletes `.envctl/<feature>/`. |
| `stop` / `start` | Pause and resume; data kept. |
| `status` | Services, health, endpoints, env lines. Needs a prior `up` or `render`. |
| `list` | Every environment of this project on the Docker host. |
| `render` | Write `.envctl/<feature>/compose.yaml` and `env` only. |
| `logs [-f] [service...]` | Compose logs. |
| `exec <service> -- <cmd...>` | Run inside a running container. |
| `init --project P [--file F ...]` | Write a starter `envctl.yaml`. |
| `hook claude` | Print Claude Code worktree hooks JSON. |
| `agent install [--global]` | Install this skill into `.claude/skills/` and `.agents/skills/`. |
| `agent snippet` | Print a CLAUDE.md / AGENTS.md paragraph. |

Exit code is 0 on success, 1 on failure with `envctl: <reason>` on stderr.

## `status --json` shape

```json
{
  "feature": "feat-x",
  "project": "mg-feat-x",
  "backend": "local",
  "running": true,
  "services": [{ "name": "postgres", "state": "running", "health": "healthy" }],
  "endpoints": [{ "service": "postgres", "target": 5432, "host": "127.0.0.1", "port": 41000 }],
  "env": ["ENVCTL_FEATURE=feat-x", "ENVCTL_PROJECT=mg-feat-x", "ENVCTL_BACKEND=local",
          "ENVCTL_PORT_MODE=registry", "ENVCTL_HOST_POSTGRES=127.0.0.1", "ENVCTL_PORT_POSTGRES_5432=41000"],
  "rendered": "/abs/path/.envctl/feat-x/compose.yaml"
}
```

In domains mode (OrbStack) `endpoints[].host` is `service.project.orb.local`,
`port` equals `target`, and no `ENVCTL_PORT_*` lines exist.

## Feature and project names

Branch → slug: lowercase, non `[a-z0-9]` runs become `-`, trimmed, max 40
chars. `feat/Imported Pricing` → `feat-imported-pricing`. Detached HEAD uses
the worktree directory name. Project name is `<prefix>-<slug>`, e.g.
`mg-feat-imported-pricing`. Containers are `<project>-<service>-1`, volumes
`<project>_<volume>`, network `<project>_default`.

## Manifest `envctl.yaml` (repo root)

```yaml
version: 1
project: mg                     # ^[a-z][a-z0-9-]{0,15}$
stack:
  files: [deploy/local/docker-compose.yml]
  env_files: [".env"]           # optional, missing files ignored
  profiles: []
ports:
  mode: auto                    # auto | domains | registry
  range: [41000, 49999]
expose:
  - { service: api, port: 8787, scheme: http, path: / }
```

Create with `envctl init --project mg --file deploy/local/docker-compose.yml`.
Add `.envctl/` to `.gitignore`. The manifest is looked up at the worktree root
only, never in parent directories.

## Port modes

| Mode | Behaviour |
| --- | --- |
| `domains` | No host ports published; reach `service.project.orb.local:<container port>`. OrbStack only. |
| `registry` | Each published port replaced by a stable port from `~/.config/envctl/ports.json`, bound to 127.0.0.1. |
| `auto` | `domains` on OrbStack, else `registry`. |

## Labels on every container

`dev.envctl.feature`, `dev.envctl.backend`, `dev.envctl.project`.
`docker ps --filter label=dev.envctl.project=<project>` finds an environment
without envctl.

## Claude Code hooks

`envctl hook claude` prints hooks for `WorktreeCreate` (creates the worktree,
runs `envctl up --no-wait`, prints the path) and `WorktreeRemove` (runs
`envctl down --volumes`). Scripts `envctl-worktree-create` and
`envctl-worktree-remove` must be on PATH and need `jq`.

## GitHub Actions

| Piece | Use |
| --- | --- |
| `uses: sam-bretz/envctl@main` (composite action) | Installs the binary; inputs `version` (default `latest`) and `token`. |
| `uses: sam-bretz/envctl/.github/workflows/validate.yml@main` | Renders `--feature ci` on pull requests and checks it with `docker compose config`. Inputs `version`, `working-directory`, `port-mode`; secret `token`. |
| `release.yml` in the envctl repo | Builds binaries on `v*` tags; archives include the hook scripts and this skill. |

While the envctl repository is private, consumers need a token with read
access to it stored as `ENVCTL_TOKEN`.

# envctl

Isolated docker-compose environments per feature branch, for humans and coding
agents working in parallel on one machine, with a per-branch VM backend planned
for preview environments.

Every git worktree gets its own compose project (`<prefix>-<branch-slug>`), its
own network and volumes, and no host port collisions. On OrbStack services are
reached by DNS (`postgres.mg-feat-x.orb.local`); elsewhere stable host ports are
allocated from a local registry.

## Status

Milestone 1 (local backend) is usable: `up`, `down`, `stop`, `start`, `status`,
`render`, `logs`, `exec`, `list`, `init`, and Claude Code worktree hooks.
Datasets (milestone 2), the EC2 per-branch backend and GitHub Actions
workflows (milestone 3+) are not built yet. See `internal/provider` for the
interface those backends implement.

## Docs

Full documentation lives in `docs/` (Astro Starlight; `cd docs && npm install && npm run dev`).
Agents can read `docs/public/agent-guide.md` for a one-page summary.

## Install

    go install github.com/sam-bretz/envctl/cmd/envctl@latest
    # hooks (optional): put hooks/claude/* on PATH

## Use

In an application repository, add `envctl.yaml` at the root (see
`examples/envctl.yaml`, or run `envctl init --project mg --file deploy/local/docker-compose.yml`).
Add `.envctl/` to `.gitignore`.

    envctl up            # start this branch's stack, wait for healthchecks
    envctl status        # services, endpoints, env file to source
    envctl list          # every environment of this project on the host
    envctl down -v       # remove containers, data and port allocations

Host tooling reads `.envctl/<feature>/env`:

    set -a; . .envctl/$(git branch --show-current | tr '/' '-')/env; set +a
    psql -h $ENVCTL_HOST_POSTGRES -p ${ENVCTL_PORT_POSTGRES_5432:-5432}

## How it isolates

`envctl` loads the compose files with compose-go, then rewrites the project:

- project name from the branch, so containers, networks and volumes are namespaced
- host port bindings stripped (domains mode) or replaced with registry ports
  (registry mode), so two environments never fight for a port
- `container_name` prefixed with the project when present
- `dev.envctl.*` labels on every service, so environments are discoverable
  from Docker alone; there is no server and no database

The rendered file is written to `.envctl/<feature>/compose.yaml` and run with
plain `docker compose`, so anything you can do with compose still works.

## Claude Code

    envctl hook claude >> .claude/settings.json   # merge by hand if hooks exist

`WorktreeCreate` creates the git worktree and runs `envctl up` in it;
`WorktreeRemove` runs `envctl down --volumes`. Both scripts need `jq`.

## Layout

    cmd/envctl              CLI
    internal/manifest       envctl.yaml contract
    internal/feature        branch -> slug -> project name
    internal/compose        compose-go render and port rewriting
    internal/ports          host port registry
    internal/dockerx        docker CLI wrapper
    internal/provider       backend interface
    internal/backend/local  local Docker host backend
    hooks/claude            worktree hooks

# envctl

Isolated docker-compose environments per feature branch, with an experimental
Bubble Tea workflow runtime for supervisor/worker agents in dedicated local VMs.

Every git worktree gets its own compose project (`<prefix>-<branch-slug>`), its
own network and volumes, and no host port collisions. On OrbStack services are
reached by DNS (`postgres.mg-feat-x.orb.local`); elsewhere stable host ports are
allocated from a local registry.

## Status

The existing local backend is usable: `up`, `down`, `stop`, `start`, `status`,
`render`, `logs`, `exec`, `list`, `init`, `agent install`, Claude Code worktree
hooks, a reusable PR validation workflow, and a release workflow.
Named environment identity and branch linking are also implemented. Datasets,
full VM workflows, invocation plugins, and agent orchestration are not yet
ready for use. The new direction is a checkpointed workflow with
supervisor/worker agents and a dedicated VM per executing revision. Plan must
resolve and verify downstream harness connections and capabilities before work
advances.

Local workflow implementation is in progress. The run state API, coordinator,
Bubble Tea shell, readiness rules, and initial provider/plugin libraries are now
present. The daemon is connected to an experimental local execution backend for
pinned guest repositories, Compose, and Codex worker/supervisor jobs. A real
two-repository fixture passed all six stages, independent QA, coordinator
restart, explicit fixture approval, and verified GitHub PR publication.
Invocation-scoped executable plugins can now be attached through CLI or TUI;
their packages are frozen and their guest probes gate Plan. PostgreSQL seeding,
checkpoint capture, and predecessor restoration now have real guest evidence.
The bundled Playwright plugin has passed real Chromium interaction, assertion,
screenshot and reconnect checks. The HTTP record emulator has passed seed,
capture, restore, corrupt-target recovery and separate-stack isolation checks.
See the [experimental fixtures guide](docs/src/content/docs/workflow-fixtures.mdx).
Checkpoint source diffs now use retained Git bundles through `envctl run diff`
and the Bubble Tea Changes view, including comparisons across historical
revisions after source checkouts and VMs are gone. Plugin process failures have
bounded recovery with durable evidence and stable external operation IDs.
A dedicated-VM active rewind has passed historical Code drain, coordinator
restart, two-client review, and source/Compose/dataset restoration through QA.
Plan now records structured capability discoveries and holds admission until
real probes pass. Failed plugin probes can request durable resource repair;
Chromium resource deletion and recovery have real guest acceptance evidence.
`envctl mcp serve` exposes local agent commands with optional run scoping and
read-only access, preserving daemon operation replay and revision checks.
Running attempts now report bounded, redacted live progress (phase, current
check, recent agent activity) to `envctl run show`, Bubble Tea and MCP. User
messages reach a running worker or supervisor by interrupting its guest job and
resuming the same harness session; a real Claude worker was steered mid-command
this way. Each message shows whether it was included at start, delivered live,
pending, or queued for the next attempt.
Complete database metadata replay, remaining restore cases, the full
parallel-agent demonstration and a second harness are still incomplete.
Source joins now require declared merge ownership and checks. The coordinator
verifies every incoming SHA against the retained output bundle; dataset joins
select a declared predecessor. Real guest conflict/replay acceptance passes.
Parallel workflows now reserve a dedicated child VM per executing node, with
independent readiness, Compose/data/plugin state and retry history. VM limits
include the parent and draining children. Two real child VMs passed source,
database, replay and teardown isolation checks; both were removed afterward.
Local checkpoints now retain nested submodule bundles and LFS bytes, including
newly created content, and restore them without the original sources. Real guest
and process-death tests cover that path. The publication broker now uploads a
changed repository's LFS objects before its branch and publishes new submodule
commits first, as create-only branches with draft PRs in the submodule
repositories that the parent PR lists. That path is verified with local Git and
file-based LFS remotes, not yet against real GitHub LFS. This remains experimental; no complete workflow milestone is yet
claimed delivered. See the
[implementation evidence and remaining work](docs/implementation-progress.md).

See the [implementation plan](docs/src/content/docs/design/workflow-runtime.mdx)
and [milestones](docs/src/content/docs/roadmap.mdx). The existing environment
lifecycle interface lives in `internal/provider`.

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

## Agents

    envctl agent install      # skill into .claude/skills and .agents/skills (Claude Code, Codex, ...)
    envctl agent snippet      # paragraph for CLAUDE.md / AGENTS.md

The skill source is `skills/envctl/`; `npx skills add sam-bretz/envctl` also works.

## CI

    uses: sam-bretz/envctl@main                                   # composite action: install the binary
    uses: sam-bretz/envctl/.github/workflows/validate.yml@main    # render + compose config on every PR

Tag `v*` to build release binaries. See `docs/src/content/docs/ci.mdx`.

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
    skills/envctl           agent skill (SKILL.md + REFERENCE.md), embedded in the binary
    action.yml              composite action: install envctl in a job
    .github/workflows       ci (this repo), validate (reusable), release (tags)

# stacks

**A fast, single-binary manager for a fleet of Docker Compose stacks — written in Go, talking straight to the Docker Engine API.**

This is the **v3.0 Go rewrite**: the original was ~20k lines of Bash + Python shelling out to the `docker` CLI. It's now a single compiled Go binary that uses the **Docker Engine API** directly (with a CLI fallback), so it's dramatically faster and has no Python runtime to babysit.

> Companion project: **[`stackd`](https://github.com/crunkazcanbe/stackd)** — a tiny boot orchestrator + watchdog that *drives* `stacks` to bring up exactly the services you want at boot and keep them alive.

## Highlights

- **Engine-API native** — talks to `/var/run/docker.sock` directly; falls back to the CLI when needed.
- **Per-stack, per-service control** — `stacks up net_1 pihole` works as well as `stacks up net_1`.
- **Only-if-necessary by default** — `up`/`repair` are non-destructive; `recreate` force-rebuilds; `fix` is the deep pass. Escalate only when something's actually wrong.
- **Traefik dynamics generator** — emits one dynamic config per stack from your compose labels (routers, services, entrypoints, DB TCP routers, subdomain harvest).
- **Sablier-style on-demand** — services can be staged created-but-stopped and woken on first request.
- **Image version history + rollback** — per-image SQLite history (keeps the last N); roll back from the menu.
- **Disk reclaim** — tiered, on-demand-safe pruning of unused images/layers.
- **Self-healing & dedupe** — reconcile networks/IPs, strip duplicate service definitions, clean orphaned volume/network declarations.
- **12-tab terminal UI** — `stacks menu` (Bubble Tea): Containers, Stacks, Networks, Settings, Updates, Images, and more, with search and inline actions.

## Install

```sh
go build -o stacks .
sudo cp stacks /usr/local/bin/
stacks version
```

Requires Docker (Engine API at the default socket). Go 1.25+ to build.

## Layout

- **Stacks** live as individual Compose files in your stacks directory (default `/home/bellzserver/MyDocker/Stacks`, override with `$STACKS_DIR`) — e.g. `net_1.yml`, `db_2.yml`.
- **Config** lives in `~/.config/stacks/stacks.yaml` (resolved via `$STACKS_CONFIG_DIR` → invoking user's home under sudo → `$XDG_CONFIG_HOME` → `~/.config`). It holds `stack_order`, the always-on `boot_stacks`, and feature toggles.

## Commands (selection)

```
stacks status                 # banner + container/stack overview
stacks ls                     # list all stacks
stacks menu                   # the full terminal UI

# lifecycle (per stack, optionally per service, with modifiers)
stacks up <stack> [service] [repair|recreate|fix|dynamics]
stacks down|start|stop|restart|recreate <stack> [service]
stacks heal | fixall          # do-everything pass

# images & disk
stacks pull <stack>           # pull images
stacks images                 # image version history / rollback
stacks reclaim                # tiered disk reclaim (on-demand-safe)
stacks clean [-a]             # prune

# routing & topology
stacks dynamics               # (re)generate Traefik dynamic configs
stacks network | volume       # inspect/manage
stacks dedupe | netdedupe     # strip duplicate service / network decls
stacks purge service|stack …  # remove + clean its orphaned net/volume decls

# misc
stacks logs|inspect|kill|rm <stack> [service]
stacks scale | proxy | snapshot | backup
stacks search <term>          # image registry search
stacks update | upgrade       # update history / self-update
```

Run `stacks` with no arguments for usage.

## Design

- **One file per command** — `status.go`, `up.go`, `fix.go`, `dynamics`, etc. — so the codebase stays easy to navigate.
- **`dispatch.go`** parses `<command> <stack> [service] [modifiers…]` the same way the original Bash did, then routes to the lifecycle handlers.
- **API-first with CLI fallback** keeps it fast on the happy path and robust everywhere else.

## License

MIT

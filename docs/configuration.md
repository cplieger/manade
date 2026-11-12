# Configuration

This page lists every setting manade reads, in the environment, the config file and each profile. It is for anyone who writes or changes the pool's config.

manade reads both once at start, so restart the container after a change. Busy runners keep running through a restart, and the next start adopts them. An invalid value never falls back to a default. The start stops with one error that names every field to fix, by its path in the file, such as `profiles.build.memory`.

## Environment

| Variable | Description | Default |
| --- | --- | --- |
| `GITHUB_APP_CLIENT_ID` | The GitHub App's client ID | required, or `GITHUB_TOKEN` |
| `GITHUB_APP_PRIVATE_KEY` | The App's private key in PEM form. `GITHUB_APP_PRIVATE_KEY_FILE` reads it from a file | required, or `GITHUB_TOKEN` |
| `GITHUB_TOKEN` | A personal access token in place of the App, the only form an enterprise accepts. `GITHUB_TOKEN_FILE` reads it from a file | _(unset)_ |
| `CONFIG_FILE` | The config file's path | `/config/config.yaml` |
| `DOCKER_SOCKET` | The Docker socket's path, the same on the host and in the container | `/var/run/docker.sock` |
| `POOL_NAME` | Names the pool's labels and networks, so two pools can share one Docker host | `manade` |
| `METRICS_LISTEN_ADDR` | Opens `/metrics` and `/healthz` on this address, such as `:9464` | _(unset)_ |
| `GITHUB_PROXY` | An `http` or `https` proxy for GitHub traffic | _(unset)_ |
| `DIAG_DIR` | A writable folder where each runner's own logs are kept | _(unset)_ |
| `DIAG_RETENTION` | How long the files in `DIAG_DIR` are kept | `168h` |
| `LOG_LEVEL` | `debug`, `info`, `warn` or `error` | `info` |

Set either the two App settings or `GITHUB_TOKEN`, never both. [Connecting to GitHub](github.md) shows how to create each one.

- `DOCKER_SOCKET` is an absolute path to a Unix socket. manade speaks no TCP to Docker. Mount the socket at the path it has on the host, because a profile with `docker: host-socket` binds that path into its jobs.
- `POOL_NAME` is 1 to 14 lowercase letters, digits or dashes, starting with a letter. Two pools on one Docker host need two names, and each pool must serve its own repositories. The log alert rules tell pools apart by it, so two pools whose logs reach one Loki need two names too.
- `METRICS_LISTEN_ADDR` is a `host:port` or `:port`. When the address cannot be bound, the start stops.
- `GITHUB_PROXY` takes a URL with a host and no path. A password in it is never logged.
- `DIAG_DIR` must be writable by uid 65532, or the start stops. manade saves each runner's `_diag` folder there as one tar file of at most 8 MiB before it removes the runner. Unset, the last 50 log lines of a failed runner are the only record.
- An unknown `LOG_LEVEL` logs at `info` with a warning.

## The config file

The file is YAML, at most 1 MiB, one document. An unknown key stops the start. Nothing in it expands: `${VAR}` stays as written, because no field needs a secret. [`config.example.yaml`](../config.example.yaml) is a working start.

| Key | Default | Description |
| --- | --- | --- |
| `capacity.cpus` | required | The CPUs all runners share |
| `capacity.memory` | required | The memory all runners share |
| `capacity.disk` | _(unset)_ | A disk budget that profiles with `disk` reserve from |
| `capacity.max_runners` | _(unset)_ | At most this many runners at once |
| `queue.head_reserve_after` | `2m` | How long the oldest job waits before smaller jobs stop passing it |
| `images.refresh` | `6h` | How often a tag is checked for a newer image, at least `10m` |
| `network.subnet` | `172.30.0.0/24` | The job network's subnet |
| `network.bridge_subnet` | `172.30.1.0/24` | The subnet of the network for `network: bridge` profiles |
| `network.dns` | _(unset)_ | IPv4 resolver addresses for every job, in place of Docker's |
| `scopes` | required | The repositories, organizations or enterprises to serve |
| `profiles` | required | The runner shapes a job picks by name in `runs-on` |

Value forms:

- A CPU count is a number with up to three decimals, such as `2` or `0.5`.
- A size is a whole number with a unit, `B`, `KiB`, `MiB`, `GiB` or `TiB`, such as `4GiB`. `0` alone means none, for `swap`.
- A duration is a number with a unit, `s`, `m` or `h`, such as `90s`, `10m` or `2h`.
- A subnet is a private IPv4 range from `/8` to `/28` inside `10.0.0.0/8`, `172.16.0.0/12` or `192.168.0.0/16`. The two subnets must not overlap. The firewall rules key on each network's interface, so a new subnet needs no rule change. The start refuses an existing network whose subnet differs, so stop manade and remove the network before you change its subnet: `docker network rm manade-isolated` for `network.subnet`, or `manade-bridge` for `network.bridge_subnet`, with your `POOL_NAME` in place of `manade`.
- A `network.dns` address is a unicast IPv4 address outside `127.0.0.0/8`, because a loopback address inside a job is the job's own container. [Keeping jobs off your network](firewall.md#dns) says when to set it.

### Scopes

Each entry names exactly one registration target.

| Key | Default | Description |
| --- | --- | --- |
| `repo` | _(unset)_ | A repository, as `owner/name` |
| `org` | _(unset)_ | An organization |
| `enterprise` | _(unset)_ | An enterprise's slug. Needs `GITHUB_TOKEN` |
| `runner_group` | `Default` | The runner group of an organization or enterprise scope |
| `installation_id` | _(unset)_ | The App installation's ID, which skips the lookup at start |

A personal account lists its repositories one by one, and they all share the pool. An organization or an enterprise registers once for all its repositories. A scope's name in a profile's `scopes` list is `owner/name` for a repository, the organization's name, or `enterprises/<slug>`.

### Profiles

`profiles` maps a name to a runner shape. The name is the scale set's name and its only `runs-on` label, so `runs-on: build` selects the `build` profile and nothing else. A name is 1 to 32 characters from lowercase letters, digits, `.`, `_` and `-`. Each profile becomes one scale set in every scope it may use.

| Key | Default | Description |
| --- | --- | --- |
| `runner_image` | `base` | A shipped image: `base`, `build`, `browser` or `docker` |
| `image` | _(unset)_ | Any image that runs GitHub's runner from its one-time config, by tag or digest |
| `command` | `["/home/runner/run.sh"]` | The command that starts the runner, replacing the image's entrypoint |
| `user` | the image's | A numeric non-root `uid:gid`, for an image whose own user is root |
| `cpus` | `2` | The CPU limit, also reserved from the budget |
| `memory` | `4GiB` | The memory limit, also reserved from the budget, at least `6MiB` |
| `swap` | `0` | Swap on top of `memory` |
| `pids` | `4096` | The process limit |
| `disk` | _(unset)_ | The container's writable layer size, reserved from `capacity.disk` |
| `shm_size` | Docker's | The size of `/dev/shm` |
| `tmpfs` | _(unset)_ | In-memory mounts, each a `path` and a `size`, counted against `memory` |
| `max_lifetime` | `2h` | A runner older than this is removed, job or not |
| `idle_timeout` | `10m` | A runner that got no job in this time is removed |
| `max_runners` | _(unset)_ | At most this many of the profile's runners at once |
| `scopes` | every scope | The scopes that get this profile's scale set |

- A profile sets `runner_image` or `image`, never both. One that sets neither uses `base`. [How manade works](how-it-works.md#the-runner-images) lists what each shipped image holds.
- `command` defaults to `["/usr/local/bin/manade-dockerd-run"]` for `runner_image: docker`.
- A profile whose `cpus`, `memory` or `disk` can never fit the budget stops the start.
- `disk` works only where Docker enforces it, which is the `btrfs` or `zfs` storage driver, or `overlay2` on XFS mounted with `pquota`. The start refuses it on any other driver and on the containerd image store, which accepts the setting and ignores it. At start manade creates one probe container to prove the limit holds.
- The start logs one line naming every profile without `disk`, because their jobs can fill the filesystem that holds Docker's writable layers.
- A job that names a profile its repository may not use never reaches the pool, because that scope has no scale set for it.

### Extra access

Every field below is off by default. Each one you set is logged as a warning at start and shown by `manade_profile_elevated` on the metrics page. [Hardening](hardening.md#profiles-with-extra-access) explains what each one costs.

| Key | Default | Description |
| --- | --- | --- |
| `allow_privilege_escalation` | `false` | Lets `sudo` and other setuid programs raise privileges inside the container |
| `capabilities` | `[]` | Linux capabilities to add, such as `[NET_ADMIN, NET_RAW]`, or `docker-default` for Docker's own set |
| `allow_root` | `false` | Runs an image whose own user is root |
| `network` | `isolated` | `bridge` lets the profile's jobs reach each other, `host` gives them the host's network |
| `docker` | `none` | `host-socket` puts the host's Docker socket in the job, which is root on the host |
| `runtime` | `runc` | `runsc` for gVisor or `sysbox-runc` for Sysbox, when the host has them |
| `privileged` | `false` | Every device and capability, which is root on the host |
| `mounts` | _(unset)_ | Named volumes, or host folders under `bind`, each with a `target` and an optional `read_only` |

The start checks these rules and names the field that breaks one:

- A profile that sets any of these fields must list its `scopes`, so no grant reaches every repository by omission.
- `allow_privilege_escalation: true` needs `capabilities` that hold `SETUID` and `SETGID`, or `privileged: true`. Without both, `sudo` cannot switch user.
- `runner_image: docker` needs `allow_privilege_escalation: true`, and either `runtime: sysbox-runc` with `capabilities: docker-default` or `privileged: true`. It cannot use `docker: host-socket`.
- A `runtime` the Docker daemon lacks stops the start.
- A `bind` source may not be, sit under or contain `DOCKER_SOCKET`, Docker's data folder, the containerd socket Docker reports, `/var/lib/containerd`, `/run/containerd`, `/run/docker`, `/proc` or `/sys`, each also in its `/var/run` form. So `/`, `/var`, `/var/lib` and `/run` are refused too. The socket reaches a job only through `docker: host-socket`.
- Paths are checked as written, because manade cannot see the host's files. A host symlink that leads to one of those paths, or a containerd folder moved from its default, is yours to avoid.

These two profiles run Docker inside the job with the shipped `docker` image, under Sysbox and under `privileged`:

```yaml
profiles:
  docker-sysbox:
    runner_image: docker
    runtime: sysbox-runc
    allow_privilege_escalation: true
    capabilities: docker-default
    cpus: 4
    memory: 8GiB
    scopes: [your-account/your-repo]

  docker-privileged:
    runner_image: docker
    privileged: true
    allow_privilege_escalation: true
    cpus: 4
    memory: 12GiB
    tmpfs:
      - path: /var/lib/docker
        size: 4GiB
    scopes: [your-account/your-repo]
```

Under `privileged`, the job's own Docker keeps its layers on the container's writable layer. There it falls back to a slow store that copies every layer, which `docker info` names `native`, or `vfs` on the classic store. The `tmpfs` at `/var/lib/docker` lets it use `overlayfs` instead, `overlay2` on the classic store. The tmpfs size counts against `memory`.

# Hardening

This page covers what manade can reach, how to narrow it, what each field that grants a job more costs and which risks remain. It is for operators deciding how much to trust the pool and its jobs.

## What the pool holds

- The Docker socket. Whoever controls the Docker API is root on the host. A read-only mount of the socket does not change that. manade holds the socket to start containers, so treat the container as root on the host. It runs as the non-root uid 65532 and reaches the socket through the socket's group.
- The GitHub credential. The App's key, or the token, is read once at start, never logged and never passed to a job. For repository scopes it is administrator of every repository the App is installed on, with their settings, collaborators, branch protection and deletion. For an organization scope it manages the organization's self-hosted runners and runner groups. Install the App on the served repositories only, use it for nothing else, and keep the key file readable by uid 65532 alone.
- No inbound surface. manade polls GitHub, so it needs no webhook, no callback and no public address. It opens a port only when `METRICS_LISTEN_ADDR` is set.

## The metrics listener

`/metrics` and `/healthz` need no login, and the listener checks no `Host` header. The pages are read-only and carry counts, timestamps and the scope and profile names from your config. Keep the port on your own network, or put a proxy with a login in front of it.

## Hardened compose settings

manade writes only its health marker in `/tmp` and, when `DIAG_DIR` is set, the runners' diagnostics. It needs no capability. Add these lines to the service in `compose.yaml`:

```yaml
    read_only: true
    tmpfs:
      - "/tmp"
    cap_drop:
      - ALL
    security_opt:
      - "no-new-privileges:true"
```

With `DIAG_DIR` set, also mount a writable folder there that uid 65532 owns.

## What a job gets by default

Every container manade creates comes from a validated profile. No raw Docker option passes through. A default profile's job gets these settings:

- It runs as the image's own user, which must resolve to a non-root uid and gid in the image's own `/etc/passwd` and `/etc/group`. The shipped images run as `runner`, uid 1001.
- It drops every Linux capability and runs with `no-new-privileges`, so `sudo` and other setuid programs fail.
- It gets no Docker socket, no host folder, no volume and no device.
- It runs on the isolated job network, which the [firewall rules](firewall.md) keep off the host and the LAN.
- It has hard CPU, memory, swap and process limits, and its container log is capped at three files of 10 MB.
- Its environment holds only its one-time runner config. That config registers one runner for one job, and GitHub retires the runner after the job.

The job still has its own repository's `GITHUB_TOKEN` and secrets, as on any runner, and reaches the internet.

A workflow cannot change any of this. Its only choice is the profile name in `runs-on`, among the profiles whose `scopes` include its repository. Profiles live in the pool's config file, never in a repository, so pushing a branch cannot grant a job more. Anyone who can edit a workflow can pick any profile allowed to that repository, so a profile's `scopes` is its access list.

## Profiles with extra access

Each field below is off by default and needs explicit `scopes`. The start logs one warning per profile that sets any of them, naming what it grants.

| Field | What it grants | What it costs |
| --- | --- | --- |
| `allow_privilege_escalation` | `sudo` and setuid programs work | Root inside the container, bound by the capabilities it holds |
| `capabilities` | Linux capabilities, or Docker's default set | Each one widens what a kernel bug can reach |
| `allow_root` | An image whose own user is root | Root inside the container owns the image's files |
| `network: bridge` | The profile's jobs reach each other | One job can attack another job on the same profile |
| `network: host` | The host's network | The job reaches the LAN and every port on the host, and no firewall rule here applies |
| `docker: host-socket` | The host's Docker socket, with its group | Root on the host. For trusted repositories only |
| `runtime: runsc` | gVisor, if the host has it | Some programs behave differently under gVisor |
| `runtime: sysbox-runc` | Sysbox, if the host has it | Support that its project calls best effort |
| `privileged` | Every device and capability | Root on the host |
| `mounts` | Named volumes and host folders | A host folder is the host's data in the job |

Sysbox ignores Docker's capability settings unless its manager is set to honour them. So a Sysbox profile with `allow_privilege_escalation: true` gives root inside the job every capability within the container's user namespace, whatever `capabilities` lists. The start's warning for that profile says so.

For Docker inside a job, a profile on the `docker` image under Sysbox avoids both `privileged` and the host socket. [Configuration](configuration.md#extra-access) shows it.

## Image trust

A moving tag trusts its publisher at every pull. manade does not check image signatures itself. Every image of this repository is signed in GitHub Actions with cosign, and this checks one:

```sh
cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/cplieger/ci/\.github/workflows/docker-release\.yaml@' \
  --certificate-github-workflow-repository cplieger/manade \
  ghcr.io/cplieger/manade-runner:base >/dev/null
```

To make every image change a reviewed edit, use `image:` with a digest, such as `ghcr.io/cplieger/manade-runner:base@sha256:<digest>`, in place of `runner_image`. manade never checks a digest for updates, so you move it yourself. Move it within 30 days of each runner release, or GitHub stops sending jobs to it.

## What manade validates

- Every config field has an exact form or vocabulary. An unknown value stops the start, never falls back.
- Every answer from Docker is read through a size cap. An answer may hold 4 MiB, the container list 16 MiB, and one pull or event message 1 MiB. A log tail may hold 4 MiB, of which manade keeps the last 64 KiB. A runner's diagnostics are capped at 8 MiB. Every call has a deadline.
- manade removes only a container that carries its own pool's labels, and only an image it replaced itself that carries the `manade.runner-image` label and no tag. It cannot delete another stack's container.
- Job names, workflow references and runner output from GitHub or Docker are cleaned of control characters before they reach a log line, and never become a label or an argument.

## Disk

A profile without `disk` lets a job fill the filesystem that holds its writable layer. Under a classic storage driver, that is Docker's data folder, `/var/lib/docker` by default. Under the containerd image store, the default for new installs of Docker 29, it is containerd's own folder, `/var/lib/containerd` by default, which Docker's `data-root` does not move. There no profile can set `disk`. Put that folder, and Docker's data folder for volumes, on a filesystem or volume with its own quota, so a full disk stops jobs and nothing else. Run `docker info --format '{{.DriverStatus}}'` to see which store your Docker uses.

## Risks that remain

- A kernel escape. Jobs share the host's kernel. Use `runtime: runsc` where that matters.
- A job's own secrets. A job can read the secrets its workflow gives it, as on any runner.
- Internet access. Jobs reach the internet, to download packages and tools.
- The host firewall. Until both [firewall rules](firewall.md) are in place, a job can reach services on the host and the LAN.
- Disk. See the section above.

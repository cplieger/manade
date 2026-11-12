# manade

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/manade/badges/size.json)](https://github.com/cplieger/manade/pkgs/container/manade) [![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)](https://github.com/cplieger/manade/pkgs/container/manade) [![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)](https://github.com/cplieger/manade/blob/main/Dockerfile) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/manade/badges/mutation.json)](https://github.com/cplieger/manade/issues?q=label%3Agremlins-tracker) [![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/manade/releases)

<!-- hub-overview BEGIN -->
manade runs the GitHub Actions jobs of your private repositories on your own Docker host. Each job gets a fresh container that is removed when the job ends. It does not run jobs from public repositories.

## What it does

manade gives your private repositories runners that cost no Actions minutes and appear only while jobs wait.

- Starts a runner about a minute after a job is queued, and runs none while nothing waits.
- Shares one CPU and memory budget across all your repositories, oldest job first.
- Lets each job pick a runner size and image by name in `runs-on`.
- Gives jobs no root and no Docker socket unless a profile you write allows it.
- Pulls its four runner images and keeps them current on its own.

## Who it is for

manade is built for a personal account or an organization with private repositories and a Linux host to spare. It polls GitHub, so it needs no public address and no webhook.

You need a Linux host with Docker Engine 19.03 or later, a GitHub App you create for the pool, and root on the host to add two firewall rules.

Consider [Actions Runner Controller](https://github.com/actions/actions-runner-controller) if you run Kubernetes and want runners as pods that it scales.

manade is free software under the GPL-3.0-or-later license.
<!-- hub-overview END -->

## Quick start

The image is on GitHub Container Registry and Docker Hub, for `amd64` and `arm64`. This is the [`compose.yaml`](compose.yaml) in this repository.

```yaml
services:
  manade:
    image: ghcr.io/cplieger/manade:latest
    container_name: manade
    restart: unless-stopped
    # Shutdown closes the GitHub sessions and removes idle runners, which takes
    # up to 45 seconds. Docker's default of 10 seconds cuts it short.
    stop_grace_period: 60s
    # The Docker socket's group, so the non-root pool can start containers. Put
    # the output of "stat -c %g /var/run/docker.sock" in .env as DOCKER_GID.
    group_add:
      - "${DOCKER_GID:?set DOCKER_GID in .env}"
    environment:
      - GITHUB_APP_CLIENT_ID  # the GitHub App's client ID, from .env
      - "GITHUB_APP_PRIVATE_KEY_FILE=/config/github-app.pem"
    volumes:
      # Create ./config with config.yaml and github-app.pem in it. Then run
      # "sudo chown 65532:65532 config/github-app.pem" and "sudo chmod 600 config/github-app.pem".
      - "./config:/config:ro"
      # Mount the socket at the path it has on the host: DOCKER_SOCKET names both.
      - "/var/run/docker.sock:/var/run/docker.sock"
```

1. Create a GitHub App and install it on the repositories the pool serves, as [Connecting to GitHub](docs/github.md) shows.
2. In the folder that holds `compose.yaml`, run `mkdir config`. Save [`config.example.yaml`](config.example.yaml) as `config/config.yaml` and list your repositories under `scopes`.
3. Save the App's private key as `config/github-app.pem`, then run the two commands in the compose comment.
4. Create a file named `.env` in the same folder with two lines: `GITHUB_APP_CLIENT_ID=` followed by the App's client ID, and `DOCKER_GID=` followed by the output of `stat -c %g /var/run/docker.sock`.
5. Add the firewall rules in [Keeping jobs off your network](docs/firewall.md). Without them, a job can reach the host and your LAN.
6. Run `docker compose up -d`.
7. Set `runs-on: default` on a job in one of those repositories, and push.

Run `docker logs manade`. You should see `Runner started` within about a minute of the push. If you see `manade stopped on a failure` instead, its `error` field names every setting to fix.

## Configuration reference

manade reads its environment and `config/config.yaml` once at start, so restart the container after a change. An invalid value stops the start with one error that names every field to fix.

| Variable | Description | Default |
| --- | --- | --- |
| `GITHUB_APP_CLIENT_ID` | The GitHub App's client ID | required, or `GITHUB_TOKEN` |
| `GITHUB_APP_PRIVATE_KEY` | The App's private key in PEM form. `GITHUB_APP_PRIVATE_KEY_FILE` reads it from a file | required, or `GITHUB_TOKEN` |
| `GITHUB_TOKEN` | A personal access token in place of the App, the only form an enterprise accepts. `GITHUB_TOKEN_FILE` reads it from a file | _(unset)_ |
| `DOCKER_SOCKET` | The Docker socket's path, the same on the host and in the container | `/var/run/docker.sock` |
| `METRICS_LISTEN_ADDR` | Opens `/metrics` and `/healthz` on this address, such as `:9464` | _(unset)_ |
| `LOG_LEVEL` | `debug`, `info`, `warn` or `error` | `info` |

| Key | Default | Description |
| --- | --- | --- |
| `capacity.cpus` | required | The CPUs all runners share |
| `capacity.memory` | required | The memory all runners share |
| `scopes` | required | The repositories, organizations or enterprises to serve |
| `profiles` | required | The runner shapes a job picks by name in `runs-on` |
| `network.subnet` | `172.30.0.0/24` | The job network's subnet |

| Mount | Description |
| --- | --- |
| `/config` | The config file and the App's key |
| `/var/run/docker.sock` | The Docker socket |

manade opens no port unless `METRICS_LISTEN_ADDR` is set. Every other setting and every profile field is in [Configuration](docs/configuration.md).

## Security

manade holds the Docker socket, which is root on the host, so run it on a host you trust it with. The App's key is administrator of every repository it is installed on. Install the App on the served repositories only, and keep the key readable by uid 65532 alone. The metrics listener has no login, so keep its port on your own network.

A job runs as a non-root user with no capabilities, no privilege escalation and no Docker socket. A profile grants more only through fields you write, and each grant is logged at start. Docker filters no outgoing traffic, so a job reaches your LAN until you add the [firewall rules](docs/firewall.md). [Hardening](docs/hardening.md) covers the fields that grant more, image signatures and the risks that remain.

## Troubleshooting

The image has a Docker healthcheck. [Knowing the app is alive](docs/monitoring.md#knowing-the-app-is-alive) says what it proves and who is told.

- The container restarts in a loop with `manade stopped on a failure`. The `error` field names each setting to fix.
- The error names `DOCKER_SOCKET` and `permission denied`. `DOCKER_GID` in `.env` is not the socket's group.
- A job stays queued. Its `runs-on` must be exactly one profile's name, and that profile's `scopes`, when set, must list the repository.
- After a crash, the next start waits about 70 seconds before it serves. GitHub holds the old session until it lapses, and manade retries every 5 seconds.

## Monitoring

manade writes logfmt logs and can serve a Prometheus metrics page. Alert rules for Prometheus and for Loki ship in [`alerts/`](alerts/). Load one of the two files. The Grafana dashboard needs Grafana 13.2 or later. [Monitoring and alerts](docs/monitoring.md) lists the series and the rules and says who tells you when the pool stops.

## Documentation

- [Connecting to GitHub](docs/github.md) creates the GitHub App or the token.
- [Configuration](docs/configuration.md) lists every setting and profile field.
- [Keeping jobs off your network](docs/firewall.md) gives the firewall rules.
- [How manade works](docs/how-it-works.md) explains the queue, the runner images and restarts.
- [Hardening](docs/hardening.md) covers what the pool and its jobs can reach, image signatures and the remaining risks.
- [Monitoring and alerts](docs/monitoring.md) lists the logs, metrics, alerts and dashboard.

## Credits

manade registers runners through [actions/scaleset](https://github.com/actions/scaleset), GitHub's scale set client, and its runner images build on GitHub's [runner image](https://github.com/actions/runner). Its image user check is ported from [moby/sys](https://github.com/moby/sys). Pooling Docker runners across a personal account's repositories follows [gharp](https://github.com/muhac/actions-runner-pool).

## Contributing

Issues and pull requests are welcome. The [shared contributing rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) apply.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI assistance. The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE). The image carries the license text of every bundled component under `/usr/share/licenses/`. Third-party attributions are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

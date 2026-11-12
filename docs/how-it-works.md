# How manade works

This page explains how manade gets jobs from GitHub, decides which job runs next, runs it and keeps going through a restart. It is for operators who want to predict what the pool does.

## Scale sets and the queue

manade registers each profile as one runner scale set in each scope that may use it. A scale set is GitHub's group of runners behind one `runs-on` label. manade polls every scale set for new work, so nothing on GitHub needs to reach your host.

On github.com, GitHub hands a scale set queued jobs on its own, up to the number of runners the scale set says it can take. manade decides that number before every poll. That is how it keeps one queue for all your repositories:

- It reads each repository's queued jobs from GitHub's REST API, inside its queued, running and waiting workflow runs.
- It ranks every waiting job across all repository scopes by the time it was queued, oldest first.
- It funds jobs in that order from the CPU and memory budget, and tells each scale set to take as many jobs as it funded.
- When GitHub hands a scale set a job, manade starts one fresh runner container for it. GitHub gives each new runner the next job of its scale set.

A smaller job may pass a larger one that does not fit yet. Once the oldest job has waited `queue.head_reserve_after`, 2 minutes by default, no other job passes it until it fits. A job whose profile is at its own `max_runners` waits for that profile and holds nothing else back.

Budget left over once every waiting job that fits is funded gives one standing slot to as many scale sets as it covers. A job queued between two reads then starts within seconds.

## How long a job waits

A job on an idle pool waits up to about 65 seconds before its runner starts, plus the time one read of the queue takes. Each read starts 10 seconds after the previous one ends, or later when many runs are in flight, to stay inside GitHub's API limits. A job that lands on a standing slot starts within seconds. On a full pool, a job waits until running jobs end and free its share of the budget.

## Organization and enterprise scopes

manade cannot read the queued jobs of an organization or an enterprise. So their scale sets stand outside the queue's order and run only on standing slots, from the budget the repository queue leaves free. They get those slots before repository scale sets do, in turns. On a full pool their jobs wait until the repository jobs leave room, however long that takes, and no metric shows that wait. With room, a backlog on one such scale set starts about one job every 5 seconds. Within a scale set, GitHub picks the order.

## Jobs GitHub does not hand over

A queued job can match a profile and still never reach its scale set, for example when another runner with the same label takes it. When a funded slot goes unused through two long polls, manade sets that job aside for 10 minutes, then for twice as long after each further miss, and logs `GitHub did not assign a queued job`. `manade_jobs_unassignable` counts these jobs.

manade also leaves out a job it cannot run now, because its profile is not allowed in its repository, its scope is not served, or its image is missing. `manade_jobs_unfundable` counts them by reason, and each returns to its place once the cause clears.

## The runner images

A profile picks one of four images manade publishes in `ghcr.io/cplieger/manade-runner`, or names any image of its own. Each shipped image is GitHub's own runner image plus only what a job cannot install without root. Toolchains install at job time through `setup-node`, `setup-go`, `setup-python` and the like, exactly as on a GitHub-hosted runner, so each workflow pins its own versions.

| Image | Built on | Adds |
| --- | --- | --- |
| `base` | GitHub's runner image | `zstd`, `xz-utils`, `zip`, `git-lfs`, `openssh-client`, `gnupg`, `gettext-base` and `libatomic1` |
| `build` | `base` | `build-essential`, `pkg-config`, `libssl-dev` and `zlib1g-dev`, for cgo, `go test -race` and native modules |
| `browser` | `build` | The system libraries, fonts and `xvfb` that Playwright's Chromium, Firefox and WebKit need. The browsers download at job time |
| `docker` | `build` | A start script that runs a Docker daemon inside the job, plus `iptables` and `procps` |

JavaScript and composite actions work on every profile. Container actions, service containers and a job-level `container:` need a Docker daemon, so they need a profile on the `docker` image or one with `docker: host-socket`. Under the default profile `sudo` fails, because the job cannot raise its privileges.

### Which tag a profile runs

`runner_image` follows the channel of the manade you run. A stable manade runs the moving tag of each image, such as `base`. A development build runs `base-dev`. Each release also publishes fixed tags, such as `base-v1.2.0`, for an `image:` reference you want to pin.

### Pulls and refresh

At start, manade pulls every profile's image that Docker does not have yet. A profile whose image cannot be pulled gets no jobs, and the pull is retried from 30 seconds up to every 10 minutes. A tag Docker already has serves jobs at once and is checked for a newer image in the background right after the start.

A tag is checked for a newer image when a runner starts and the last check is older than `images.refresh`, 6 hours by default. The check runs in the background, so the runner starts at once from the image already checked. A new image is used from the next start, after manade checks that its user is not root. Running jobs are never touched. An image referenced by digest is never checked again.

When a newer shipped image replaces an older one, manade removes the older one once no container uses it. It never removes an image you named in `image:`, and it never prunes. An image replaced before a restart stays in Docker's cache, and this command clears it:

```sh
docker image prune --filter label=manade.runner-image
```

### GitHub's version rules

GitHub enforces these on github.com. A runner must be version 2.329.0 or later to register. GitHub stops sending jobs to a runner that has not updated within 30 days of a release. A critical security release stops jobs to older runners at once. The shipped images follow GitHub's runner releases, and `ManadeImageStale` warns when a profile's image has gone 72 hours without a good check, long before the 30 days run out.

## Restarts and shutdown

manade keeps no database. Its state is the labels on the containers it creates and what GitHub reports about each scale set, so a restart rebuilds everything from the two.

- On `SIGTERM`, it tells every scale set to take no more jobs, closes their sessions and removes idle runners. Busy runners keep running and finish their jobs. It gives up after 45 seconds, which is why the compose file sets a 60-second stop grace.
- On start, it adopts the runners still running from before. A busy one keeps its share of the budget until it ends. An idle one is removed.
- Jobs GitHub handed over while manade was down get runners as soon as each scale set's session opens, as far as the budget allows.
- After a crash, GitHub holds the old session for about 70 seconds. manade retries every 5 seconds and serves once it lapses.

Only one manade may serve a set of scale sets at a time. Run it with a fixed `container_name`, as the compose file does, so a redeploy stops the old container before the new one starts. A second process on the same scopes stops serving them and logs `Another process holds the scale set's session`.

## Scale sets manade does not name

manade creates a missing scale set and updates one whose labels differ. It never deletes one, because deleting a scale set strands its queued jobs. A scale set the config no longer names is left in place and logged as `A scale set the config does not name is left in place`, and `manade_scale_sets_orphaned` counts it. In an organization's runner group, that includes the scale sets another tool registered there. Delete one by hand once no workflow uses it, from the repository's or the organization's **Settings**, then **Actions**, then **Runners**.

## What manade needs from Docker

- Docker Engine 19.03 or later, which speaks Docker's API version 1.40. manade uses the highest version both sides support.
- A kernel that enforces memory, swap, process and CPU limits. Docker drops a limit the kernel cannot apply and starts the container anyway, so manade refuses to start on such a host and names the missing limit. It also treats any warning Docker gives at container creation as a failed start.
- Every runtime a profile names, such as `sysbox-runc`.

# Monitoring and alerts

This page lists what manade logs, the metrics it can serve, the alert rules and the dashboard that ship with it, and how you learn that the pool stopped. It is for operators who run Prometheus, Loki, Grafana or only Docker.

manade can serve a metrics page and ships metric alert rules. If you run Loki and no Prometheus, a log rule file gives you the same alerts, read from the log lines the pool writes about every 2.5 minutes.

## What you get

| You run | You get | You set up |
| --- | --- | --- |
| Prometheus, Mimir or VictoriaMetrics, with Alertmanager (the monitoring guide's starting stack adds Loki) | Eleven alerts, `up`, the dashboard | `METRICS_LISTEN_ADDR`, a scrape job named `manade`, `alerts/promql.yaml` |
| Loki and Alertmanager, no metric store | The same eleven alerts, each for the whole pool | `alerts/logql.yaml`, `LOG_LEVEL=info` |
| Grafana-managed alerting only | The rules imported into Grafana | A data source, the rules of either file |
| Docker or Unraid only | Docker health | An uptime prober on `/healthz`, or something that watches Docker health |

## Metrics

Set `METRICS_LISTEN_ADDR`, such as `:9464`, to serve `/metrics` in the Prometheus text format. Unset, manade opens no port. The page has no login and the listener checks no `Host` header, so keep the port on your network. [Reaching an app's metrics page](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#reaching-an-apps-metrics-page) shows how to scrape it.

The `scope` and `profile` labels carry the names from your config: `owner/name`, an organization's name or `enterprises/<slug>`, and your profile names. No job, run or runner name ever becomes a label. Every series is registered at start, and every labelled counter starts at 0, so `increase()` sees the first event after a restart. The page also carries the Go runtime and process families (`go_*`, `process_*`) under the names Go's official Prometheus client uses.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `manade_capacity_cpus` | gauge | | The CPU budget |
| `manade_capacity_memory_bytes` | gauge | | The memory budget |
| `manade_capacity_disk_bytes` | gauge | | The disk budget, only when `capacity.disk` is set |
| `manade_reserved_cpus` | gauge | | CPUs that live runners reserve |
| `manade_reserved_memory_bytes` | gauge | | Memory that live runners reserve |
| `manade_reserved_disk_bytes` | gauge | | Disk that live runners reserve, only when `capacity.disk` is set |
| `manade_runners` | gauge | `scope`, `profile`, `state` | Runners now: `starting`, `idle` or `busy` |
| `manade_jobs_queued` | gauge | `scope`, `profile` | Jobs waiting their turn in the queue |
| `manade_jobs_assigned_unstarted` | gauge | `scope`, `profile` | Jobs GitHub handed the scale set that no runner has taken yet |
| `manade_jobs_unassignable` | gauge | `scope`, `profile` | Waiting jobs set aside because GitHub left their funded slot unused for two polls |
| `manade_jobs_unfundable` | gauge | `scope`, `profile`, `reason` | Waiting jobs the pool cannot run now: `not_allowed`, `not_serving` or `image_missing` |
| `manade_queue_head_wait_seconds` | gauge | | How long the oldest repository job in the queue has waited |
| `manade_rest_reads_total` | counter | `result` | Queued-job reads: `changed`, `unchanged` or `error`. An `unchanged` read cost no API quota |
| `manade_queue_read_last_success_timestamp_seconds` | gauge | `scope` | When the last complete read of a repository's queued jobs ended |
| `manade_runner_starts_total` | counter | `scope`, `profile`, `result` | Runner starts: `ok` or `error` |
| `manade_runner_exits_total` | counter | `scope`, `profile`, `reason` | How runners ended: `completed`, `failed`, `oom`, `lifetime`, `idle` or `lost` |
| `manade_job_queue_seconds` | histogram | `profile` | Time from a job's queueing to its runner taking it |
| `manade_job_duration_seconds` | histogram | `profile`, `result` | Time a job ran: `succeeded`, `failed`, `canceled` or `other` |
| `manade_poll_last_success_timestamp_seconds` | gauge | `scope`, `profile` | When each scale set's last good poll ended |
| `manade_reconcile_last_success_timestamp_seconds` | gauge | | When the last good reconciler pass ended |
| `manade_booting` | gauge | | `1` from the start until the pool has finished starting, then `0` |
| `manade_scope_serving` | gauge | `scope` | `1` while the scope is served, `0` while it is stopped |
| `manade_scale_sets_orphaned` | gauge | `scope` | Scale sets in the scope's runner group that the config does not name, as of the start |
| `manade_profile_elevated` | gauge | `profile`, `grant` | `1` for each extra-access field a profile sets |
| `manade_image_checked_timestamp_seconds` | gauge | `profile` | When the profile's image last had a good check, `0` until the first, which runs right after the start |
| `manade_github_errors_total` | counter | `op` | GitHub failures: `session`, `poll`, `jit`, `scaleset`, `visibility`, `installation` or `queue` |
| `manade_docker_errors_total` | counter | `op` | Docker failures: `pull`, `create`, `start`, `inspect`, `remove`, `list`, `events` or `archive` |

The poll, queue read and reconciler timestamps start at the time manade started, so a loop that never succeeds reads as stale from the start.

Before it serves any job, manade pulls every runner image it lacks, then reads GitHub. A first start on a slow link can take many minutes. While it lasts, `manade_booting` reads `1` and every scope reads stopped. A start that fails exits, and Docker starts the container again. So the rules read the latest `manade_booting` of the last 10 minutes, which outlasts a restart. `ManadePollStalled`, `ManadeScopeNotServing`, `ManadeQueueReadsStale`, `ManadeReconcileStalled` and `ManadeImageStale` wait while it reads `1`. `ManadeBootStalled` fires once it has read `1` for 30 minutes, whether one start never ends or every start fails.

## Logs

manade writes logfmt lines to standard error, in UTC, at `info` by default. It logs one line per runner and job event, such as `Runner started`, `Job started`, `Job finished` and `Container exited`, with the scope, the profile and the runner. A runner that exits with an error also logs the last 50 lines of its own output. Names that come from GitHub or Docker are cleaned of control characters first.

The reconciler writes the `pass record` line at `info` on every fifth pass. That is about every 2.5 minutes, and more often while runners exit, because exits trigger extra passes. Each start writes `manade is starting` as it begins and `manade finished starting` at its end. In between, it writes the boot record `manade is still starting` every 2.5 minutes. All three are at `info`, and the last two carry `boot_seconds`, the seconds since the start. These four lines also carry `pool`, the pool's `POOL_NAME`. The log rules read only these lines, so they need `LOG_LEVEL=info`.

A start that fails writes no end line. The log `ManadeBootStalled` reads the latest of the three boot lines in the last 10 minutes, and fires once it has not been the end line for 30 minutes.

The pass record's fields are numbers:

| Field | Meaning |
| --- | --- |
| `poll_age_seconds` | Age of the oldest scale set's last good poll |
| `scopes_not_serving` | Scopes the pool stopped serving |
| `starts` | Good runner starts since the last record |
| `start_errors` | Failed runner starts since the last record |
| `assigned_unstarted` | Jobs GitHub handed over that no runner has taken, summed over every scale set |
| `head_wait_seconds` | How long the oldest repository job has waited |
| `queue_read_age_seconds` | Age of the oldest repository's last complete queue read |
| `oom_exits` | Runners the kernel killed at their memory limit since the last record |
| `reconcile_age_seconds` | Age of the last good reconciler pass |
| `image_check_age_seconds` | Age of the oldest profile image's last good check |

## Alerting

Load [`alerts/promql.yaml`](../alerts/promql.yaml) into Prometheus, the Mimir ruler or vmalert, as [Loading metric alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-metric-alert-rules) shows. If you run Loki and no metric store, load [`alerts/logql.yaml`](../alerts/logql.yaml) into Loki's ruler instead, as [Loading an app's alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-an-apps-alert-rules) shows. Load one of the two files, not both, because they hold the same alerts under the same names.

| Alert | Fires when | Severity |
| --- | --- | --- |
| `ManadeDown` | The scrape fails or finds no target for 5 minutes, or the log shows no pass record or boot record for 10 minutes | critical |
| `ManadeBootStalled` | manade has not finished a start in 30 minutes, in one start or in starts that keep failing, so it serves no job | critical |
| `ManadePollStalled` | A scale set's last good poll is over 5 minutes old for 5 minutes, so GitHub hands it no jobs | critical |
| `ManadeScopeNotServing` | The pool has stopped serving a scope for 10 minutes | critical |
| `ManadeRunnerStartsFailing` | A scale set's runner starts failed, and none succeeded, in the last 15 minutes | critical |
| `ManadeAssignedNotStarted` | Jobs GitHub handed a scale set have had no runner for 10 minutes | critical |
| `ManadeQueueBackedUp` | The oldest repository job in the queue has waited over 30 minutes | warning |
| `ManadeQueueReadsStale` | A repository's last complete queue read is over 5 minutes old for 10 minutes | warning |
| `ManadeRunnerOOMKilled` | A runner hit its memory limit in the last hour | warning |
| `ManadeReconcileStalled` | The reconciler has not finished a good pass in 5 minutes | warning |
| `ManadeImageStale` | A profile's image has had no good check in 72 hours, or none since the start, for 15 minutes | warning |

The metric rules name the scope or the profile at fault. The log rules group the lines by their `pool` field, and the pass record sums the whole pool, so each log alert names its pool and covers every scope at once. When one fires, the pool's warning and error lines name the scope or profile.

The log `ManadeDown` also waits while a pool has written one or two `manade is starting` lines in the last 10 minutes. That covers a first start until its first boot record, and also one failed start that Docker retries or a redeploy. A third start in that time is a loop, and it fires.

A pool that stops stays paged by name in the log `ManadeDown` for a day. After that it is out of the rule's reach, and the rule names the selector instead, while no pool writes. So when you remove a pool or change its `POOL_NAME`, silence `ManadeDown` for the old name in Alertmanager for a day.

`ManadeDown` is the only rule that reads the scrape's `job` label, through the `up` series. Name your scrape job `manade`, or change its two `job="manade"` matchers to your job's name exactly. Thresholds, `for:` windows and the `severity` labels are starting points.

In the log rules, the `container` selector is the label your log collector sets. Change it to any selector that finds manade's lines, such as `{service_name="manade"}`, or add matchers to it. The rules group by the lines' `pool` field, never by the selector's labels. So give each pool the selector finds its own `POOL_NAME`, and give no log stream a `pool` label of its own.

### When CI stops

The critical alerts mean CI on the pool has stopped. Until you fix the pool, move the affected workflows back to GitHub-hosted runners and rerun their jobs. A job whose runner comes from a repository variable moves with one setting. With `runs-on: ${{ vars.CI_RUNNER || 'ubuntu-latest' }}`, delete the `CI_RUNNER` variable and the job runs on `ubuntu-latest`.

## Dashboard

[`grafana-dashboard.json`](../grafana-dashboard.json) imports as the manade dashboard. It needs Grafana 13.2 or later and a Prometheus data source, and reads only `/metrics`. [Importing an app's dashboard](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#importing-an-apps-dashboard) shows how to load it. Pick your data source in **Data source** in the dashboard controls menu, then save the dashboard. It shows whether the pool keeps up with CI, and if not, why. It has three tabs:

1. Overview shows a Needs attention row while an alert fires, the budget in use, the jobs waiting and the runners by profile and state.
2. Jobs shows the queue wait and the job duration per profile, and the jobs finished by result.
3. Pool health shows starts and exits by reason, OOM kills, and GitHub and Docker errors. It also shows the age of each poll, queue read and image check, the jobs set aside, the profiles with extra access and the orphaned scale sets.

Needs attention reads Prometheus's `ALERTS` series, so its tiles show only once `alerts/promql.yaml` is loaded into the ruler that writes that series. The dashboard file is versioned with the app, so use the copy from the tag of the image you run.

## Knowing the app is alive

| Route | Method | Port | Login | Answers |
| --- | --- | --- | --- | --- |
| `/healthz` | `GET`, `HEAD` | `METRICS_LISTEN_ADDR`, unset by default | none, and no `Host` check | `200 {"status":"ok"}`, or `503 {"status":"unhealthy","reasons":[...]}` |
| `/metrics` | `GET` | the same port | none, so keep the port on your network | Prometheus text format |

**What healthy means.** The pool has finished its start and opened every scale set's session. Every scale set's listener has polled GitHub within its deadline, the reconciler has finished a good pass within its deadline, and the pool is not shutting down. The reconciler needs Docker, so a Docker daemon that stops answering makes the pool unhealthy. GitHub being unreachable never makes it unhealthy, because a poll that fails still counts as a poll. It shows on `/metrics` as `manade_github_errors_total` and `ManadePollStalled`, and in the log.

**Docker health.** The image checks itself every 30 seconds with `manade health`, which reads the health file the pool rewrites every 10 seconds. A file older than 30 seconds reads as a wedged pool. Docker marks the container unhealthy after 3 failures. It allows 30 minutes after a start, the same bound as `ManadeBootStalled`, for a first pull of every image on a slow link and the wait for a crashed predecessor's session to lapse. The first passing check ends that allowance. Docker's health status notifies nobody on its own, and Docker does not restart an unhealthy container.

**Who is told.**

| You run | A dead pool | A wedged pool |
| --- | --- | --- |
| Prometheus with this app's rules | `ManadeDown` | `ManadePollStalled` or `ManadeReconcileStalled`, and `/healthz` turns 503. `ManadeBootStalled` for a start that never ends or keeps failing |
| Loki only | `ManadeDown`, when no pass record or boot record arrives, and `ManadeBootStalled` when every start fails | `ManadePollStalled`, `ManadeBootStalled`, or `ManadeDown` once the reconciler stops writing the record |
| Docker or Unraid only | An uptime prober on `/healthz`, or something that watches Docker health | The same |

**Probing from outside.** Set `METRICS_LISTEN_ADDR`, then check `GET http://<host>:<port>/healthz` and pass on status 200 only. Do not match the body. Use a 5-second timeout, check every 60 seconds, alert after 3 failures, and wait 30 minutes after a first start. A Kubernetes liveness probe that must ignore readiness uses a `tcpSocket` probe on the port. Without `METRICS_LISTEN_ADDR` the pool opens no port, so nothing outside the container can probe it. Watch Docker health instead.

Without `DIAG_DIR`, the last 50 log lines of a failed runner are the only record of its failure. Set `DIAG_DIR` to keep each runner's own diagnostics as well.

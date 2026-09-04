# Scheduling the checks

`giro verify` exists, is tested, and does nothing until something runs it. This
is that something, in three shapes. Pick one.

Every check reports what it examined and exits non-zero on a finding, so a
scheduler needs no wrapper and no log parsing: **run it, look at the exit code.**

---

## Alert on two conditions, not one

This is the part people leave out.

```bash
giro verify --stale-after=4h --recon-after=4h    # did anything go wrong?
giro verify --last --max-age=25h                 # did anything stop looking?
```

The first is the alert everyone writes. The second is the one that matters when
the scheduler dies, the credentials expire, or a deployment quietly drops the
job — because until somebody notices, **a detector that stopped running looks
exactly like a book with nothing wrong.**

`--max-age` slightly exceeds the interval, so a single skipped run is not an
alert but two in a row are.

Both take `--json` for a monitor that would rather parse than grep.

---

## cron

```cron
# every hour, on the hour: run the checks
0 * * * * cd /srv/giro && ./giro verify --stale-after=4h --recon-after=4h

# every hour at :30: check that they are still running
30 * * * * cd /srv/giro && ./giro verify --last --max-age=2h
```

cron mails you the output of a command that exits non-zero, which is the
simplest alerting there is and enough to start with. Set `MAILTO`.

---

## systemd

Two units, because the service and its schedule are separate things.

`giro-verify.service`:

```ini
[Unit]
Description=giro invariant checks
After=network-online.target

[Service]
Type=oneshot
WorkingDirectory=/srv/giro
EnvironmentFile=/etc/giro/env
ExecStart=/srv/giro/giro verify --stale-after=4h --recon-after=4h
User=giro
```

`giro-verify.timer`:

```ini
[Unit]
Description=run giro's checks hourly

[Timer]
OnCalendar=hourly
# so a fleet does not stampede the database on the hour
RandomizedDelaySec=300
Persistent=true

[Install]
WantedBy=timers.target
```

```bash
systemctl enable --now giro-verify.timer
systemctl list-timers giro-verify   # when it last ran, when it runs next
```

`Persistent=true` runs a missed occurrence once the machine is back, which is
what you want for a check rather than a job with a deadline.

Failures land in the journal and in whatever watches `systemd` unit state:

```bash
journalctl -u giro-verify --since today
```

---

## Kubernetes

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: giro-verify
spec:
  schedule: "0 * * * *"
  # a check that overruns should not have a second copy started on top of it
  concurrencyPolicy: Forbid
  # keep enough history to see a pattern rather than only the last failure
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 10
  jobTemplate:
    spec:
      # no retries: a finding is not a transient error, and retrying it three
      # times only means three identical alerts
      backoffLimit: 0
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: verify
              image: giro:latest
              args: ["verify", "--stale-after=4h", "--recon-after=4h"]
              envFrom:
                - secretRef:
                    name: giro-database
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: giro-verify-liveness
spec:
  schedule: "30 * * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      backoffLimit: 0
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: liveness
              image: giro:latest
              args: ["verify", "--last", "--max-age=2h"]
              envFrom:
                - secretRef:
                    name: giro-database
```

Alert on `kube_job_status_failed` for both.

---

## Which connection it uses

`giro verify` reads `DATABASE_URL` and only ever reads and appends. It needs no
more privilege than the serving role, so **point it at the same restricted
role** — not at the owner.

```bash
DATABASE_URL=postgres://giro_service@db:5432/giro
```

It writes one `verification_runs` row per check, which the application role is
granted and cannot delete.

`giro_service` is a login role that owns nothing and inherits its privileges
from `giro_app`. It cannot `alter table`, which is what stops it switching off
the triggers that enforce the ledger's invariants — a table's owner can, so a
process connected as the owner has guards that are documentation rather than
enforcement. `just db-app-role` creates both; the full model is in the
[README](../README.md#the-three-roles).

Every command reads `DATABASE_URL` and nothing else, so the owner is separated
by *which environment it appears in*, not by a second variable name:

```bash
# the service, and this scheduled job
DATABASE_URL=postgres://giro_service@db:5432/giro

# the migration step only — a deploy job, an init container, a release task
DATABASE_URL=postgres://giro_owner@db:5432/giro
```

Keep those two environments apart and the owner's credential is never present
where requests are served. `giro serve` warns at boot if the connection it was
handed can disable its own guards, which is what catches it when they are not.

`giro account` also reads `DATABASE_URL` and also needs no more than the
restricted role, so whatever runs `giro verify` on a schedule can run it too.
Setting account policy is [deliberately not an
endpoint](../README.md#what-has-no-http-api), which means a deployment serving
giro still needs some path to a shell — a task runner, a job, a release step.

---

## Before any of this matters: recovery

[**RECOVERY.md**](RECOVERY.md) is the one to read before you need it.

The short version: a ledger cannot be re-derived, because nothing upstream
knows the balances. And a restore silently reuses transaction ids — the trigger
that forbids it fires on an `UPDATE`, and a restore replaces the table. Every
check still passes afterwards; the book is internally perfect and no longer
means what it meant.

So add one line to whatever already runs `giro verify`:

```bash
giro verify && giro recover tip
```

```
main:4291:3f4WLTEnfJp93aSndGTqdTjg547dZIt-5uyF9raDO4g
```

Ship it. It is the only thing a restore cannot reach, and it is what
`giro recover check` compares against afterwards.

---

## Telemetry, and what it does not cover

`giro verify` answers "is the book sound". It says nothing about how the ledger
is behaving right now, and the two failure modes are different: a wrong book is
a correctness problem, a contended one is a capacity problem, and neither
detects the other.

[`obs`](../obs/) is the second half — a separate Go module, so the engine keeps
one direct dependency:

```go
observer, shutdown, err := obs.Setup(ctx, "giro", obs.Options{
    SlowLock: 50 * time.Millisecond,
})
if err != nil {
    return err
}
defer shutdown(ctx)

store := storage.New(pool, "main").Observe(observer)
```

Where it goes is the standard OpenTelemetry environment, not configuration of
giro's:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
OTEL_EXPORTER_OTLP_PROTOCOL=grpc
OTEL_METRICS_EXPORTER=otlp     # or prometheus, console, none
OTEL_TRACES_EXPORTER=otlp      # or console, none
```

`none` is a real setting, so an environment can run without telemetry rather
than without the wiring.

**A collector is optional.** The SDK speaks OTLP straight to a backend, which
is enough for development. In production one earns its place by letting the
application hand data off quickly and by being the single place batching,
retries and redaction are configured.
[`otel-collector.yaml`](otel-collector.yaml) here is a starting configuration
for the upstream binary — giro ships no collector, and if you want one carrying
only the components you use, that is what the upstream builder is for.

**One thing to decide before this leaves your network.** Account addresses are
deliberately on spans and never on metrics, which is the right split for
cardinality — but it does mean a span attribute carries customer identifiers.
The collector config has a commented-out processor that drops them at the
boundary; the trace still shows the shape of the commit and how long the lock
took.

### Two alerts that only telemetry can give you

| Signal | Means | |
|---|---|---|
| `giro.commit.restarts > 0` | A commit lost a deadlock. Sorted lock ordering is supposed to make that impossible, so this is a correctness signal wearing a performance signal's clothes. | **Page.** |
| `giro.refusals{reason="contention_exhausted"} > 0` | A transaction gave up after the retry limit. A caller was told no for a reason unrelated to their money. | **Page.** |
| `giro.lock.wait` p99 climbing | `world` is the hottest row by construction — every deposit locks it. This is the number that says when to split it per counterparty. | Capacity planning, not a page. |

Do **not** alert on the refusal rate as a whole. Most of it is
`insufficient_funds`, which is the ledger working.

---

## What to page on, and what to look at in the morning

| Check | A finding means | When |
|---|---|---|
| `conservation` | Value was created or destroyed | **Page.** |
| `projection` | The tables disagree with the log | **Page.** |
| `log` | An entry was edited or removed | **Page.** |
| `effective_volumes` | The backdating cache disagrees with a replay | Page. |
| `balance_permissions` | A balance sits outside a bound it may not cross | Morning, unless it is large. Often a deliberate revocation. |
| `closed_accounts` | A closed account holds money | Morning. Somebody must reopen it and deal with it. |
| `conversions` | Amounts disagree with the rate recorded beside them | Morning. |
| `stale_balances` | Money has stopped moving | Morning. A question, not a fault. |
| `reconciliation` | Statement lines are still unmatched | Morning. This is the daily work queue. |

The first three mean the ledger is wrong about itself, which is the thing it
exists to make impossible. The rest mean the world and the book disagree, or
somebody needs to make a decision.

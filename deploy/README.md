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

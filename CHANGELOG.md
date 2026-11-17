# Changelog

This project follows [Semantic Versioning](https://semver.org/).
Until 1.0, the database schema and configuration may change between minor
versions; the notes always say what to do.

## [0.7.4] - 2026-11-17

- Changed: inside the colored Slack boxes the status emoji (large in Slack) are
  replaced by plain text symbols: ✗ failed/down, ✓ OK/up, ■ Monit stopped,
  ▸ Monit started. Emoji remain only in the host down/up and emergency headlines.
- Changed: for the system check, which Monit names after the host, the message
  shows `system` instead of repeating the host name (shorter line, less wrapping).

Upgrade: replace the binary and restart.

## [0.7.3] - 2026-11-17

- Fixed: "service failed" Slack notifications were missing when another rule of
  the same type on the same service was already failing (e.g. `loadavg (1min)`
  while `loadavg (5min)` keeps failing: both are Resource events), so only the
  "service OK" notifications arrived. A failure is now treated as a Monit re-send
  (and not notified) only when it arrives while the service is still
  initializing after a Monit start/reload, which is when Monit re-sends them.

Upgrade: replace the binary and restart. No schema changes.

## [0.7.2] - 2026-11-17

- Fixed: occasional deadlocks ("database conflict, retrying ... services
  cleanup: Error 1213"). The DELETE of services removed from monitrc ran on every
  report and could scan and lock rows of other hosts; services are now deleted by
  id and only when they really disappeared. Service groups are written only when
  they change, instead of delete+insert on every report.
- Added: admission control for bursts. At most `BMON_MAX_INFLIGHT` reports are
  stored at a time (default = `BMON_DB_CONNS`); a report waiting longer than
  `BMON_QUEUE_WAIT` (1s) is refused with 503 so that Monit retries on its next
  cycle instead of timing out. `/healthz` reports `busy_503` and `inflight`.
- Changed: `-db-timeout` default from 10s to 3s, so the answer always arrives
  within Monit's default 5 s timeout; new env vars `BMON_DB_CONNS`,
  `BMON_DB_TIMEOUT`, `BMON_MAX_INFLIGHT`, `BMON_QUEUE_WAIT`.

Upgrade: replace the binary and restart. No schema changes.

## [0.7.1] - 2026-11-16

- Changed: Slack notifications no longer have the small grey line at the bottom
  (service type, event type, IP address, time): only the main text is sent.

Upgrade: replace the binary and restart.

## [0.7.0] - 2026-11-15

- Added: emergency mode. When `BMON_EMERGENCY_HOSTS` hosts (default 10) go down
  within `BMON_EMERGENCY_WINDOW` (default 5m), one `EMERGENCY: N HOSTS DOWN`
  message with the mention replaces the single host down messages; while it lasts,
  host down/up are counted and the service/Monit notifications of the hosts
  involved (eventqueue backlog) are suppressed; progress updates at most every
  `BMON_EMERGENCY_UPDATE` (default 3m); `EMERGENCY OVER` lists the hosts still down.
- Added: many hosts back up at once outside an emergency → one `N HOSTS UP` message.
- Added: `emergency` notification kind for `BMON_SLACK_NOTIFY`; `/healthz` reports `emergency`.
- Changed: host down/up notifications are held 10 seconds before being sent.

Upgrade: replace the binary and restart. No schema changes; emergency mode is
on by default (`BMON_EMERGENCY_HOSTS=0` disables it).

## [0.6.0] - 2026-11-15

- Added: Slack notifications through an incoming webhook (`BMON_SLACK_WEBHOOK`):
  service failed / back to OK, Monit stopped / started, host down / up. Problems
  have a red bar, recoveries a green one; host down/up also get a headline, and
  host down carries a mention (`BMON_SLACK_MENTION`, default `<!channel>`).
- Added: `BMON_SLACK_NOTIFY` selects which kinds are sent (default `all`).
- Added: `-slack-test` posts one sample of each kind and exits.
- No duplicate notifications for events resent from Monit's eventqueue or
  re-sent by Monit after a start/reload; bursts are grouped and paced for Slack.
- Fixed: after a Monit start/reload, services reported as initializing
  (monitor=2, status 0) no longer reset a failing service to OK in the
  database; the last known status is kept until Monit checks it again.

Upgrade: replace the binary and restart. No schema changes. To enable Slack,
add `BMON_SLACK_WEBHOOK` to `/etc/bmon-collector/env` (see README).

## [0.5.0] - 2026-11-12

- Added: the Monit httpd credentials included in the reports (`<server><credentials>`)
  are stored in the new columns `hosts.httpd_username` and `hosts.httpd_password`,
  so that Bmon-web can send service actions to the agents, like M/Monit.
- Added: `BMON_CREDENTIALS_KEY` / `-credentials-key` (base64, 32 bytes) encrypts the
  stored passwords with AES-256-GCM, bound to the host's `monit_id`. Without a key they
  are stored in clear (a warning is logged at startup).
- Added: `BMON_STORE_CREDENTIALS=false` / `-store-credentials=false` keeps the previous
  behaviour (credentials never stored).
- Changed: the documentation no longer recommends `and register without credentials`;
  it is still supported, but hosts configured that way cannot receive actions.

Upgrade: replace the binary and restart; the two columns are added automatically.
Set `BMON_CREDENTIALS_KEY` (`openssl rand -base64 32`) in `/etc/bmon-collector/env`
before restarting, and put the same value in the Bmon-web configuration.
On the hosts, remove `and register without credentials` where you want actions.

## [0.4.0] - 2026-11-11

- Changed: the whole project is now in English — log messages and log keys,
  error messages, flag help, code comments, schema comments, documentation.
- Changed: the messages of the events Bmon writes to the `events` table
  (host offline / back online) are now in English. Events already stored keep
  their original text.

Upgrade: replace the binary and restart. No schema changes.

**Note for log parsing:** log messages and keys have changed. If you have
filters or alerts on the logs, update them. Main renames:

| 0.3.0 | 0.4.0 |
|---|---|
| `autenticazione fallita`, `utente=` | `authentication failed`, `user=` |
| `host offline: nessun report da Monit`, `ultimo_report=`, `silenzio=`, `soglia=` | `host offline: no report from Monit`, `last_report=`, `silence=`, `threshold=` |
| `host di nuovo online`, `offline_da=`, `durata=` | `host back online`, `offline_since=`, `duration=` |
| `evento` | `event` |
| `nuovo host registrato` | `new host registered` |
| `salvataggio fallito` | `failed to store report` |
| `Bmon collector in ascolto`, `versione=` | `Bmon collector listening`, `version=` |

## [0.3.0] - 2026-11-11

- New: offline host detection. When a host has not sent a report for more
  than `max(BMON_OFFLINE_MIN, BMON_OFFLINE_CYCLES × poll)` (default 3m / 3
  cycles) the collector logs a WARN, sets `hosts.offline_since` and records an
  event (`service='Monit'`, `event_id=0`, `state=1`). When the host returns it
  logs that it is back online with the outage duration and records a `state=0` event.
- No false alarms after a collector restart.
- `/healthz` reports `hosts` and `hosts_offline`.

Upgrade: replace the binary and restart. The `hosts.offline_since` column is
added automatically at startup.

## [0.2.0] - 2026-11-11

- New: service groups (`group` in monitrc) are stored in the
  `service_groups` table and updated on every report.
- New: when authentication fails, the log shows the username that was tried
  (never the password).

Upgrade: replace the binary and restart. The new table is created at startup;
existing data is left untouched.

## [0.1.0] - 2026-11-10

First release.

- `/collector` endpoint compatible with the M/Monit protocol (`set mmonit`),
  Basic Auth with multiple credentials
- MySQL/MariaDB storage of hosts, current service state (metrics as JSON)
  and events; schema created at startup
- Removal of services dropped from monitrc, idempotent events on eventqueue resends
- Log warning for cloned VMs sharing the same `.monit.id`
- Automatic retry on deadlocks and lock timeouts, READ COMMITTED transactions
- Periodic event purge (`-event-retention-days`, default 180)
- `/healthz`, `-version` flag, `Server` header
- systemd unit, nginx configuration (direct and mirror for migration)

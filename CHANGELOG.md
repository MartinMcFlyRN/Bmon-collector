# Changelog

This project follows [Semantic Versioning](https://semver.org/).
Until 1.0, the database schema and configuration may change between minor
versions; the notes always say what to do.

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

# Bmon collector

Version **0.5.0**. License: [AGPL-3.0](LICENSE). First component of Bmon, a replacement for M/Monit: it
receives the reports that Monit agents send with `set mmonit` and stores them
in MySQL/MariaDB.

It speaks **the same protocol as M/Monit** (XML POST to `/collector` with Basic
Auth), so nothing needs to be installed on the monitored hosts: only the URL
changes (or not even that, see "Migration").

What it does:

- registers hosts by `monit_id` (the content of `.monit.id`): hostname, version, OS, CPU/RAM, poll interval
- keeps the **current state** of every service (status, monitor, collection time, when the status last changed) plus all type-specific metrics in a JSON column
- removes services that disappear from monitrc
- detects **offline hosts** (Monit stops sending reports) and reports when they come back online
- stores service **groups** (the `group` statement in monitrc) in the `service_groups` table
- keeps the **event history**, with no duplicates when Monit resends events from its eventqueue
- logs a warning for cloned VMs sharing the same `.monit.id` (same id, changing hostname)
- exposes `/healthz` (database ping and counters)

- stores the **Monit httpd credentials** sent in the reports (optionally encrypted), so that [Bmon-web](https://github.com/MartinMcFlyRN/Bmon-web) can send start/stop/restart/monitor/unmonitor to the agents, like M/Monit

Tested with a real Monit 5.33 + MariaDB 10.11. With 500 simulated hosts of 30
services each: about 365 reports/s on 2 vCPUs, zero errors. 500 hosts with a
30 s poll interval generate about 17 reports/s.

## Build

Requires Go ≥ 1.24 (Debian 13: `apt install golang`).

```sh
go mod tidy          # first time only: fetches the MySQL driver and creates go.sum
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bmon-collector .
```

The binary is static and can be copied to the server as is. `bmon-collector -version`
prints the version, which also appears in the startup log, in `/healthz` and in
the `Server` response header.

To tag a non-release build:

```sh
go build -ldflags "-s -w -X main.Version=0.4.0-dev+$(git rev-parse --short HEAD)" -o bmon-collector .
```

## Database

```sql
CREATE DATABASE bmon_collector CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER 'bmon_collector'@'localhost' IDENTIFIED BY '...';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, REFERENCES
  ON bmon_collector.* TO 'bmon_collector'@'localhost';
```

The tables (`hosts`, `services`, `events`, `service_groups`) are created at
startup, see `schema.sql`; columns added in later versions are migrated
automatically. MySQL ≥ 5.7 or MariaDB ≥ 10.2. Transactions use READ COMMITTED:
if the MySQL server has binary logging enabled it must use `binlog_format=ROW`
(the default since MySQL 5.7.7).

## Installation

```sh
install -m 755 bmon-collector /usr/local/bin/
useradd --system --no-create-home --shell /usr/sbin/nologin bmon-collector
install -d -m 750 /etc/bmon-collector
install -m 600 deploy/env.example /etc/bmon-collector/env   # then edit it
install -m 644 deploy/bmon-collector.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now bmon-collector
journalctl -u bmon-collector -f
```

Configuration (environment variables or flags, see `bmon-collector -h`):

| Variable | Flag | Default | |
|---|---|---|---|
| `BMON_LISTEN` | `-listen` | `127.0.0.1:8080` | behind nginx |
| `BMON_DSN` | `-dsn` | required | `user:pass@tcp(host:3306)/db` |
| `BMON_AUTH` | `-auth` | required | `user:pass[,user2:pass2]` |
| `BMON_TRUSTED_PROXIES` | `-trusted-proxies` | `127.0.0.1/32,::1/128` | proxies allowed to set `X-Real-IP` |
| `BMON_LOG_LEVEL` | `-log-level` | `info` | `debug` logs every report |
| `BMON_STORE_CREDENTIALS` | `-store-credentials` | `true` | store the Monit httpd credentials found in the reports |
| `BMON_CREDENTIALS_KEY` | `-credentials-key` | empty | base64 32-byte key: stored passwords are encrypted (AES-256-GCM). Empty = stored in clear |
| `BMON_OFFLINE_CYCLES` | `-offline-cycles` | `3` | poll cycles without a report before a host is offline |
| `BMON_OFFLINE_MIN` | `-offline-min` | `3m` | minimum silence, regardless of the poll interval |
| | `-offline-check-every` | `30s` | how often silent hosts are checked |
| | `-event-retention-days` | `180` | `0` = never delete |
| | `-db-conns` | `20` | |
| | `-max-body` | 16 MiB | |

If a password contains `"`, `'`, `\` or `$`, wrap the value in single quotes in
the env file (`BMON_AUTH='monit:pa$$word'`), since systemd interprets quotes and
backslashes. A comma cannot be used in a password, because it separates
credential pairs.

## Monit configuration on the hosts

```
set mmonit https://monit:PASSWORD@collector.example.com/collector
```

Publish the collector **over HTTPS only**: Basic Auth and the httpd
credentials below travel in every report.

### Service actions from Bmon-web

Like M/Monit, Bmon-web sends actions (start, stop, restart, monitor, unmonitor)
to the httpd of each Monit agent, with the user and password that Monit puts in
its reports. For that, on each host:

- `set mmonit` **without** `and register without credentials` (otherwise
  Monit sends no credentials and actions are not available for that host);
- `set httpd` must listen on an address reachable from the Bmon-web server and
  allow it, for example:

  ```
  set httpd port 2812 and
      use address 0.0.0.0           # or the host's LAN/public address
      allow 203.0.113.5             # address of the Bmon-web server
      allow admin:LONG-PASSWORD     # these are the credentials Bmon-web will use
  ```

  When httpd listens on `0.0.0.0` or on loopback, Bmon-web connects to the
  address the reports come from. Hosts behind NAT, or on LANs the Bmon-web
  server cannot reach, cannot receive actions (same limit as M/Monit).

Encrypt the stored passwords with a key, and give the same key to Bmon-web
(`monit.credentials_key`):

```sh
openssl rand -base64 32        # → BMON_CREDENTIALS_KEY in /etc/bmon-collector/env
```

Passwords are rewritten (and re-encrypted) at every report, so setting or
changing the key takes effect within one poll cycle. To keep the pre-0.5.0
behaviour and never store them: `BMON_STORE_CREDENTIALS=false`.

Also recommended: `set eventqueue basedir /var/lib/monit/events slots 1000`.
If the collector is unreachable, events are queued and resent, with no duplicates.

## Migration without touching the hosts

`deploy/nginx.conf` → **Option B**: nginx keeps sending traffic to M/Monit and
mirrors every POST to the collector (`mirror`). Both systems can be compared
for as long as needed, then the switch happens on the proxy. The hosts are not
touched in either phase.

## Offline hosts

Every 30 seconds the collector looks for hosts that have not sent a report for
more than `max(BMON_OFFLINE_MIN, BMON_OFFLINE_CYCLES × host poll interval)`;
with the defaults that is 3 minutes or 3 cycles. When a host crosses the
threshold:

- the log shows `level=WARN msg="host offline: no report from Monit" host=... last_report=... silence=...`
- `hosts.offline_since` is set
- an event is recorded in the `events` table with `service='Monit'`, `event_id=0`, `state=1`

On the next report the log shows `host back online ... duration=...`,
`offline_since` goes back to NULL and an event with `state=0` is recorded. Each
transition is reported once, not on every check.

The check cannot tell a powered-off host from a stopped Monit or a network
problem: in all three cases the report simply does not arrive.

After a collector restart, hosts are not marked offline until the collector has
been running for at least the threshold, so a collector outage does not produce
hundreds of false alarms.

A decommissioned host stays offline (without further log lines). To remove it:
`DELETE FROM hosts WHERE hostname = '...'`, which also deletes its services and events.

`/healthz` reports `hosts` and `hosts_offline`.

## Useful queries

```sql
-- offline hosts, and for how long
SELECT hostname, remote_addr, offline_since,
       TIMEDIFF(UTC_TIMESTAMP(), offline_since) AS down_for
FROM hosts WHERE offline_since IS NOT NULL ORDER BY offline_since;

-- failing services, oldest first
SELECT h.hostname, s.name, s.status, s.status_changed_at
FROM services s JOIN hosts h ON h.id = s.host_id
WHERE s.status <> 0 AND s.monitor = 1
ORDER BY s.status_changed_at;

-- filesystems above 85%
SELECT h.hostname, s.name, JSON_EXTRACT(s.data, '$.block.percent') AS pct
FROM services s JOIN hosts h ON h.id = s.host_id
WHERE s.type = 0 AND JSON_EXTRACT(s.data, '$.block.percent') > 85;

-- state of every service in group H24, across all hosts
SELECT h.hostname, s.name, s.status, s.status_changed_at
FROM service_groups g
JOIN hosts h    ON h.id = g.host_id
JOIN services s ON s.host_id = g.host_id AND s.name = g.service
WHERE g.group_name = 'H24'
ORDER BY s.status <> 0 DESC, h.hostname;

-- defined groups and how many services they contain
SELECT group_name, COUNT(*) services, COUNT(DISTINCT host_id) hosts
FROM service_groups GROUP BY group_name ORDER BY group_name;

-- latest events
SELECT h.hostname, e.service, e.state, e.message, e.collected_at
FROM events e JOIN hosts h ON h.id = e.host_id
ORDER BY e.collected_at DESC LIMIT 50;
```

Codes: `services.type` → 0 filesystem, 1 directory, 2 file, 3 process,
4 host, 5 system, 6 fifo, 7 program, 8 net. `events.state` → 0 succeeded,
1 failed, 2 changed, 3 changed not, 4 init. `status` is the bitmask of failing
events (e.g. 0x20 connection, 0x200 non exist); 0 means OK.

## Not there yet (next steps)

- **Metrics history**: only the latest value is kept today, in `services.data`.
  Charts need a time series: either an aggregated table in MySQL or a TSDB. This
  has to be decided before building the UI.
- **Notifications**: email/chat alerts for offline hosts and Monit events.
- **UI / read API**.
- **Actions for unreachable hosts**: actions go through Monit's httpd (0.5.0
  stores the credentials for Bmon-web), which is impossible for hosts on LANs
  the web server cannot reach. Another mechanism will be needed for them.

## License

Bmon collector is free software, released under the **GNU Affero General Public
License, version 3** (AGPL-3.0). The full text is in [LICENSE](LICENSE), from
<https://www.gnu.org/licenses/agpl-3.0.txt>.

In short: you may use, study, modify and redistribute it. If you distribute it,
or let other people use a modified version over a network, you must make the
corresponding source code available to them under the same license.

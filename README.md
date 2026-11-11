# Bmon collector

Versione **0.1.0**. Primo componente di Bmon, il sostituto di M/Monit: riceve i report che gli agent Monit
inviano con `set mmonit` e li salva su MySQL/MariaDB.

Usa **lo stesso protocollo di M/Monit** (POST XML su `/collector` con Basic Auth),
quindi sugli host non va installato niente: si cambia solo l'URL (o neanche
quello, vedi "Migrazione").

Cosa fa:

- registra gli host per `monit_id` (il contenuto di `.monit.id`): hostname, versione, OS, CPU/RAM, poll
- tiene lo **stato corrente** di ogni servizio (status, monitor, data della raccolta, quando è cambiato lo status) e tutte le metriche specifiche del tipo in una colonna JSON
- elimina i servizi che spariscono dal monitrc
- salva lo **storico eventi**, senza duplicati quando Monit li reinvia dalla eventqueue
- segnala nel log le VM clonate con lo stesso `.monit.id` (stesso id, hostname che cambia)
- espone `/healthz` (ping al DB e contatori)

Non salva mai le credenziali dell'httpd di Monit che gli agent mandano nel payload.

Testato con Monit 5.33 reale + MariaDB 10.11. Con 500 host simulati da 30 servizi
ciascuno: circa 365 report/s su 2 vCPU, zero errori. 500 host con poll di 30 s
fanno circa 17 report/s.

## Build

Serve Go ≥ 1.24 (Debian 13: `apt install golang`).

```sh
go mod tidy          # la prima volta: scarica il driver MySQL e crea go.sum
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bmon-collector .
```

Il binario è statico, si copia così com'è sul server. `bmon-collector -version`
stampa la versione, che compare anche nel log di avvio, in `/healthz` e
nell'header `Server` delle risposte.

Per marcare una build non di release:

```sh
go build -ldflags "-s -w -X main.Version=0.1.0-dev+$(git rev-parse --short HEAD)" -o bmon-collector .
```

## Database

```sql
CREATE DATABASE bmon_collector CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER 'bmon_collector'@'localhost' IDENTIFIED BY '...';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, REFERENCES
  ON bmon_collector.* TO 'bmon_collector'@'localhost';
```

Le tabelle (`hosts`, `services`, `events`) vengono create all'avvio, vedi `schema.sql`.
MySQL ≥ 5.7 o MariaDB ≥ 10.2. Le transazioni usano READ COMMITTED: se il server
MySQL ha il binlog attivo deve essere `binlog_format=ROW` (il default da MySQL 5.7.7).

## Installazione

```sh
install -m 755 bmon-collector /usr/local/bin/
useradd --system --no-create-home --shell /usr/sbin/nologin bmon-collector
install -d -m 750 /etc/bmon-collector
install -m 600 deploy/env.example /etc/bmon-collector/env   # poi modificarlo
install -m 644 deploy/bmon-collector.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now bmon-collector
journalctl -u bmon-collector -f
```

Configurazione (variabili d'ambiente oppure flag, `bmon-collector -h`):

| Variabile | Flag | Default | |
|---|---|---|---|
| `BMON_LISTEN` | `-listen` | `127.0.0.1:8080` | dietro nginx |
| `BMON_DSN` | `-dsn` | obbligatoria | `user:pass@tcp(host:3306)/db` |
| `BMON_AUTH` | `-auth` | obbligatoria | `utente:pass[,utente2:pass2]` |
| `BMON_TRUSTED_PROXIES` | `-trusted-proxies` | `127.0.0.1/32,::1/128` | da chi accettare `X-Real-IP` |
| `BMON_LOG_LEVEL` | `-log-level` | `info` | `debug` logga ogni report |
| | `-event-retention-days` | `180` | `0` = mai cancellare |
| | `-db-conns` | `20` | |
| | `-max-body` | 16 MiB | |

## Configurazione di Monit sugli host

```
set mmonit https://monit:PASSWORD@collector.example.it/collector
    and register without credentials
```

`and register without credentials` evita che Monit mandi a ogni report le
credenziali del suo httpd. Il collector non le usa, quindi è meglio che non
viaggino. Il collector va comunque pubblicato **solo in HTTPS**, perché
la Basic Auth viaggia in chiaro.

Consigliato anche `set eventqueue basedir /var/lib/monit/events slots 1000`:
se il collector è irraggiungibile gli eventi vengono accodati e reinviati,
senza duplicati.

## Migrazione senza toccare gli host

`deploy/nginx.conf` → **Opzione B**: nginx continua a mandare il traffico a
M/Monit e ne duplica ogni POST verso il collector (`mirror`). Così si confrontano
i due sistemi per tutto il tempo che serve, poi si fa il cambio sul proxy.
Gli host non vengono toccati in nessuna delle due fasi.

## Query utili

```sql
-- host che non comunicano da più di 3 cicli di poll
SELECT hostname, remote_addr, last_seen
FROM hosts
WHERE last_seen < UTC_TIMESTAMP() - INTERVAL 3 * poll_interval SECOND;

-- servizi in errore, i più vecchi per primi
SELECT h.hostname, s.name, s.status, s.status_changed_at
FROM services s JOIN hosts h ON h.id = s.host_id
WHERE s.status <> 0 AND s.monitor = 1
ORDER BY s.status_changed_at;

-- filesystem sopra l'85%
SELECT h.hostname, s.name, JSON_EXTRACT(s.data, '$.block.percent') AS pct
FROM services s JOIN hosts h ON h.id = s.host_id
WHERE s.type = 0 AND JSON_EXTRACT(s.data, '$.block.percent') > 85;

-- ultimi eventi
SELECT h.hostname, e.service, e.state, e.message, e.collected_at
FROM events e JOIN hosts h ON h.id = e.host_id
ORDER BY e.collected_at DESC LIMIT 50;
```

Codici: `services.type` → 0 filesystem, 1 directory, 2 file, 3 process,
4 host, 5 system, 6 fifo, 7 program, 8 net. `events.state` → 0 succeeded,
1 failed, 2 changed, 3 changed not, 4 init. `status` è la bitmask degli
eventi in errore (es. 0x20 connection, 0x200 non exist), 0 vuol dire OK.

## Cosa manca (prossimi passi)

- **Storico metriche**: oggi c'è solo l'ultimo valore, in `services.data`. Per
  i grafici serve una serie temporale. Le opzioni sono una tabella aggregata in
  MySQL oppure un TSDB, e va deciso prima di scrivere la UI.
- **Alerting**: host offline (`last_seen`) e notifiche sugli eventi.
- **UI / API** di lettura.
- **Azioni** (start/stop/restart): M/Monit le fa chiamando l'httpd di Monit, cosa
  impossibile per gli host nelle LAN non raggiungibili. Ci vorrà un altro meccanismo.

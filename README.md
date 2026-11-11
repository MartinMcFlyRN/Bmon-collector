# Bmon collector

Versione **0.3.0**. Primo componente di Bmon, il sostituto di M/Monit: riceve i report che gli agent Monit
inviano con `set mmonit` e li salva su MySQL/MariaDB.

Usa **lo stesso protocollo di M/Monit** (POST XML su `/collector` con Basic Auth),
quindi sugli host non va installato niente: si cambia solo l'URL (o neanche
quello, vedi "Migrazione").

Cosa fa:

- registra gli host per `monit_id` (il contenuto di `.monit.id`): hostname, versione, OS, CPU/RAM, poll
- tiene lo **stato corrente** di ogni servizio (status, monitor, data della raccolta, quando è cambiato lo status) e tutte le metriche specifiche del tipo in una colonna JSON
- elimina i servizi che spariscono dal monitrc
- segnala gli **host offline** (Monit che smette di inviare report) e il loro ritorno online
- salva i **gruppi** dei servizi (direttiva `group` del monitrc) nella tabella `service_groups`
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

Le tabelle (`hosts`, `services`, `events`, `service_groups`) vengono create all'avvio, vedi `schema.sql`.
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
| `BMON_OFFLINE_CYCLES` | `-offline-cycles` | `3` | cicli di poll senza report prima di dichiarare offline |
| `BMON_OFFLINE_MIN` | `-offline-min` | `3m` | silenzio minimo, qualunque sia il poll |
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

## Host offline

Ogni 30 secondi il collector controlla gli host che non inviano report da più di
`max(BMON_OFFLINE_MIN, BMON_OFFLINE_CYCLES × poll dell'host)`, con i default
3 minuti oppure 3 cicli. Quando un host supera la soglia:

- il log riporta `level=WARN msg="host offline: nessun report da Monit" host=... ultimo_report=... silenzio=...`
- `hosts.offline_since` viene valorizzato
- nella tabella `events` viene registrato un evento con `service='Monit'`, `event_id=0`, `state=1`

Al primo report successivo il log riporta `host di nuovo online ... durata=...`,
`offline_since` torna NULL e viene registrato un evento con `state=0`. Ogni
transizione viene segnalata una sola volta, non a ogni controllo.

Il controllo non distingue un host spento da Monit fermo o da un problema di
rete: in tutti e tre i casi il report non arriva.

Dopo un riavvio del collector gli host non vengono dichiarati offline finché
il collector non è attivo da almeno la soglia, così un fermo del collector
non genera centinaia di falsi allarmi.

Un host dismesso resta offline (senza nuovi log). Per toglierlo:
`DELETE FROM hosts WHERE hostname = '...'`, che elimina anche i suoi servizi ed eventi.

`/healthz` riporta `hosts` e `hosts_offline`.

## Query utili

```sql
-- host offline, da quanto tempo
SELECT hostname, remote_addr, offline_since,
       TIMEDIFF(UTC_TIMESTAMP(), offline_since) AS da
FROM hosts WHERE offline_since IS NOT NULL ORDER BY offline_since;

-- servizi in errore, i più vecchi per primi
SELECT h.hostname, s.name, s.status, s.status_changed_at
FROM services s JOIN hosts h ON h.id = s.host_id
WHERE s.status <> 0 AND s.monitor = 1
ORDER BY s.status_changed_at;

-- filesystem sopra l'85%
SELECT h.hostname, s.name, JSON_EXTRACT(s.data, '$.block.percent') AS pct
FROM services s JOIN hosts h ON h.id = s.host_id
WHERE s.type = 0 AND JSON_EXTRACT(s.data, '$.block.percent') > 85;

-- stato di tutti i servizi del gruppo H24, su tutti gli host
SELECT h.hostname, s.name, s.status, s.status_changed_at
FROM service_groups g
JOIN hosts h    ON h.id = g.host_id
JOIN services s ON s.host_id = g.host_id AND s.name = g.service
WHERE g.group_name = 'H24'
ORDER BY s.status <> 0 DESC, h.hostname;

-- gruppi definiti e quanti servizi ne fanno parte
SELECT group_name, COUNT(*) servizi, COUNT(DISTINCT host_id) host
FROM service_groups GROUP BY group_name ORDER BY group_name;

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

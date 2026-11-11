# Changelog

Il progetto segue il [Semantic Versioning](https://semver.org/lang/it/).
Fino alla 1.0 schema del database e configurazione possono cambiare tra
versioni minori: le note indicano sempre cosa fare.

## [0.2.0] - 2026-11-11

- Nuovo: i gruppi dei servizi (`group` nel monitrc) vengono salvati nella
  tabella `service_groups` e aggiornati a ogni report
- Nuovo: quando l'autenticazione fallisce il log riporta il nome utente
  tentato (mai la password)

Aggiornamento: sostituire il binario e riavviare. La nuova tabella viene
creata all'avvio, i dati esistenti non vengono toccati.

## [0.1.0] - 2026-11-10

Prima versione.

- Endpoint `/collector` compatibile con il protocollo M/Monit (`set mmonit`),
  Basic Auth con più credenziali
- Salvataggio su MySQL/MariaDB di host, stato corrente dei servizi (metriche in JSON)
  ed eventi; schema creato all'avvio
- Rimozione dei servizi tolti dal monitrc, eventi idempotenti sui reinvii della eventqueue
- Avviso nel log per VM clonate con lo stesso `.monit.id`
- Retry automatico su deadlock e lock timeout, transazioni READ COMMITTED
- Pulizia periodica degli eventi (`-event-retention-days`, default 180)
- `/healthz`, flag `-version`, header `Server`
- Unit systemd, configurazione nginx (diretta e mirror per la migrazione)

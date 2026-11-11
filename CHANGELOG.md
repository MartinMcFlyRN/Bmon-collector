# Changelog

Il progetto segue il [Semantic Versioning](https://semver.org/lang/it/).
Fino alla 1.0 schema del database e configurazione possono cambiare tra
versioni minori: le note indicano sempre cosa fare.

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

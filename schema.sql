-- Schema del collector. Viene applicato all'avvio (solo CREATE ... IF NOT EXISTS).
-- Tutti i DATETIME sono in UTC.

CREATE TABLE IF NOT EXISTS hosts (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  monit_id        VARCHAR(64)  NOT NULL,          -- contenuto di ~/.monit.id
  hostname        VARCHAR(255) NOT NULL,
  incarnation     BIGINT       NOT NULL,          -- timestamp di avvio del daemon Monit
  monit_version   VARCHAR(32)  NOT NULL,
  poll_interval   INT          NOT NULL,          -- secondi
  monit_uptime    BIGINT       NOT NULL,
  control_file    VARCHAR(512) NOT NULL DEFAULT '',
  os_name         VARCHAR(64)  NOT NULL DEFAULT '',
  os_release      VARCHAR(128) NOT NULL DEFAULT '',
  os_version      VARCHAR(255) NOT NULL DEFAULT '',
  machine         VARCHAR(32)  NOT NULL DEFAULT '',
  cpu_count       INT          NOT NULL DEFAULT 0,
  memory_kb       BIGINT       NOT NULL DEFAULT 0,
  swap_kb         BIGINT       NOT NULL DEFAULT 0,
  httpd_address   VARCHAR(255) NOT NULL DEFAULT '',
  httpd_port      INT          NOT NULL DEFAULT 0,
  httpd_ssl       TINYINT(1)   NOT NULL DEFAULT 0,
  remote_addr     VARCHAR(45)  NOT NULL DEFAULT '',
  services_total  INT          NOT NULL DEFAULT 0,
  services_failed INT          NOT NULL DEFAULT 0,
  first_seen      DATETIME(3)  NOT NULL,
  last_seen       DATETIME(3)  NOT NULL,
  UNIQUE KEY uk_monit_id (monit_id),
  KEY idx_hostname (hostname),
  KEY idx_last_seen (last_seen)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Stato corrente: una riga per (host, servizio), aggiornata a ogni report.
CREATE TABLE IF NOT EXISTS services (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  host_id           BIGINT UNSIGNED NOT NULL,
  name              VARCHAR(255) NOT NULL,
  type              TINYINT UNSIGNED NOT NULL,     -- 0 fs,1 dir,2 file,3 process,4 host,5 system,6 fifo,7 program,8 net
  status            BIGINT UNSIGNED NOT NULL,      -- bitmask eventi in errore, 0 = OK
  status_hint       BIGINT UNSIGNED NOT NULL,
  monitor           TINYINT UNSIGNED NOT NULL,     -- 0 no, 1 sì, 2 init
  monitor_mode      TINYINT UNSIGNED NOT NULL,
  pending_action    TINYINT UNSIGNED NOT NULL,
  collected_at      DATETIME(6) NOT NULL,
  status_changed_at DATETIME(3) NOT NULL,          -- ultimo cambio di status (per "in errore da X minuti")
  data              JSON NOT NULL,                 -- metriche/campi specifici del tipo
  updated_at        DATETIME(3) NOT NULL,
  UNIQUE KEY uk_host_service (host_id, name),
  KEY idx_status (status),
  CONSTRAINT fk_services_host FOREIGN KEY (host_id) REFERENCES hosts (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Storico eventi. La unique key rende idempotenti i reinvii dalla eventqueue di Monit.
CREATE TABLE IF NOT EXISTS events (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  host_id      BIGINT UNSIGNED NOT NULL,
  service      VARCHAR(255) NOT NULL,
  service_type TINYINT UNSIGNED NOT NULL,
  event_id     BIGINT UNSIGNED NOT NULL,           -- tipo evento Monit (0x200 NonExist, 0x20 Connection, ...)
  state        TINYINT UNSIGNED NOT NULL,          -- 0 succeeded, 1 failed, 2 changed, 3 changed not, 4 init
  action       TINYINT UNSIGNED NOT NULL,
  message      TEXT NOT NULL,
  collected_at DATETIME(6) NOT NULL,
  received_at  DATETIME(3) NOT NULL,
  UNIQUE KEY uk_event (host_id, collected_at, service, event_id),
  KEY idx_collected (collected_at),
  KEY idx_host_service (host_id, service, collected_at),
  CONSTRAINT fk_events_host FOREIGN KEY (host_id) REFERENCES hosts (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- go_server 的 MySQL / MariaDB 建库脚本。
--
-- 与 schema.sql 是同一份逻辑 schema，只是类型写法按 MySQL 方言翻译。
-- 强约束（TestSchemaParity 会校验）：表名、列名必须与 schema.sql 完全一致，
-- 只有这样两边才能互相接管对方的库。
--
-- 最低版本：MySQL 8.0.13 / MariaDB 10.2
--   依赖"表达式默认值"给 TEXT 列写 DEFAULT (expr)。更老的版本会把
--   DEFAULT ('...') 当成语法错误；低版本请把带默认值的 TEXT 列改成 VARCHAR。
--
-- 翻译规则（对照 schema.sql）：
--   INTEGER PRIMARY KEY AUTOINCREMENT  → INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY
--   指向自增主键的 *_id 列            → INT UNSIGNED（与主键类型一致）
--   BOOLEAN                            → TINYINT(1)
--   REAL                               → DOUBLE
--   DATETIME(3) DEFAULT (DATETIME('now')) → DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
--   TEXT NOT NULL DEFAULT 'x'          → TEXT NOT NULL DEFAULT ('x')
--   需要做主键/唯一索引的 TEXT 列       → VARCHAR(255)（MySQL 的 TEXT 不能做键）
--   CREATE [UNIQUE] INDEX ...          → 内联 KEY / UNIQUE KEY（MySQL 无 CREATE INDEX IF NOT EXISTS）
--   所有标识符一律加反引号              → user / group / key / default / interval 在 MySQL 是保留字
--
-- 刻意没有写外键约束：store.MonitorStore.Delete 等删除路径会显式清理全部关联表
-- （见 monitor.go 注释"官方库开启了外键级联，这里仍显式清理，兼容关闭外键的老库"），
-- 不依赖级联；不建外键同时避免了插入顺序约束和约束名跨表唯一的问题。
--
-- 时区约定：连接串强制 session time_zone='+00:00'，因此 CURRENT_TIMESTAMP 与
-- TIMESTAMP(3) 列都按 UTC 存取，和官方 mysql2 timezone:"Z" 的语义一致。

CREATE TABLE IF NOT EXISTS `user` (
    `id`              INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `username`        VARCHAR(255) NOT NULL,
    `password`        VARCHAR(255),
    `active`          TINYINT(1) NOT NULL DEFAULT 1,
    `timezone`        VARCHAR(150),
    `twofa_secret`    VARCHAR(64),
    `twofa_status`    TINYINT(1) NOT NULL DEFAULT 0,
    `twofa_last_token` VARCHAR(6),
    UNIQUE KEY `user_username_unique` (`username`)
);

-- user_id 在官方 2026-05 的迁移里从 INTEGER 改成了可空的 VARCHAR(255)
-- （better-auth 的用户 ID 是 uuid）
CREATE TABLE IF NOT EXISTS `docker_host` (
    `id`            INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `user_id`       VARCHAR(255),
    `docker_daemon` VARCHAR(255),
    `docker_type`   VARCHAR(255),
    `name`          VARCHAR(255)
);

CREATE TABLE IF NOT EXISTS `proxy` (
    `id`           INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `user_id`      INT UNSIGNED NOT NULL,
    `protocol`     VARCHAR(10) NOT NULL,
    `host`         VARCHAR(255) NOT NULL,
    `port`         SMALLINT NOT NULL,
    `auth`         TINYINT(1) NOT NULL DEFAULT 0,
    `username`     VARCHAR(255),
    `password`     VARCHAR(255),
    `active`       TINYINT(1) NOT NULL DEFAULT 1,
    `default`      TINYINT(1) NOT NULL DEFAULT 0,
    `created_date` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    KEY `proxy_user_id` (`user_id`)
);

CREATE TABLE IF NOT EXISTS `group` (
    `id`             INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `name`           VARCHAR(255) NOT NULL,
    `created_date`   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `public`         TINYINT(1) NOT NULL DEFAULT 0,
    `active`         TINYINT(1) NOT NULL DEFAULT 1,
    `weight`         INT NOT NULL DEFAULT 1000,
    `status_page_id` INT UNSIGNED
);

CREATE TABLE IF NOT EXISTS `monitor` (
    `id`                          INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `name`                        VARCHAR(150),
    `active`                      TINYINT(1) NOT NULL DEFAULT 1,
    `user_id`                     INT UNSIGNED,
    `interval`                    INT NOT NULL DEFAULT 20,
    `url`                         TEXT,
    `type`                        VARCHAR(20),
    `weight`                      INT DEFAULT 2000,
    `hostname`                    VARCHAR(255),
    `port`                        INT,
    `created_date`                DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `keyword`                     VARCHAR(255),
    `maxretries`                  INT NOT NULL DEFAULT 0,
    `ignore_tls`                  TINYINT(1) NOT NULL DEFAULT 0,
    `upside_down`                 TINYINT(1) NOT NULL DEFAULT 0,
    `maxredirects`                INT NOT NULL DEFAULT 10,
    `accepted_statuscodes_json`   TEXT NOT NULL DEFAULT ('["200-299"]'),
    `dns_resolve_type`            VARCHAR(5),
    `dns_resolve_server`          VARCHAR(255),
    `dns_last_result`             TEXT,
    `retry_interval`              INT NOT NULL DEFAULT 0,
    `push_token`                  VARCHAR(32),
    `method`                      TEXT NOT NULL DEFAULT ('GET'),
    `body`                        TEXT,
    `headers`                     TEXT,
    `basic_auth_user`             TEXT,
    `basic_auth_pass`             TEXT,
    `docker_host`                 INT UNSIGNED,
    `docker_container`            VARCHAR(255),
    `proxy_id`                    INT UNSIGNED,
    `expiry_notification`         TINYINT(1) DEFAULT 1,
    `mqtt_topic`                  TEXT,
    `mqtt_success_message`        VARCHAR(255),
    `mqtt_username`               VARCHAR(255),
    `mqtt_password`               VARCHAR(255),
    `database_connection_string`  VARCHAR(2000),
    `database_query`              TEXT,
    `auth_method`                 VARCHAR(250),
    `auth_domain`                 TEXT,
    `auth_workstation`            TEXT,
    `grpc_url`                    VARCHAR(255),
    `grpc_protobuf`               TEXT,
    `grpc_body`                   TEXT,
    `grpc_metadata`               TEXT,
    `grpc_method`                 TEXT,
    `grpc_service_name`           TEXT,
    `grpc_enable_tls`             TINYINT(1) NOT NULL DEFAULT 0,
    `radius_username`             VARCHAR(255),
    `radius_password`             VARCHAR(255),
    `radius_calling_station_id`   VARCHAR(50),
    `radius_called_station_id`    VARCHAR(50),
    `radius_secret`               VARCHAR(255),
    `resend_interval`             INT NOT NULL DEFAULT 0,
    `packet_size`                 INT NOT NULL DEFAULT 56,
    `game`                        VARCHAR(255),
    `http_body_encoding`          VARCHAR(25),
    `description`                 TEXT,
    `tls_ca`                      TEXT,
    `tls_cert`                    TEXT,
    `tls_key`                     TEXT,
    `parent`                      INT UNSIGNED,
    `invert_keyword`              TINYINT(1) NOT NULL DEFAULT 0,
    `json_path`                   TEXT,
    `expected_value`              VARCHAR(255),
    `json_path_operator`          VARCHAR(10) DEFAULT '==',
    `kafka_producer_topic`        VARCHAR(255),
    `kafka_producer_brokers`      TEXT,
    `kafka_producer_ssl`          TINYINT(1) NOT NULL DEFAULT 0,
    `kafka_producer_allow_auto_topic_creation` TINYINT(1) NOT NULL DEFAULT 0,
    `kafka_producer_sasl_options` TEXT,
    `kafka_producer_message`      TEXT,
    `oauth_client_id`             TEXT,
    `oauth_client_secret`         TEXT,
    `oauth_token_url`             TEXT,
    `oauth_scopes`                TEXT,
    `oauth_auth_method`           TEXT,
    `timeout`                     DOUBLE NOT NULL DEFAULT 0,
    `gamedig_given_port_only`     TINYINT(1) NOT NULL DEFAULT 1,
    `ip_family`                   VARCHAR(10),
    `cache_bust`                  TINYINT(1) DEFAULT 0,
    `save_response`               TINYINT(1) DEFAULT 0,
    `save_error_response`         TINYINT(1) DEFAULT 0,
    `response_max_length`         INT DEFAULT 1024,
    `screenshot`                  TINYINT(1) DEFAULT 0,
    `screenshot_delay`            INT DEFAULT 0,
    `conditions`                  TEXT DEFAULT ('[]'),
    `remote_browser`              INT UNSIGNED,
    `snmp_oid`                    TEXT DEFAULT ('1.3.6.1.2.1.1.3.0'),
    `snmp_version`                VARCHAR(10) DEFAULT '2c',
    `snmp_varlist`                TEXT,
    `snmp_community`              TEXT,
    `system_service_name`         VARCHAR(255),
    `tailscale_hostname`          VARCHAR(255),
    `tailscale_ping_count`        INT DEFAULT 4,
    `globalping_locations`        TEXT,
    `globalping_limit`            INT DEFAULT 10,
    `globalping_measurement_type` VARCHAR(50) DEFAULT 'ping',
    `ws_ignore_sec_websocket_accept` TINYINT(1) DEFAULT 0,
    `ws_subprotocol`              TEXT,
    `mqtt_check_type`             VARCHAR(20) DEFAULT 'topic',
    `rabbitmq_nodes`              TEXT,
    `rabbitmq_username`           VARCHAR(255),
    `rabbitmq_password`           VARCHAR(255),
    `snmp_v3_username`            VARCHAR(255),
    `smtp_security`               VARCHAR(255),
    `ws_ignore_sec_websocket_accept_header` TINYINT(1) NOT NULL DEFAULT 0,
    `ping_count`                  INT NOT NULL DEFAULT 1,
    `ping_numeric`                TINYINT(1) NOT NULL DEFAULT 1,
    `ping_per_request_timeout`    INT NOT NULL DEFAULT 2,
    `manual_status`               SMALLINT,
    `oauth_audience`              VARCHAR(255),
    `mqtt_websocket_path`         VARCHAR(255),
    `domain_expiry_notification`  TINYINT(1) DEFAULT 0,
    `subtype`                     VARCHAR(10),
    `location`                    VARCHAR(255),
    `protocol`                    VARCHAR(20),
    `expected_tls_alert`          VARCHAR(50),
    `retry_only_on_status_code_failure` TINYINT(1) NOT NULL DEFAULT 0,
    `bearer_token`                TEXT,
    `gamedig_token`               TEXT,
    `ssh_username`                VARCHAR(255),
    `ssh_password`                VARCHAR(255),
    `sftp_path`                   VARCHAR(255),
    `ssh_private_key`             TEXT,
    `ssh_passphrase`              VARCHAR(255),
    `ssh_auth_method`             VARCHAR(255) DEFAULT 'password',
    `ntp_stratum_threshold`       INT DEFAULT 5,
    `ntp_time_offset_threshold`   INT DEFAULT 1000,
    `ntp_root_dispersion_threshold` INT DEFAULT 500,
    `config`                      TEXT NOT NULL DEFAULT ('{}'),
    KEY `monitor_active` (`active`),
    KEY `monitor_user_id` (`user_id`)
);

CREATE TABLE IF NOT EXISTS `monitor_group` (
    `id`         INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `monitor_id` INT UNSIGNED NOT NULL,
    `group_id`   INT UNSIGNED NOT NULL,
    `weight`     INT NOT NULL DEFAULT 1000,
    `send_url`   TINYINT(1) NOT NULL DEFAULT 0,
    `custom_url` TEXT,
    KEY `fk` (`monitor_id`, `group_id`)
);

CREATE TABLE IF NOT EXISTS `heartbeat` (
    `id`         INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `important`  TINYINT(1) NOT NULL DEFAULT 0,
    `monitor_id` INT UNSIGNED NOT NULL,
    `status`     SMALLINT NOT NULL,
    `msg`        TEXT,
    `time`       DATETIME(3) NOT NULL,
    `ping`       INT,
    `duration`   INT NOT NULL DEFAULT 0,
    `down_count` INT NOT NULL DEFAULT 0,
    `end_time`   DATETIME(3),
    `retries`    INT NOT NULL DEFAULT 0,
    `response`   TEXT,
    KEY `monitor_time_index` (`monitor_id`, `time`),
    KEY `monitor_important_time_index` (`monitor_id`, `important`, `time`),
    KEY `heartbeat_important` (`important`),
    KEY `heartbeat_monitor_id` (`monitor_id`)
);

CREATE TABLE IF NOT EXISTS `stat_minutely` (
    `id`         INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `monitor_id` INT UNSIGNED NOT NULL,
    `timestamp`  INT NOT NULL,
    `ping`       DOUBLE NOT NULL,
    `ping_min`   DOUBLE NOT NULL DEFAULT 0,
    `ping_max`   DOUBLE NOT NULL DEFAULT 0,
    `up`         SMALLINT NOT NULL,
    `down`       SMALLINT NOT NULL,
    `extras`     TEXT,
    UNIQUE KEY `stat_minutely_unique` (`monitor_id`, `timestamp`)
);

CREATE TABLE IF NOT EXISTS `stat_hourly` (
    `id`         INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `monitor_id` INT UNSIGNED NOT NULL,
    `timestamp`  INT NOT NULL,
    `ping`       DOUBLE NOT NULL,
    `ping_min`   DOUBLE NOT NULL DEFAULT 0,
    `ping_max`   DOUBLE NOT NULL DEFAULT 0,
    `up`         SMALLINT NOT NULL,
    `down`       SMALLINT NOT NULL,
    `extras`     TEXT,
    UNIQUE KEY `stat_hourly_unique` (`monitor_id`, `timestamp`)
);

CREATE TABLE IF NOT EXISTS `stat_daily` (
    `id`         INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `monitor_id` INT UNSIGNED NOT NULL,
    `timestamp`  INT NOT NULL,
    `ping`       DOUBLE NOT NULL,
    `ping_min`   DOUBLE NOT NULL DEFAULT 0,
    `ping_max`   DOUBLE NOT NULL DEFAULT 0,
    `up`         SMALLINT NOT NULL,
    `down`       SMALLINT NOT NULL,
    `extras`     TEXT,
    UNIQUE KEY `stat_daily_unique` (`monitor_id`, `timestamp`)
);

CREATE TABLE IF NOT EXISTS `notification` (
    `id`         INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `name`       VARCHAR(255),
    `active`     TINYINT(1) NOT NULL DEFAULT 1,
    `user_id`    INT UNSIGNED,
    `is_default` TINYINT(1) NOT NULL DEFAULT 0,
    `config`     TEXT
);

CREATE TABLE IF NOT EXISTS `monitor_notification` (
    `id`              INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `monitor_id`      INT UNSIGNED NOT NULL,
    `notification_id` INT UNSIGNED NOT NULL,
    KEY `monitor_notification_index` (`monitor_id`, `notification_id`)
);

CREATE TABLE IF NOT EXISTS `notification_sent_history` (
    `id`         INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `type`       VARCHAR(50) NOT NULL,
    `monitor_id` INT UNSIGNED NOT NULL,
    `days`       INT NOT NULL,
    UNIQUE KEY `notification_sent_history_unique` (`type`, `monitor_id`, `days`)
);

CREATE TABLE IF NOT EXISTS `tag` (
    `id`           INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `name`         VARCHAR(255) NOT NULL,
    `color`        VARCHAR(255) NOT NULL,
    `created_date` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
);

CREATE TABLE IF NOT EXISTS `monitor_tag` (
    `id`         INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `monitor_id` INT UNSIGNED NOT NULL,
    `tag_id`     INT UNSIGNED NOT NULL,
    `value`      TEXT
);

CREATE TABLE IF NOT EXISTS `monitor_tls_info` (
    `id`         INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `monitor_id` INT UNSIGNED NOT NULL,
    `info_json`  TEXT
);

CREATE TABLE IF NOT EXISTS `domain_expiry` (
    `id`                            INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `domain`                        VARCHAR(255) NOT NULL,
    `last_check`                    DATETIME(3),
    `expiry`                        DATETIME(3),
    `last_expiry_notification_sent` INT DEFAULT NULL,
    UNIQUE KEY `domain_expiry_domain_unique` (`domain`)
);

CREATE TABLE IF NOT EXISTS `status_page` (
    `id`                      INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `slug`                    VARCHAR(255) NOT NULL,
    `title`                   VARCHAR(255) NOT NULL,
    `description`             TEXT,
    `icon`                    VARCHAR(255) NOT NULL,
    `theme`                   VARCHAR(30) NOT NULL,
    `published`               TINYINT(1) NOT NULL DEFAULT 1,
    `search_engine_index`     TINYINT(1) NOT NULL DEFAULT 1,
    `show_tags`               TINYINT(1) NOT NULL DEFAULT 0,
    `password`                VARCHAR(255),
    `created_date`            DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `modified_date`           DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `footer_text`             TEXT,
    `custom_css`              TEXT,
    `show_powered_by`         TINYINT(1) NOT NULL DEFAULT 1,
    `analytics_id`            VARCHAR(255),
    `analytics_script_url`    VARCHAR(255),
    `analytics_type`          VARCHAR(255) DEFAULT NULL,
    `auto_refresh_interval`   INT DEFAULT 300,
    `rss_title`               VARCHAR(255),
    `show_only_last_heartbeat` TINYINT(1) NOT NULL DEFAULT 0,
    `show_certificate_expiry` TINYINT(1) NOT NULL DEFAULT 0,
    UNIQUE KEY `status_page_slug_unique` (`slug`)
);

CREATE TABLE IF NOT EXISTS `status_page_cname` (
    `id`             INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `status_page_id` INT UNSIGNED,
    `domain`         VARCHAR(255) NOT NULL,
    UNIQUE KEY `status_page_cname_domain_unique` (`domain`)
);

CREATE TABLE IF NOT EXISTS `incident` (
    `id`                INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `title`             VARCHAR(255) NOT NULL,
    `content`           TEXT NOT NULL,
    `style`             VARCHAR(30) NOT NULL DEFAULT 'warning',
    `created_date`      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `last_updated_date` DATETIME(3),
    `pin`               TINYINT(1) NOT NULL DEFAULT 1,
    `active`            TINYINT(1) NOT NULL DEFAULT 1,
    `status_page_id`    INT UNSIGNED
);

CREATE TABLE IF NOT EXISTS `maintenance` (
    `id`              INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `title`           VARCHAR(150) NOT NULL,
    `description`     TEXT NOT NULL DEFAULT (''),
    `user_id`         INT UNSIGNED,
    `active`          TINYINT(1) NOT NULL DEFAULT 1,
    `strategy`        VARCHAR(50) NOT NULL DEFAULT 'single',
    `start_date`      DATETIME(3),
    `end_date`        DATETIME(3),
    `start_time`      TIME,
    `end_time`        TIME,
    `weekdays`        VARCHAR(250) DEFAULT '[]',
    `days_of_month`   TEXT DEFAULT ('[]'),
    `interval_day`    INT,
    `cron`            TEXT,
    `timezone`        VARCHAR(255),
    `duration`        INT,
    `last_start_date` DATETIME(3),
    KEY `maintenance_active` (`active`),
    KEY `manual_active` (`strategy`, `active`),
    KEY `maintenance_user_id` (`user_id`)
);

CREATE TABLE IF NOT EXISTS `maintenance_timeslot` (
    `id`             INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `maintenance_id` INT UNSIGNED NOT NULL,
    `start_date`     DATETIME(3) NOT NULL,
    `end_date`       DATETIME(3),
    `generated_next` TINYINT(1) DEFAULT 0,
    KEY `maintenance_timeslot_maintenance_id` (`maintenance_id`),
    KEY `active_timeslot_index` (`maintenance_id`, `start_date`, `end_date`),
    KEY `generated_next_index` (`generated_next`)
);

CREATE TABLE IF NOT EXISTS `maintenance_status_page` (
    `id`             INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `status_page_id` INT UNSIGNED NOT NULL,
    `maintenance_id` INT UNSIGNED NOT NULL
);

CREATE TABLE IF NOT EXISTS `monitor_maintenance` (
    `id`             INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `monitor_id`     INT UNSIGNED NOT NULL,
    `maintenance_id` INT UNSIGNED NOT NULL,
    KEY `monitor_maintenance_maintenance_id` (`maintenance_id`),
    KEY `monitor_maintenance_monitor_id` (`monitor_id`)
);

CREATE TABLE IF NOT EXISTS `setting` (
    `id`    INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `key`   VARCHAR(200) NOT NULL,
    `value` TEXT,
    `type`  VARCHAR(20),
    UNIQUE KEY `setting_key_unique` (`key`)
);

CREATE TABLE IF NOT EXISTS `api_key` (
    `id`           INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `key`          VARCHAR(255) NOT NULL,
    `name`         VARCHAR(255) NOT NULL,
    `user_id`      INT UNSIGNED NOT NULL,
    `created_date` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `active`       TINYINT(1) NOT NULL DEFAULT 1,
    `expires`      DATETIME(3)
);

CREATE TABLE IF NOT EXISTS `remote_browser` (
    `id`      INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `name`    VARCHAR(255) NOT NULL,
    `url`     VARCHAR(255) NOT NULL,
    `user_id` INT UNSIGNED NOT NULL
);

-- better-auth 核心表（官方 2025-10-29 迁移原样照搬）。
-- 注意：官方在 SQLite 里用 TEXT 主键；MySQL 的 TEXT 不能做主键/索引，
-- 这里统一按 VARCHAR(255)（better-auth 的 id / token / username 都远小于 255）。
CREATE TABLE IF NOT EXISTS `better_auth_user` (
    `id`              VARCHAR(255) PRIMARY KEY,
    `name`            TEXT NOT NULL,
    `email`           TEXT NOT NULL,
    `emailVerified`   INT NOT NULL DEFAULT 0,
    `image`           TEXT,
    `createdAt`       TIMESTAMP(3) NOT NULL,
    `updatedAt`       TIMESTAMP(3) NOT NULL,
    `username`        VARCHAR(255),
    `displayUsername` TEXT,
    `role`            TEXT,
    `banned`          INT DEFAULT 0,
    `banReason`       TEXT,
    `banExpires`      TIMESTAMP(3),
    `twoFactorEnabled` INT DEFAULT 0,
    UNIQUE KEY `better_auth_user_username_unique` (`username`)
);

CREATE TABLE IF NOT EXISTS `better_auth_session` (
    `id`        VARCHAR(255) PRIMARY KEY,
    `userId`    VARCHAR(255) NOT NULL,
    `token`     VARCHAR(255) NOT NULL,
    `expiresAt` TIMESTAMP(3) NOT NULL,
    `ipAddress` TEXT,
    `userAgent` TEXT,
    `createdAt` TIMESTAMP(3) NOT NULL,
    `updatedAt` TIMESTAMP(3) NOT NULL,
    `impersonatedBy` TEXT,
    KEY `better_auth_session_token` (`token`)
);

CREATE TABLE IF NOT EXISTS `better_auth_account` (
    `id`                    VARCHAR(255) PRIMARY KEY,
    `userId`                VARCHAR(255) NOT NULL,
    `accountId`             TEXT NOT NULL,
    `providerId`            TEXT NOT NULL,
    `accessToken`           TEXT,
    `refreshToken`          TEXT,
    `accessTokenExpiresAt`  TIMESTAMP(3),
    `refreshTokenExpiresAt` TIMESTAMP(3),
    `scope`                 TEXT,
    `idToken`               TEXT,
    `password`              TEXT,
    `createdAt`             TIMESTAMP(3) NOT NULL,
    `updatedAt`             TIMESTAMP(3) NOT NULL
);

CREATE TABLE IF NOT EXISTS `better_auth_verification` (
    `id`         VARCHAR(255) PRIMARY KEY,
    `identifier` TEXT NOT NULL,
    `value`      TEXT NOT NULL,
    `expiresAt`  TIMESTAMP(3) NOT NULL,
    `createdAt`  TIMESTAMP(3) NOT NULL,
    `updatedAt`  TIMESTAMP(3) NOT NULL
);

-- 官方 2026-06-29 迁移（@better-auth/api-key）原样照搬，列名 referenceId/configId 必须一致，
-- 否则接管官方库 / 官方接管 Go 库都会找不到列。
CREATE TABLE IF NOT EXISTS `better_auth_apikey` (
    `id`                  VARCHAR(255) PRIMARY KEY,
    `configId`            TEXT NOT NULL DEFAULT ('default'),
    `name`                TEXT,
    `start`               TEXT,
    `prefix`              TEXT,
    `key`                 VARCHAR(255) NOT NULL,
    `referenceId`         VARCHAR(255),
    `refillInterval`      INT,
    `refillAmount`        INT,
    `lastRefillAt`        TIMESTAMP(3),
    `enabled`             INT DEFAULT 1,
    `rateLimitEnabled`    INT DEFAULT 1,
    `rateLimitTimeWindow` INT,
    `rateLimitMax`        INT,
    `requestCount`        INT DEFAULT 0,
    `remaining`           INT,
    `lastRequest`         TIMESTAMP(3),
    `expiresAt`           TIMESTAMP(3),
    `createdAt`           TIMESTAMP(3) NOT NULL,
    `updatedAt`           TIMESTAMP(3) NOT NULL,
    `permissions`         TEXT,
    `metadata`            TEXT,
    KEY `better_auth_apikey_key` (`key`)
);

CREATE TABLE IF NOT EXISTS `better_auth_twofactor` (
    `id`                      VARCHAR(255) PRIMARY KEY,
    `secret`                  TEXT NOT NULL,
    `backupCodes`             TEXT NOT NULL,
    `userId`                  VARCHAR(255) NOT NULL,
    `verified`                INT DEFAULT 1,
    `failedVerificationCount` INT NOT NULL DEFAULT 0,
    `lockedUntil`             TIMESTAMP(3)
);

-- go_server 自用元数据表，官方版本不会读它
CREATE TABLE IF NOT EXISTS `_gokuma_meta` (
    `key`   VARCHAR(100) PRIMARY KEY,
    `value` TEXT
);

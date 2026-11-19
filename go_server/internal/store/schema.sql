-- go_server 的建库脚本。
--
-- 设计约束：
--   1. 表名、字段名、状态值必须与官方 Uptime Kuma 的 SQLite 库完全一致，
--      这样两边的 data/kuma.db 可以互相切换。
--   2. 全部使用 CREATE TABLE IF NOT EXISTS，可重复执行。
--   3. 官方 schema 由 66 个 knex 迁移叠加而成；这里是"基线 + 已知增量"的合并结果。
--      首次接入官方库时不会执行本文件（由 store.Migrate 判断），所以不会冲突。

PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS [user] (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      VARCHAR(255) NOT NULL,
    password      VARCHAR(255),
    active        BOOLEAN NOT NULL DEFAULT 1,
    timezone      VARCHAR(150),
    twofa_secret  VARCHAR(64),
    twofa_status  BOOLEAN NOT NULL DEFAULT 0,
    twofa_last_token VARCHAR(6)
);
CREATE UNIQUE INDEX IF NOT EXISTS user_username_unique ON [user] (username);

-- user_id 在官方 2026-05 的迁移里从 INTEGER 改成了可空的 VARCHAR(255)
-- （better-auth 的用户 ID 是 uuid），这里直接按新结构建表，新旧库都能读写。
CREATE TABLE IF NOT EXISTS [docker_host] (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id       VARCHAR(255),
    docker_daemon VARCHAR(255),
    docker_type   VARCHAR(255),
    name          VARCHAR(255)
);

CREATE TABLE IF NOT EXISTS [proxy] (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL,
    protocol     VARCHAR(10) NOT NULL,
    host         VARCHAR(255) NOT NULL,
    port         SMALLINT NOT NULL,
    auth         BOOLEAN NOT NULL DEFAULT 0,
    username     VARCHAR(255),
    password     VARCHAR(255),
    active       BOOLEAN NOT NULL DEFAULT 1,
    [default]    BOOLEAN NOT NULL DEFAULT 0,
    created_date DATETIME NOT NULL DEFAULT (DATETIME('now'))
);
CREATE INDEX IF NOT EXISTS proxy_user_id ON [proxy] (user_id);

CREATE TABLE IF NOT EXISTS [group] (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          VARCHAR(255) NOT NULL,
    created_date  DATETIME NOT NULL DEFAULT (DATETIME('now')),
    [public]      BOOLEAN NOT NULL DEFAULT 0,
    active        BOOLEAN NOT NULL DEFAULT 1,
    weight        INTEGER NOT NULL DEFAULT 1000,
    status_page_id INTEGER
);

CREATE TABLE IF NOT EXISTS [monitor] (
    id                          INTEGER PRIMARY KEY AUTOINCREMENT,
    name                        VARCHAR(150),
    active                      BOOLEAN NOT NULL DEFAULT 1,
    user_id                     INTEGER REFERENCES [user] (id) ON DELETE SET NULL ON UPDATE CASCADE,
    interval                    INTEGER NOT NULL DEFAULT 20,
    url                         TEXT,
    type                        VARCHAR(20),
    weight                      INTEGER DEFAULT 2000,
    hostname                    VARCHAR(255),
    port                        INTEGER,
    created_date                DATETIME NOT NULL DEFAULT (DATETIME('now')),
    keyword                     VARCHAR(255),
    maxretries                  INTEGER NOT NULL DEFAULT 0,
    ignore_tls                  BOOLEAN NOT NULL DEFAULT 0,
    upside_down                 BOOLEAN NOT NULL DEFAULT 0,
    maxredirects                INTEGER NOT NULL DEFAULT 10,
    accepted_statuscodes_json   TEXT NOT NULL DEFAULT '["200-299"]',
    dns_resolve_type            VARCHAR(5),
    dns_resolve_server          VARCHAR(255),
    dns_last_result             TEXT,
    retry_interval              INTEGER NOT NULL DEFAULT 0,
    push_token                  VARCHAR(32),
    method                      TEXT NOT NULL DEFAULT 'GET',
    body                        TEXT,
    headers                     TEXT,
    basic_auth_user             TEXT,
    basic_auth_pass             TEXT,
    docker_host                 INTEGER REFERENCES [docker_host] (id),
    docker_container            VARCHAR(255),
    proxy_id                    INTEGER REFERENCES [proxy] (id),
    expiry_notification         BOOLEAN DEFAULT 1,
    mqtt_topic                  TEXT,
    mqtt_success_message        VARCHAR(255),
    mqtt_username               VARCHAR(255),
    mqtt_password               VARCHAR(255),
    database_connection_string  VARCHAR(2000),
    database_query              TEXT,
    auth_method                 VARCHAR(250),
    auth_domain                 TEXT,
    auth_workstation            TEXT,
    grpc_url                    VARCHAR(255),
    grpc_protobuf               TEXT,
    grpc_body                   TEXT,
    grpc_metadata               TEXT,
    grpc_method                 TEXT,
    grpc_service_name           TEXT,
    grpc_enable_tls             BOOLEAN NOT NULL DEFAULT 0,
    radius_username             VARCHAR(255),
    radius_password             VARCHAR(255),
    radius_calling_station_id   VARCHAR(50),
    radius_called_station_id    VARCHAR(50),
    radius_secret               VARCHAR(255),
    resend_interval             INTEGER NOT NULL DEFAULT 0,
    packet_size                 INTEGER NOT NULL DEFAULT 56,
    game                        VARCHAR(255),
    http_body_encoding          VARCHAR(25),
    description                 TEXT,
    tls_ca                      TEXT,
    tls_cert                    TEXT,
    tls_key                     TEXT,
    parent                      INTEGER REFERENCES [monitor] (id) ON DELETE SET NULL ON UPDATE CASCADE,
    invert_keyword              BOOLEAN NOT NULL DEFAULT 0,
    json_path                   TEXT,
    expected_value              VARCHAR(255),
    json_path_operator          VARCHAR(10) DEFAULT '==',
    kafka_producer_topic        VARCHAR(255),
    kafka_producer_brokers      TEXT,
    kafka_producer_ssl          BOOLEAN NOT NULL DEFAULT 0,
    kafka_producer_allow_auto_topic_creation BOOLEAN NOT NULL DEFAULT 0,
    kafka_producer_sasl_options TEXT,
    kafka_producer_message      TEXT,
    oauth_client_id             TEXT,
    oauth_client_secret         TEXT,
    oauth_token_url             TEXT,
    oauth_scopes                TEXT,
    oauth_auth_method           TEXT,
    timeout                     DOUBLE NOT NULL DEFAULT 0,
    gamedig_given_port_only     BOOLEAN NOT NULL DEFAULT 1,
    ip_family                   VARCHAR(10),
    cache_bust                  BOOLEAN DEFAULT 0,
    save_response               BOOLEAN DEFAULT 0,
    save_error_response         BOOLEAN DEFAULT 0,
    response_max_length         INTEGER DEFAULT 1024,
    screenshot                  BOOLEAN DEFAULT 0,
    screenshot_delay            INTEGER DEFAULT 0,
    conditions                  TEXT DEFAULT '[]',
    remote_browser              INTEGER,
    snmp_oid                    TEXT DEFAULT '1.3.6.1.2.1.1.3.0',
    snmp_version                VARCHAR(10) DEFAULT '2c',
    snmp_varlist                TEXT,
    snmp_community              TEXT,
    system_service_name         VARCHAR(255),
    tailscale_hostname          VARCHAR(255),
    tailscale_ping_count        INTEGER DEFAULT 4,
    globalping_locations        TEXT,
    globalping_limit            INTEGER DEFAULT 10,
    globalping_measurement_type VARCHAR(50) DEFAULT 'ping',
    ws_ignore_sec_websocket_accept BOOLEAN DEFAULT 0,
    ws_subprotocol              TEXT,
    mqtt_check_type             VARCHAR(20) DEFAULT 'topic',
    rabbitmq_nodes              TEXT,
    rabbitmq_username           VARCHAR(255),
    rabbitmq_password           VARCHAR(255),
    -- 以下为 2024-08 之后各次迁移新增，缺了会导致对应监控类型存不住配置
    snmp_v3_username            VARCHAR(255),
    smtp_security               VARCHAR(255),
    ws_ignore_sec_websocket_accept_header BOOLEAN NOT NULL DEFAULT 0,
    ping_count                  INTEGER NOT NULL DEFAULT 1,
    ping_numeric                BOOLEAN NOT NULL DEFAULT 1,
    ping_per_request_timeout    INTEGER NOT NULL DEFAULT 2,
    manual_status               SMALLINT,
    oauth_audience              VARCHAR(255),
    mqtt_websocket_path         VARCHAR(255),
    domain_expiry_notification  BOOLEAN DEFAULT 0,
    subtype                     VARCHAR(10),
    location                    VARCHAR(255),
    protocol                    VARCHAR(20),
    expected_tls_alert          VARCHAR(50),
    retry_only_on_status_code_failure BOOLEAN NOT NULL DEFAULT 0,
    bearer_token                TEXT,
    gamedig_token               TEXT,
    ssh_username                VARCHAR(255),
    ssh_password                VARCHAR(255),
    sftp_path                   VARCHAR(255),
    ssh_private_key             TEXT,
    ssh_passphrase              VARCHAR(255),
    ssh_auth_method             VARCHAR(255) DEFAULT 'password',
    ntp_stratum_threshold       INTEGER DEFAULT 5,
    ntp_time_offset_threshold   INTEGER DEFAULT 1000,
    ntp_root_dispersion_threshold INTEGER DEFAULT 500,
    config                      TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS monitor_active ON [monitor] (active);
CREATE INDEX IF NOT EXISTS monitor_user_id ON [monitor] (user_id);

CREATE TABLE IF NOT EXISTS [monitor_group] (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL REFERENCES [monitor] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    group_id   INTEGER NOT NULL REFERENCES [group] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    weight     INTEGER NOT NULL DEFAULT 1000,
    send_url   BOOLEAN NOT NULL DEFAULT 0,
    custom_url TEXT
);
CREATE INDEX IF NOT EXISTS fk ON [monitor_group] (monitor_id, group_id);

CREATE TABLE IF NOT EXISTS [heartbeat] (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    important  BOOLEAN NOT NULL DEFAULT 0,
    monitor_id INTEGER NOT NULL REFERENCES [monitor] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    status     SMALLINT NOT NULL,
    msg        TEXT,
    time       DATETIME NOT NULL,
    ping       INTEGER,
    duration   INTEGER NOT NULL DEFAULT 0,
    down_count INTEGER NOT NULL DEFAULT 0,
    end_time   DATETIME,
    retries    INTEGER NOT NULL DEFAULT 0,
    response   TEXT
);
CREATE INDEX IF NOT EXISTS monitor_time_index ON [heartbeat] (monitor_id, time);
CREATE INDEX IF NOT EXISTS monitor_important_time_index ON [heartbeat] (monitor_id, important, time);
CREATE INDEX IF NOT EXISTS heartbeat_important ON [heartbeat] (important);
CREATE INDEX IF NOT EXISTS heartbeat_monitor_id ON [heartbeat] (monitor_id);

CREATE TABLE IF NOT EXISTS [stat_minutely] (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL REFERENCES [monitor] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    timestamp  INTEGER NOT NULL,
    ping       REAL NOT NULL,
    ping_min   REAL NOT NULL DEFAULT 0,
    ping_max   REAL NOT NULL DEFAULT 0,
    up         SMALLINT NOT NULL,
    down       SMALLINT NOT NULL,
    extras     TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS stat_minutely_unique ON [stat_minutely] (monitor_id, timestamp);

CREATE TABLE IF NOT EXISTS [stat_hourly] (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL REFERENCES [monitor] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    timestamp  INTEGER NOT NULL,
    ping       REAL NOT NULL,
    ping_min   REAL NOT NULL DEFAULT 0,
    ping_max   REAL NOT NULL DEFAULT 0,
    up         SMALLINT NOT NULL,
    down       SMALLINT NOT NULL,
    extras     TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS stat_hourly_unique ON [stat_hourly] (monitor_id, timestamp);

CREATE TABLE IF NOT EXISTS [stat_daily] (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL REFERENCES [monitor] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    timestamp  INTEGER NOT NULL,
    ping       REAL NOT NULL,
    ping_min   REAL NOT NULL DEFAULT 0,
    ping_max   REAL NOT NULL DEFAULT 0,
    up         SMALLINT NOT NULL,
    down       SMALLINT NOT NULL,
    extras     TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS stat_daily_unique ON [stat_daily] (monitor_id, timestamp);

CREATE TABLE IF NOT EXISTS [notification] (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       VARCHAR(255),
    active     BOOLEAN NOT NULL DEFAULT 1,
    user_id    INTEGER,
    is_default BOOLEAN NOT NULL DEFAULT 0,
    config     TEXT
);

CREATE TABLE IF NOT EXISTS [monitor_notification] (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id      INTEGER NOT NULL REFERENCES [monitor] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    notification_id INTEGER NOT NULL REFERENCES [notification] (id) ON DELETE CASCADE ON UPDATE CASCADE
);
CREATE INDEX IF NOT EXISTS monitor_notification_index ON [monitor_notification] (monitor_id, notification_id);

CREATE TABLE IF NOT EXISTS [notification_sent_history] (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    type       VARCHAR(50) NOT NULL,
    monitor_id INTEGER NOT NULL,
    days       INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS notification_sent_history_unique ON [notification_sent_history] (type, monitor_id, days);

CREATE TABLE IF NOT EXISTS [tag] (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         VARCHAR(255) NOT NULL,
    color        VARCHAR(255) NOT NULL,
    created_date DATETIME NOT NULL DEFAULT (DATETIME('now'))
);

CREATE TABLE IF NOT EXISTS [monitor_tag] (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL REFERENCES [monitor] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    tag_id     INTEGER NOT NULL REFERENCES [tag] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    value      TEXT
);

CREATE TABLE IF NOT EXISTS [monitor_tls_info] (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL REFERENCES [monitor] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    info_json  TEXT
);

CREATE TABLE IF NOT EXISTS [domain_expiry] (
    id                            INTEGER PRIMARY KEY AUTOINCREMENT,
    domain                        VARCHAR(255) NOT NULL,
    last_check                    DATETIME,
    expiry                        DATETIME,
    last_expiry_notification_sent INTEGER DEFAULT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS domain_expiry_domain_unique ON [domain_expiry] (domain);

CREATE TABLE IF NOT EXISTS [status_page] (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    slug                    VARCHAR(255) NOT NULL,
    title                   VARCHAR(255) NOT NULL,
    description             TEXT,
    icon                    VARCHAR(255) NOT NULL,
    theme                   VARCHAR(30) NOT NULL,
    published               BOOLEAN NOT NULL DEFAULT 1,
    search_engine_index     BOOLEAN NOT NULL DEFAULT 1,
    show_tags               BOOLEAN NOT NULL DEFAULT 0,
    password                VARCHAR(255),
    created_date            DATETIME NOT NULL DEFAULT (DATETIME('now')),
    modified_date           DATETIME NOT NULL DEFAULT (DATETIME('now')),
    footer_text             TEXT,
    custom_css              TEXT,
    show_powered_by         BOOLEAN NOT NULL DEFAULT 1,
    -- 2025-02-17 迁移把 google_analytics_tag_id 改名为 analytics_id
    analytics_id            VARCHAR(255),
    analytics_script_url    VARCHAR(255),
    analytics_type          VARCHAR(255) DEFAULT NULL,
    auto_refresh_interval   INTEGER DEFAULT 300,
    rss_title               VARCHAR(255),
    show_only_last_heartbeat BOOLEAN NOT NULL DEFAULT 0,
    show_certificate_expiry BOOLEAN NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS status_page_slug_unique ON [status_page] (slug);

CREATE TABLE IF NOT EXISTS [status_page_cname] (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    status_page_id INTEGER REFERENCES [status_page] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    domain        VARCHAR(255) NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS status_page_cname_domain_unique ON [status_page_cname] (domain);

CREATE TABLE IF NOT EXISTS [incident] (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    title             VARCHAR(255) NOT NULL,
    content           TEXT NOT NULL,
    style             VARCHAR(30) NOT NULL DEFAULT 'warning',
    created_date      DATETIME NOT NULL DEFAULT (DATETIME('now')),
    last_updated_date DATETIME,
    pin               BOOLEAN NOT NULL DEFAULT 1,
    active            BOOLEAN NOT NULL DEFAULT 1,
    status_page_id    INTEGER
);

CREATE TABLE IF NOT EXISTS [maintenance] (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    title         VARCHAR(150) NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    user_id       INTEGER REFERENCES [user] (id) ON DELETE SET NULL ON UPDATE CASCADE,
    active        BOOLEAN NOT NULL DEFAULT 1,
    strategy      VARCHAR(50) NOT NULL DEFAULT 'single',
    start_date    DATETIME,
    end_date      DATETIME,
    start_time    TIME,
    end_time      TIME,
    weekdays      VARCHAR(250) DEFAULT '[]',
    days_of_month TEXT DEFAULT '[]',
    interval_day  INTEGER,
    cron          TEXT,
    timezone      VARCHAR(255),
    duration      INTEGER,
    -- 2025-06-13 迁移新增，recurring-interval 用它判断间隔是否已满足
    last_start_date DATETIME
);
CREATE INDEX IF NOT EXISTS maintenance_active ON [maintenance] (active);
CREATE INDEX IF NOT EXISTS manual_active ON [maintenance] (strategy, active);
CREATE INDEX IF NOT EXISTS maintenance_user_id ON [maintenance] (user_id);

CREATE TABLE IF NOT EXISTS [maintenance_timeslot] (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    maintenance_id INTEGER NOT NULL REFERENCES [maintenance] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    start_date     DATETIME NOT NULL,
    end_date       DATETIME,
    generated_next BOOLEAN DEFAULT 0
);
CREATE INDEX IF NOT EXISTS maintenance_timeslot_maintenance_id ON [maintenance_timeslot] (maintenance_id);
CREATE INDEX IF NOT EXISTS active_timeslot_index ON [maintenance_timeslot] (maintenance_id, start_date, end_date);
CREATE INDEX IF NOT EXISTS generated_next_index ON [maintenance_timeslot] (generated_next);

CREATE TABLE IF NOT EXISTS [maintenance_status_page] (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    status_page_id INTEGER NOT NULL REFERENCES [status_page] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    maintenance_id INTEGER NOT NULL REFERENCES [maintenance] (id) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE IF NOT EXISTS [monitor_maintenance] (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id     INTEGER NOT NULL REFERENCES [monitor] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    maintenance_id INTEGER NOT NULL REFERENCES [maintenance] (id) ON DELETE CASCADE ON UPDATE CASCADE
);
CREATE INDEX IF NOT EXISTS monitor_maintenance_maintenance_id ON [monitor_maintenance] (maintenance_id);
CREATE INDEX IF NOT EXISTS monitor_maintenance_monitor_id ON [monitor_maintenance] (monitor_id);

CREATE TABLE IF NOT EXISTS [setting] (
    id    INTEGER PRIMARY KEY AUTOINCREMENT,
    [key] VARCHAR(200) NOT NULL,
    value TEXT,
    type  VARCHAR(20)
);
CREATE UNIQUE INDEX IF NOT EXISTS setting_key_unique ON [setting] ([key]);

CREATE TABLE IF NOT EXISTS [api_key] (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    [key]        VARCHAR(255) NOT NULL,
    name         VARCHAR(255) NOT NULL,
    user_id      INTEGER NOT NULL REFERENCES [user] (id) ON DELETE CASCADE ON UPDATE CASCADE,
    created_date DATETIME NOT NULL DEFAULT (DATETIME('now')),
    active       BOOLEAN NOT NULL DEFAULT 1,
    expires      DATETIME
);

CREATE TABLE IF NOT EXISTS [remote_browser] (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         VARCHAR(255) NOT NULL,
    url          VARCHAR(255) NOT NULL,
    user_id      INTEGER NOT NULL
);

-- better-auth 核心表（官方 2025-10-29 迁移原样照搬）
CREATE TABLE IF NOT EXISTS [better_auth_user] (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    email         TEXT NOT NULL,
    emailVerified INTEGER NOT NULL DEFAULT 0,
    image         TEXT,
    createdAt     TIMESTAMP NOT NULL,
    updatedAt     TIMESTAMP NOT NULL,
    username      TEXT,
    displayUsername TEXT,
    role          TEXT,
    banned        INTEGER DEFAULT 0,
    banReason     TEXT,
    banExpires    TIMESTAMP,
    twoFactorEnabled INTEGER DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS better_auth_user_username_unique ON [better_auth_user] (username);

CREATE TABLE IF NOT EXISTS [better_auth_session] (
    id        TEXT PRIMARY KEY,
    userId    TEXT NOT NULL REFERENCES [better_auth_user] (id),
    token     TEXT NOT NULL,
    expiresAt TIMESTAMP NOT NULL,
    ipAddress TEXT,
    userAgent TEXT,
    createdAt TIMESTAMP NOT NULL,
    updatedAt TIMESTAMP NOT NULL,
    impersonatedBy TEXT
);
CREATE INDEX IF NOT EXISTS better_auth_session_token ON [better_auth_session] (token);

CREATE TABLE IF NOT EXISTS [better_auth_account] (
    id                    TEXT PRIMARY KEY,
    userId                TEXT NOT NULL REFERENCES [better_auth_user] (id),
    accountId             TEXT NOT NULL,
    providerId            TEXT NOT NULL,
    accessToken           TEXT,
    refreshToken          TEXT,
    accessTokenExpiresAt  TIMESTAMP,
    refreshTokenExpiresAt TIMESTAMP,
    scope                 TEXT,
    idToken               TEXT,
    password              TEXT,
    createdAt             TIMESTAMP NOT NULL,
    updatedAt             TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS [better_auth_verification] (
    id         TEXT PRIMARY KEY,
    identifier TEXT NOT NULL,
    value      TEXT NOT NULL,
    expiresAt  TIMESTAMP NOT NULL,
    createdAt  TIMESTAMP NOT NULL,
    updatedAt  TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS [better_auth_apikey] (
    id           TEXT PRIMARY KEY,
    name         TEXT,
    start        TEXT,
    prefix       TEXT,
    [key]        TEXT NOT NULL,
    userId       TEXT NOT NULL REFERENCES [better_auth_user] (id),
    refillInterval INTEGER,
    refillAmount INTEGER,
    lastRefillAt TIMESTAMP,
    enabled      INTEGER DEFAULT 1,
    rateLimitEnabled INTEGER DEFAULT 1,
    rateLimitTimeWindow INTEGER,
    rateLimitMax INTEGER,
    requestCount INTEGER DEFAULT 0,
    remaining    INTEGER,
    lastRequest  TIMESTAMP,
    expiresAt    TIMESTAMP,
    createdAt    TIMESTAMP NOT NULL,
    updatedAt    TIMESTAMP NOT NULL,
    permissions  TEXT,
    metadata     TEXT
);

CREATE TABLE IF NOT EXISTS [better_auth_twofactor] (
    id          TEXT PRIMARY KEY,
    secret      TEXT NOT NULL,
    backupCodes TEXT NOT NULL,
    userId      TEXT NOT NULL REFERENCES [better_auth_user] (id),
    verified    INTEGER DEFAULT 1,
    failedVerificationCount INTEGER NOT NULL DEFAULT 0,
    lockedUntil TIMESTAMP
);

-- go_server 自用元数据表，官方版本不会读它
CREATE TABLE IF NOT EXISTS [_gokuma_meta] (
    [key]   VARCHAR(100) PRIMARY KEY,
    value   TEXT
);

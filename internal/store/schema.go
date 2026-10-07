package store

// 本文件集中定义两种数据库方言的建表语句。
//
// 约定（见 docs/implementation.md 10.2）：
//   - 时间统一用 BIGINT/INTEGER 存 Unix 毫秒，避免 DATETIME/TIMESTAMP 语义差异；
//   - 布尔统一用 0/1；
//   - 主键统一由应用生成 UUID 字符串，避免 AUTOINCREMENT / AUTO_INCREMENT 差异；
//   - meta 等半结构化字段统一用 TEXT 存 JSON，不使用 JSON 类型以保证方言一致。
//
// 两套语句必须保持结构等价（字段、索引、约束一一对应）。

// sqliteSchema 是 SQLite 的建表语句（按顺序执行）。
var sqliteSchema = []string{
	`CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS users (
		id            TEXT PRIMARY KEY,
		username      TEXT NOT NULL UNIQUE,
		role          TEXT NOT NULL,
		password_hash TEXT NOT NULL DEFAULT '',
		enabled       INTEGER NOT NULL DEFAULT 1,
		quota_bytes   INTEGER NOT NULL DEFAULT 0,
		used_bytes    INTEGER NOT NULL DEFAULT 0,
		remark        TEXT NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL,
		updated_at    INTEGER NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS certificates (
		id          TEXT PRIMARY KEY,
		user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		serial      TEXT NOT NULL UNIQUE,
		fingerprint TEXT NOT NULL UNIQUE,
		spki_sha256 TEXT NOT NULL,
		status      TEXT NOT NULL,
		bound_ip    TEXT NOT NULL DEFAULT '',
		bound_mac   TEXT NOT NULL DEFAULT '',
		not_before  INTEGER NOT NULL,
		not_after   INTEGER NOT NULL,
		created_at  INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_certificates_user ON certificates(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_certificates_spki ON certificates(spki_sha256)`,

	`CREATE TABLE IF NOT EXISTS repositories (
		id               TEXT PRIMARY KEY,
		name             TEXT NOT NULL UNIQUE,
		mode             TEXT NOT NULL,
		owner_id         TEXT NOT NULL REFERENCES users(id),
		parent_disk_id   TEXT,
		parent_version   INTEGER NOT NULL DEFAULT 0,
		parent_condition TEXT,
		max_diff_disks   INTEGER NOT NULL DEFAULT 50,
		quota_bytes      INTEGER NOT NULL DEFAULT 0,
		used_bytes       INTEGER NOT NULL DEFAULT 0,
		state            TEXT NOT NULL,
		meta             TEXT NOT NULL DEFAULT '{}',
		created_at       INTEGER NOT NULL,
		updated_at       INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_repositories_owner ON repositories(owner_id)`,
	`CREATE INDEX IF NOT EXISTS idx_repositories_state ON repositories(state)`,

	`CREATE TABLE IF NOT EXISTS disks (
		id                  TEXT PRIMARY KEY,
		repo_id             TEXT NOT NULL REFERENCES repositories(id),
		kind                TEXT NOT NULL,
		vhdx_path           TEXT NOT NULL UNIQUE,
		parent_id           TEXT,
		parent_version      INTEGER NOT NULL DEFAULT 0,
		content_fingerprint TEXT NOT NULL DEFAULT '',
		size_bytes          INTEGER NOT NULL,
		physical_bytes      INTEGER NOT NULL DEFAULT 0,
		vhd_type            TEXT NOT NULL,
		state               TEXT NOT NULL,
		desired_state       TEXT NOT NULL DEFAULT '',
		observed_state      TEXT NOT NULL DEFAULT '',
		mounted             INTEGER NOT NULL DEFAULT 0,
		file_system         TEXT NOT NULL DEFAULT '',
		created_at          INTEGER NOT NULL,
		updated_at          INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_disks_repo ON disks(repo_id)`,
	`CREATE INDEX IF NOT EXISTS idx_disks_parent ON disks(parent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_disks_kind ON disks(kind)`,

	`CREATE TABLE IF NOT EXISTS allocations (
		id         TEXT PRIMARY KEY,
		repo_id    TEXT NOT NULL REFERENCES repositories(id),
		disk_id    TEXT NOT NULL REFERENCES disks(id),
		user_id    TEXT NOT NULL,
		state      TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_allocations_disk ON allocations(disk_id)`,
	`CREATE INDEX IF NOT EXISTS idx_allocations_repo_user ON allocations(repo_id, user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_allocations_state ON allocations(state)`,

	`CREATE TABLE IF NOT EXISTS repo_members (
		repo_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
		user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		perm    TEXT NOT NULL,
		PRIMARY KEY (repo_id, user_id)
	)`,

	`CREATE TABLE IF NOT EXISTS iscsi_targets (
		id                     TEXT PRIMARY KEY,
		target_name            TEXT NOT NULL UNIQUE,
		disk_id                TEXT,
		purpose                TEXT NOT NULL,
		allocation_id          TEXT,
		auth_mode              TEXT NOT NULL,
		chap_user              TEXT NOT NULL DEFAULT '',
		chap_secret_enc        BLOB,
		reverse_chap_secret_enc BLOB,
		enabled                INTEGER NOT NULL DEFAULT 0,
		desired_enabled        INTEGER NOT NULL DEFAULT 1,
		read_only              INTEGER NOT NULL DEFAULT 0,
		applied_fingerprint    TEXT NOT NULL DEFAULT '',
		applied_at             INTEGER NOT NULL DEFAULT 0,
		actual_iqn             TEXT NOT NULL DEFAULT '',
		created_at             INTEGER NOT NULL,
		updated_at             INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_iscsi_targets_disk ON iscsi_targets(disk_id)`,
	`CREATE INDEX IF NOT EXISTS idx_iscsi_targets_alloc ON iscsi_targets(allocation_id)`,
	`CREATE INDEX IF NOT EXISTS idx_iscsi_targets_purpose ON iscsi_targets(purpose)`,

	`CREATE TABLE IF NOT EXISTS iscsi_initiator_ids (
		target_id TEXT NOT NULL REFERENCES iscsi_targets(id) ON DELETE CASCADE,
		id_type   TEXT NOT NULL,
		value     TEXT NOT NULL,
		PRIMARY KEY (target_id, id_type, value)
	)`,

	`CREATE TABLE IF NOT EXISTS leases (
		id            TEXT PRIMARY KEY,
		allocation_id TEXT NOT NULL,
		target_name   TEXT NOT NULL DEFAULT '',
		user_id       TEXT NOT NULL,
		client_id     TEXT NOT NULL,
		owner_token   TEXT NOT NULL DEFAULT '',
		mount_point   TEXT NOT NULL DEFAULT '',
		state         TEXT NOT NULL,
		expires_at    INTEGER NOT NULL,
		last_seen_at  INTEGER NOT NULL,
		created_at    INTEGER NOT NULL
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_leases_alloc_client ON leases(allocation_id, client_id)`,
	`CREATE INDEX IF NOT EXISTS idx_leases_state_expires ON leases(state, expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_leases_client ON leases(client_id)`,

	`CREATE TABLE IF NOT EXISTS jobs (
		id          TEXT PRIMARY KEY,
		type        TEXT NOT NULL,
		ref_id      TEXT NOT NULL DEFAULT '',
		idem_key    TEXT NOT NULL DEFAULT '',
		payload     TEXT NOT NULL DEFAULT '{}',
		state       TEXT NOT NULL,
		progress    INTEGER NOT NULL DEFAULT 0,
		attempt     INTEGER NOT NULL DEFAULT 0,
		last_error  TEXT NOT NULL DEFAULT '',
		lock_key    TEXT NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL,
		started_at  INTEGER NOT NULL DEFAULT 0,
		finished_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_idem ON jobs(idem_key) WHERE idem_key <> ''`,
	`CREATE INDEX IF NOT EXISTS idx_jobs_state ON jobs(state)`,
	`CREATE INDEX IF NOT EXISTS idx_jobs_lock_key ON jobs(lock_key)`,

	`CREATE TABLE IF NOT EXISTS uploads (
		id             TEXT PRIMARY KEY,
		user_id        TEXT NOT NULL,
		repo_name      TEXT NOT NULL,
		mode           TEXT NOT NULL,
		total_files    INTEGER NOT NULL DEFAULT 0,
		total_bytes    INTEGER NOT NULL DEFAULT 0,
		received_bytes INTEGER NOT NULL DEFAULT 0,
		staging_dir    TEXT NOT NULL,
		manifest       TEXT NOT NULL DEFAULT '{}',
		state          TEXT NOT NULL,
		created_at     INTEGER NOT NULL,
		updated_at     INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_uploads_user_state ON uploads(user_id, state)`,

	`CREATE TABLE IF NOT EXISTS upload_chunks (
		upload_id   TEXT NOT NULL REFERENCES uploads(id) ON DELETE CASCADE,
		chunk_index INTEGER NOT NULL,
		rel_path    TEXT NOT NULL,
		offset      INTEGER NOT NULL,
		size        INTEGER NOT NULL,
		checksum    TEXT NOT NULL,
		received_at INTEGER NOT NULL,
		PRIMARY KEY (upload_id, chunk_index)
	)`,

	`CREATE TABLE IF NOT EXISTS audit_logs (
		id         TEXT PRIMARY KEY,
		user_id    TEXT NOT NULL DEFAULT '',
		action     TEXT NOT NULL,
		resource   TEXT NOT NULL DEFAULT '',
		detail     TEXT NOT NULL DEFAULT '',
		ip         TEXT NOT NULL DEFAULT '',
		result     TEXT NOT NULL,
		created_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_user ON audit_logs(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_logs(action)`,

	`CREATE TABLE IF NOT EXISTS settings (
		k          TEXT PRIMARY KEY,
		v          TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	)`,

	// storages 是可在线管理的"存储"（VHDX 根目录）实体，数据库为唯一真源。
	// name / path 的唯一性在数据库层用 UNIQUE 约束保留；大小写不敏感的去重由应用层校验
	// （Windows 路径大小写不敏感，而 SQLite 的 TEXT UNIQUE 是区分大小写的）。
	`CREATE TABLE IF NOT EXISTS storages (
		id         TEXT PRIMARY KEY,
		name       TEXT NOT NULL UNIQUE,
		path       TEXT NOT NULL UNIQUE,
		enabled    INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_storages_name ON storages(name)`,
	`CREATE INDEX IF NOT EXISTS idx_storages_path ON storages(path)`,

	// storage_volumes 登记存储的底层卷（v3 新增，见 docs/implementation.md 5.1）。
	// 与 storages 一对一，**无行 = 目录模式**（Windows 全量 / Linux 未登记卷信息的历史存储）。
	// 挂载点不在此表存储，一律取 storages.path（避免双写漂移）。
	`CREATE TABLE IF NOT EXISTS storage_volumes (
		storage_id  TEXT PRIMARY KEY REFERENCES storages(id) ON DELETE CASCADE,
		kind        TEXT NOT NULL,
		managed     INTEGER NOT NULL DEFAULT 0,
		state       TEXT NOT NULL,
		ref         TEXT NOT NULL DEFAULT '',
		pool_ref    TEXT NOT NULL DEFAULT '',
		file_system TEXT NOT NULL DEFAULT '',
		size_bytes  INTEGER NOT NULL DEFAULT 0,
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_storage_volumes_state ON storage_volumes(state)`,
}

// mysqlSchema 是 MySQL 的等价建表语句。
//
// 差异点：TEXT 不能作主键/唯一索引，统一用 VARCHAR；BLOB 用 LONGBLOB；
// 部分索引（WHERE 子句）MySQL 不支持，改为在应用层保证幂等键唯一性。
var mysqlSchema = []string{
	`CREATE TABLE IF NOT EXISTS schema_version (
		version    BIGINT PRIMARY KEY,
		applied_at BIGINT NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS users (
		id            VARCHAR(64) PRIMARY KEY,
		username      VARCHAR(128) NOT NULL UNIQUE,
		role          VARCHAR(32) NOT NULL,
		password_hash VARCHAR(255) NOT NULL DEFAULT '',
		enabled       TINYINT(1) NOT NULL DEFAULT 1,
		quota_bytes   BIGINT NOT NULL DEFAULT 0,
		used_bytes    BIGINT NOT NULL DEFAULT 0,
		remark        VARCHAR(512) NOT NULL DEFAULT '',
		created_at    BIGINT NOT NULL,
		updated_at    BIGINT NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS certificates (
		id          VARCHAR(64) PRIMARY KEY,
		user_id     VARCHAR(64) NOT NULL,
		serial      VARCHAR(128) NOT NULL UNIQUE,
		fingerprint VARCHAR(128) NOT NULL UNIQUE,
		spki_sha256 VARCHAR(128) NOT NULL,
		status      VARCHAR(32) NOT NULL,
		bound_ip    VARCHAR(128) NOT NULL DEFAULT '',
		bound_mac   VARCHAR(64) NOT NULL DEFAULT '',
		not_before  BIGINT NOT NULL,
		not_after   BIGINT NOT NULL,
		created_at  BIGINT NOT NULL,
		KEY idx_certificates_user (user_id),
		KEY idx_certificates_spki (spki_sha256),
		CONSTRAINT fk_certificates_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS repositories (
		id               VARCHAR(64) PRIMARY KEY,
		name             VARCHAR(255) NOT NULL UNIQUE,
		mode             VARCHAR(32) NOT NULL,
		owner_id         VARCHAR(64) NOT NULL,
		parent_disk_id   VARCHAR(64) NULL,
		parent_version   INT NOT NULL DEFAULT 0,
		parent_condition VARCHAR(32) NULL,
		max_diff_disks   INT NOT NULL DEFAULT 50,
		quota_bytes      BIGINT NOT NULL DEFAULT 0,
		used_bytes       BIGINT NOT NULL DEFAULT 0,
		state            VARCHAR(32) NOT NULL,
		meta             LONGTEXT NOT NULL,
		created_at       BIGINT NOT NULL,
		updated_at       BIGINT NOT NULL,
		KEY idx_repositories_owner (owner_id),
		KEY idx_repositories_state (state)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS disks (
		id                  VARCHAR(64) PRIMARY KEY,
		repo_id             VARCHAR(64) NOT NULL,
		kind                VARCHAR(32) NOT NULL,
		vhdx_path           VARCHAR(512) NOT NULL UNIQUE,
		parent_id           VARCHAR(64) NULL,
		parent_version      INT NOT NULL DEFAULT 0,
		content_fingerprint VARCHAR(255) NOT NULL DEFAULT '',
		size_bytes          BIGINT NOT NULL,
		physical_bytes      BIGINT NOT NULL DEFAULT 0,
		vhd_type            VARCHAR(32) NOT NULL,
		state               VARCHAR(32) NOT NULL,
		desired_state       VARCHAR(32) NOT NULL DEFAULT '',
		observed_state      VARCHAR(32) NOT NULL DEFAULT '',
		mounted             TINYINT(1) NOT NULL DEFAULT 0,
		file_system         VARCHAR(16) NOT NULL DEFAULT '',
		created_at          BIGINT NOT NULL,
		updated_at          BIGINT NOT NULL,
		KEY idx_disks_repo (repo_id),
		KEY idx_disks_parent (parent_id),
		KEY idx_disks_kind (kind)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS allocations (
		id         VARCHAR(64) PRIMARY KEY,
		repo_id    VARCHAR(64) NOT NULL,
		disk_id    VARCHAR(64) NOT NULL,
		user_id    VARCHAR(64) NOT NULL,
		state      VARCHAR(32) NOT NULL,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL,
		UNIQUE KEY uk_allocations_disk (disk_id),
		KEY idx_allocations_repo_user (repo_id, user_id),
		KEY idx_allocations_state (state)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS repo_members (
		repo_id VARCHAR(64) NOT NULL,
		user_id VARCHAR(64) NOT NULL,
		perm    VARCHAR(32) NOT NULL,
		PRIMARY KEY (repo_id, user_id),
		CONSTRAINT fk_members_repo FOREIGN KEY (repo_id) REFERENCES repositories(id) ON DELETE CASCADE,
		CONSTRAINT fk_members_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS iscsi_targets (
		id                      VARCHAR(64) PRIMARY KEY,
		target_name             VARCHAR(255) NOT NULL UNIQUE,
		disk_id                 VARCHAR(64) NULL,
		purpose                 VARCHAR(32) NOT NULL,
		allocation_id           VARCHAR(64) NULL,
		auth_mode               VARCHAR(32) NOT NULL,
		chap_user               VARCHAR(128) NOT NULL DEFAULT '',
		chap_secret_enc         LONGBLOB NULL,
		reverse_chap_secret_enc LONGBLOB NULL,
		enabled                 TINYINT(1) NOT NULL DEFAULT 0,
		desired_enabled         TINYINT(1) NOT NULL DEFAULT 1,
		read_only               TINYINT(1) NOT NULL DEFAULT 0,
		applied_fingerprint     VARCHAR(64) NOT NULL DEFAULT '',
		applied_at              BIGINT NOT NULL DEFAULT 0,
		actual_iqn              VARCHAR(255) NOT NULL DEFAULT '',
		created_at              BIGINT NOT NULL,
		updated_at              BIGINT NOT NULL,
		KEY idx_iscsi_targets_disk (disk_id),
		KEY idx_iscsi_targets_alloc (allocation_id),
		KEY idx_iscsi_targets_purpose (purpose)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS iscsi_initiator_ids (
		target_id VARCHAR(64) NOT NULL,
		id_type   VARCHAR(32) NOT NULL,
		value     VARCHAR(255) NOT NULL,
		PRIMARY KEY (target_id, id_type, value),
		CONSTRAINT fk_initiator_target FOREIGN KEY (target_id) REFERENCES iscsi_targets(id) ON DELETE CASCADE
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS leases (
		id            VARCHAR(64) PRIMARY KEY,
		allocation_id VARCHAR(64) NOT NULL,
		target_name   VARCHAR(255) NOT NULL DEFAULT '',
		user_id       VARCHAR(64) NOT NULL,
		client_id     VARCHAR(128) NOT NULL,
		owner_token   VARCHAR(128) NOT NULL DEFAULT '',
		mount_point   VARCHAR(512) NOT NULL DEFAULT '',
		state         VARCHAR(32) NOT NULL,
		expires_at    BIGINT NOT NULL,
		last_seen_at  BIGINT NOT NULL,
		created_at    BIGINT NOT NULL,
		UNIQUE KEY uk_leases_alloc_client (allocation_id, client_id),
		KEY idx_leases_state_expires (state, expires_at),
		KEY idx_leases_client (client_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS jobs (
		id          VARCHAR(64) PRIMARY KEY,
		type        VARCHAR(64) NOT NULL,
		ref_id      VARCHAR(64) NOT NULL DEFAULT '',
		idem_key    VARCHAR(255) NOT NULL DEFAULT '',
		payload     LONGTEXT NOT NULL,
		state       VARCHAR(32) NOT NULL,
		progress    INT NOT NULL DEFAULT 0,
		attempt     INT NOT NULL DEFAULT 0,
		last_error  LONGTEXT NOT NULL,
		lock_key    VARCHAR(255) NOT NULL DEFAULT '',
		created_at  BIGINT NOT NULL,
		started_at  BIGINT NOT NULL DEFAULT 0,
		finished_at BIGINT NOT NULL DEFAULT 0,
		UNIQUE KEY uk_jobs_idem (idem_key),
		KEY idx_jobs_state (state),
		KEY idx_jobs_lock_key (lock_key)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS uploads (
		id             VARCHAR(64) PRIMARY KEY,
		user_id        VARCHAR(64) NOT NULL,
		repo_name      VARCHAR(255) NOT NULL,
		mode           VARCHAR(16) NOT NULL,
		total_files    INT NOT NULL DEFAULT 0,
		total_bytes    BIGINT NOT NULL DEFAULT 0,
		received_bytes BIGINT NOT NULL DEFAULT 0,
		staging_dir    VARCHAR(512) NOT NULL,
		manifest       LONGTEXT NOT NULL,
		state          VARCHAR(32) NOT NULL,
		created_at     BIGINT NOT NULL,
		updated_at     BIGINT NOT NULL,
		KEY idx_uploads_user_state (user_id, state)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS upload_chunks (
		upload_id   VARCHAR(64) NOT NULL,
		chunk_index INT NOT NULL,
		rel_path    VARCHAR(1024) NOT NULL,
		offset      BIGINT NOT NULL,
		size        BIGINT NOT NULL,
		checksum    VARCHAR(128) NOT NULL,
		received_at BIGINT NOT NULL,
		PRIMARY KEY (upload_id, chunk_index),
		CONSTRAINT fk_chunks_upload FOREIGN KEY (upload_id) REFERENCES uploads(id) ON DELETE CASCADE
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS audit_logs (
		id         VARCHAR(64) PRIMARY KEY,
		user_id    VARCHAR(64) NOT NULL DEFAULT '',
		action     VARCHAR(128) NOT NULL,
		resource   VARCHAR(255) NOT NULL DEFAULT '',
		detail     LONGTEXT NOT NULL,
		ip         VARCHAR(64) NOT NULL DEFAULT '',
		result     VARCHAR(32) NOT NULL,
		created_at BIGINT NOT NULL,
		KEY idx_audit_created (created_at),
		KEY idx_audit_user (user_id),
		KEY idx_audit_action (action)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	`CREATE TABLE IF NOT EXISTS settings (
		k          VARCHAR(128) PRIMARY KEY,
		v          LONGTEXT NOT NULL,
		updated_at BIGINT NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	// storages 与 SQLite 版本结构等价（见 schema.go 顶部约定）。
	`CREATE TABLE IF NOT EXISTS storages (
		id         VARCHAR(64) PRIMARY KEY,
		name       VARCHAR(128) NOT NULL UNIQUE,
		path       VARCHAR(512) NOT NULL UNIQUE,
		enabled    TINYINT(1) NOT NULL DEFAULT 1,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL,
		KEY idx_storages_name (name),
		KEY idx_storages_path (path)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,

	// storage_volumes 与 SQLite 版本结构等价（见 schema.go 顶部约定）。
	// 外键级联删除交由应用层保证（MySQL 侧不加 FOREIGN KEY，与其它表保持一致）。
	`CREATE TABLE IF NOT EXISTS storage_volumes (
		storage_id  VARCHAR(64) PRIMARY KEY,
		kind        VARCHAR(16) NOT NULL,
		managed     TINYINT(1) NOT NULL DEFAULT 0,
		state       VARCHAR(16) NOT NULL,
		ref         VARCHAR(512) NOT NULL DEFAULT '',
		pool_ref    VARCHAR(160) NOT NULL DEFAULT '',
		file_system VARCHAR(32) NOT NULL DEFAULT '',
		size_bytes  BIGINT NOT NULL DEFAULT 0,
		created_at  BIGINT NOT NULL,
		updated_at  BIGINT NOT NULL,
		KEY idx_storage_volumes_state (state)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,
}

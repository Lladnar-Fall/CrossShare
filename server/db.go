package main

import (
	"database/sql"
	"log"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

var DB *sql.DB

const schema = `
CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    email TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS devices (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    name TEXT NOT NULL,
    os TEXT NOT NULL,
    token_hash TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    last_seen DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS items (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    origin_device TEXT NOT NULL REFERENCES devices(id),
    kind TEXT NOT NULL,
    mime TEXT NOT NULL DEFAULT 'text/plain',
    filename TEXT DEFAULT '',
    size INTEGER NOT NULL DEFAULT 0,
    sha256 TEXT NOT NULL DEFAULT '',
    payload TEXT,
    blob_path TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS deliveries (
    item_id TEXT NOT NULL REFERENCES items(id),
    device_id TEXT NOT NULL REFERENCES devices(id),
    state TEXT NOT NULL DEFAULT 'pending',
    delivered_at DATETIME,
    PRIMARY KEY (item_id, device_id)
);
`

func InitDB() {
	var err error
	DB, err = sql.Open("sqlite3", Config.DBPath)
	if err != nil {
		log.Fatal(err)
	}

	if _, err := DB.Exec(schema); err != nil {
		log.Fatal("Failed to run migrations:", err)
	}

	// Migration: remember content encoding (e.g. "gzip") for offline backlog.
	if _, err := DB.Exec("ALTER TABLE items ADD COLUMN encoding TEXT DEFAULT ''"); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			log.Fatal("Failed to migrate items table:", err)
		}
	}

	// WAL mode + relaxed sync = ~1000x faster writes
	DB.Exec("PRAGMA journal_mode=WAL")
	DB.Exec("PRAGMA synchronous=NORMAL")
}

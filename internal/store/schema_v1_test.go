package store

// 0.1.x（索引库格式 1）的表结构，用于测试升级。
const schemaV1 = `
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT);
CREATE TABLE IF NOT EXISTS volumes(
  id TEXT PRIMARY KEY, label TEXT NOT NULL DEFAULT '', serial TEXT NOT NULL DEFAULT '',
  guid TEXT NOT NULL DEFAULT '', capacity INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL DEFAULT 'local', last_mount TEXT NOT NULL DEFAULT '', last_seen INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS roots(
  id INTEGER PRIMARY KEY, volume_id TEXT NOT NULL, rel_path TEXT NOT NULL,
  paused INTEGER NOT NULL DEFAULT 0, opts TEXT NOT NULL DEFAULT '{}', added_at INTEGER NOT NULL,
  last_scan INTEGER NOT NULL DEFAULT 0, UNIQUE(volume_id, rel_path));
CREATE TABLE IF NOT EXISTS files(
  id INTEGER PRIMARY KEY, root_id INTEGER NOT NULL, volume_id TEXT NOT NULL,
  rel_path TEXT NOT NULL, inner TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL, ext TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0, mtime INTEGER NOT NULL DEFAULT 0,
  state INTEGER NOT NULL DEFAULT 0, err TEXT NOT NULL DEFAULT '', hash TEXT NOT NULL DEFAULT '',
  text_len INTEGER NOT NULL DEFAULT 0, ocr INTEGER NOT NULL DEFAULT 0, meta TEXT NOT NULL DEFAULT '',
  prio INTEGER NOT NULL DEFAULT 1, indexed_at INTEGER NOT NULL DEFAULT 0,
  UNIQUE(volume_id, rel_path, inner));
CREATE INDEX IF NOT EXISTS files_root ON files(root_id);
CREATE INDEX IF NOT EXISTS files_state ON files(state, prio, id);
CREATE INDEX IF NOT EXISTS files_hash ON files(hash) WHERE hash <> '';
CREATE INDEX IF NOT EXISTS files_stats ON files(root_id, state, prio, ocr);
CREATE INDEX IF NOT EXISTS files_queue ON files(prio, mtime) WHERE state=0;
CREATE TABLE IF NOT EXISTS texts(file_id INTEGER PRIMARY KEY, flags INTEGER NOT NULL DEFAULT 0, data BLOB NOT NULL, locs BLOB);
CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(c, n, content='', contentless_delete=1, tokenize='unicode61 remove_diacritics 0');
CREATE VIRTUAL TABLE IF NOT EXISTS fts_v USING fts5vocab(fts, 'row');
`

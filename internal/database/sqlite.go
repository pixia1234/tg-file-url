package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/pixia1234/tg-file-url/internal/config"
	"github.com/pixia1234/tg-file-url/internal/files"
)

type Store struct {
	db *sql.DB
}

type Stats struct {
	Users int64 `json:"users"`
	Files int64 `json:"files"`
}

func Open(cfg *config.Config) (*Store, error) {
	if err := cfg.EnsureDataDir(); err != nil {
		return nil, fmt.Errorf("create sqlite directory: %w", err)
	}

	db, err := sql.Open("sqlite3", cfg.SQLiteDSN())
	if err != nil {
		return nil, err
	}

	store := &Store{db: db}
	if err := store.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) init(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA foreign_keys = ON;`,
		`CREATE TABLE IF NOT EXISTS users (
			user_id INTEGER PRIMARY KEY,
			username TEXT,
			first_name TEXT,
			last_name TEXT,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()),
			last_seen_at INTEGER NOT NULL DEFAULT (unixepoch())
		);`,
		`CREATE TABLE IF NOT EXISTS files (
			message_id INTEGER PRIMARY KEY,
			secure_hash TEXT NOT NULL,
			link_token TEXT NOT NULL,
			storage_chat_id INTEGER NOT NULL,
			source_chat_id INTEGER NOT NULL,
			source_message_id INTEGER NOT NULL,
			file_id TEXT NOT NULL,
			file_unique_id TEXT NOT NULL,
			file_name TEXT NOT NULL,
			mime_type TEXT NOT NULL,
			file_size INTEGER NOT NULL,
			uploader_user_id INTEGER NOT NULL,
			uploader_display_name TEXT NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		);`,
		`CREATE TABLE IF NOT EXISTS authorized_users (
			user_id INTEGER PRIMARY KEY,
			authorized_by INTEGER NOT NULL,
			authorized_at INTEGER NOT NULL DEFAULT (unixepoch())
		);`,
		`CREATE TABLE IF NOT EXISTS banned_users (
			user_id INTEGER PRIMARY KEY,
			reason TEXT NOT NULL,
			banned_by INTEGER NOT NULL,
			banned_at INTEGER NOT NULL DEFAULT (unixepoch())
		);`,
		`CREATE TABLE IF NOT EXISTS banned_channels (
			channel_id INTEGER PRIMARY KEY,
			reason TEXT NOT NULL,
			banned_by INTEGER NOT NULL,
			banned_at INTEGER NOT NULL DEFAULT (unixepoch())
		);`,
		`CREATE TABLE IF NOT EXISTS restart_notices (
			notice_id INTEGER PRIMARY KEY CHECK (notice_id = 1),
			chat_id INTEGER NOT NULL,
			message_id INTEGER NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_files_secure_hash_message ON files(secure_hash, message_id);`,
		`CREATE INDEX IF NOT EXISTS idx_files_created_at ON files(created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_authorized_users_authorized_at ON authorized_users(authorized_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_banned_users_banned_at ON banned_users(banned_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_banned_channels_banned_at ON banned_channels(banned_at DESC);`,
	}

	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}

	if err := s.ensureFilesLinkTokenColumn(ctx); err != nil {
		return err
	}
	if err := s.backfillLinkTokens(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_files_link_token ON files(link_token);`); err != nil {
		return err
	}

	return nil
}

func (s *Store) ensureFilesLinkTokenColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(files);`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &primaryKey); err != nil {
			return err
		}
		if name == "link_token" {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `ALTER TABLE files ADD COLUMN link_token TEXT;`)
	return err
}

func (s *Store) backfillLinkTokens(ctx context.Context) error {
	type pendingToken struct {
		messageID int64
		uniqueID  string
	}

	rows, err := s.db.QueryContext(
		ctx,
		`SELECT message_id, file_unique_id
		FROM files
		WHERE COALESCE(link_token, '') = ''`,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	var pending []pendingToken
	for rows.Next() {
		var item pendingToken
		if err := rows.Scan(&item.messageID, &item.uniqueID); err != nil {
			return err
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if len(pending) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	stmt, err := tx.PrepareContext(ctx, `UPDATE files SET link_token = ? WHERE message_id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, item := range pending {
		if _, err := stmt.ExecContext(ctx, files.ComputeLinkToken(item.uniqueID, item.messageID), item.messageID); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) UpsertUser(ctx context.Context, userID int64, username, firstName, lastName string) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO users (user_id, username, first_name, last_name, created_at, last_seen_at)
		 VALUES (?, ?, ?, ?, unixepoch(), unixepoch())
		 ON CONFLICT(user_id) DO UPDATE SET
			username = excluded.username,
			first_name = excluded.first_name,
			last_name = excluded.last_name,
			last_seen_at = unixepoch();`,
		userID, username, firstName, lastName,
	)
	return err
}

func (s *Store) SaveFile(ctx context.Context, record files.Record) error {
	if record.SecureHash == "" {
		record.SecureHash = files.ComputeSecureHash(record.FileUniqueID)
	}
	if record.LinkToken == "" {
		record.LinkToken = files.ComputeLinkToken(record.FileUniqueID, record.MessageID)
	}

	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO files (
			message_id, secure_hash, link_token, storage_chat_id, source_chat_id, source_message_id,
			file_id, file_unique_id, file_name, mime_type, file_size,
			uploader_user_id, uploader_display_name, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, unixepoch())
		ON CONFLICT(message_id) DO UPDATE SET
			secure_hash = excluded.secure_hash,
			link_token = excluded.link_token,
			storage_chat_id = excluded.storage_chat_id,
			source_chat_id = excluded.source_chat_id,
			source_message_id = excluded.source_message_id,
			file_id = excluded.file_id,
			file_unique_id = excluded.file_unique_id,
			file_name = excluded.file_name,
			mime_type = excluded.mime_type,
			file_size = excluded.file_size,
			uploader_user_id = excluded.uploader_user_id,
			uploader_display_name = excluded.uploader_display_name;`,
		record.MessageID,
		record.SecureHash,
		record.LinkToken,
		record.StorageChatID,
		record.SourceChatID,
		record.SourceMessageID,
		record.FileID,
		record.FileUniqueID,
		record.FileName,
		record.MimeType,
		record.FileSize,
		record.UploaderUserID,
		record.UploaderDisplayName,
	)
	return err
}

func (s *Store) GetFile(ctx context.Context, messageID int64) (files.Record, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT
			message_id, secure_hash, link_token, storage_chat_id, source_chat_id, source_message_id,
			file_id, file_unique_id, file_name, mime_type, file_size,
			uploader_user_id, uploader_display_name, created_at
		FROM files
		WHERE message_id = ?`,
		messageID,
	)

	var record files.Record
	var createdAtUnix int64
	if err := row.Scan(
		&record.MessageID,
		&record.SecureHash,
		&record.LinkToken,
		&record.StorageChatID,
		&record.SourceChatID,
		&record.SourceMessageID,
		&record.FileID,
		&record.FileUniqueID,
		&record.FileName,
		&record.MimeType,
		&record.FileSize,
		&record.UploaderUserID,
		&record.UploaderDisplayName,
		&createdAtUnix,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return files.Record{}, ErrNotFound
		}
		return files.Record{}, err
	}

	record.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
	return record, nil
}

func (s *Store) GetFileByLinkToken(ctx context.Context, linkToken string) (files.Record, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT
			message_id, secure_hash, link_token, storage_chat_id, source_chat_id, source_message_id,
			file_id, file_unique_id, file_name, mime_type, file_size,
			uploader_user_id, uploader_display_name, created_at
		FROM files
		WHERE link_token = ?`,
		linkToken,
	)

	var record files.Record
	var createdAtUnix int64
	if err := row.Scan(
		&record.MessageID,
		&record.SecureHash,
		&record.LinkToken,
		&record.StorageChatID,
		&record.SourceChatID,
		&record.SourceMessageID,
		&record.FileID,
		&record.FileUniqueID,
		&record.FileName,
		&record.MimeType,
		&record.FileSize,
		&record.UploaderUserID,
		&record.UploaderDisplayName,
		&createdAtUnix,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return files.Record{}, ErrNotFound
		}
		return files.Record{}, err
	}

	record.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
	return record, nil
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var stats Stats
	row := s.db.QueryRowContext(
		ctx,
		`SELECT
			(SELECT COUNT(*) FROM users),
			(SELECT COUNT(*) FROM files)`,
	)

	if err := row.Scan(&stats.Users, &stats.Files); err != nil {
		return Stats{}, err
	}
	return stats, nil
}

var ErrNotFound = errors.New("record not found")

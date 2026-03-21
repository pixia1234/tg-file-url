package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type AuthorizedUser struct {
	UserID       int64
	AuthorizedBy int64
	AuthorizedAt time.Time
}

type BannedUser struct {
	UserID   int64
	Reason   string
	BannedBy int64
	BannedAt time.Time
}

type BannedChannel struct {
	ChannelID int64
	Reason    string
	BannedBy  int64
	BannedAt  time.Time
}

type RestartNotice struct {
	ChatID    int64
	MessageID int64
	CreatedAt time.Time
}

func (s *Store) AuthorizeUser(ctx context.Context, userID, authorizedBy int64) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO authorized_users (user_id, authorized_by, authorized_at)
		 VALUES (?, ?, unixepoch())
		 ON CONFLICT(user_id) DO UPDATE SET
			authorized_by = excluded.authorized_by,
			authorized_at = unixepoch();`,
		userID, authorizedBy,
	)
	return err
}

func (s *Store) DeauthorizeUser(ctx context.Context, userID int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM authorized_users WHERE user_id = ?`, userID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (s *Store) IsUserAuthorized(ctx context.Context, userID int64) (bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT 1 FROM authorized_users WHERE user_id = ? LIMIT 1`, userID)
	var exists int
	err := row.Scan(&exists)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

func (s *Store) ListAuthorizedUsers(ctx context.Context) ([]AuthorizedUser, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT user_id, authorized_by, authorized_at
		 FROM authorized_users
		 ORDER BY authorized_at DESC, user_id ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []AuthorizedUser
	for rows.Next() {
		var user AuthorizedUser
		var authorizedAt int64
		if err := rows.Scan(&user.UserID, &user.AuthorizedBy, &authorizedAt); err != nil {
			return nil, err
		}
		user.AuthorizedAt = time.Unix(authorizedAt, 0).UTC()
		users = append(users, user)
	}

	return users, rows.Err()
}

func (s *Store) BanUser(ctx context.Context, userID, bannedBy int64, reason string) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO banned_users (user_id, reason, banned_by, banned_at)
		 VALUES (?, ?, ?, unixepoch())
		 ON CONFLICT(user_id) DO UPDATE SET
			reason = excluded.reason,
			banned_by = excluded.banned_by,
			banned_at = unixepoch();`,
		userID, reason, bannedBy,
	)
	return err
}

func (s *Store) UnbanUser(ctx context.Context, userID int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM banned_users WHERE user_id = ?`, userID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (s *Store) GetBannedUser(ctx context.Context, userID int64) (BannedUser, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT user_id, reason, banned_by, banned_at
		 FROM banned_users
		 WHERE user_id = ?`,
		userID,
	)

	var ban BannedUser
	var bannedAt int64
	if err := row.Scan(&ban.UserID, &ban.Reason, &ban.BannedBy, &bannedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BannedUser{}, ErrNotFound
		}
		return BannedUser{}, err
	}
	ban.BannedAt = time.Unix(bannedAt, 0).UTC()
	return ban, nil
}

func (s *Store) BanChannel(ctx context.Context, channelID, bannedBy int64, reason string) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO banned_channels (channel_id, reason, banned_by, banned_at)
		 VALUES (?, ?, ?, unixepoch())
		 ON CONFLICT(channel_id) DO UPDATE SET
			reason = excluded.reason,
			banned_by = excluded.banned_by,
			banned_at = unixepoch();`,
		channelID, reason, bannedBy,
	)
	return err
}

func (s *Store) UnbanChannel(ctx context.Context, channelID int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM banned_channels WHERE channel_id = ?`, channelID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (s *Store) GetBannedChannel(ctx context.Context, channelID int64) (BannedChannel, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT channel_id, reason, banned_by, banned_at
		 FROM banned_channels
		 WHERE channel_id = ?`,
		channelID,
	)

	var ban BannedChannel
	var bannedAt int64
	if err := row.Scan(&ban.ChannelID, &ban.Reason, &ban.BannedBy, &bannedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BannedChannel{}, ErrNotFound
		}
		return BannedChannel{}, err
	}
	ban.BannedAt = time.Unix(bannedAt, 0).UTC()
	return ban, nil
}

func (s *Store) ListAllUserIDs(ctx context.Context) ([]int64, error) {
	return s.listInt64Column(ctx, `SELECT user_id FROM users ORDER BY user_id ASC`)
}

func (s *Store) ListAuthorizedUserIDs(ctx context.Context) ([]int64, error) {
	return s.listInt64Column(ctx, `SELECT user_id FROM authorized_users ORDER BY user_id ASC`)
}

func (s *Store) ListRegularUserIDs(ctx context.Context) ([]int64, error) {
	return s.listInt64Column(
		ctx,
		`SELECT user_id
		 FROM users
		 WHERE user_id NOT IN (SELECT user_id FROM authorized_users)
		 ORDER BY user_id ASC`,
	)
}

func (s *Store) DeleteUser(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE user_id = ?`, userID)
	return err
}

func (s *Store) SaveRestartNotice(ctx context.Context, chatID, messageID int64) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO restart_notices (notice_id, chat_id, message_id, created_at)
		 VALUES (1, ?, ?, unixepoch())
		 ON CONFLICT(notice_id) DO UPDATE SET
			chat_id = excluded.chat_id,
			message_id = excluded.message_id,
			created_at = unixepoch();`,
		chatID, messageID,
	)
	return err
}

func (s *Store) GetRestartNotice(ctx context.Context) (RestartNotice, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT chat_id, message_id, created_at
		 FROM restart_notices
		 WHERE notice_id = 1`,
	)

	var notice RestartNotice
	var createdAt int64
	if err := row.Scan(&notice.ChatID, &notice.MessageID, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RestartNotice{}, ErrNotFound
		}
		return RestartNotice{}, err
	}
	notice.CreatedAt = time.Unix(createdAt, 0).UTC()
	return notice, nil
}

func (s *Store) ClearRestartNotice(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM restart_notices WHERE notice_id = 1`)
	return err
}

func (s *Store) listInt64Column(ctx context.Context, query string, args ...any) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var values []int64
	for rows.Next() {
		var value int64
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}

	return values, rows.Err()
}

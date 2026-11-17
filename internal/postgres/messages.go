package postgres

import (
	"context"
	"math"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MITRAKER/realtime-chat/internal/auth"
	"github.com/MITRAKER/realtime-chat/internal/chat"
)

// MessageStore implements chat.Store.
type MessageStore struct{ pool *pgxpool.Pool }

func NewMessageStore(pool *pgxpool.Pool) *MessageStore { return &MessageStore{pool: pool} }

func (s *MessageStore) SaveMessage(ctx context.Context, room string, from auth.User, text string) (chat.Outbound, error) {
	m := chat.Outbound{Type: chat.TypeMessage, Room: room, UserID: from.ID, User: from.Username, Text: text}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO messages (room, user_id, text) VALUES ($1, $2, $3)
		 RETURNING id, created_at`,
		room, from.ID, text,
	).Scan(&m.ID, &m.Time)
	m.Time = m.Time.UTC()
	return m, err
}

// History uses "keyset pagination": instead of OFFSET 500 (which makes
// Postgres read and throw away 500 rows), we ask for rows below the last
// id the browser has. With the (room, id) index that is equally fast for
// page 1 and page 1000, and new messages arriving don't shift the pages.
func (s *MessageStore) History(ctx context.Context, room string, before int64, limit int) ([]chat.Outbound, bool, error) {
	if before <= 0 {
		before = math.MaxInt64
	}
	// Ask for one extra row: if it comes back, there are older messages.
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.user_id, u.username, m.text, m.created_at
		 FROM messages m JOIN users u ON u.id = m.user_id
		 WHERE m.room = $1 AND m.id < $2
		 ORDER BY m.id DESC
		 LIMIT $3`,
		room, before, limit+1,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	msgs := make([]chat.Outbound, 0, limit+1)
	for rows.Next() {
		m := chat.Outbound{Type: chat.TypeMessage, Room: room}
		if err := rows.Scan(&m.ID, &m.UserID, &m.User, &m.Text, &m.Time); err != nil {
			return nil, false, err
		}
		m.Time = m.Time.UTC()
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(msgs) > limit
	if more {
		msgs = msgs[:limit]
	}
	slices.Reverse(msgs) // newest-first from the query; the browser wants oldest-first
	return msgs, more, nil
}

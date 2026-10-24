package db

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// NewConversationFile describes one attachment row: the queryable index of a
// message.text event's file (conversation, event, uploader, storage key,
// type, size and original filename). The event log stays the truth; this is
// an index only ever inserted, never updated.
type NewConversationFile struct {
	ConversationID int64
	EventID        int64 // the message.text event
	ActorID        int64 // who uploaded
	Key            string
	Mime           string
	Size           int64
	Name           string // original filename, display only
}

// InsertConversationFile inserts one row. q should be the caller's
// transaction, so the index commits (or rolls back) with the event it points
// at — inside Post both happen in one transaction.
func (s *Store) InsertConversationFile(ctx context.Context, q DBTX, in NewConversationFile) error {
	_, err := q.Exec(ctx, s.q("InsertConversationFile"), pgx.NamedArgs{
		"conversation_id": in.ConversationID, "event_id": in.EventID, "actor_id": in.ActorID,
		"key": in.Key, "mime": in.Mime, "size": in.Size, "name": in.Name,
	})
	return err
}

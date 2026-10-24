package svc_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

// Posting with files writes one conversation_files row per file in the same
// transaction as the message: the conversation, the message.text event, the
// uploader, the key, and the type, size and name.
func TestPostInsertsConversationFiles(t *testing.T) {
	store := dbtest.New(t, nil)
	s := svc.New(store, &config.Config{Queue: config.Queue{MaxAttempts: 3}})
	ctx := context.Background()
	conv, _ := store.CreateConversation(ctx)
	user, _ := store.FindOrCreateActor(ctx, store.Pool, types.ActorExternal, "u1")
	files := []svc.PostedFile{
		{Key: "conversations/x/a.png", Mime: "image/png", Size: 12, Name: "a.png"},
		{Key: "conversations/x/b.txt", Mime: "text/plain", Size: 3, Name: "b.txt"},
	}
	if _, err := s.Post(ctx, svc.PostInput{ConversationID: conv.ID, Actor: user, Scope: "u1", Text: "hi", Files: files}); err != nil {
		t.Fatal(err)
	}
	type row struct {
		key, mime, name string
		size            int64
	}
	rows, err := store.Pool.Query(ctx, `SELECT f.key, f.mime, f.size, f.name
		FROM conversation_files f
		JOIN conversation_events e ON e.id = f.event_id AND e.type = 'message.text'
		JOIN actors a ON a.id = f.actor_id AND a.type = 'external' AND a.ref = 'u1'
		WHERE f.conversation_id = $1
		ORDER BY f.id`, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.key, &r.mime, &r.size, &r.name); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (row{"conversations/x/a.png", "image/png", "a.png", 12}) ||
		got[1] != (row{"conversations/x/b.txt", "text/plain", "b.txt", 3}) {
		t.Fatalf("indexed files: %+v", got)
	}
}

// The row insert lives inside Post's transaction: any failure rolls the
// message events and the file rows back together.
func TestPostFilesFailTogether(t *testing.T) {
	store := dbtest.New(t, nil)
	s := svc.New(store, &config.Config{Queue: config.Queue{MaxAttempts: 3}})
	ctx := context.Background()
	conv, _ := store.CreateConversation(ctx)
	user, _ := store.FindOrCreateActor(ctx, store.Pool, types.ActorExternal, "u1")
	// A storage key is unique: reposting the same one fails the second
	// transaction's file insert, after its events were appended.
	in := svc.PostInput{ConversationID: conv.ID, Actor: user, Scope: "u1", Text: "one",
		Files: []svc.PostedFile{{Key: "conversations/c/dup.png", Mime: "image/png", Size: 1, Name: "a.png"}}}
	if _, err := s.Post(ctx, in); err != nil {
		t.Fatal(err)
	}
	var before int64
	store.Pool.QueryRow(ctx, `SELECT count(*) FROM conversation_events WHERE conversation_id = $1`, conv.ID).Scan(&before)
	var bad *svc.BadRequestError
	if _, err := s.Post(ctx, in); err == nil || errors.As(err, &bad) {
		t.Fatalf("reposting a storage key must fail with a server error, got %v", err)
	}
	var after int64
	store.Pool.QueryRow(ctx, `SELECT count(*) FROM conversation_events WHERE conversation_id = $1`, conv.ID).Scan(&after)
	if after != before {
		t.Fatalf("a failed post must append no events: %d -> %d", before, after)
	}
	var files int64
	store.Pool.QueryRow(ctx, `SELECT count(*) FROM conversation_files`).Scan(&files)
	if files != 1 {
		t.Fatalf("only the committed post's file row may exist, got %d", files)
	}
}

// The attachment name is display text: cleaned in place (so the message
// stores the same text), at most 255 bytes, single-line.
func TestValidateAttachmentNames(t *testing.T) {
	cfg := &config.Config{Agent: config.Agent{Settings: config.Settings{Vision: true}}}
	fileAtt := func(name string) []types.Attachment {
		return []types.Attachment{{File: "conversations/c/a.png", Mime: "image/png", Name: name}}
	}
	if err := svc.ValidateAttachments(cfg, &cfg.Agent, fileAtt(strings.Repeat("a", 255))); err != nil {
		t.Errorf("255 bytes: %v", err)
	}
	for name, att := range map[string][]types.Attachment{
		"too long": fileAtt(strings.Repeat("a", 256)),
		"newline":  fileAtt("a\nb"),
		"carriage": fileAtt("a\rb"),
	} {
		if err := svc.ValidateAttachments(cfg, &cfg.Agent, att); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	if err := svc.ValidateAttachments(cfg, &cfg.Agent, fileAtt("")); err != nil {
		t.Errorf("an empty name stays optional: %v", err)
	}
	att := fileAtt(" ré.txt\x00 ")
	if err := svc.ValidateAttachments(cfg, &cfg.Agent, att); err != nil {
		t.Fatal(err)
	}
	if att[0].Name != "ré.txt" {
		t.Fatalf("the name must be cleaned in place: %q", att[0].Name)
	}
}

package httpapi

import (
	"net/http"
	"strings"
	"uuid"

	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

const (
	maxHostEventBody = 128 << 10 // a 16 KiB text can take 6 bytes a byte as JSON escapes
	maxKeyLen        = 128
	defaultSource    = "host"
)

type hostEventRequest struct {
	Type   string  `json:"type"` // "note" or "message"
	Text   string  `json:"text"`
	Key    string  `json:"key"`
	Source *string `json:"source"` // the host's name for itself; absent, null or "" is "host"
}

// postHostEvent is server to server: your backend tells the model something
// about a conversation (a note) or tells the user and the model (a message). It
// never starts a turn: the event is filed under the turn that is running, else
// the one that runs first, else the newest, and the model reads it at its next
// step or turn. 201 with the event the first time; a repeat of key, whatever
// else it says, is 200 with the first event.
func (s *Server) postHostEvent(w http.ResponseWriter, r *http.Request) error {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return &httpError{http.StatusNotFound, "not found"}
	}
	var req hostEventRequest
	if err := bindStrictJSON(w, r, maxHostEventBody, &req); err != nil {
		return err
	}
	var typ types.EventType
	switch req.Type {
	case "note":
		typ = types.EventContextNote
	case "message":
		typ = types.EventHostMessage
	default:
		return &httpError{http.StatusBadRequest, `type must be "note" or "message"`}
	}
	if !validKey(req.Key) {
		return &httpError{http.StatusBadRequest, "key is required: 1 to 128 of letters, digits and . _ : / @ + = -"}
	}
	ctx := r.Context()
	conv, err := s.store.ConversationByUUID(ctx, id)
	if err != nil {
		return err // unknown: 404
	}
	source := defaultSource
	if req.Source != nil && *req.Source != "" {
		source = *req.Source
	}
	row, created, err := s.svc.AppendNoteOnce(ctx, svc.NoteInput{
		ConversationID: conv.ID, HostRef: source, Type: typ, Text: req.Text, Key: req.Key, Limit: svc.MaxHostEventsPerTurn})
	if err != nil {
		return err
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	return writeJSON(w, code, row.Envelope())
}

const keyChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._:/@+=-"

// validKey: 1 to 128 bytes of ASCII letters, digits and . _ : / @ + = -
// (ids, uuids, "order-17:paid"); no space, quote, control or non-ASCII byte.
func validKey(k string) bool {
	if k == "" || len(k) > maxKeyLen {
		return false
	}
	for i := range len(k) {
		if strings.IndexByte(keyChars, k[i]) < 0 {
			return false
		}
	}
	return true
}

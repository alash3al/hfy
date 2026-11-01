package httpapi

import (
	"net/http"
	"uuid"

	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

const (
	maxHostEventBody = 128 << 10 // a 16 KiB text can take 6 bytes a byte as JSON escapes
	defaultSource    = "host"
)

type hostEventRequest struct {
	Type   string  `json:"type"` // "note" or "message"
	Text   string  `json:"text"`
	Source *string `json:"source"` // the host's name for itself; absent, null or "" is "host"
}

// postHostEvent is server to server: your backend tells the model something
// about a conversation (a note) or tells the user and the model (a message). It
// never starts a turn: the event is filed under the turn that is running, else
// the one that runs first, else the newest, and the model reads it at its next
// step or turn. 201 with the event.
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
	ctx := r.Context()
	conv, err := s.store.ConversationByUUID(ctx, id)
	if err != nil {
		return err // unknown: 404
	}
	source := defaultSource
	if req.Source != nil && *req.Source != "" {
		source = *req.Source
	}
	actor, err := s.svc.HostActor(ctx, source)
	if err != nil {
		return err
	}
	row, err := s.svc.AppendNote(ctx, svc.NoteInput{
		ConversationID: conv.ID, Actor: actor, Type: typ, Text: req.Text, Limit: svc.MaxHostEventsPerTurn})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, row.Envelope())
}

package history

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// DefaultWebJournalRetention is how long after it was last written or
// touched a send journal entry is kept before the agent asks the relay
// whether its request is finished (pruneSettledJournal). Age alone never
// drops an entry: a chatgpt-web request never expires at the relay, so one
// whose agent was offline for days still comes back, and an entry dropped
// while its request could still come back would let it be typed into its
// conversation a second time. Only once the relay says the request is
// finished (answered, failed, declined, cancelled, expired) or no longer
// this agent's is its entry dropped.
const DefaultWebJournalRetention = 24*time.Hour + 30*time.Minute + time.Hour

// webJournal is the send journal: request id -> what was sent for it. It
// holds ids and times only, never messages or replies. A requeued request
// found here is resumed by reading its reply from the conversation again
// rather than by sending it again (or by keeping reply text on disk).
type webJournal struct {
	Requests map[string]webSend `json:"requests"`
}

const (
	webSendIntent   = "image_intent"
	webSendSent     = "sent"
	webSendAnswered = "answered"
)

type webSend struct {
	Input          bool   `json:"input,omitempty"`
	ConversationID string `json:"conversation_id"`
	// UserMessageID is this request's user message, once seen.
	UserMessageID string `json:"user_message_id,omitempty"`
	// PrevUserID is the conversation's last user message before the send.
	PrevUserID  string    `json:"prev_user_id,omitempty"`
	SubmittedAt time.Time `json:"submitted_at,omitzero"`
	// Recorded is when the entry was first written; Touched, when later,
	// is when the wait last renewed its claim. Pruning goes by the later.
	Recorded time.Time `json:"recorded"`
	Touched  time.Time `json:"touched,omitzero"`
	// State is sent, or answered once the reply was built and sent.
	State string `json:"state"`
}

func (w *WebAgent) journalRetention() time.Duration {
	if w.JournalRetention > 0 {
		return w.JournalRetention
	}
	return DefaultWebJournalRetention
}

// loadJournal reads the journal, dropping entries whose ids are malformed.
// It never drops one for its age (see pruneSettledJournal). A missing or
// unreadable journal is empty.
func (w *WebAgent) loadJournal() webJournal {
	j := webJournal{Requests: map[string]webSend{}}
	if w.JournalPath == "" {
		return j
	}
	b, err := os.ReadFile(w.JournalPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			w.logf("journal %s: %v (starting fresh)", w.JournalPath, err)
		}
		return j
	}
	if err := json.Unmarshal(b, &j); err != nil {
		w.logf("journal %s: %v (starting fresh)", w.JournalPath, err)
		j = webJournal{}
	}
	if j.Requests == nil {
		j.Requests = map[string]webSend{}
	}
	for id, e := range j.Requests {
		if e.State == webSendIntent || (e.Input && e.State != webSendAnswered) {
			continue
		}
		if !validNativeID(e.ConversationID) || (e.UserMessageID != "" && !validNativeID(e.UserMessageID)) {
			delete(j.Requests, id)
		}
	}
	return j
}

// pruneSettledJournal drops the journal entries older than the retention
// whose requests the relay says are finished, or are no longer this
// agent's (not found). An entry whose request is still live (held, queued,
// delivered, claimed or waiting for input), or whose status could not be
// read (the relay unreachable), is kept, however old: a request can come
// back after any outage, and its entry is what stops it being sent again.
// Unresolved image intents are never dropped, as before. A journal that
// cannot be read safely is left as it is. Callers hold w.mu.
func (w *WebAgent) pruneSettledJournal(ctx context.Context) {
	if w.JournalPath == "" || w.Relay == nil {
		return
	}
	j, err := w.loadInputJournal()
	if err != nil {
		return
	}
	cutoff := w.clk().Now().Add(-w.journalRetention())
	changed := false
	for id, e := range j.Requests {
		if e.State == webSendIntent || (e.Input && e.State != webSendAnswered) {
			continue
		}
		if !e.Recorded.Before(cutoff) || !e.Touched.Before(cutoff) {
			continue
		}
		if w.requestSettled(ctx, id) {
			delete(j.Requests, id)
			changed = true
		}
	}
	if changed {
		if err := w.writeJournal(j); err != nil {
			w.logf("journal %s: %v", w.JournalPath, err)
		}
	}
}

// requestSettled reports whether the relay says request id is finished or
// no longer this agent's. Any other answer, an error included, is false.
func (w *WebAgent) requestSettled(ctx context.Context, id string) bool {
	if !validNativeID(id) {
		return true
	}
	gctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := w.Relay.Get(gctx, id, 0)
	if err != nil {
		return client.IsStatus(err, http.StatusNotFound) || client.IsStatus(err, http.StatusForbidden)
	}
	switch res.Status {
	case envelope.StatusAnswered, envelope.StatusFailed, envelope.StatusDeclined, envelope.StatusCancelled, envelope.StatusExpired:
		return true
	}
	return false
}

// journal records (or updates) reqID's entry and prunes old ones. Callers
// hold w.mu.
func (w *WebAgent) journal(reqID string, e webSend) {
	if w.JournalPath == "" {
		return
	}
	// Never let a later text send overwrite an unreadable image journal.
	if _, err := w.loadInputJournal(); err != nil {
		w.logf("journal could not be read; not overwriting it")
		return
	}
	j := w.loadJournal()
	j.Requests[reqID] = e
	b, err := json.MarshalIndent(j, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(w.JournalPath), 0o700)
	}
	if err == nil {
		err = writeFileAtomic(w.JournalPath, append(b, '\n'), 0o600)
	}
	if err != nil {
		w.logf("journal %s: %v", w.JournalPath, err)
	}
}

// Image sends fail closed on journal read/write errors. Unresolved intents
// have no retention deadline, including new chats without conversation ids.
func (w *WebAgent) loadInputJournal() (webJournal, error) {
	j := webJournal{Requests: map[string]webSend{}}
	if w.JournalPath == "" {
		return j, errors.New("image input requires a send journal")
	}
	b, err := os.ReadFile(w.JournalPath)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	if j.Requests == nil {
		return j, errors.New("invalid image send journal")
	}
	for _, e := range j.Requests {
		if e.State != webSendIntent && e.State != webSendSent && e.State != webSendAnswered {
			return j, errors.New("unknown send journal state")
		}
		if e.State != webSendIntent && !validNativeID(e.ConversationID) {
			return j, errors.New("invalid journal conversation")
		}
	}
	return j, nil
}

func (w *WebAgent) journalStrict(reqID string, e webSend) error {
	j, err := w.loadInputJournal()
	if err != nil {
		return err
	}
	j.Requests[reqID] = e
	return w.writeJournal(j)
}

// writeJournal replaces the journal with j, synced to disk (0600).
func (w *WebAgent) writeJournal(j webJournal) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(w.JournalPath), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(w.JournalPath), ".image-journal-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), w.JournalPath); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(w.JournalPath))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

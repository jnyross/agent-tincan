package history

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// relayClock is a web agent clock that also drives the relay: each sleep
// moves the relay store's clock forward with it and runs a relay sweep, the
// pass that requeues a claim whose lease ran out. So an answer that takes
// an hour, against the relay's 30-minute claim lease (10 for an urgent
// ask), runs in milliseconds through the real relay, client and web agent.
type relayClock struct {
	mesh *testrelay.Mesh
	mu   sync.Mutex
	t    time.Time
	from time.Time
}

// newLongRig is a web rig whose relay, store and web agent all run on one
// relayClock.
func newLongRig(t *testing.T) (*webRig, *relayClock) {
	t.Helper()
	now := time.Now()
	c := &relayClock{t: now, from: now}
	rig := newWebRigWith(t, relay.Config{Now: c.Now}, true)
	c.mesh = rig.mesh
	rig.mesh.Store.SetClock(c.Now)
	rig.agent.clock = c
	return rig, c
}

func (c *relayClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *relayClock) elapsed() time.Duration { return c.Now().Sub(c.from) }

func (c *relayClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	c.mesh.Server.Sweep(ctx)
	return nil
}

// longAnswer asks chatgpt-web on the relay clock while ChatGPT takes about
// 50 minutes (the default read schedule's 20-second polls) to finish, and
// returns the result the asker reads. The proof tests use only names that
// exist before the fix, so the same test shows the failure there.
func longAnswer(t *testing.T, rig *webRig, clock *relayClock, urgent bool) (envelope.Result, string) {
	t.Helper()
	rig.agent.PollInterval = 0
	rig.browser.inProgress = 150
	ctx := t.Context()
	grok := rig.mesh.Client(t, "grokbot")
	req, err := grok.Send(ctx, "chatgpt-web", "Think hard for an hour about tin cans", envelope.KindAsk, "", urgent)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := rig.agent.PollOnce(ctx); err != nil || n != 1 {
		t.Fatalf("PollOnce = %d, %v", n, err)
	}
	res, err := grok.Get(ctx, req.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return res, req.ID
}

// assertDeliveredOnce checks a long answer reached the asker as answered,
// after more than 30 minutes, with one send, one tab close, and no requeue
// or lapsed-claim note on the way.
func assertDeliveredOnce(t *testing.T, rig *webRig, res envelope.Result, clock *relayClock, id string) {
	t.Helper()
	if res.Reply == nil || res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "You said: Think hard for an hour about tin cans") {
		body := ""
		if res.Reply != nil {
			body = res.Reply.Body
		}
		t.Fatalf("after %s: status %s, reply %q, relay note %+v", clock.elapsed().Round(time.Second), res.Status, body, res.RelayNote)
	}
	if got := clock.elapsed(); got < 45*time.Minute {
		t.Fatalf("the answer took only %s; the proof needs one longer than the claim lease", got)
	}
	if res.RelayNote != nil {
		t.Fatalf("the asker was told %q", res.RelayNote.Note)
	}
	events, err := rig.mesh.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.RequestID == id && (e.Event == "requeued" || e.Event == "expired") {
			t.Fatalf("the relay %s the request mid-answer", e.Event)
		}
	}
	if got := rig.browser.sent(); len(got) != 1 {
		t.Fatalf("sends = %d, want exactly one: nothing may be typed into the running conversation", len(got))
	}
	if got := rig.browser.closed(); len(got) != 1 {
		t.Fatalf("closes = %v", got)
	}
}

// Limit (a), behaviourally: a ChatGPT answer that takes about 50 minutes,
// with chatgpt-web on its defaults, is delivered rather than cut off at the
// old 8-minute request timeout.
func TestWebLongChatGPTAnswerIsDelivered(t *testing.T) {
	rig, clock := newLongRig(t)
	res, id := longAnswer(t, rig, clock, false)
	assertDeliveredOnce(t, rig, res, clock, id)
}

// Limit (c), behaviourally: with a request timeout long enough, a claim the
// agent holds through a 50-minute wait is never requeued at the relay's
// 30-minute lease (10 for an urgent ask), and the asker is never told it
// lapsed: the agent's progress notes renew it.
func TestWebLongAnswerKeepsItsClaim(t *testing.T) {
	for _, urgent := range []bool{false, true} {
		name := "ask"
		if urgent {
			name = "urgent"
		}
		t.Run(name, func(t *testing.T) {
			rig, clock := newLongRig(t)
			rig.agent.RequestTimeout = 5 * time.Hour
			res, id := longAnswer(t, rig, clock, urgent)
			assertDeliveredOnce(t, rig, res, clock, id)
		})
	}
}

// No follow-up, behaviourally: a request the relay hands back after the
// agent died hours into its wait is resumed from the send journal, never
// typed into the conversation again. Before the fix the journal dropped an
// entry after 90 minutes, so a resumed 4-hour wait would send the message
// a second time.
func TestWebRequeueHoursIntoAWaitIsNotSentAgain(t *testing.T) {
	rig := newWebRig(t)
	rig.ask(t, "grokbot", "hello") // conv-1, user message u0
	ctx := t.Context()
	grok := rig.mesh.Client(t, "grokbot")
	sent, err := grok.Send(ctx, "chatgpt-web", "hello", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	hoursAgo := time.Now().Add(-3 * time.Hour).UTC()
	rig.agent.journal(sent.ID, webSend{ConversationID: "conv-1", UserMessageID: "u0", SubmittedAt: hoursAgo, Recorded: hoursAgo, State: webSendSent})
	res := rig.serve(t, grok, sent.ID)
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "You said: hello") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if got := rig.browser.sent(); len(got) != 1 {
		t.Fatalf("sends = %d, want only the first ask: the resumed request was typed into the conversation again", len(got))
	}
}

// No follow-up, behaviourally: while ChatGPT is still writing an answer in
// a conversation (here, to an ask that gave up waiting), a later ask that
// would continue that conversation is refused before anything is typed,
// whether it continues the remembered conversation or names it.
func TestWebNeverTypesIntoAConversationStillAnswering(t *testing.T) {
	rig := newWebRig(t)
	rig.agent.RequestTimeout = 300 * time.Millisecond
	rig.browser.inProgress = -1
	if res := rig.ask(t, "grokbot", "a question that keeps answering"); res.Status != envelope.StatusFailed {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	for _, body := range []string{"and a follow-up", "conversation: conv-1\nand a follow-up"} {
		res := rig.ask(t, "grokbot", body)
		if got := rig.browser.sent(); len(got) != 1 {
			t.Fatalf("%q: sends = %d: a follow-up was typed into a conversation still answering", body, len(got))
		}
		if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "Nothing was sent") {
			t.Fatalf("%q: %s %q", body, res.Status, res.Reply.Body)
		}
	}
	// A new chat is not that conversation, so it goes.
	if res := rig.ask(t, "grokbot", "new chat\nsomething else"); len(rig.browser.sent()) != 2 {
		t.Fatalf("new chat: %s %q", res.Status, res.Reply.Body)
	}
}

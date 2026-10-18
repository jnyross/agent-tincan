package history

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// The behavioural proofs. Each runs the real relay (internal/testrelay),
// the real client and the real web agent, with a fake browser, all on one
// fake clock: relayClock. The proofs use only names that exist before the
// fix, so the same file run on the old code shows what it did.

// relayClock is a web agent clock that also drives the relay: each sleep
// moves the relay store's clock forward with it, runs any hooks that fall
// due, and runs a relay sweep, the pass that requeues a claim whose lease
// ran out. So hours of an answer being written run in seconds.
type relayClock struct {
	mesh  *testrelay.Mesh
	mu    sync.Mutex
	t     time.Time
	from  time.Time
	hooks []clockHook
}

type clockHook struct {
	at time.Duration
	fn func()
}

// newLongRig is a web rig whose relay, store and web agent all run on one
// relayClock, reading on the default 20-second cadence.
func newLongRig(t *testing.T) (*webRig, *relayClock) {
	t.Helper()
	now := time.Now()
	c := &relayClock{t: now, from: now}
	rig := newWebRigWith(t, relay.Config{Now: c.Now}, true)
	c.mesh = rig.mesh
	rig.mesh.Store.SetClock(c.Now)
	rig.agent.clock = c
	rig.agent.PollInterval = 0
	return rig, c
}

func (c *relayClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *relayClock) elapsed() time.Duration { return c.Now().Sub(c.from) }

// at runs fn once, from inside the first sleep that reaches d of fake time.
func (c *relayClock) at(d time.Duration, fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hooks = append(c.hooks, clockHook{d, fn})
}

func (c *relayClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.t = c.t.Add(d)
	var due []func()
	keep := c.hooks[:0]
	for _, h := range c.hooks {
		if c.t.Sub(c.from) >= h.at {
			due = append(due, h.fn)
		} else {
			keep = append(keep, h)
		}
	}
	c.hooks = keep
	c.mu.Unlock()
	for _, fn := range due {
		fn()
	}
	c.mesh.Server.Sweep(ctx)
	return nil
}

// fakeTab plays the send's tab for the tab-state operation: open until the
// web agent closes it or the test kills it, writing while the
// conversation's last turn is still being written, unless idle says the
// page stopped showing it.
type fakeTab struct {
	mu         sync.Mutex
	dead, idle bool
}

func (f *fakeTab) set(dead, idle bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dead, f.idle = dead, idle
}

// withTab puts tab in front of the rig's fake browser. The operation name
// is written out so this file builds where the operation does not exist.
func withTab(rig *webRig, tab *fakeTab) {
	fb := rig.browser
	rig.agent.Native = &Client{Channel: channelFunc(func(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		if req.Op != Op("chatgpt.tab") {
			return fb.Exchange(ctx, req, recv)
		}
		id := req.Args.ConversationID
		closed := false
		for _, c := range fb.closed() {
			closed = closed || c == id
		}
		fb.mu.Lock()
		writing := false
		if turns := fb.convs[id]; len(turns) > 0 {
			writing = turns[len(turns)-1].pending != 0
		}
		fb.mu.Unlock()
		tab.mu.Lock()
		open := !closed && !tab.dead
		generating := open && writing && !tab.idle
		tab.mu.Unlock()
		b, _ := json.Marshal(map[string]any{"open": open, "generating": generating, "assistant_chars": 7})
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: b})
		return err
	})}
}

// askOnce sends body to chatgpt-web from grokbot, has the agent handle it
// on the relay clock, and returns what the asker reads.
func askOnce(t *testing.T, rig *webRig, body string, urgent bool) (envelope.Result, string) {
	t.Helper()
	ctx := t.Context()
	grok := rig.mesh.Client(t, "grokbot")
	req, err := grok.Send(ctx, "chatgpt-web", body, envelope.KindAsk, "", urgent)
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

func replyBody(res envelope.Result) string {
	if res.Reply == nil {
		return ""
	}
	return res.Reply.Body
}

// assertNeverRequeued checks the relay never requeued or expired request id
// and never told its asker anything.
func assertNeverRequeued(t *testing.T, rig *webRig, res envelope.Result, id string) {
	t.Helper()
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
}

// assertAnsweredOnce checks a long answer reached the asker as answered,
// after at least min of fake time, with one send, one tab close and no
// requeue or lapsed-claim note on the way.
func assertAnsweredOnce(t *testing.T, rig *webRig, res envelope.Result, clock *relayClock, id, prompt string, min time.Duration) {
	t.Helper()
	if res.Status != envelope.StatusAnswered || !strings.Contains(replyBody(res), "You said: "+prompt) {
		t.Fatalf("after %s: status %s, reply %q, relay note %+v", clock.elapsed().Round(time.Second), res.Status, replyBody(res), res.RelayNote)
	}
	if got := clock.elapsed(); got < min {
		t.Fatalf("the answer took only %s; the proof needs at least %s", got, min)
	}
	assertNeverRequeued(t, rig, res, id)
	if got := rig.browser.sent(); len(got) != 1 {
		t.Fatalf("sends = %d, want exactly one: nothing may be typed into the running conversation", len(got))
	}
	if got := rig.browser.closed(); len(got) != 1 {
		t.Fatalf("closes = %v", got)
	}
}

// Limit (a), behaviourally: chatgpt-web with no deadline set has none. A
// ChatGPT answer still being written at 6 hours (it finishes at about 6.5)
// is delivered, not cut off; on the way the asker can read the agent's
// progress note, and the relay never requeues it.
func TestWebLongChatGPTAnswerIsDelivered(t *testing.T) {
	rig, clock := newLongRig(t)
	withTab(rig, &fakeTab{})
	// One read a minute keeps hours of answer cheap under -race; the claim
	// is still renewed every 4 minutes inside each wait.
	rig.agent.PollInterval = time.Minute
	rig.browser.inProgress = 390 // about 6.5 hours
	const prompt = "Think as long as you need about tin cans"
	ctx := t.Context()
	grok := rig.mesh.Client(t, "grokbot")
	req, err := grok.Send(ctx, "chatgpt-web", prompt, envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	var at6h envelope.Result
	clock.at(6*time.Hour, func() { at6h, _ = grok.Get(context.Background(), req.ID, 0) })
	if n, err := rig.agent.PollOnce(ctx); err != nil || n != 1 {
		t.Fatalf("PollOnce = %d, %v", n, err)
	}
	res, err := grok.Get(ctx, req.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertAnsweredOnce(t, rig, res, clock, req.ID, prompt, 6*time.Hour)
	if at6h.Status != envelope.StatusClaimed || at6h.Progress == nil || !strings.HasPrefix(at6h.Progress.Note, "Still waiting for ChatGPT") {
		t.Fatalf("at 6h the asker read status %s, progress %+v", at6h.Status, at6h.Progress)
	}
}

// Limit (c), behaviourally: a claim the agent holds through a 50-minute
// wait (here with a 5-hour deadline set) is never requeued at the relay's
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
			withTab(rig, &fakeTab{})
			rig.agent.RequestTimeout = 5 * time.Hour
			rig.browser.inProgress = 150
			res, id := askOnce(t, rig, "Think hard for an hour about tin cans", urgent)
			assertAnsweredOnce(t, rig, res, clock, id, "Think hard for an hour about tin cans", 45*time.Minute)
		})
	}
}

// The stuck check, behaviourally: with no deadline, an answer that is
// written for 2 hours and then dies (its tab closed, with the conversation
// unchanged) ends the ask cleanly a few minutes later: failed, saying where
// to look, with its tab closed, sent once and never requeued. It is the
// tab and the conversation that end it, not the clock.
func TestWebStuckTabEndsTheAskCleanly(t *testing.T) {
	for _, c := range []struct {
		name       string
		dead, idle bool
	}{{"tab closed", true, false}} {
		t.Run(c.name, func(t *testing.T) {
			rig, clock := newLongRig(t)
			tab := &fakeTab{}
			withTab(rig, tab)
			rig.browser.inProgress = -1 // the conversation never finishes
			clock.at(2*time.Hour, func() { tab.set(c.dead, c.idle) })
			res, id := askOnce(t, rig, "An answer that dies", false)
			body := replyBody(res)
			if res.Status != envelope.StatusFailed || !strings.Contains(body, "stopped writing its answer") || !strings.Contains(body, "conv-1") {
				t.Fatalf("after %s: %s %q", clock.elapsed().Round(time.Second), res.Status, body)
			}
			if got := clock.elapsed(); got < 2*time.Hour || got > 2*time.Hour+10*time.Minute {
				t.Fatalf("the ask ended after %s, want a few minutes after the tab died at 2h", got)
			}
			assertNeverRequeued(t, rig, res, id)
			if got := rig.browser.sent(); len(got) != 1 {
				t.Fatalf("sends = %d", len(got))
			}
			if got := rig.browser.closed(); len(got) != 1 || got[0] != "conv-1" {
				t.Fatalf("closes = %v", got)
			}
		})
	}
}

// silentAPI makes the conversation's reads show the answer in progress
// with no text at all, as ChatGPT Pro does while it thinks: no reply text
// grows, so nothing in the conversation changes until it finishes.
func silentAPI(rig *webRig) {
	inner := rig.agent.Native.Channel
	rig.agent.Native = &Client{Channel: channelFunc(func(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		if req.Op != OpChatGPTDetail {
			return inner.Exchange(ctx, req, recv)
		}
		return inner.Exchange(ctx, req, func(r NativeResponse) (bool, error) {
			var d map[string]any
			if json.Unmarshal(r.Result, &d) == nil {
				if nodes, ok := d["mapping"].(map[string]any); ok {
					for _, v := range nodes {
						n, _ := v.(map[string]any)
						if m, _ := n["message"].(map[string]any); m["status"] == "in_progress" {
							m["content"] = map[string]any{"content_type": "text", "parts": []any{""}}
						}
					}
					r.Result, _ = json.Marshal(d)
				}
			}
			return recv(r)
		})
	})}
}

// P1 (fix round 1), behaviourally: ChatGPT Pro thinks for over 2 hours with
// no token output and no stop button or streaming mark the extension knows:
// its tab is open and reports neither writing nor anything to count. The
// conversation shows the answer in progress with no text. The ask is never
// declared stuck while it thinks; it is answered once the answer comes,
// sent once and never requeued. Before the fix it failed as stuck after
// 4m30s.
func TestWebProThinkingWithNoMarkIsNeverStuck(t *testing.T) {
	rig, clock := newLongRig(t)
	withTab(rig, &fakeTab{idle: true})
	silentAPI(rig)
	rig.agent.PollInterval = time.Minute
	rig.browser.inProgress = 135 // a little over 2 hours
	res, id := askOnce(t, rig, "Think silently for two hours", false)
	assertAnsweredOnce(t, rig, res, clock, id, "Think silently for two hours", 2*time.Hour)
}

// P1 (fix round 1), behaviourally: a second ask queued behind a ChatGPT
// answer that takes over a day is not expired at the relay's 24 hours: at
// 26 hours it is still queued, with no relay note, and it is answered after
// the long one. Before the fix it expired at 24 hours ("This request
// expired after 24h with no reply").
func TestWebSecondAskQueuedPast24HoursIsNotExpired(t *testing.T) {
	rig, clock := newLongRig(t)
	withTab(rig, &fakeTab{})
	// Reads every 5 minutes instead of on the 20-second cadence, so over a
	// day of answer takes 320 reads; the claim is still renewed every 4
	// minutes inside each wait.
	rig.agent.PollInterval = 5 * time.Minute
	rig.browser.inProgress = 320 // about 26h40m
	ctx := context.Background()
	muse := rig.mesh.Client(t, "muse")
	var second string
	var at26h envelope.Result
	clock.at(time.Hour, func() {
		req, err := muse.Send(ctx, "chatgpt-web", "new chat\nQueued for over a day", envelope.KindAsk, "", false)
		if err != nil {
			t.Error(err)
		}
		second = req.ID
	})
	clock.at(26*time.Hour, func() { at26h, _ = muse.Get(ctx, second, 0) })
	first, firstID := askOnce(t, rig, "More than a day", false)
	assertAnsweredOnce(t, rig, first, clock, firstID, "More than a day", 26*time.Hour)
	if second == "" || at26h.Status != envelope.StatusQueued || at26h.RelayNote != nil {
		note := ""
		if at26h.RelayNote != nil {
			note = at26h.RelayNote.Note
		}
		t.Fatalf("at 26h the second ask was %s, relay note %q", at26h.Status, note)
	}
	rig.browser.inProgress = 1
	if n, err := rig.agent.PollOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("second PollOnce = %d, %v", n, err)
	}
	res, err := muse.Get(t.Context(), second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != envelope.StatusAnswered || !strings.Contains(replyBody(res), "You said: Queued for over a day") {
		t.Fatalf("second ask: %s %q", res.Status, replyBody(res))
	}
	assertNeverRequeued(t, rig, res, second)
}

// The per-ask deadline is opt-in: an ask whose first line is "timeout: 30m"
// ends when its 30 minutes run out, even though its tab is still writing,
// and the line itself is never typed into ChatGPT.
func TestWebPerAskDeadlineStillWorks(t *testing.T) {
	rig, clock := newLongRig(t)
	withTab(rig, &fakeTab{})
	rig.browser.inProgress = -1
	res, id := askOnce(t, rig, "timeout: 30m\nA question with a deadline", false)
	if res.Status != envelope.StatusFailed || !strings.Contains(replyBody(res), "did not finish answering in time") {
		t.Fatalf("after %s: %s %q", clock.elapsed().Round(time.Second), res.Status, replyBody(res))
	}
	if got := clock.elapsed(); got < 29*time.Minute || got > 30*time.Minute+time.Second {
		t.Fatalf("the ask ended after %s, want its 30 minutes", got)
	}
	if got := rig.browser.sent(); len(got) != 1 || got[0].Message != "A question with a deadline" {
		t.Fatalf("sent %+v", got)
	}
	assertNeverRequeued(t, rig, res, id)
}

// A second ask during a long one is queued at the relay, not timed out:
// sent an hour into a 6-hour answer, it is still queued (not expired, not
// failed, its asker told nothing) at 3 hours, and is answered once the
// long one is.
func TestWebSecondAskDuringALongOneIsQueuedThenServed(t *testing.T) {
	rig, clock := newLongRig(t)
	withTab(rig, &fakeTab{})
	rig.agent.PollInterval = time.Minute
	rig.browser.inProgress = 365 // about 6 hours
	ctx := context.Background()
	muse := rig.mesh.Client(t, "muse")
	var second string
	var at3h envelope.Result
	clock.at(time.Hour, func() {
		req, err := muse.Send(ctx, "chatgpt-web", "new chat\nA quick one", envelope.KindAsk, "", false)
		if err != nil {
			t.Error(err)
		}
		second = req.ID
	})
	clock.at(3*time.Hour, func() { at3h, _ = muse.Get(ctx, second, 0) })
	first, firstID := askOnce(t, rig, "The long one", false)
	assertAnsweredOnce(t, rig, first, clock, firstID, "The long one", 6*time.Hour)
	if second == "" || at3h.Status != envelope.StatusQueued || at3h.RelayNote != nil || at3h.Reply != nil {
		t.Fatalf("at 3h the second ask was %s, note %+v, reply %+v", at3h.Status, at3h.RelayNote, at3h.Reply)
	}
	rig.browser.inProgress = 1
	if n, err := rig.agent.PollOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("second PollOnce = %d, %v", n, err)
	}
	res, err := muse.Get(t.Context(), second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != envelope.StatusAnswered || !strings.Contains(replyBody(res), "You said: A quick one") {
		t.Fatalf("second ask: %s %q", res.Status, replyBody(res))
	}
	assertNeverRequeued(t, rig, res, second)
	if got := rig.browser.sent(); len(got) != 2 {
		t.Fatalf("sends = %d", len(got))
	}
}

// No follow-up, behaviourally: a request the relay hands back after the
// agent died hours into its wait is resumed from the send journal, never
// typed into the conversation again. Before the fix the journal dropped an
// entry after 90 minutes, so a resumed long wait would send the message a
// second time.
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

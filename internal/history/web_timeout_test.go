package history

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// Limit (a): ChatGPT has no deadline by default, the other sites keep 8
// minutes, web serve can set or remove one, a request can name its own
// with a timeout: line, and nothing caps a deadline.
func TestWebRequestTimeoutPerSiteAndPerAsk(t *testing.T) {
	type want struct {
		d  time.Duration
		ok bool
	}
	for _, c := range []struct {
		site Source
		set  time.Duration
		none bool
		want want
	}{
		{SourceChatGPT, 0, false, want{0, false}},
		{SourceClaudeAI, 0, false, want{DefaultWebRequestTimeout, true}},
		{SourceGemini, 0, false, want{DefaultWebRequestTimeout, true}},
		{SourceDots, 0, false, want{DefaultWebRequestTimeout, true}},
		{SourceChatGPT, 90 * time.Minute, false, want{90 * time.Minute, true}},
		{SourceClaudeAI, 2 * time.Hour, false, want{2 * time.Hour, true}},
		{SourceClaudeAI, 0, true, want{0, false}},
		{SourceChatGPT, 72 * time.Hour, false, want{72 * time.Hour, true}},
	} {
		d, ok := (&WebAgent{Site: c.site, RequestTimeout: c.set, NoDeadline: c.none}).requestTimeout()
		if (want{d, ok}) != c.want {
			t.Errorf("%s set %s none %t: %s %t, want %+v", c.site, c.set, c.none, d, ok, c.want)
		}
	}

	for _, c := range []struct {
		site Source
		body string
		want want
	}{
		{SourceChatGPT, "timeout: 6h\nhello", want{6 * time.Hour, true}},
		{SourceChatGPT, "Timeout: 90m\nnew chat\nhello", want{90 * time.Minute, true}},
		{SourceChatGPT, "new chat\ntimeout: 30m\nhello", want{30 * time.Minute, true}},
		{SourceClaudeAI, "timeout: 1h\nhello", want{time.Hour, true}},
		{SourceChatGPT, "timeout: 72h\nhello", want{72 * time.Hour, true}},
		{SourceChatGPT, "timeout: how do I set one in nginx?", want{0, false}},
		{SourceChatGPT, "timeout: 0s\nhello", want{0, false}},
		{SourceChatGPT, "timeout: -5m\nhello", want{0, false}},
		{SourceChatGPT, "hello\ntimeout: 6h", want{0, false}},
		{SourceClaudeAI, "timeout: soon\nhello", want{DefaultWebRequestTimeout, true}},
		// A dot's body is sent as is: the line is text there.
		{SourceDots, "timeout: 6h\nhello", want{DefaultWebRequestTimeout, true}},
	} {
		d, ok := (&WebAgent{Site: c.site}).timeoutFor(envelope.Request{Body: c.body})
		if (want{d, ok}) != c.want {
			t.Errorf("%s %q: %s %t, want %+v", c.site, c.body, d, ok, c.want)
		}
	}
}

// The timeout: line is read and left out of the message; it may come
// before the threading line, and a timeout: line that is not a duration is
// the message itself.
func TestParseWebRequestTimeoutLine(t *testing.T) {
	for _, c := range []struct {
		body, msg, conv string
		mode            webThread
		timeout         time.Duration
	}{
		{"timeout: 6h\nhello", "hello", "", threadContinue, 6 * time.Hour},
		{"timeout: 30m\nnew chat\nhello", "hello", "", threadNew, 30 * time.Minute},
		{"timeout: 2h\nconversation: abc-123\nhello", "hello", "abc-123", threadConversation, 2 * time.Hour},
		{"timeout: soon\nhello", "timeout: soon\nhello", "", threadContinue, 0},
		{"new chat\ntimeout: 6h\nhello", "hello", "", threadNew, 6 * time.Hour},
		{"conversation: abc-123\ntimeout: 6h\nhello", "hello", "abc-123", threadConversation, 6 * time.Hour},
		{"timeout: 1h\nnew chat\ntimeout: 6h\nhello", "timeout: 6h\nhello", "", threadNew, time.Hour},
		{"hello\ntimeout: 6h", "hello\ntimeout: 6h", "", threadContinue, 0},
		// Leading blank lines before the timeout line: the relay's failover
		// reads the same request as naming a conversation.
		{"\n\ntimeout: 6h\nconversation: abc-123\nhello", "hello", "abc-123", threadConversation, 6 * time.Hour},
	} {
		wr, err := parseWebRequest(c.body)
		if err != nil || wr.message != c.msg || wr.mode != c.mode || wr.convID != c.conv || wr.timeout != c.timeout {
			t.Errorf("%q: %+v %v", c.body, wr, err)
		}
	}
	for _, body := range []string{"timeout: 6h\n", "new chat\ntimeout: 6h"} {
		if _, err := parseWebRequest(body); err == nil {
			t.Errorf("%q has no message", body)
		}
	}
}

// Limit (a), the poll budget: with no deadline on its context, the reply
// wait gives up after the agent's request timeout on its own clock, not
// after a fixed 8 minutes.
func TestWebReplyWaitBudgetFollowsRequestTimeout(t *testing.T) {
	rig := newWebRig(t)
	rig.agent.PollInterval = 0
	rig.agent.RequestTimeout = 3 * time.Hour
	clock := newFakeClock()
	rig.agent.clock = clock
	rig.browser.convs["conv-1"] = []*fakeTurn{{prompt: "hello", reply: "never finished", sentAt: time.Now(), pending: -1}}
	start := clock.Now()
	_, _, err := rig.agent.waitReply(context.Background(), "conv-1", replyAnchor{message: "hello", bound: "u0"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if got := clock.Now().Sub(start); got < 3*time.Hour-time.Minute || got > 3*time.Hour {
		t.Fatalf("the wait gave up after %s, want about 3h", got)
	}
}

// tabChannel answers <site>.tab with state and passes everything else to
// the fake browser.
func tabChannel(fb *fakeBrowser, state func() (TabState, *NativeError)) channelFunc {
	return func(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
		if req.Op != OpChatGPTTab {
			return fb.Exchange(ctx, req, recv)
		}
		st, nerr := state()
		if nerr != nil {
			_, err := recv(NativeResponse{ID: req.ID, Error: nerr})
			return err
		}
		b, _ := json.Marshal(st)
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: b})
		return err
	}
}

// The stuck watch, read by read: a changing reply or a tab still open on
// its conversation, whatever its page shows, counts as alive and starts
// the count again; StuckChecks reads in a row with the tab dead and the
// reply unchanged end the wait; so do StuckChecks not-found reads, whatever
// the tab shows. An extension that cannot report its tab is never taken as
// stuck.
func TestWebStuckWatchCountsOnlyReadsWithNoSignOfLife(t *testing.T) {
	rig := newWebRig(t)
	rig.agent.StuckChecks = 3
	var st TabState
	var nerr *NativeError
	rig.agent.Native = &Client{Channel: tabChannel(rig.browser, func() (TabState, *NativeError) { return st, nerr })}
	ctx := t.Context()
	s := rig.agent.newStuckWatch("conv-1")
	step := func(sig string, readErr error, wantEnd error) {
		t.Helper()
		if got := s.check(ctx, sig, readErr); !errors.Is(got, wantEnd) || (wantEnd == nil && got != nil) {
			t.Fatalf("sig %q err %v tab %+v: %v, want %v", sig, readErr, st, got, wantEnd)
		}
	}
	st = TabState{Open: true, Generating: true}
	for range 10 {
		step("same", nil, nil) // a page writing its answer stays alive however long
	}
	st = TabState{Open: true}
	for range 50 {
		step("same", nil, nil) // an open tab showing nothing (a silent Pro think) is alive
	}
	step("grew", nil, nil) // the conversation's reply changed
	st = TabState{}        // the tab is dead (closed, crashed, moved away)
	step("grew", nil, nil)
	step("grew", errors.New("flaky read"), nil)
	step("grew", nil, errStuck)

	s = rig.agent.newStuckWatch("conv-1")
	st = TabState{Open: true, Generating: true}
	notFound := unavailable(SourceChatGPT, ErrNotFound, "gone")
	step("", notFound, nil)
	step("", notFound, nil)
	step("", notFound, errConversationGone)

	s = rig.agent.newStuckWatch("conv-1")
	nerr = &NativeError{Code: "bad_request", Message: "unknown operation"}
	for range 10 {
		step("same", nil, nil) // an older extension: no tab state, never stuck
	}
	if (*stuckWatch)(nil).check(ctx, "", nil) != nil {
		t.Fatal("a nil watch ended the wait")
	}
}

// Limit (c): the claim is renewed with a content-free progress note every
// LeaseRenew, also inside one long sleep (a site's long Retry-After), so no
// single wait outlasts the lease.
func TestWebClaimKeeperRenewsThroughALongSleep(t *testing.T) {
	rig := newWebRig(t)
	ctx := t.Context()
	grok := rig.mesh.Client(t, "grokbot")
	req, err := grok.Send(ctx, "chatgpt-web", "a secret question", envelope.KindAsk, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.agent.Relay.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	rig.agent.clock = clock
	kctx := rig.agent.keepClaim(ctx, req)
	keep := claimKeeperFrom(kctx)
	if keep == nil {
		t.Fatal("no keeper")
	}
	if err := keep.sleep(kctx, clock, time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := clock.Now().Sub(keep.start); got != time.Hour {
		t.Fatalf("slept %s", got)
	}
	if got := keep.last.Sub(keep.start); got != 60*time.Minute {
		t.Fatalf("last renewal %s after the claim, want 60m (every %s)", got, DefaultWebLeaseRenew)
	}
	res, err := grok.Get(ctx, req.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Progress == nil || res.Progress.Note != "Still waiting for ChatGPT to finish its answer (1h00m so far)." {
		t.Fatalf("progress %+v", res.Progress)
	}
	if strings.Contains(res.Progress.Note, "secret") {
		t.Fatal("the note carries request text")
	}
	if res.Status != envelope.StatusClaimed {
		t.Fatalf("status %s", res.Status)
	}
	// A keeper-free context only sleeps.
	if err := (*claimKeeper)(nil).sleep(ctx, clock, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestWaitedText(t *testing.T) {
	for d, want := range map[time.Duration]string{20 * time.Second: "0m", 25 * time.Minute: "25m", 125 * time.Minute: "2h05m", 4 * time.Hour: "4h00m"} {
		if got := waitedText(d); got != want {
			t.Errorf("%s: %q, want %q", d, got, want)
		}
	}
}

package history

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// Limit (a): a web agent's request timeout is the site's own unless set,
// ChatGPT's is hours, a request can name its own with a timeout: line, and
// none goes over MaxWebRequestTimeout.
func TestWebRequestTimeoutPerSiteAndPerAsk(t *testing.T) {
	agent := func(site Source, set time.Duration) *WebAgent {
		return &WebAgent{Site: site, RequestTimeout: set}
	}
	for _, c := range []struct {
		site Source
		set  time.Duration
		want time.Duration
	}{
		{SourceChatGPT, 0, DefaultChatGPTRequestTimeout},
		{SourceClaudeAI, 0, DefaultWebRequestTimeout},
		{SourceGemini, 0, DefaultWebRequestTimeout},
		{SourceDots, 0, DefaultWebRequestTimeout},
		{SourceChatGPT, 90 * time.Minute, 90 * time.Minute},
		{SourceClaudeAI, 2 * time.Hour, 2 * time.Hour},
		{SourceChatGPT, 48 * time.Hour, MaxWebRequestTimeout},
	} {
		if got := agent(c.site, c.set).requestTimeout(); got != c.want {
			t.Errorf("%s with %s: %s, want %s", c.site, c.set, got, c.want)
		}
	}
	if DefaultChatGPTRequestTimeout < 4*time.Hour {
		t.Fatalf("ChatGPT's default is %s", DefaultChatGPTRequestTimeout)
	}

	for _, c := range []struct {
		site Source
		body string
		want time.Duration
	}{
		{SourceChatGPT, "timeout: 6h\nhello", 6 * time.Hour},
		{SourceChatGPT, "Timeout: 90m\nnew chat\nhello", 90 * time.Minute},
		{SourceClaudeAI, "timeout: 1h\nhello", time.Hour},
		{SourceChatGPT, "timeout: 72h\nhello", MaxWebRequestTimeout},
		{SourceChatGPT, "timeout: how do I set one in nginx?", DefaultChatGPTRequestTimeout},
		{SourceChatGPT, "timeout: 0s\nhello", DefaultChatGPTRequestTimeout},
		{SourceChatGPT, "timeout: -5m\nhello", DefaultChatGPTRequestTimeout},
		{SourceChatGPT, "hello\ntimeout: 6h", DefaultChatGPTRequestTimeout},
		// A dot's body is sent as is: the line is text there.
		{SourceDots, "timeout: 6h\nhello", DefaultWebRequestTimeout},
	} {
		if got := agent(c.site, 0).timeoutFor(envelope.Request{Body: c.body}); got != c.want {
			t.Errorf("%s %q: %s, want %s", c.site, c.body, got, c.want)
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
	clock := newFakeClock()
	rig.agent.clock = clock
	rig.browser.convs["conv-1"] = []*fakeTurn{{prompt: "hello", reply: "never finished", sentAt: time.Now(), pending: -1}}
	start := clock.Now()
	_, _, err := rig.agent.waitReply(context.Background(), "conv-1", replyAnchor{message: "hello", bound: "u0"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if got := clock.Now().Sub(start); got < DefaultChatGPTRequestTimeout-time.Minute || got > DefaultChatGPTRequestTimeout {
		t.Fatalf("the wait gave up after %s, want about %s", got, DefaultChatGPTRequestTimeout)
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

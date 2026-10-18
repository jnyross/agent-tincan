package history

// The reply wait's timing policy: how often a web agent reads the
// conversation while the reply is written, how it backs off on rate limits
// and other failures, and how it keeps its relay presence during the wait.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// DefaultWebPollSchedule is how the conversation is read while the reply
// is being written: the first read 5s after the send, then 5s, 8s and 12s
// apart, then every 20s. Each read is a request to the site on the owner's
// account, so the wait backs off instead of hammering it.
var DefaultWebPollSchedule = []time.Duration{5 * time.Second, 5 * time.Second, 8 * time.Second, 12 * time.Second, 20 * time.Second}

// DefaultClaudeStablePolls and DefaultClaudeStableFor: a claude.ai reply
// with no stop_reason is finished once the same text is read on 4
// consecutive polls spanning at least 10 seconds, so a pause mid-reply is
// not taken for the end. At DefaultWebPollSchedule's cadence 4 polls
// always span more than 10 seconds; the time rule matters for a fast
// PollInterval.
const (
	DefaultClaudeStablePolls = 4
	DefaultClaudeStableFor   = 10 * time.Second
)

// DefaultGeminiStablePolls and DefaultGeminiStableFor: Gemini's detail
// has no finished marker, and Gemini can hold its text still for a long
// while as it thinks or searches mid-answer, so its reply counts as
// finished only once the same text is read on 4 consecutive polls
// spanning at least 45 seconds (claude.ai's window is 10).
const (
	DefaultGeminiStablePolls = 4
	DefaultGeminiStableFor   = 45 * time.Second
)

// DefaultCopilotStablePolls and DefaultCopilotStableFor: Copilot's page
// JSON marks no answer finished, and a web search can hold its text still
// mid-answer, so a reply counts as finished once the same text is read on
// 3 consecutive polls spanning at least 20 seconds.
const (
	DefaultCopilotStablePolls = 3
	DefaultCopilotStableFor   = 20 * time.Second
)

// DefaultDotStablePolls and DefaultDotStableFor: a dot's DM marks no
// answer finished, and a dot can answer in several messages a while
// apart, so its reply counts as finished once the same run of dot
// messages is read on 3 consecutive polls spanning at least 45 seconds.
const (
	DefaultDotStablePolls = 3
	DefaultDotStableFor   = 45 * time.Second
)

// A 429 while waiting for a reply waits the site's Retry-After, or backs
// off from RateLimitBackoffStart, doubling up to RateLimitBackoffMax.
const (
	RateLimitBackoffStart = 30 * time.Second
	RateLimitBackoffMax   = 5 * time.Minute
)

// maxErrorBackoff caps the backoff after other transient read failures
// (an HTTP 5xx, a flaky read).
const maxErrorBackoff = 2 * time.Minute

// DefaultWebStuckChecks is how many reads in a row a request with no
// deadline may show no sign of its answer being written before it ends as
// stuck (stuckWatch): at the 20-second read cadence, about 5 minutes of
// reads. It is a count of reads that each found nothing alive, not a
// deadline: one sign of life starts the count again.
const DefaultWebStuckChecks = 15

// DefaultWebLeaseRenew is how often a web agent posts a progress note
// while it waits for a reply. Each note renews the claim by its lease, and
// 4 minutes stays inside the shortest one (an urgent request's 10).
const DefaultWebLeaseRenew = 4 * time.Minute

// webClock is the reply wait's time source.
type webClock interface {
	Now() time.Time
	// Sleep waits d, or until ctx ends and then returns ctx's error.
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (w *WebAgent) clk() webClock {
	if w.clock != nil {
		return w.clock
	}
	return realClock{}
}

// keepPresence refreshes the agent's relay presence every
// PresenceInterval until the returned stop is called. While a request
// waits minutes for its reply the agent is not long-polling and would
// otherwise show offline. See client.Relay.KeepPresence.
func (w *WebAgent) keepPresence(ctx context.Context) (stop func()) {
	return w.Relay.KeepPresence(ctx, client.Presence{Every: w.PresenceInterval, Logf: w.logf})
}

// pollDelay is the wait before the next detail read once reads have
// succeeded: DefaultWebPollSchedule (or the fixed PollInterval).
func (w *WebAgent) pollDelay(reads int) time.Duration {
	if w.PollInterval > 0 {
		return w.PollInterval
	}
	return DefaultWebPollSchedule[min(reads, len(DefaultWebPollSchedule)-1)]
}

// rateLimitBackoff is the wait after the n-th consecutive 429 (n >= 1)
// that carried no Retry-After.
func rateLimitBackoff(n int) time.Duration {
	d := RateLimitBackoffStart
	for i := 1; i < n && d < RateLimitBackoffMax; i++ {
		d *= 2
	}
	return min(d, RateLimitBackoffMax)
}

// stableRule is the site's text-stability rule with the agent's
// overrides, or nil when the site has none.
func (w *WebAgent) stableRule() *stableRule {
	site := w.site().stable
	if site == nil {
		return nil
	}
	r := *site
	if w.ClaudeStablePolls > 0 {
		r.polls = w.ClaudeStablePolls
	}
	if w.ClaudeStableFor > 0 {
		r.span = w.ClaudeStableFor
	}
	return &r
}

// waitReply reads the conversation on the poll schedule until the reply
// to this request's message is finished, and returns that read and the id
// of this request's user message. On a site with a text-stability rule
// (claude.ai, Gemini, Copilot), a reply without a stop_reason counts as finished once the
// same reply is read on the rule's number of consecutive polls spanning
// at least its time (see stableRule). onBind, when set,
// is called once with the user message id when it is first seen. Errors
// that will not clear (logged out, the API changed, the extension gone)
// end the wait at once, as does a later user turn with no reply to this
// one. A 429 waits the site's Retry-After or backs off exponentially, and
// holds every request to the site back meanwhile; when that wait would
// outlast ctx the rate limit is returned. Other failures (an HTTP 5xx)
// back off too, and are retried until ctx ends.
func (w *WebAgent) waitReply(ctx context.Context, convID string, a replyAnchor, onBind func(string)) (json.RawMessage, string, error) {
	clock := w.clk()
	// The budget is ctx's deadline on the wait's own clock. A request with
	// no deadline has no budget: it ends when the reply is finished, on an
	// error that will not clear, or when the stuck watch finds nothing
	// alive.
	var budgetEnd time.Time
	if dl, ok := ctx.Deadline(); ok {
		budgetEnd = clock.Now().Add(time.Until(dl))
	} else if d, ok := w.requestTimeout(); ok {
		budgetEnd = clock.Now().Add(d)
	}
	over := func(d time.Duration) bool { return !budgetEnd.IsZero() && clock.Now().Add(d).After(budgetEnd) }
	var stuck *stuckWatch
	if budgetEnd.IsZero() {
		stuck = w.newStuckWatch(convID)
	}
	keep := claimKeeperFrom(ctx)
	stable := w.stableRule()
	op := w.live().detailOp
	prev, seen := "", 0
	var first time.Time
	reads, limited, failures := 0, 0, 0
	delay := w.pollDelay(0)
	for {
		if over(delay) {
			return nil, a.bound, context.DeadlineExceeded
		}
		if err := keep.sleep(ctx, clock, delay); err != nil {
			return nil, a.bound, err
		}
		raw, err := w.authRequest(ctx, op, OpArgs{ID: convID})
		if cerr := ctx.Err(); cerr != nil {
			return nil, a.bound, cerr
		}
		after, isLimit := rateLimited(err)
		switch {
		case err == nil:
			reads++
			limited, failures = 0, 0
			delay = w.pollDelay(reads)
			p, perr := w.progress(raw, a)
			if errors.Is(perr, errThreadTooLong) {
				return nil, a.bound, perr
			}
			if perr != nil {
				return nil, a.bound, unavailable(w.Site, ErrEndpointChanged, "unexpected conversation shape")
			}
			if p.userID != "" && a.bound == "" {
				a.bound = p.userID
				if onBind != nil {
					onBind(a.bound)
				}
			}
			if p.orphaned {
				return nil, a.bound, errOrphaned
			}
			if p.limited {
				// The turn ended on the account's rate or plan limit with
				// no answer to deliver (progressOf sets limited only then;
				// an answer that carries a stray limit error is returned
				// as usual): the site is left alone for a while, and the
				// asker is told the message went through.
				after := w.site().planLimitCooldown
				if after <= 0 {
					after = RateLimitBackoffMax
				}
				w.noteLimit(w.Native.cooldown().now().Add(after))
				w.logf("conversation %s: %s answered with a rate or plan limit; holding requests back for %s", convID, siteLabel(w.Site), after)
				return nil, a.bound, &UnavailableError{Source: w.Site, Kind: ErrRateLimited, Detail: "plan limit", RetryAfter: after}
			}
			if p.finished {
				return raw, a.bound, nil
			}
			if serr := stuck.check(ctx, p.sig, nil); serr != nil {
				return nil, a.bound, serr
			}
			switch {
			case !p.found || stable == nil:
				prev, seen = "", 0
			case p.sig == prev:
				seen++
				if seen >= stable.polls && clock.Now().Sub(first) >= stable.span {
					return raw, a.bound, nil
				}
			default:
				prev, seen, first = p.sig, 1, clock.Now()
			}
		case isLimit:
			limited++
			if after <= 0 {
				after = rateLimitBackoff(limited)
			}
			// Everything else in this process holds back as long.
			w.Native.cooldown().Note(w.Site, after)
			if serr := stuck.check(ctx, "", err); serr != nil {
				return nil, a.bound, serr
			}
			if over(after) {
				w.logf("conversation %s: %s is rate-limiting this account; the wait of %s is past this request's budget", convID, siteLabel(w.Site), after)
				return nil, a.bound, err
			}
			w.logf("conversation %s: detail: %v (rate limited; next read in %s)", convID, err, after)
			delay = after
		case errors.Is(err, ErrNotLoggedIn), errors.Is(err, ErrEndpointChanged), errors.Is(err, ErrExtensionNotConnected), errors.Is(err, ErrChromeNotRunning), errors.Is(err, ErrRejected),
			errors.Is(err, ErrPermissionMissing), errors.Is(err, ErrBlocked), errors.Is(err, ErrPaused):
			return nil, a.bound, err
		default:
			if serr := stuck.check(ctx, "", err); serr != nil {
				return nil, a.bound, serr
			}
			failures++
			delay = min(w.pollDelay(reads)<<min(failures, 10), maxErrorBackoff)
			kind := "retrying"
			if serverError(err) {
				kind = "server error; retrying"
			}
			w.logf("conversation %s: detail: %v (%s in %s)", convID, err, kind, delay)
		}
	}
}

// claimKeeper keeps one claimed request's lease alive while the agent
// waits for its reply: every LeaseRenew it posts a progress note, which
// renews the claim, so the relay never requeues (and never tells the
// asker it lapsed) an ask whose answer is still being written, however
// long that takes. The note says only how long the wait has run.
type claimKeeper struct {
	w     *WebAgent
	id    string
	every time.Duration
	// start is when the request was claimed; last is the last renewal
	// (the claim itself to begin with).
	start, last time.Time
	// off: the relay takes no progress notes, so there is nothing to
	// renew with.
	off bool
	// onRenew, when set, runs after each renewal (the wait refreshes its
	// send journal entry, so the entry outlives a wait of any length).
	onRenew func(now time.Time)
}

type claimKeeperKey struct{}

// keepClaim starts keeping req's claim, made just now, alive for the rest
// of ctx's request.
func (w *WebAgent) keepClaim(ctx context.Context, req envelope.Request) context.Context {
	every := w.LeaseRenew
	if every <= 0 {
		every = DefaultWebLeaseRenew
	}
	now := w.clk().Now()
	return context.WithValue(ctx, claimKeeperKey{}, &claimKeeper{w: w, id: req.ID, every: every, start: now, last: now})
}

func claimKeeperFrom(ctx context.Context) *claimKeeper {
	k, _ := ctx.Value(claimKeeperKey{}).(*claimKeeper)
	return k
}

// sleep waits d on clock, renewing the claim whenever a renewal falls due
// meanwhile, so no single wait (a long Retry-After included) outlasts the
// lease. A nil keeper only sleeps.
func (k *claimKeeper) sleep(ctx context.Context, clock webClock, d time.Duration) error {
	if k == nil {
		return clock.Sleep(ctx, d)
	}
	for {
		k.renewIfDue(ctx, clock.Now())
		if d <= 0 {
			return nil
		}
		step := d
		if !k.off {
			step = min(step, max(k.last.Add(k.every).Sub(clock.Now()), 0))
		}
		if err := clock.Sleep(ctx, step); err != nil {
			return err
		}
		d -= step
	}
}

func (k *claimKeeper) renewIfDue(ctx context.Context, now time.Time) {
	if k.off || now.Before(k.last.Add(k.every)) {
		return
	}
	k.last = now
	note := fmt.Sprintf("Still waiting for %s to finish its answer (%s so far).", siteLabel(k.w.Site), waitedText(now.Sub(k.start)))
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := k.w.Relay.Progress(pctx, k.id, note)
	switch {
	case err == nil:
	case errors.Is(err, client.ErrProgressUnsupported):
		k.off = true
		k.w.logf("request %s: the relay takes no progress notes, so the claim cannot be renewed while the reply is written (upgrade the relay)", k.id)
	default:
		// A lapsed or moved claim refuses the note; the reply still lands
		// on a requeued request, so the wait goes on.
		k.w.logf("request %s: renewing the claim: %v", k.id, err)
	}
	if k.onRenew != nil {
		k.onRenew(now)
	}
}

// errStuck ends a request with no deadline whose answer shows no sign of
// being written any more; errConversationGone one whose conversation can
// no longer be read at all.
var (
	errStuck            = errors.New("the answer stopped: its tab is gone or idle and the conversation has not changed")
	errConversationGone = errors.New("the conversation is gone")
)

// stuckWatch decides when a request with no deadline is dead, from what
// the page and the conversation show rather than from the clock. After
// every read that does not find the answer finished, the answer counts as
// alive when the conversation's reply changed since the last read, or the
// send's tab is open and its page shows the answer being written (the
// site's stop button or streaming mark) or its answer text grew. Only
// limit reads in a row with none of those end the request (errStuck).
// The conversation itself being unreadable as not found for limit reads in
// a row ends it too (errConversationGone), whatever the tab shows. An
// extension that cannot report its tab (one older than this release) is
// taken as alive, so with it only a finished answer, an error that will
// not clear or a gone conversation ends the request.
type stuckWatch struct {
	w      *WebAgent
	convID string
	limit  int
	sig    string
	chars  int
	// idle and missing count reads in a row with no sign of life and with
	// the conversation not found.
	idle, missing int
	// blind: the extension cannot report its tab; logged once.
	blind bool
}

func (w *WebAgent) newStuckWatch(convID string) *stuckWatch {
	limit := w.StuckChecks
	if limit <= 0 {
		limit = DefaultWebStuckChecks
	}
	return &stuckWatch{w: w, convID: convID, limit: limit}
}

// check takes one read: sig is the reply's fingerprint when the read
// succeeded, readErr the read's error otherwise. It returns the error that
// ends the request, or nil to keep waiting. A nil watch never ends one.
func (s *stuckWatch) check(ctx context.Context, sig string, readErr error) error {
	if s == nil {
		return nil
	}
	if errors.Is(readErr, ErrNotFound) {
		if s.missing++; s.missing >= s.limit {
			return errConversationGone
		}
	} else {
		s.missing = 0
	}
	alive := readErr == nil && sig != s.sig
	if readErr == nil {
		s.sig = sig
	}
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	st, err := s.w.Native.TabState(tctx, s.w.Site, s.convID)
	cancel()
	switch {
	case err != nil:
		if !s.blind {
			s.blind = true
			s.w.logf("conversation %s: the extension cannot report its tab (%v); waiting for the answer without the stuck check", s.convID, err)
		}
		alive = true
	case st.Open && st.Generating:
		alive = true
	case st.Open && st.AssistantChars != s.chars:
		alive = true
	}
	if err == nil {
		s.chars = st.AssistantChars
	}
	if alive {
		s.idle = 0
		return nil
	}
	if s.idle++; s.idle >= s.limit {
		s.w.logf("conversation %s: no sign of the answer being written on %d reads in a row (tab open %t); ending the request", s.convID, s.idle, st.Open)
		return errStuck
	}
	return nil
}

// waitedText is a wait's length in whole minutes ("25m", "2h05m").
func waitedText(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}

# Upstream PR draft (mvanhorn/agent-tincan)

Not opened. This is the title and body for a PR from
`jnyross/agent-tincan:upstream/chatgpt-web-no-deadline` to
`mvanhorn/agent-tincan:main`, for the owner to review first. This file is
fork-only and is not on the upstream branch.

## Title

fix(web): no deadline on ChatGPT answers: dead-tab check, claim renewal, no queue expiry

## Body

### Problem

A ChatGPT Pro (or deep research) answer can take an hour or more. Asked
through `chatgpt-web`, it was cut off three ways:

1. **Request deadline.** `DefaultWebRequestTimeout` (8 minutes) bounded every
   web agent request, and the reply wait's poll budget fell back to the same
   constant. After 8 minutes the asker got "ChatGPT did not finish answering in
   time" while the answer was still being written.
2. **Tab lifetime.** The extension closed a finished send's tab after
   `KEEP_TAB_MS` (10 minutes) whatever the page showed, which can stop an
   answer still being written.
3. **Claim lease.** The web agent posts nothing while it waits, so after the
   30-minute claim lease (10 for an urgent ask) the relay requeued the request
   and told the asker the claim lapsed. The send journal also dropped entries
   after 90 minutes, so a request resumed after a long wait would have been
   typed into the conversation a second time.

### Changes

- **(a) No deadline on ChatGPT.** A `chatgpt-web` request has no deadline by
  default (`webSite.noDeadline`). It ends only when the answer is finished, the
  wait fails in a way that will not clear (logged out, extension or Chrome
  gone, API changed, grant revoked, anti-bot check, rate or plan limit), the
  conversation is gone, or the answer is stuck (below). The other sites keep
  their 8-minute deadline. `tincan web serve --request-timeout <d>` (or
  `TINCAN_WEB_REQUEST_TIMEOUT`) sets a deadline on every request and `0` /
  `none` removes it on every site. A per-ask deadline is opt-in: a line
  `timeout: <duration>`, first or right after the `new chat` /
  `conversation:` line. Nothing caps any of these. A `timeout:` line whose
  value is not a positive Go duration is left as message text; a dot
  (one-thread site) sends the body as is. The reply wait's poll budget follows
  the same setting, and a request with no deadline has no budget.
- **Dead-tab check, from tab state, not a clock.** On a request with no
  deadline, every read that does not find the answer finished counts as alive
  when the conversation's reply changed since the last read, or the send's
  tab is still open on its conversation, whatever its page shows: ChatGPT
  Pro can think for hours with no stop button, streaming mark or token
  output, so an idle-looking page is never taken for a dead one. Only 15
  reads in a row (`DefaultWebStuckChecks`, about 5 minutes at the 20-second
  cadence) that find the tab dead (closed, crashed, sent off the site, or
  moved to another conversation) with the reply unchanged end the request
  as stuck, with a reply naming the conversation; 15 not-found reads in a
  row end it as gone. The tab is read with a new read-only extension
  operation, `<site>.tab {conversation_id}` (`OpChatGPTTab`, `TabState`),
  answering `{open, generating, thinking, assistant_chars}` for a tab a send
  left open, from the same isolated-world `pageProbe` a send uses. The probe
  gains `assistantChars` (a length) and `thinking`: ChatGPT's thinking
  indicator or reasoning or summary panel (`SELECTORS.chatgpt.thinking`, and
  `thinkingText` matched in the page's status and alert regions). Like close
  it reaches no site and needs no grant. An extension older than this cannot
  answer it; the agent then logs once and never calls the request stuck.
- **(b) Tab kept while the answer is written.** A kept tab is looked at
  every `KEEP_TAB_MS`: it stays while the Go side is still waiting on it (a
  `<site>.tab` read within `KEEP_TAB_MS`) or `pageProbe` shows the answer
  being written or thought through, and closes once neither holds or the
  probe fails. So the keeper and the dead-tab check make one decision: a tab
  the Go side waits on is never closed under it. There is no cap by default
  (`createSender`'s `keepMaxMs` can set one). The Go side still closes it as
  soon as the reply is read or the request ends.
- **(c) Claim renewal.** While it waits for a reply, the web agent posts a
  progress note every 4 minutes (`DefaultWebLeaseRenew`, inside the urgent
  10-minute lease), on the wait's own clock and also inside one long sleep
  (a long `Retry-After`). The note carries no request or answer text:
  "Still waiting for ChatGPT to finish its answer (1h20m so far)." Against a
  relay without progress notes it logs once and carries on. This uses the
  existing `/v1/requests/{id}/progress` route, so no protocol change.
  `client.ErrProgressUnsupported` names that case.
- **Other asks queue, with no expiry.** A web agent still answers one
  request at a time. Asks that arrive during a long one stay `queued` at the
  relay and are handled in turn. An ask to a `chatgpt-web` target (by its
  kind, or by that name when it has none, `onboard.RuntimeKind`) gets
  `NoRequestExpiry` instead of the 24-hour `RequestTTL`, so it never expires
  while it waits; other web agents keep 24 hours. Queueing was the least
  risky choice; a second tab per ask would mean concurrent waits sharing the
  journal, the state file and the send path.
- **No follow-up into a running conversation.** The send journal no longer
  drops an entry for its age: since a chatgpt-web request never expires, one
  can come back after the agent was offline for days. An entry not written or
  touched (each claim renewal touches it, `webSend.Touched`) for 25.5 hours is
  dropped only once the relay says its request is finished (answered,
  failed, declined, cancelled, expired) or no longer this agent's
  (`pruneSettledJournal`, run before each request); a live request, or a
  relay that cannot be read, keeps it. Journal times use the agent's
  injectable clock. So a request the relay hands back after a wait or an
  agent outage of any length is resumed, never re-sent. ChatGPT asks also read the
  conversation before the send and refuse ("Nothing was sent") when its
  latest answer is still being written, in addition to the extension's page
  check.
- **Failover.** The relay's `failoverIneligible` looks past leading blank
  lines, a leading `timeout:` line and blank lines after it, so an ask that
  names a conversation still never moves to another vendor, however it is
  laid out; `greptile.json`'s rule says the same.
- Docs: README, `docs/adapters/web-agents.md`, `docs/protocol.md`,
  `docs/trust-model.md` (the tab operation) and `extension/README.md`.

### Notes for review

- A `--notify` ask's claim has no lease, so (c) bites on ordinary asks, which
  is what the behavioural tests use.
- With no deadline, a long ChatGPT answer holds `chatgpt-web` until it ends,
  and later asks wait queued, without expiring.
- **Known follow-up (P2, not fixed here).** A request whose tab stays open
  and readable on its conversation is never ended by the dead-tab check,
  whatever the page shows. So an answer whose backend never marks it done
  holds the single worker until the owner closes that tab (then it ends as
  stuck), the conversation is gone, or an error that will not clear, and asks
  queue behind it meanwhile. Relatedly, `tabState` compares the tab's
  conversation id only on a conversation URL, so a tab the site moves to a
  same-site page that is not a conversation is not yet taken as dead. Quiet
  text or UI alone cannot tell this from a valid silent Pro think, so an
  inactivity deadline would bring back the round-1 P1; a fix needs positive
  evidence that the answer failed.
- The thinking selectors and words are written from the reviewer's
  synthetic fixtures, not captured from a live Pro session. They matter only
  to the keeper for a tab nobody waits on; the Go side's check does not use
  them.
- An extension reload forced after its 5-minute wait still closes kept tabs,
  as before; a request with no deadline then ends as stuck unless the
  conversation keeps changing.
- `answerStaleAfter` (24 hours) only decides whether an old unfinished turn
  still blocks a new send into its conversation; it bounds no request.

### Testing

`make test` (`go test -race ./...`), `make vet`, `make lint` (golangci-lint
v2.11.4: 0 issues) and the extension's send, ops, input and options tests
pass. Two tests in `extension/test/background.test.js` ("background connects
to the native host ..." and "an older hello that finishes last is dropped")
fail the same way on an unchanged main checkout with node v22.23.2;
`background.js` is untouched.

The long behavioural proofs read once a minute (once every 5 minutes for the
26-hour one) instead of on the 20-second cadence, which the claims they check
do not depend on, so they stay cheap under `-race`: `internal/history` takes
about 8 minutes alone under `-race` on a loaded 16-core host, inside the
default 10-minute package timeout. On that host, full `go test -race ./...`
runs while the load average was 16 to 38 also failed timing-sensitive tests
in `internal/wake`, `internal/relay` and `internal/council`; each of those
packages passed alone with `-race -count=1`. The last full run,
`go test -race ./... -timeout 30m` on the final head (load average about
30), passed 19 packages with no data race and failed one: `internal/wake`
hit the 30-minute timeout inside `TestLateFailedSendDoesNotReplaceNewerOK`.
Run alone, `go test -race -count=1 ./internal/wake` passed in 141 seconds.

Both `internal/wake` failures are already on main, and this PR touches no
wake code or test (wake's only changed dependency is a new unused sentinel
error in `internal/client`):

- `TestRequestFollowUpAfterSilent2xx` (`webhook calls = 2, want 1`, line
  720): its follow-up fires from a real 20 ms `time.AfterFunc` while the test
  polls every 5 ms, so a starved scheduler lets it see the second call.
  `-race -count=200` under the same host load failed 3 times on this branch
  before the final fix and 6 times on main, with the same message.
- `TestLateFailedSendDoesNotReplaceNewerOK`: when its check at line 1018
  fails, `t.Fatalf` runs before `close(rc.holdFirst)`, so the
  `httptest.Server` cleanup waits forever on the held handler and the
  package times out. `-race -count=300` of the two tests hung there on both
  this branch (after 8m48s) and main (after 8m59s).

New tests:

- Fix round 2 (review finding): `TestWebOutageLongerThanJournalRetentionIsNeverSentTwice`
  (through the real relay, client and agent: the agent dies mid-answer,
  stays down 27 hours or 30 days while the request waits on the relay,
  then restarts; the ask is answered once and never typed again) and
  `TestWebJournalKeepsEntriesUntilTheRelaySaysTheyAreSettled` (old entries
  are all kept while the relay cannot be read; with the relay up only the
  finished and unknown ones go).
- Fix round 1 (review findings): `TestWebProThinkingWithNoMarkIsNeverStuck`
  (a 2-hour think with no token output and no known mark, through the real
  relay, client and agent), the extension tests "a Pro thinking tab with no
  stop button stays open for hours", "a reasoning panel with no stop button
  keeps the tab", "a kept tab the Go side is watching stays open with no
  recognised mark" and "tabState of a kept tab moved to another conversation
  is not open"; `TestWebSecondAskQueuedPast24HoursIsNotExpired` (an ask
  queued for over a day behind a 26h40m answer) and
  `TestChatGPTWebRequestsDoNotExpire` (relay, by kind and by name);
  `TestFailoverNeverMovesATimeoutPrefixedConversationAsk` (the real relay's
  send route with the target limited) and new `TestFailoverIneligible` and
  parser cases with leading blank lines.
- Unit: `TestWebRequestTimeoutPerSiteAndPerAsk`,
  `TestParseWebRequestTimeoutLine`, `TestWebReplyWaitBudgetFollowsRequestTimeout`
  (a), `TestWebStuckWatchCountsOnlyReadsWithNoSignOfLife` (stuck check),
  `TestWebClaimKeeperRenewsThroughALongSleep`, `TestWaitedText` (c),
  `TestWebRequestTimeoutFlagAndEnv`, `TestWebServeRejectsBadRequestTimeout`
  (CLI), `TestFailoverIneligible` (new cases), the op tables
  (`chatgpt.tab`), and the extension tests for the kept tab (kept while
  writing over 30 minutes then closed; never closed by the timer while writing
  over 30 hours; an opt-in cap; an unprobeable tab closed) and `tabState`
  (open, writing, growing, gone; sent off the site).
- Behavioural, through the real relay (`internal/testrelay`), client and web
  agent with a fake browser and fake tab, on one fake clock that also moves
  the relay store's clock and runs relay sweeps:
  - `TestWebLongChatGPTAnswerIsDelivered`: on defaults, an answer still being
    written at 6 hours (it finishes at about 6.5) is delivered once; at 6 hours
    the asker reads it claimed with a progress note.
  - `TestWebLongAnswerKeepsItsClaim` (ask and urgent): never requeued at the
    30- or 10-minute lease.
  - `TestWebStuckTabEndsTheAskCleanly`: an answer written for 2 hours whose
    tab then closes ends a few minutes later as stuck, failed with the
    conversation named, its tab closed, sent once.
  - `TestWebProThinkingWithNoMarkIsNeverStuck`: a silent think of over 2
    hours, tab open and showing nothing known, is answered once.
  - `TestWebSecondAskQueuedPast24HoursIsNotExpired`: an ask queued an hour
    into a 26h40m answer is still `queued` at 26 hours and then answered.
  - `TestWebPerAskDeadlineStillWorks`: `timeout: 30m` ends the ask at 30
    minutes, and the line is not typed.
  - `TestWebSecondAskDuringALongOneIsQueuedThenServed`: a second ask sent an
    hour into a 6-hour answer is still `queued` at 3 hours, with no relay
    note, and is answered after the long one.
  - `TestWebRequeueHoursIntoAWaitIsNotSentAgain`,
    `TestWebNeverTypesIntoAConversationStillAnswering`: no follow-up.

  Each asserts no `requeued`/`expired` audit row and no relay note to the
  asker.

Before the fix (`internal/history/web_long_test.go` copied onto main):

```
--- FAIL: TestWebLongChatGPTAnswerIsDelivered
    after 7m50s: status failed, reply "Sorry, ChatGPT did not finish answering in time. ..."
--- FAIL: TestWebLongAnswerKeepsItsClaim/ask
    the asker was told "chatgpt-web claimed this 30m ago and did not reply or post progress before its claim lease ran out; it has been requeued. ..."
--- FAIL: TestWebLongAnswerKeepsItsClaim/urgent
    the asker was told "chatgpt-web claimed this 10m ago and did not reply or post progress before its claim lease ran out; it has been requeued. ..."
--- FAIL: TestWebStuckTabEndsTheAskCleanly/tab_closed
    after 7m50s: failed "Sorry, ChatGPT did not finish answering in time. ..."
--- FAIL: TestWebPerAskDeadlineStillWorks
    the ask ended after 7m50s, want its 30 minutes
--- FAIL: TestWebSecondAskDuringALongOneIsQueuedThenServed
    after 7m50s: status failed, reply "Sorry, ChatGPT did not finish answering in time. ..."
--- FAIL: TestWebRequeueHoursIntoAWaitIsNotSentAgain
    sends = 2, want only the first ask: the resumed request was typed into the conversation again
--- FAIL: TestWebNeverTypesIntoAConversationStillAnswering
    "and a follow-up": sends = 2: a follow-up was typed into a conversation still answering
```

The review findings, on the head before fix round 1 (`ad7ee48`):

```
--- FAIL: TestWebProThinkingWithNoMarkIsNeverStuck
    after 16m0s: status failed, reply "Sorry, ChatGPT stopped writing its answer: ..."
    (the proof reads once a minute; at the default 20-second cadence it was 4m30s)
--- FAIL: TestWebSecondAskQueuedPast24HoursIsNotExpired
    at 26h the second ask was expired, relay note "This request expired after 24h with no reply. chatgpt-web never picked it up. ..."
--- FAIL: TestFailoverNeverMovesATimeoutPrefixedConversationAsk
    "\ntimeout: 6h\nconversation: abc-123\nprivate follow-up": moved to muse (moved_from [grokbot]), across vendors
not ok - a Pro thinking tab with no stop button stays open for hours (thinking status text)
    the thinking tab was closed after 10 minutes
not ok - a reasoning panel with no stop button keeps the tab, and tabState reports it thinking
not ok - a kept tab the Go side is watching stays open with no recognised mark; unwatched and quiet it closes
not ok - tabState of a kept tab moved to another conversation is not open, and the tab is left to the owner
```

All of them pass after it.

The fix round 2 finding, on the head before it (`6a81f63`):

```
--- FAIL: TestWebOutageLongerThanJournalRetentionIsNeverSentTwice/27h0m0s
    sends = 2 after a 27h0m0s outage: the request was typed into ChatGPT again
--- FAIL: TestWebOutageLongerThanJournalRetentionIsNeverSentTwice/720h0m0s
    sends = 2 after a 720h0m0s outage: the request was typed into ChatGPT again
```

Both pass after it, and so does the reviewer's crash-restart harness
(`after 27h outage: status=queued ... retained_entries=1 sends=1`, then
`restart: PollOnce=1 status=answered sends=1`).

On main, the extension's earlier new tests: `tab closed after 10 minutes while
the answer was still being written`, `the tab was closed while the answer was
still being written` (30 hours) and `s.tabState is not a function`. All pass
after the fix. Not run against a live relay or a real ChatGPT account.

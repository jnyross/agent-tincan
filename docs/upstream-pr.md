# Upstream PR draft (mvanhorn/agent-tincan)

Not opened. This is the title and body for a PR from
`jnyross/agent-tincan:upstream/chatgpt-web-no-deadline` to
`mvanhorn/agent-tincan:main`, for the owner to review first. This file is
fork-only and is not on the upstream branch.

## Title

fix(web): no deadline on ChatGPT answers: stuck-tab check, claim renewal, tab kept while writing

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
- **Stuck check, from page state, not a clock.** On a request with no
  deadline, every read that does not find the answer finished counts as alive
  when the conversation's reply changed since the last read, or the send's
  tab is open and its page shows the answer being written (the site's stop
  button or streaming mark) or its answer text grew. 15 reads in a row
  (`DefaultWebStuckChecks`, about 5 minutes at the 20-second cadence) with
  none of those end the request as stuck, with a reply naming the
  conversation; 15 not-found reads in a row end it as gone. One sign of life
  restarts the count, so a long silent "thinking" phase with the stop button
  showing stays alive. The page is read with a new read-only extension
  operation, `<site>.tab {conversation_id}` (`OpChatGPTTab`, `TabState`),
  answering `{open, generating, assistant_chars}` for a tab a send left open,
  from the same isolated-world `pageProbe` a send uses (which gains
  `assistantChars`, a length). Like close it reaches no site and needs no
  grant. An extension older than this cannot answer it; the agent then logs
  once and never calls the request stuck.
- **(b) Tab kept while the answer is written.** A kept tab is probed every
  `KEEP_TAB_MS` with `pageProbe`: it stays while the page still shows the
  answer being written and closes once it does not or the probe fails. There
  is no cap by default (`createSender`'s `keepMaxMs` can set one). The Go side
  still closes it as soon as the reply is read or the request ends.
- **(c) Claim renewal.** While it waits for a reply, the web agent posts a
  progress note every 4 minutes (`DefaultWebLeaseRenew`, inside the urgent
  10-minute lease), on the wait's own clock and also inside one long sleep
  (a long `Retry-After`). The note carries no request or answer text:
  "Still waiting for ChatGPT to finish its answer (1h20m so far)." Against a
  relay without progress notes it logs once and carries on. This uses the
  existing `/v1/requests/{id}/progress` route, so no protocol change.
  `client.ErrProgressUnsupported` names that case.
- **Other asks queue.** A web agent still answers one request at a time.
  Asks that arrive during a long one stay `queued` at the relay, with nothing
  timing them out (the agent's presence peeks claim nothing), and are handled
  in turn. Queueing changed no code and was the least risky choice; a second
  tab per ask would mean concurrent waits sharing the journal, the state file
  and the send path.
- **No follow-up into a running conversation.** Each claim renewal touches
  the request's send journal entry (`webSend.Touched`), and entries are pruned
  25.5 hours after their last touch, so a request the relay hands back after a
  wait of any length is resumed, never re-sent. ChatGPT asks also read the
  conversation before the send and refuse ("Nothing was sent") when its
  latest answer is still being written, in addition to the extension's page
  check.
- **Failover.** The relay's `failoverIneligible` looks past a leading
  `timeout:` line, so an ask that names a conversation still never moves to
  another vendor; `greptile.json`'s rule says the same.
- Docs: README, `docs/adapters/web-agents.md`, `docs/protocol.md`,
  `docs/trust-model.md` (the tab operation) and `extension/README.md`.

### Notes for review

- A `--notify` ask's claim has no lease, so (c) bites on ordinary asks, which
  is what the behavioural tests use.
- With no deadline, a long ChatGPT answer holds `chatgpt-web` until it ends,
  and later asks wait queued. The relay's own lifetime for an unclaimed
  request (24 hours by default) still applies to them.
- The stuck check and the kept tab depend on ChatGPT's stop button or
  streaming mark (`SELECTORS.chatgpt` `stop` / `streaming`) showing in a
  background tab for the whole of a long Pro or deep research answer. That is
  not verified against the live page. If the page stopped showing them early
  while the conversation's data stays unchanged, the request would end as
  stuck about 5 minutes later with a reply naming the conversation; the answer
  itself goes on server-side and is in the conversation. If a page kept
  showing its stop button forever, the request would wait until the owner
  closes that tab (then it ends as stuck) or the conversation is gone.
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

New tests:

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
  - `TestWebStuckTabEndsTheAskCleanly` (tab closed; page idle): an answer
    written for 2 hours that then dies ends a few minutes later as stuck,
    failed with the conversation named, its tab closed, sent once.
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
--- FAIL: TestWebStuckTabEndsTheAskCleanly/tab_closed, /page_idle
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

and the extension's new tests on main: `tab closed after 10 minutes while
the answer was still being written`, `the tab was closed while the answer was
still being written` (30 hours) and `s.tabState is not a function`. All pass
after the fix. Not run against a live relay or a real ChatGPT account.

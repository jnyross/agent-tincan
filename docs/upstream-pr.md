# Upstream PR draft (mvanhorn/agent-tincan)

Not opened. This is the title and body for a PR from
`jnyross/agent-tincan:upstream/chatgpt-web-no-deadline` to
`mvanhorn/agent-tincan:main`, for the owner to review first. This file is
fork-only and is not on the upstream branch.

## Title

fix(web): let a long ChatGPT answer finish: hours-long request timeout, claim renewal, tab kept while writing

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
   after 90 minutes, so a request resumed hours into its wait would have been
   typed into the conversation a second time.

### Changes

- **(a) Request timeout per site and per ask.** A site can carry its own
  request timeout: ChatGPT's is 4 hours (`DefaultChatGPTRequestTimeout`); the
  others keep 8 minutes. `tincan web serve --request-timeout <d>` or
  `TINCAN_WEB_REQUEST_TIMEOUT` overrides it for an agent, and a line
  `timeout: <duration>`, first or right after the `new chat` /
  `conversation:` line, sets one request's own. The relay's failover check
  now looks past a leading `timeout:` line, so an ask that names a
  conversation still never moves to another vendor. Every value is capped at 24 hours
  (`MaxWebRequestTimeout`, the relay's default request lifetime). A
  `timeout:` line whose value is not a positive Go duration is left as message
  text; a dot (one-thread site) sends the body as is. The reply wait's poll
  budget follows the same setting instead of the constant.
- **(b) Tab kept while the answer is written.** A kept tab is probed every
  `KEEP_TAB_MS` with the existing read-only `pageProbe` (stop button or
  streaming mark): it stays while the page still shows the answer being
  written, closes once it does not or the probe fails, and closes anyway at
  `KEEP_TAB_MAX_MS` (24 hours). The Go side still closes it as soon as the
  reply is read.
- **(c) Claim renewal.** While it waits for a reply, the web agent posts a
  progress note every 4 minutes (`DefaultWebLeaseRenew`, inside the urgent
  10-minute lease), on the wait's own clock and also inside one long sleep
  (a long `Retry-After`). The note carries no request or answer text:
  "Still waiting for ChatGPT to finish its answer (1h20m so far)." Against a
  relay without progress notes it logs once and carries on. This uses the
  existing `/v1/requests/{id}/progress` route, so no protocol change.
  `client.ErrProgressUnsupported` names that case.
- **No follow-up into a running conversation.** The send journal keeps
  entries for `MaxWebRequestTimeout + 30m + 1h`, so a request the relay hands
  back hours into its wait is resumed, never re-sent. ChatGPT asks also read
  the conversation before the send and refuse ("Nothing was sent") when its
  latest answer is still being written, in addition to the extension's page
  check.
- Docs: README and `docs/adapters/web-agents.md` describe the timeouts, the
  `timeout:` line, the progress notes, the tab rule and the journal retention.

### Notes for review

- A `--notify` ask's claim has no lease, so (c) bites on ordinary asks, which
  is what the behavioural test uses.
- A web agent answers one request at a time, so a 4-hour ChatGPT wait holds
  `chatgpt-web` for that long; other asks to it wait. The per-ask `timeout:`
  line can raise the bound as well as lower it, up to 24 hours, so any agent
  allowed to ask a web agent can hold it for up to a day. That matches the
  trust model (joined agents are trusted, and the allowlist decides who may
  ask), but it is new: before, no ask held a web agent past 8 minutes.
- (b) depends on ChatGPT's stop button or streaming mark (`SELECTORS.chatgpt`
  `stop` / `streaming`) showing in a background tab for the whole of a long
  Pro or deep research answer. That is not verified against the live page.
  If the page stops showing them early, the tab closes at the next 10-minute
  check, as before; ChatGPT writes the answer server-side and the Go side
  reads it through the API, so the answer is still delivered. Council stages keep
  their own limits and go on without a late member.
- An extension reload forced after its 5-minute wait still closes kept tabs,
  as before.

### Testing

`make test` (`go test -race ./...`), `make vet`, `make lint` (golangci-lint
v2.11.4: 0 issues) and `make extension-test` pass. Two tests in
`extension/test/background.test.js` ("background connects to the native host
..." and "an older hello that finishes last is dropped") fail intermittently on
an unchanged main checkout as well; the send tests are stable.

New tests:

- Unit: `TestFailoverIneligible` (new cases), `TestWebRequestTimeoutPerSiteAndPerAsk`,
  `TestParseWebRequestTimeoutLine`, `TestWebReplyWaitBudgetFollowsRequestTimeout`
  (a), `TestWebClaimKeeperRenewsThroughALongSleep`, `TestWaitedText` (c),
  `TestWebRequestTimeoutFlagAndEnv`, `TestWebServeRejectsBadRequestTimeout`
  (CLI), and the extension tests "a kept tab stays open while its answer is
  still being written (over 30 minutes), then closes", "a kept tab still
  writing at keepMaxMs is closed anyway ...", "a kept tab that can no longer
  be probed is closed at keepMs" (b).
- Behavioural, through the real relay (`internal/testrelay`), client and web
  agent with a fake browser, on one fake clock that also moves the relay
  store's clock and runs relay sweeps: `TestWebLongChatGPTAnswerIsDelivered`
  (a, a ~50-minute answer on defaults), `TestWebLongAnswerKeepsItsClaim`
  (c, ask and urgent), `TestWebRequeueHoursIntoAWaitIsNotSentAgain` and
  `TestWebNeverTypesIntoAConversationStillAnswering` (no follow-up). Each
  asserts one send, one tab close, no `requeued`/`expired` audit row and no
  relay note to the asker.

Before the fix (`internal/history/web_long_test.go` copied onto main):

```
--- FAIL: TestWebLongChatGPTAnswerIsDelivered
    after 7m50s: status failed, reply "Sorry, ChatGPT did not finish answering in time. ..."
--- FAIL: TestWebLongAnswerKeepsItsClaim/ask
    the asker was told "chatgpt-web claimed this 30m ago and did not reply or post progress before its claim lease ran out; it has been requeued. ..."
--- FAIL: TestWebLongAnswerKeepsItsClaim/urgent
    the asker was told "chatgpt-web claimed this 10m ago and did not reply or post progress before its claim lease ran out; it has been requeued. ..."
--- FAIL: TestWebRequeueHoursIntoAWaitIsNotSentAgain
    sends = 2, want only the first ask: the resumed request was typed into the conversation again
--- FAIL: TestWebNeverTypesIntoAConversationStillAnswering
    "and a follow-up": sends = 2: a follow-up was typed into a conversation still answering
```

and the extension's new tab test on main: `tab closed after 10 minutes while
the answer was still being written`. All pass after the fix. Not run against a
live relay or a real ChatGPT account.

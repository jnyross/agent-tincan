package relay

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// P2 (fix round 1), through the real relay's send route: an ask that names
// a conversation after a timeout: line never moves to another vendor when
// its web agent is limited, even with blank lines before the timeout line
// or between it and the conversation line. The web agent reads all of them
// as an explicit conversation (TestParseWebRequestTimeoutLine).
func TestFailoverNeverMovesATimeoutPrefixedConversationAsk(t *testing.T) {
	for _, body := range []string{
		"timeout: 6h\nconversation: abc-123\nprivate follow-up",
		"\ntimeout: 6h\nconversation: abc-123\nprivate follow-up",
		"\r\n\n  \ntimeout: 6h\nconversation: abc-123\nprivate follow-up",
		"timeout: 6h\n\nconversation: abc-123\nprivate follow-up",
	} {
		h, clk := failoverHarness(t)
		limitAt(h, clk, grokAddr, time.Hour)
		in, _ := json.Marshal(map[string]string{"to": "grokbot", "body": body})
		var sent envelope.SendResponse
		h.do(instinctAddr, "POST", "/v1/send", string(in), http.StatusCreated, &sent)
		if sent.To != "grokbot" || len(sent.MovedFrom) != 0 {
			t.Errorf("%q: moved to %s (moved_from %v), across vendors", body, sent.To, sent.MovedFrom)
		}
	}
}

package adapter_test

import (
	"fmt"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
)

// TestResumedIDMustComeBack (second PR #5 review, finding 7): on a resume,
// a reply naming another session is an error on every vendor, and the
// resumed id is kept. Fails on bc0daa5.
func TestResumedIDMustComeBack(t *testing.T) {
	resumed := "0b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e"
	other := "7c4f1e2a-3b5d-4a6c-9e8f-1a2b3c4d5e6f"
	for _, tt := range []struct {
		a   adapter.Adapter
		out string
	}{
		{claude.New(), `{"type":"result","result":"ok","session_id":"%s"}`},
		{grok.New(), `{"result":"ok","sessionId":"%s"}`},
		{codex.New(), `{"type":"thread.started","thread_id":"%s"}`},
	} {
		req := adapter.HeadlessRequest{Kind: adapter.Review, Resume: resumed}
		res, err := tt.a.ParseHeadless(req, adapter.Output{Stdout: []byte(fmt.Sprintf(tt.out, other))})
		if err == nil || res.SessionID != resumed {
			t.Errorf("%s: other id = %+v, %v; want the resumed id and an error", tt.a.Vendor(), res, err)
		}
		res, err = tt.a.ParseHeadless(req, adapter.Output{Stdout: []byte(fmt.Sprintf(tt.out, resumed))})
		if err != nil || res.SessionID != resumed {
			t.Errorf("%s: same id = %+v, %v", tt.a.Vendor(), res, err)
		}
	}
}

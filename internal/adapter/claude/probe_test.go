package claude

import (
	"context"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/probe"
)

func TestProbeFake(t *testing.T) {
	h := fakecli.NewHarness(t, fakecli.Build(t))
	p, err := Adapter{Path: h.Fakes.Claude}.Probe(context.Background(), probe.ExecRunner(10*time.Second))
	if err != nil || p.Vendor != adapter.Claude || p.Version != "2.1.311" || !probe.Has(p, probe.CapSessionID) || p.Connected {
		t.Errorf("Probe = %+v, %v", p, err)
	}
}

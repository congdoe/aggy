package grok

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
	p, err := Adapter{Path: h.Fakes.Grok}.Probe(context.Background(), probe.ExecRunner(10*time.Second))
	if err != nil || p.Vendor != adapter.Grok || p.Version != "1.0.70" || !probe.Has(p, probe.CapTools) || p.Connected {
		t.Errorf("Probe = %+v, %v", p, err)
	}
}

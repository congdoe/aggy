package grok

import (
	"context"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/probe"
)

func TestProbeFake(t *testing.T) {
	h := fakecli.NewHarness(t, fakecli.Build(t))
	p, err := Adapter{Path: h.Fakes.Grok}.Probe(context.Background(), execrun.ExecRunner(10*time.Second))
	if err != nil || p.Vendor != adapter.Grok || p.Version != "1.0.70" || !probe.Has(p, probe.CapTools) || p.Connected {
		t.Errorf("Probe = %+v, %v", p, err)
	}
}

// TestTestedVersions (T6603): the probed fake's version is in range, and
// a newer one gets a warning.
func TestTestedVersions(t *testing.T) {
	a := New()
	if w := adapter.VersionWarning(a, "1.0.70", ""); w != "" {
		t.Errorf("tested version warned: %s", w)
	}
	if w := adapter.VersionWarning(a, "99.0.0", ""); w == "" {
		t.Error("an untested version gave no warning")
	}
}

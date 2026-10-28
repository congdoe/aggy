package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/limits"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/termsafe"
)

const usageUsage = `Usage: aggy usage

Shows what each vendor has left, only where its CLI writes that on this
machine (today: Codex's rate limits in its session files; "near limit"
at or past [switch] warn_at), and what
aggy's own borrows used in the last 7 days. The second part is what aggy
saw, not the vendor's count, bill, or allowance.
`

// usageWindow is how far back aggy-observed consumption goes.
const usageWindow = 7 * 24 * time.Hour

// runUsage is aggy usage. It reads the index read-only and never creates
// it, and reads vendor files only through internal/limits (safefs).
// ccusage and grok usage are not used (T6696).
func runUsage(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("usage", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usageUsage)
			return 0
		}
		fmt.Fprintf(stderr, "aggy usage: %v\n\n%s", err, usageUsage)
		return 2
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "aggy usage: unexpected arguments\n\n%s", usageUsage)
		return 2
	}
	// An unreadable config only changes the near-limit mark (L60-5).
	warnAt := config.DefaultWarnAt
	if cfg, err := config.LoadDefault(); err != nil {
		fmt.Fprintf(stderr, "aggy usage: %s; near limit uses switch.warn_at %v\n", termsafe.Line(err.Error()), warnAt)
	} else {
		warnAt = cfg.Switch.WarnAt
	}
	now := time.Now()
	ctx := context.Background()
	s, version, err := openIndexReadOnly(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aggy usage: %v\n", store.ReadError(err))
		return 1
	}
	var rows []store.BorrowUsage
	var statuses []store.VendorStatus
	if s != nil {
		defer func() { _ = s.Close() }()
		// An index older than the vendors table has no sign-in record, so
		// every vendor is listed; a failed read of the table is an error.
		if version >= vendorsSchema {
			if statuses, err = s.VendorStatuses(ctx); err != nil {
				fmt.Fprintf(stderr, "aggy usage: %v\n", store.ReadError(err))
				return 1
			}
		}
		if rows, err = s.BorrowUsageSince(ctx, now.Add(-usageWindow)); err != nil {
			fmt.Fprintf(stderr, "aggy usage: %v\n", store.ReadError(err))
			return 1
		}
	}
	vendors := usageVendors(statuses)

	fmt.Fprintln(stdout, "Remaining allowance, as each vendor's CLI wrote it on this machine:")
	for _, v := range vendors {
		if v != adapter.Codex {
			fmt.Fprintf(stdout, "  %-7s not written locally; aggy cannot see what is left\n", v)
			continue
		}
		var quotas []limits.CodexQuota
		if dir, err := codexSessionsDir(); err == nil {
			quotas = limits.CodexAllowance(dir, now, "")
		}
		if len(quotas) == 0 {
			fmt.Fprintf(stdout, "  %-7s nothing written in the last %d days\n", v, int(limits.Max.Hours()/24))
			continue
		}
		for _, line := range codexAllowanceLines(quotas, now) {
			fmt.Fprintf(stdout, "  %-7s %s\n", v, line)
		}
		if u, ok := limits.CodexUsed(quotas, now); ok && warnAt > 0 && u.Used >= warnAt {
			fmt.Fprintf(stdout, "  %-7s near limit: %d%% used, at or past switch.warn_at %v\n", v, int(math.Floor(u.Used*100+1e-9)), warnAt)
		}
	}

	fmt.Fprintf(stdout, "\nConsumption aggy observed, last %d days: aggy's own borrows only, as the CLIs\n"+
		"reported them. Not the vendor's count, bill, or allowance; on-screen sessions are not counted.\n",
		int(usageWindow.Hours()/24))
	by := map[string]store.BorrowUsage{}
	for _, r := range rows {
		by[r.Vendor] = r
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  VENDOR\tBORROWS\tTOKENS\tTIME")
	for _, v := range vendors {
		r := by[string(v)]
		tokens := fmt.Sprint(r.Tokens)
		if r.WithTokens < r.Borrows {
			tokens += fmt.Sprintf(" (%d of %d borrows reported tokens)", r.WithTokens, r.Borrows)
		}
		fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\n", v, r.Borrows, tokens, r.Time.Round(time.Second))
	}
	_ = tw.Flush()
	return 0
}

// vendorsSchema is the oldest schema with the vendors table (0006).
const vendorsSchema = 6

// openIndexReadOnly opens the index read-only and returns its schema
// version, or a nil store when there is none (or it predates the borrows
// table, schema 1): nothing to show then.
func openIndexReadOnly(ctx context.Context) (*store.Store, int, error) {
	path, err := store.DefaultPath()
	if err != nil {
		return nil, 0, err
	}
	if _, err := safefs.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	} else if err != nil {
		return nil, 0, err
	}
	s, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return nil, 0, err
	}
	v, err := s.Version(ctx)
	if err != nil || v < 1 {
		_ = s.Close()
		return nil, 0, err
	}
	return s, v, nil
}

// usageVendors is every vendor except those setup recorded as signed
// out (design.md, "Any subset of vendors": a skipped vendor is absent
// from usage).
func usageVendors(statuses []store.VendorStatus) []adapter.Vendor {
	out := []adapter.Vendor{}
	for _, v := range adapter.Vendors() {
		skip := false
		for _, s := range statuses {
			skip = skip || s.Vendor == string(v) && s.Connected == string(adapter.SignedOut)
		}
		if !skip {
			out = append(out, v)
		}
	}
	return out
}

// codexAllowanceLines renders each window of the session's Codex quotas:
// the share left and the reset time, as Codex last wrote them. Codex's
// credits balance is not shown: its unit is not recorded.
func codexAllowanceLines(quotas []limits.CodexQuota, now time.Time) []string {
	var out []string
	for _, q := range quotas {
		name := "ordinary quota"
		if q.ID != "codex" {
			name = termsafe.Field(q.Name) + " quota"
		}
		asOf := ""
		if t, err := time.Parse(time.RFC3339Nano, q.At); err == nil {
			asOf = ", as of " + t.Local().Format("2006-01-02 15:04")
		}
		for _, w := range []*limits.CodexWindow{q.Primary, q.Secondary} {
			if w == nil {
				continue
			}
			s := fmt.Sprintf("%s, %s window: ", name, windowLabel(w.WindowMinutes))
			left := shareLeft(w.UsedPercent)
			switch reset := time.Unix(w.ResetsAt, 0); {
			case w.ResetsAt <= 0:
				s += left + ", reset time not written"
			case !reset.After(now):
				// The written share is from before the reset: stale.
				s += "reset at " + reset.Local().Format("2006-01-02 15:04") + "; what is left now is unknown"
			default:
				s += left + ", resets " + reset.Local().Format("2006-01-02 15:04")
			}
			out = append(out, s+asOf)
		}
	}
	return out
}

// usedShare is how much of v's limit is used, where its CLI writes that
// on this machine (Codex: limits.CodexUsed over the rollouts, read through
// cache, the rollout of thread first when known); ok is false elsewhere.
// Claude and Grok write none that docs/verification.md records, so they
// never warn (T6699).
func usedShare(v adapter.Vendor, cache *limits.AllowanceCache, thread string) (supervisor.Share, bool) {
	if v != adapter.Codex {
		return supervisor.Share{}, false
	}
	dir, err := codexSessionsDir()
	if err != nil {
		return supervisor.Share{}, false
	}
	now := time.Now()
	u, ok := limits.CodexUsed(cache.Get(dir, now, thread), now)
	if !ok {
		return supervisor.Share{}, false
	}
	return supervisor.Share{Used: u.Used, Window: fmt.Sprintf("%dm@%d", u.Minutes, u.ResetsAt), Reset: time.Unix(u.ResetsAt, 0)}, true
}

// shareLeft renders what a window has left, floored to a tenth so it is
// never overstated, and never "0%" for a window not used up (L49-3).
func shareLeft(used float64) string {
	left := min(max(100-used, 0), 100)
	switch tenths := math.Floor(left * 10); {
	case left == 0:
		return "0% left"
	case tenths == 0:
		return "under 0.1% left"
	default:
		return strconv.FormatFloat(tenths/10, 'f', -1, 64) + "% left"
	}
}

// windowLabel names a window by its length: "5h", "7d", "90m".
func windowLabel(minutes int64) string {
	switch {
	case minutes <= 0:
		return "unnamed"
	case minutes%(24*60) == 0:
		return fmt.Sprintf("%dd", minutes/(24*60))
	case minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	}
	return fmt.Sprintf("%dm", minutes)
}

package borrow

import (
	"context"
	"errors"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
)

// invalidError is a request that failed validation.
type invalidError struct{ error }

func (e invalidError) Unwrap() error { return e.error }

// Invalid reports whether err is a request that failed validation, before
// anything was chosen or recorded.
func Invalid(err error) bool {
	var ie invalidError
	return errors.As(err, &ie)
}

// adapterCode is the reason code of an adapter's refusal.
func adapterCode(err error) diag.Code {
	switch {
	case errors.Is(err, adapter.ErrNoSandbox):
		return diag.CodeNoSandbox()
	case errors.Is(err, adapter.ErrFileTools):
		return diag.CodeFileTools()
	case errors.Is(err, adapter.ErrNoCapability):
		return diag.CodeNoCapability()
	}
	return diag.CodeUnsupported()
}

// OutcomeCode is the reason code of a failed borrow's outcome.
func OutcomeCode(outcome string) diag.Code {
	switch adapter.ErrorClass(outcome) {
	case adapter.ErrRateLimit:
		return diag.CodeRateLimit()
	case adapter.ErrAuth:
		return diag.CodeAuth()
	case adapter.ErrBilling:
		return diag.CodeBilling()
	case adapter.ErrServer:
		return diag.CodeServer()
	case adapter.ErrInvalidRequest:
		return diag.CodeVendorInvalid()
	case adapter.ErrMaxTurns:
		return diag.CodeMaxTurns()
	case adapter.ErrTimeout:
		return diag.CodeTimeout()
	case adapter.ErrLockdown:
		return diag.CodeLockdown()
	}
	if outcome == Interrupted {
		return diag.CodeInterrupted()
	}
	return diag.CodeChildFailed()
}

// logEnd records how the borrow ended: a failure under its code (a
// lockdown rejection under the vendor's own event), then the end. The
// child's reply and error text are never stored: they are model and
// vendor output.
func (b *Borrower) logEnd(ctx context.Context, ids diag.Rec, res Result, session, cause string) {
	if res.Status != "done" {
		rec := ids
		rec.Event, rec.Code, rec.Cause = diag.EvBorrowFailed(), OutcomeCode(res.Outcome), cause
		if rec.Code == diag.CodeLockdown() {
			rec.Event = diag.LockdownEvent(string(res.Vendor))
		}
		rec.Attrs = map[string]any{"outcome": res.Outcome, "kind": string(res.Kind)}
		cause = b.Diag.Log(ctx, rec)
	}
	rec := ids
	rec.Event, rec.Cause, rec.Duration, rec.VendorSession = diag.EvBorrowFinished(), cause, res.Elapsed, session
	rec.Attrs = map[string]any{
		"kind": string(res.Kind), "status": res.Status, "outcome": res.Outcome, "tokens": res.Tokens,
		"artifacts": len(res.Artifacts), "refused": len(res.Refused),
	}
	b.Diag.Log(ctx, rec)
}

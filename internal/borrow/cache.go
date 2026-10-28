package borrow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// The research cache: the same question reuses a report for a day
// (news) or a week (research), unless --fresh. Entries live in the state
// dir, never in the repo, so a checked-in file cannot pose as a cached
// report; the repo's Markdown copy is rendered from the entry.

// CacheDir holds the cache entries, inside the state dir's research dir.
const CacheDir = "cache"

// maxCacheEntry caps a cache entry read back.
const maxCacheEntry = 8 << 20

// cacheTTL is how long a report answers the same question.
func cacheTTL(k adapter.Kind) time.Duration {
	if k == adapter.News {
		return 24 * time.Hour
	}
	return 7 * 24 * time.Hour
}

// cacheKey identifies a question: the kind, the task, and the
// constraints, each lowercased with its whitespace collapsed.
func cacheKey(req Request) string {
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s", req.Kind, norm(req.Task))
	for _, c := range req.Constraints {
		fmt.Fprintf(h, "\x00%s", norm(c))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (b *Borrower) cachePath(key string) string {
	return filepath.Join(b.StateDir, ResearchDir, CacheDir, key+".json")
}

// cacheStore records r as the answer to its question.
func (b *Borrower) cacheStore(r researchReport) error {
	if _, err := mkdirNoLinks(b.StateDir, ResearchDir, CacheDir); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return safefs.WriteFile(b.cachePath(r.Key), data, 0o600)
}

// cached returns the cached answer to req and true when there is a
// fresh one, after writing its Markdown copy to req's research dir. A
// cached report is used only when it is what a run would do now: its
// vendor is the first one routing picks (with --vendor applied, and the
// parent skip and allowed_vendors), and its model is that vendor's
// configured one.
func (b *Borrower) cached(ctx context.Context, req Request) (Result, bool, error) {
	if !web(req.Kind) || req.Fresh || req.Vendor == Both {
		return Result{}, false, nil
	}
	key := cacheKey(req)
	data, err := safefs.ReadFileLimit(b.cachePath(key), maxCacheEntry)
	if err != nil {
		return Result{}, false, nil
	}
	var r researchReport
	if json.Unmarshal(data, &r) != nil || r.Key != key || r.Kind != string(req.Kind) || !ValidID(r.BorrowID) {
		return Result{}, false, nil
	}
	v, err := adapter.ParseVendor(r.Vendor)
	if err != nil || (req.Vendor != "" && req.Vendor != r.Vendor) {
		return Result{}, false, nil
	}
	// A clock set back, or an entry from the future, is not fresh.
	if age := b.Now().Sub(r.AsOf); age < -5*time.Minute || age >= cacheTTL(req.Kind) {
		return Result{}, false, nil
	}
	rr := RouteRequest{Kind: req.Kind, Parent: req.Parent, Repo: req.Repo}
	if req.Vendor != "" {
		rr.Override = v // req.Vendor == r.Vendor, checked above
	}
	if vs, err := b.Router.Choose(ctx, rr); err != nil || len(vs) == 0 || vs[0] != v || b.Config.Vendors.For(v).Model != r.Model {
		return Result{}, false, nil
	}
	p, err := b.placeResearch(req, r)
	if err != nil {
		return Result{}, false, fmt.Errorf("writing the cached %s report: %w", req.Kind, err)
	}
	res := Result{
		ID: r.BorrowID, Kind: req.Kind, Vendor: v, Model: r.Model, Status: "done", Outcome: "ok",
		Text: r.Text, ResearchPath: p, CachedAt: r.AsOf,
	}
	if inbox := ReportPath(b.StateDir, r.BorrowID); statOK(inbox) {
		res.ReportPath = inbox
	}
	return res, true, nil
}

func statOK(p string) bool {
	_, err := safefs.Lstat(p)
	return err == nil
}

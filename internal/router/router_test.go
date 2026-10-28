package router

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
)

const (
	claude = adapter.Claude
	codex  = adapter.Codex
	grok   = adapter.Grok
)

func vs(v ...adapter.Vendor) []adapter.Vendor { return v }

func TestRouteDefaults(t *testing.T) {
	tests := []struct {
		kind adapter.Kind
		want []adapter.Vendor
	}{
		{adapter.News, vs(grok)},
		{adapter.Research, vs(grok)},
		{adapter.Image, vs(codex, grok)},
		{adapter.Video, vs(grok)},
		{adapter.Review, vs(codex, grok, claude)},
		{adapter.Edit, vs(codex, grok)},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			got, err := Route(config.Default().Routing, tt.kind)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Route = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRoute(t *testing.T) {
	tests := []struct {
		name    string
		route   config.Route
		kind    adapter.Kind
		want    []adapter.Vendor
		wantErr string
	}{
		{name: "order kept", route: config.Route{Vendors: vs(grok, codex)}, kind: adapter.Image, want: vs(grok, codex)},
		{name: "duplicates removed, first place kept", route: config.Route{Vendors: vs(grok, codex, grok, codex)}, kind: adapter.News, want: vs(grok, codex)},
		{name: "empty list", route: config.Route{Vendors: vs()}, kind: adapter.Video, want: vs()},
		{name: "review list in any order", route: config.Route{Vendors: vs(claude, grok)}, kind: adapter.Review, want: vs(claude, grok)},
		{name: "not-author expands", route: config.Route{NotAuthor: true}, kind: adapter.Review, want: NotAuthorOrder},
		{name: "not-author outside review", route: config.Route{NotAuthor: true}, kind: adapter.News, wantErr: `routing.news: "not-author" is only valid for review`},
		{name: "unknown vendor", route: config.Route{Vendors: vs(grok, "gemini")}, kind: adapter.Research, wantErr: `routing.research: unknown vendor "gemini"`},
		{name: "codex in research is kept", route: config.Route{Vendors: vs(codex, grok)}, kind: adapter.Research, want: vs(codex, grok)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r config.Routing
			switch tt.kind {
			case adapter.Image:
				r.Image = tt.route
			case adapter.Video:
				r.Video = tt.route
			case adapter.Research:
				r.Research = tt.route
			case adapter.News:
				r.News = tt.route
			case adapter.Review:
				r.Review = tt.route
			case adapter.Edit:
				r.Edit = tt.route
			}
			got, err := Route(r, tt.kind)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Route = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRouteNotAuthorIsACopy(t *testing.T) {
	got, _ := Route(config.Default().Routing, adapter.Review)
	got[0] = "x"
	if NotAuthorOrder[0] != codex {
		t.Fatal("Route returned NotAuthorOrder itself")
	}
}

func TestRefusal(t *testing.T) {
	for _, k := range adapter.Kinds() {
		for _, v := range adapter.Vendors() {
			err := Refusal(k, v)
			want := v == codex && (k == adapter.Research || k == adapter.News)
			if (err != nil) != want {
				t.Errorf("Refusal(%s, %s) = %v, want refused %v", k, v, err, want)
			}
			if err != nil && (!errors.Is(err, adapter.ErrUnsupported) || !strings.Contains(err.Error(), "apply_patch")) {
				t.Errorf("Refusal(%s, %s) = %v, want ErrUnsupported naming apply_patch", k, v, err)
			}
		}
	}
}

// TestChooseTable covers the table lookups of Choose; the skip rules
// have their own tests.
func TestChooseTable(t *testing.T) {
	user := config.Default()
	user.Routing.Research = config.Route{Vendors: vs(codex, grok)}
	user.Routing.Video = config.Route{Vendors: vs()}
	tests := []struct {
		name      string
		cfg       config.Config
		req       Request
		want      []adapter.Vendor
		wantNotes []string
		wantErr   string
	}{
		{name: "default news", cfg: config.Default(), req: Request{Kind: "news"}, want: vs(grok)},
		{name: "default image", cfg: config.Default(), req: Request{Kind: "image"}, want: vs(codex, grok)},
		{name: "review minus the author", cfg: config.Default(), req: Request{Kind: "review", Parent: grok}, want: vs(codex, claude)},
		{name: "review with no parent", cfg: config.Default(), req: Request{Kind: "review"}, want: vs(codex, grok, claude)},
		{name: "edit is routed", cfg: config.Default(), req: Request{Kind: "edit"}, want: vs(codex, grok)},
		{name: "user codex research is kept with a note", cfg: user, req: Request{Kind: "research"}, want: vs(codex, grok), wantNotes: []string{"codex: research: not supported by this CLI until T6638"}},
		{name: "override replaces the list", cfg: config.Default(), req: Request{Kind: "image", Override: grok}, want: vs(grok)},
		{name: "override may name a vendor off the list", cfg: config.Default(), req: Request{Kind: "video", Override: claude}, want: vs(claude)},
		{name: "unknown kind", cfg: config.Default(), req: Request{Kind: "chat"}, wantErr: `unknown kind "chat"`},
		{name: "unknown override", cfg: config.Default(), req: Request{Kind: "news", Override: "gemini"}, wantErr: `--vendor: unknown vendor "gemini"`},
		{name: "unknown parent", cfg: config.Default(), req: Request{Kind: "news", Parent: "gemini"}, wantErr: `parent: unknown vendor "gemini"`},
		{name: "kind turned off", cfg: user, req: Request{Kind: "video"}, wantErr: "no vendor available for video: [routing] video lists no vendor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New(tt.cfg, Deps{Statuses: allConnected()})
			d, err := r.Decide(context.Background(), tt.req)
			got, cerr := r.Choose(context.Background(), tt.req)
			if (err == nil) != (cerr == nil) {
				t.Fatalf("Decide err %v, Choose err %v", err, cerr)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) || !slices.Equal(d.Candidates, tt.want) {
				t.Errorf("Choose = %v, Decide = %v, want %v", got, d.Candidates, tt.want)
			}
			if len(d.Notes) != len(tt.wantNotes) {
				t.Fatalf("notes = %q, want %q", d.Notes, tt.wantNotes)
			}
			for i, n := range tt.wantNotes {
				if !strings.Contains(d.Notes[i], n) {
					t.Errorf("note %d = %q, want %q", i, d.Notes[i], n)
				}
			}
		})
	}
}

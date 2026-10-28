package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/patch"
)

// stub is a minimal Adapter that proves the interface can be implemented
// with commands built and output parsed, and no process started.
type stub struct{}

var _ Adapter = stub{}

func (stub) Vendor() Vendor { return Grok }

func (stub) TestedVersions() VersionRange { return VersionRange{Min: "1.0.70", Max: "1.0.70"} }

func (stub) Probe(ctx context.Context, run Runner) (Probe, error) {
	out, err := run(ctx, Command{Path: "fake-vendor", Args: []string{"--version"}})
	if err != nil {
		return Probe{}, err
	}
	return Probe{
		Vendor:    Grok,
		Path:      "fake-vendor",
		Version:   strings.TrimSpace(string(out.Stdout)),
		Connected: out.ExitCode == 0,
		ProbedAt:  time.Now(),
	}, nil
}

func (stub) Interactive(req InteractiveRequest) (Command, error) {
	var args []string
	if req.Resume != "" {
		args = append(args, "--resume", req.Resume)
	}
	return Command{Path: "fake-vendor", Args: args, Dir: req.Cwd, Stdin: StdinTerminal}, nil
}

func (stub) Headless(req HeadlessRequest) (Command, error) {
	if req.PromptFile == "" {
		return Command{}, errors.New("prompt file required")
	}
	args := []string{"--prompt-file", req.PromptFile}
	if req.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	return Command{Path: "fake-vendor", Args: args, Dir: req.Cwd, Env: req.Env}, nil
}

func (stub) ParseHeadless(_ HeadlessRequest, out Output) (HeadlessResult, error) {
	var v struct {
		SessionID string `json:"sessionId"`
		Result    string `json:"result"`
		Error     string `json:"error"`
	}
	if err := json.NewDecoder(bytes.NewReader(out.Stdout)).Decode(&v); err != nil {
		return HeadlessResult{}, err
	}
	res := HeadlessResult{SessionID: v.SessionID, Text: v.Result}
	if v.Error == "rate_limit" {
		res.Error = ErrRateLimit
	}
	return res, nil
}

func (stub) Artifacts(HeadlessRequest, HeadlessResult) ([]Artifact, error) { return nil, nil }

func (stub) TranscriptTail(SessionRef, int) (Transcript, error) {
	return Transcript{}, ErrNoTranscript
}

func (stub) ConfigPatches(ConfigContext) ([]patch.Patch, error) { return nil, nil }

func TestStubAdapter(t *testing.T) {
	var a Adapter = stub{}
	var ran []Command
	run := func(_ context.Context, c Command) (Output, error) {
		ran = append(ran, c)
		return Output{Stdout: []byte("1.0.70\n")}, nil
	}
	p, err := a.Probe(context.Background(), run)
	if err != nil || p.Version != "1.0.70" || !p.Connected {
		t.Fatalf("Probe = %+v, %v", p, err)
	}
	if len(ran) != 1 {
		t.Errorf("runner called %d times", len(ran))
	}

	req := HeadlessRequest{Kind: News, PromptFile: "/tmp/p.md", Cwd: "/tmp/x", MaxTurns: 3}
	cmd, err := a.Headless(req)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Stdin != StdinNull {
		t.Error("headless stdin must default to null")
	}
	res, err := a.ParseHeadless(req, Output{Stdout: []byte(`{"sessionId":"s1","result":"ok","error":"rate_limit"}`)})
	if err != nil || res.SessionID != "s1" || res.Error != ErrRateLimit {
		t.Errorf("ParseHeadless = %+v, %v", res, err)
	}
	if _, err := a.TranscriptTail(SessionRef{}, 5); !errors.Is(err, ErrNoTranscript) {
		t.Errorf("TranscriptTail = %v", err)
	}
}

func TestParseVendor(t *testing.T) {
	tests := []struct {
		in      string
		want    Vendor
		wantErr bool
	}{
		{"claude", Claude, false},
		{"codex", Codex, false},
		{"grok", Grok, false},
		{"Claude", "", true},
		{"gemini", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := ParseVendor(tt.in)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("ParseVendor(%q) = %q, %v", tt.in, got, err)
		}
	}
}

func TestParseKind(t *testing.T) {
	tests := []struct {
		in       string
		want     Kind
		wantErr  bool
		readOnly bool
	}{
		{"image", Image, false, true},
		{"video", Video, false, true},
		{"research", Research, false, true},
		{"news", News, false, true},
		{"review", Review, false, true},
		{"edit", Edit, false, false},
		{"chat", "", true, true},
	}
	for _, tt := range tests {
		got, err := ParseKind(tt.in)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("ParseKind(%q) = %q, %v", tt.in, got, err)
		}
		if err == nil && got.ReadOnly() != tt.readOnly {
			t.Errorf("%s.ReadOnly() = %v", got, got.ReadOnly())
		}
	}
}

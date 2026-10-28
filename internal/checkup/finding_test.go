package checkup

import (
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// secret is a value no output may hold.
const secret = "sk-ant-SECRETVALUE0123456789"

func TestAPIKeyEnv(t *testing.T) {
	tests := []struct {
		name    string
		environ []string
		want    map[adapter.Vendor]string // vendor: variable named
	}{
		{"none", []string{"HOME=/h", "PATH=/bin"}, nil},
		{"empty is unset", []string{"ANTHROPIC_API_KEY=", "OPENAI_API_KEY="}, nil},
		{"all three", []string{"ANTHROPIC_API_KEY=" + secret, "OPENAI_API_KEY=" + secret, "XAI_API_KEY=" + secret},
			map[adapter.Vendor]string{adapter.Claude: "ANTHROPIC_API_KEY", adapter.Codex: "OPENAI_API_KEY", adapter.Grok: "XAI_API_KEY"}},
		{"last entry wins", []string{"XAI_API_KEY=" + secret, "XAI_API_KEY="}, nil},
		{"other names ignored", []string{"anthropic_api_key=" + secret, "ANTHROPIC_API_KEY_FILE=/x", "GEMINI_API_KEY=" + secret}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := APIKeyEnv(tt.environ)
			if len(got) != len(tt.want) {
				t.Fatalf("findings = %+v", got)
			}
			for _, f := range got {
				if f.Code != CodeAPIKeyEnv || f.Level != LevelWarn || !strings.HasPrefix(f.Message, tt.want[f.Vendor]+" is set") {
					t.Errorf("finding = %+v", f)
				}
				if !strings.Contains(f.Message, "may bill the API instead of the subscription") {
					t.Errorf("message = %q", f.Message)
				}
				if strings.Contains(f.Message, secret) || strings.Contains(f.Message, "SECRET") {
					t.Errorf("the value leaked: %q", f.Message)
				}
			}
		})
	}
}

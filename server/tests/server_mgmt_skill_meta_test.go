//go:build ignore

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/skill"
	"gopkg.in/yaml.v3"
)

func TestSkillMetaUpdateRoundTrip(t *testing.T) {
	body := "\n# Instructions\n\nKeep this body: exactly.\n---\n\tIndented content\n"
	tests := []struct {
		name        string
		header      string
		request     string
		wantMCPs    []string
		wantDesc    string
		wantLicense string
		wantCompat  string
		wantMeta    map[string]any
	}{
		{
			name:     "associate MCP without changing other fields",
			header:   "name: demo\ndescription: Original\nallowed-tools: Bash(npm:*)\n",
			request:  `{"mcps":[" ScopeSentry ","","browser"]}`,
			wantMCPs: []string{"ScopeSentry", "browser"},
			wantDesc: "Original",
		},
		{
			name: "preserve block lists and nested metadata",
			header: "name: demo\ndescription: |-\n  First line: detail\n  Second line\n" +
				"allowed-tools:\n  - Bash(npm:*)\n  - Read\n" +
				"metadata:\n  owner:\n    name: team\n  tags: [web, recon]\n" +
				"mcps:\n  - old-browser\n",
			request:  `{"mcps":["ScopeSentry"]}`,
			wantMCPs: []string{"ScopeSentry"},
			wantDesc: "First line: detail\nSecond line",
			wantMeta: map[string]any{
				"owner": map[string]any{"name": "team"},
				"tags":  []any{"web", "recon"},
			},
		},
		{
			name:        "quote updated strings and MCP names",
			header:      "name: demo\ndescription: Original\n",
			request:     `{"mcps":["team: browser","browser #1"],"description":"New: detail # text\nSecond line","license":"License: custom","compatibility":"Linux: headless"}`,
			wantMCPs:    []string{"team: browser", "browser #1"},
			wantDesc:    "New: detail # text\nSecond line",
			wantLicense: "License: custom",
			wantCompat:  "Linux: headless",
		},
		{
			name:     "clear canonical and legacy MCP fields",
			header:   "name: demo\ndescription: Original\nmcps: browser\nmcp: ScopeSentry\n",
			request:  `{"mcps":[]}`,
			wantDesc: "Original",
		},
		{
			name:        "preserve omitted MCP association",
			header:      "name: demo\ndescription: Original\nmcp: ScopeSentry\n",
			request:     `{"license":"MIT"}`,
			wantMCPs:    []string{"ScopeSentry"},
			wantDesc:    "Original",
			wantLicense: "MIT",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			skillDir := filepath.Join(dir, "demo")
			if err := os.Mkdir(skillDir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(skillDir, "SKILL.md")
			if err := os.WriteFile(path, []byte("---\n"+tt.header+"---\n"+body), 0o644); err != nil {
				t.Fatal(err)
			}
			s := &Server{skillDir: dir, m: &Manager{}}
			req := httptest.NewRequest(http.MethodPut, "/api/skills/demo/meta", strings.NewReader(tt.request))
			req.SetPathValue("name", "demo")
			rr := httptest.NewRecorder()
			s.fsUpdateSkillMeta(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("update status=%d body=%s", rr.Code, rr.Body)
			}

			reg, err := skill.LoadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			loaded, ok := reg.Get("demo")
			if !ok || !slices.Equal(loaded.MCPs, tt.wantMCPs) || loaded.Description != tt.wantDesc {
				t.Fatalf("runtime mcps=%v description=%q, want mcps=%v description=%q",
					loaded.MCPs, loaded.Description, tt.wantMCPs, tt.wantDesc)
			}
			if loaded.License != tt.wantLicense || loaded.Compatibility != tt.wantCompat {
				t.Fatalf("runtime license=%q compatibility=%q", loaded.License, loaded.Compatibility)
			}
			if loaded.Instructions != strings.TrimSpace(body) {
				t.Fatalf("runtime instructions changed: %q", loaded.Instructions)
			}

			list := httptest.NewRecorder()
			s.fsListSkills(list, httptest.NewRequest(http.MethodGet, "/api/skills", nil))
			var response struct {
				Skills []skillFileNode `json:"skills"`
			}
			if err := json.Unmarshal(list.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if list.Code != http.StatusOK || len(response.Skills) != 1 ||
				!slices.Equal(response.Skills[0].MCPs, tt.wantMCPs) {
				t.Fatalf("list status=%d body=%s", list.Code, list.Body)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasSuffix(raw, []byte("---\n"+body)) {
				t.Fatalf("instruction body was not preserved verbatim: %q", raw)
			}
			end := strings.Index(string(raw)[4:], "\n---") + 4
			var metadata map[string]any
			if err := yaml.Unmarshal(raw[4:end], &metadata); err != nil {
				t.Fatal(err)
			}
			if tt.wantMeta != nil && !reflect.DeepEqual(metadata["metadata"], tt.wantMeta) {
				t.Fatalf("nested metadata=%v want %v", metadata["metadata"], tt.wantMeta)
			}
			if strings.Contains(tt.header, "allowed-tools:") && metadata["allowed-tools"] == nil {
				t.Fatal("unknown allowed-tools field was lost")
			}
		})
	}
}

func TestSkillMetaRejectsMalformedFrontmatter(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "missing header", raw: "# Instructions\n"},
		{name: "unclosed header", raw: "---\nname: demo\n"},
		{name: "sequence header", raw: "---\n- demo\n---\nbody\n"},
		{name: "duplicate key", raw: "---\nname: demo\nname: other\n---\nbody\n"},
		{
			name: "ScopeSentry markdown inside header",
			raw: "---\n\n## name: scopesentry-mcp\ndescription: ScopeSentry\n\n" +
				"# ScopeSentry MCP 使用指南\n\n面向已部署的实例。\n\n---\n\n## Tools\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			skillDir := filepath.Join(dir, "demo")
			if err := os.Mkdir(skillDir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(skillDir, "SKILL.md")
			if err := os.WriteFile(path, []byte(tt.raw), 0o644); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPut, "/api/skills/demo/meta", strings.NewReader(`{"mcps":["ScopeSentry"]}`))
			req.SetPathValue("name", "demo")
			rr := httptest.NewRecorder()
			(&Server{skillDir: dir}).fsUpdateSkillMeta(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rr.Code, rr.Body)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.raw {
				t.Fatal("rejected update changed the skill file")
			}
		})
	}
}

func TestBundledScopeSentryMetadata(t *testing.T) {
	reg, err := skill.LoadDir(filepath.Join("..", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	sk, ok := reg.Get("scopesentry")
	if !ok || sk.Description == "" || !strings.HasPrefix(sk.Instructions, "# ScopeSentry MCP 使用指南") {
		t.Fatalf("bundled ScopeSentry metadata did not load: name=%q description=%q", sk.Name, sk.Description)
	}
}

//go:build ignore

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func workspaceFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	fixture := t.TempDir()
	workDir := filepath.Join(fixture, "workspace")
	outside := filepath.Join(fixture, "outside")
	for _, dir := range []string{workDir, outside} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{m: &Manager{dir: workDir}, jwtKey: []byte("workspace-test-key-32bytes-long-000")}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	return s, token, outside
}

func workspaceRequest(s *Server, token string, handler http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.requireAuth(handler).ServeHTTP(w, req)
	return w
}

func workspaceUploadRequest(t *testing.T, path, name, content string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/workspace/upload?path="+path, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

// Directory junctions reproduce the reported Windows boundary escape even when
// the current account cannot create symbolic links.
func workspaceDirLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	} else if runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("create fixture junction: %v: %s", err, out)
	}
}

func TestWorkspaceRejectsOutsideLinks(t *testing.T) {
	s, token, outside := workspaceFixture(t)
	marker := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(marker, []byte("outside-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceDirLink(t, outside, filepath.Join(s.m.dir, "escape"))
	cases := []struct {
		name    string
		handler http.HandlerFunc
		req     *http.Request
	}{
		{"read", s.wsRead, httptest.NewRequest("GET", "/api/workspace/read?path=escape/secret.txt", nil)},
		{"list", s.wsList, httptest.NewRequest("GET", "/api/workspace/list?path=escape", nil)},
		{"download", s.wsDownload, httptest.NewRequest("GET", "/api/workspace/download?path=escape/secret.txt", nil)},
		{"write existing", s.wsWrite, httptest.NewRequest("POST", "/api/workspace/write", strings.NewReader(`{"path":"escape/secret.txt","content":"changed"}`))},
		{"write new", s.wsWrite, httptest.NewRequest("POST", "/api/workspace/write", strings.NewReader(`{"path":"escape/new.txt","content":"changed"}`))},
		{"mkdir", s.wsMkdir, httptest.NewRequest("POST", "/api/workspace/mkdir", strings.NewReader(`{"path":"escape/new-directory"}`))},
		{"upload", s.wsUpload, workspaceUploadRequest(t, "escape", "secret.txt", "changed")},
		{"delete child", s.wsDelete, httptest.NewRequest("DELETE", "/api/workspace/delete?path=escape/secret.txt", nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := workspaceRequest(s, token, tc.handler, tc.req)
			if w.Code < 400 {
				t.Fatalf("outside access succeeded: status=%d body=%s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "outside-marker") {
				t.Fatal("outside file content leaked")
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "outside-marker" {
				t.Fatalf("outside file changed: data=%q err=%v", data, err)
			}
		})
	}
	for _, name := range []string{"new.txt", "new-directory"} {
		if _, err := os.Stat(filepath.Join(outside, name)); !os.IsNotExist(err) {
			t.Fatalf("created outside path %q: %v", name, err)
		}
	}
	// Removing the junction itself is safe; it must never remove its target tree.
	w := workspaceRequest(s, token, s.wsDelete, httptest.NewRequest("DELETE", "/api/workspace/delete?path=escape", nil))
	if w.Code != 200 {
		t.Fatalf("remove link: %d %s", w.Code, w.Body.String())
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "outside-marker" {
		t.Fatalf("removing link affected its target: %q %v", data, err)
	}
}

func TestWorkspaceUploadRejectsOutsideFileLink(t *testing.T) {
	s, token, outside := workspaceFixture(t)
	marker := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(marker, []byte("outside-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(marker, filepath.Join(s.m.dir, "secret.txt")); err != nil {
		t.Skipf("file symlinks unavailable (directory junction covered separately): %v", err)
	}
	w := workspaceRequest(s, token, s.wsUpload, workspaceUploadRequest(t, "", "secret.txt", "changed"))
	if w.Code < 400 {
		t.Fatalf("upload followed outside file link: %d %s", w.Code, w.Body.String())
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "outside-marker" {
		t.Fatalf("outside file changed: %q %v", data, err)
	}
}

func TestWorkspaceNormalOperations(t *testing.T) {
	s, token, _ := workspaceFixture(t)
	write := httptest.NewRequest("POST", "/api/workspace/write", strings.NewReader(`{"path":"nested/报告.txt","content":"workspace content"}`))
	if w := workspaceRequest(s, token, s.wsWrite, write); w.Code != 200 {
		t.Fatalf("write: %d %s", w.Code, w.Body.String())
	}
	read := workspaceRequest(s, token, s.wsRead, httptest.NewRequest("GET", "/api/workspace/read?path=nested/报告.txt", nil))
	var data struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if read.Code != 200 || data.Path != "nested/报告.txt" || data.Content != "workspace content" {
		t.Fatalf("read: %d %s", read.Code, read.Body.String())
	}
	list := workspaceRequest(s, token, s.wsList, httptest.NewRequest("GET", "/api/workspace/list", nil))
	var listing struct {
		Path    string    `json:"path"`
		Entries []wsEntry `json:"entries"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if list.Code != 200 || listing.Path != "" || len(listing.Entries) != 1 || !listing.Entries[0].Dir || listing.Entries[0].Path != "nested" {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	downloadReq := httptest.NewRequest("GET", "/api/workspace/download?path=nested/报告.txt", nil)
	downloadReq.Header.Set("Range", "bytes=0-8")
	download := workspaceRequest(s, token, s.wsDownload, downloadReq)
	if download.Code != 206 || download.Body.String() != "workspace" || !strings.Contains(download.Header().Get("Content-Disposition"), "filename*=UTF-8''") {
		t.Fatalf("download: %d %s", download.Code, download.Body.String())
	}
	if w := workspaceRequest(s, token, s.wsUpload, workspaceUploadRequest(t, "nested", "uploaded.txt", "uploaded")); w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	if content, err := os.ReadFile(filepath.Join(s.m.dir, "nested", "uploaded.txt")); err != nil || string(content) != "uploaded" {
		t.Fatalf("uploaded content: %q %v", content, err)
	}
	if w := workspaceRequest(s, token, s.wsMkdir, httptest.NewRequest("POST", "/api/workspace/mkdir", strings.NewReader(`{"path":"new/subdir"}`))); w.Code != 200 {
		t.Fatalf("mkdir: %d %s", w.Code, w.Body.String())
	}
	if w := workspaceRequest(s, token, s.wsDelete, httptest.NewRequest("DELETE", "/api/workspace/delete?path=nested", nil)); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.m.dir, "nested")); !os.IsNotExist(err) {
		t.Fatalf("nested directory was not removed: %v", err)
	}
}

func TestWorkspaceTraversalAndRootProtection(t *testing.T) {
	s, token, _ := workspaceFixture(t)
	for _, path := range []string{"../outside/secret.txt", "nested/../../outside", "//outside/secret.txt"} {
		req := httptest.NewRequest("GET", "/api/workspace/read?path="+path, nil)
		if w := workspaceRequest(s, token, s.wsRead, req); w.Code != 400 {
			t.Errorf("traversal %q: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"", ".", "/"} {
		w := workspaceRequest(s, token, s.wsDelete, httptest.NewRequest("DELETE", "/api/workspace/delete?path="+path, nil))
		if w.Code != 400 {
			t.Errorf("delete workspace root %q: %d %s", path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s.requireAuth(http.HandlerFunc(s.wsRead)).ServeHTTP(w, httptest.NewRequest("GET", "/api/workspace/read?path=x", nil))
	if w.Code != 401 {
		t.Fatalf("unauthenticated request: %d", w.Code)
	}
}

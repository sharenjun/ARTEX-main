package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Workspace file manager — browse / view / edit / download / upload / delete the
// shared work dir (s.m.dir), where all agents write their artifacts. Every path is
// confined to an os.Root, including symlink/junction resolution during I/O. All routes sit
// behind requireAuth (see Handler()).

const (
	maxWorkspaceRead   = 2 << 20   // 2 MiB: files bigger than this aren't inlined for view/edit (download instead)
	maxWorkspaceUpload = 512 << 20 // 512 MiB per upload request
)

// wsResolve normalizes a workspace-relative path. Lexical checks reject traversal;
// os.Root enforces the actual filesystem boundary at every operation.
func (s *Server) wsResolve(rel string) (string, bool) {
	rel = strings.TrimPrefix(strings.TrimSpace(rel), "/")
	clean := filepath.Clean(filepath.FromSlash(rel))
	if !filepath.IsLocal(clean) {
		return "", false
	}
	return clean, true
}

// wsRel renders a normalized path for the frontend (forward slashes, empty root).
func (s *Server) wsRel(rel string) string {
	if rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// wsRoot pins the trusted workspace directory for the lifetime of a request.
// Checking EvalSymlinks and then using absolute paths would allow link-swap races.
func (s *Server) wsRoot(w http.ResponseWriter) (*os.Root, bool) {
	root, err := os.OpenRoot(s.m.dir)
	if err != nil {
		writeErr(w, 500, "无法打开工作区："+err.Error())
		return nil, false
	}
	return root, true
}

type wsEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix millis
}

// GET /api/workspace/list?path=<rel>
func (s *Server) wsList(w http.ResponseWriter, r *http.Request) {
	relPath, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "非法路径")
		return
	}
	root, opened := s.wsRoot(w)
	if !opened {
		return
	}
	defer root.Close()
	dir, err := root.Open(relPath)
	if err != nil {
		writeErr(w, 404, "路径不存在")
		return
	}
	defer dir.Close()
	fi, err := dir.Stat()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !fi.IsDir() {
		writeErr(w, 400, "不是目录")
		return
	}
	ents, err := dir.ReadDir(-1)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]wsEntry, 0, len(ents))
	for _, e := range ents {
		info, err := root.Stat(filepath.Join(relPath, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, wsEntry{
			Name:  e.Name(),
			Path:  s.wsRel(filepath.Join(relPath, e.Name())),
			Dir:   info.IsDir(),
			Size:  info.Size(),
			MTime: info.ModTime().UnixMilli(),
		})
	}
	// 目录在前，各自按名称排序。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, 200, map[string]any{"path": s.wsRel(relPath), "entries": out})
}

// GET /api/workspace/read?path=<rel> — inline text for view/edit. Binary or oversize
// files return {binary:true}/{too_large:true} with no content (use download instead).
func (s *Server) wsRead(w http.ResponseWriter, r *http.Request) {
	relPath, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "非法路径")
		return
	}
	root, opened := s.wsRoot(w)
	if !opened {
		return
	}
	defer root.Close()
	file, err := root.Open(relPath)
	if err != nil {
		writeErr(w, 404, "文件不存在")
		return
	}
	defer file.Close()
	fi, err := file.Stat()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if fi.IsDir() {
		writeErr(w, 400, "是目录，不能作为文件读取")
		return
	}
	if fi.Size() > maxWorkspaceRead {
		writeJSON(w, 200, map[string]any{"path": s.wsRel(relPath), "size": fi.Size(), "too_large": true, "binary": true})
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, maxWorkspaceRead+1))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if len(data) > maxWorkspaceRead {
		writeJSON(w, 200, map[string]any{"path": s.wsRel(relPath), "size": len(data), "too_large": true, "binary": true})
		return
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		writeJSON(w, 200, map[string]any{"path": s.wsRel(relPath), "size": fi.Size(), "binary": true})
		return
	}
	writeJSON(w, 200, map[string]any{"path": s.wsRel(relPath), "size": fi.Size(), "binary": false, "content": string(data)})
}

// POST /api/workspace/write  {path, content} — create/overwrite a text file.
func (s *Server) wsWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	relPath, ok := s.wsResolve(req.Path)
	if !ok || relPath == "." {
		writeErr(w, 400, "非法路径")
		return
	}
	root, opened := s.wsRoot(w)
	if !opened {
		return
	}
	defer root.Close()
	if fi, err := root.Stat(relPath); err == nil && fi.IsDir() {
		writeErr(w, 400, "目标是目录")
		return
	}
	if err := root.MkdirAll(filepath.Dir(relPath), 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := root.WriteFile(relPath, []byte(req.Content), 0o644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": s.wsRel(relPath)})
}

// POST /api/workspace/mkdir  {path}
func (s *Server) wsMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	relPath, ok := s.wsResolve(req.Path)
	if !ok || relPath == "." {
		writeErr(w, 400, "非法路径")
		return
	}
	root, opened := s.wsRoot(w)
	if !opened {
		return
	}
	defer root.Close()
	if err := root.MkdirAll(relPath, 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": s.wsRel(relPath)})
}

// DELETE /api/workspace/delete?path=<rel> — removes a file or a directory tree
// (confined to the work dir; the root itself can't be deleted).
func (s *Server) wsDelete(w http.ResponseWriter, r *http.Request) {
	relPath, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "非法路径")
		return
	}
	if relPath == "." {
		writeErr(w, 400, "不能删除工作区根目录")
		return
	}
	root, opened := s.wsRoot(w)
	if !opened {
		return
	}
	defer root.Close()
	if _, err := root.Lstat(relPath); err != nil {
		writeErr(w, 404, "路径不存在")
		return
	}
	if err := root.RemoveAll(relPath); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// GET /api/workspace/download?path=<rel> — stream a file as an attachment.
func (s *Server) wsDownload(w http.ResponseWriter, r *http.Request) {
	relPath, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "非法路径")
		return
	}
	root, opened := s.wsRoot(w)
	if !opened {
		return
	}
	defer root.Close()
	file, err := root.Open(relPath)
	if err != nil {
		writeErr(w, 404, "文件不存在")
		return
	}
	defer file.Close()
	fi, err := file.Stat()
	if err != nil || fi.IsDir() {
		writeErr(w, 404, "文件不存在")
		return
	}
	name := filepath.Base(relPath)
	// RFC 5987 filename* keeps non-ASCII names intact; plain filename is the fallback.
	w.Header().Set("Content-Disposition", "attachment; filename=\""+sanitizeFilename(name)+"\"; filename*=UTF-8''"+url.PathEscape(name))
	http.ServeContent(w, r, name, fi.ModTime(), file)
}

// POST /api/workspace/upload?path=<dir> — multipart form field "file" (one or more).
func (s *Server) wsUpload(w http.ResponseWriter, r *http.Request) {
	dirPath, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "非法路径")
		return
	}
	root, opened := s.wsRoot(w)
	if !opened {
		return
	}
	defer root.Close()
	if fi, err := root.Stat(dirPath); err != nil || !fi.IsDir() {
		writeErr(w, 400, "目标目录不存在")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkspaceUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "解析上传失败或超出大小限制："+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErr(w, 400, "缺少上传文件(表单字段 file)")
		return
	}
	saved := 0
	for _, hdr := range files {
		name := filepath.Base(hdr.Filename) // strip any path component
		if name == "" || name == "." || name == ".." {
			continue
		}
		destPath, okd := s.wsResolve(filepath.Join(s.wsRel(dirPath), name))
		if !okd {
			continue
		}
		if err := saveWorkspaceUpload(root, hdr, destPath); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		saved++
	}
	writeJSON(w, 200, map[string]any{"uploaded": saved})
}

func saveWorkspaceUpload(root *os.Root, hdr *multipart.FileHeader, dest string) error {
	src, err := hdr.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := root.Create(dest)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, src)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func saveUpload(hdr *multipart.FileHeader, dest string) error {
	src, err := hdr.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, src)
	return err
}

// sanitizeFilename strips characters unsafe for a Content-Disposition filename token.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\"", "")
	name = strings.ReplaceAll(name, "\\", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\r", "")
	return name
}

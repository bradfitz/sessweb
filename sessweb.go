// Command sessweb serves an HTML view of Claude Code sessions.
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yuin/goldmark"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"tailscale.com/client/local"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

var (
	tsnetSrv *tsnet.Server
	ownerID  tailcfg.UserID // owner of the system tailscaled node, set at startup
)

func main() {
	ownerName, uid := getOwnerInfo()
	ownerID = uid

	hostname := os.Getenv("TS_HOSTNAME")
	if hostname == "" {
		hostname = "cc-" + ownerName
	}

	tsnetSrv = &tsnet.Server{
		Hostname: hostname,
	}
	defer tsnetSrv.Close()

	ln, err := tsnetSrv.Listen("tcp", ":80")
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("listening on http://%s/", tsnetSrv.Hostname)
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/session/", handleSession)
	log.Fatal(http.Serve(ln, mux))
}

// getOwnerInfo queries the system tailscaled for the owner's local name
// (e.g. "bradfitz") and UserID. Falls back to $USER and 0 on failure.
func getOwnerInfo() (string, tailcfg.UserID) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := (&local.Client{}).Status(ctx)
	if err != nil {
		log.Printf("warning: can't get tailscaled status: %v; falling back to $USER", err)
		return os.Getenv("USER"), 0
	}
	if st.Self == nil {
		return os.Getenv("USER"), 0
	}
	uid := st.Self.UserID
	if u, ok := st.User[uid]; ok {
		if localPart, _, ok := strings.Cut(u.LoginName, "@"); ok {
			return localPart, uid
		}
	}
	return os.Getenv("USER"), uid
}

// claudeDir returns the path to ~/.claude.
func claudeDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// cacheDir returns ~/.cache/sessweb, creating it if needed.
func cacheDir() string {
	dir, _ := os.UserCacheDir()
	d := filepath.Join(dir, "sessweb")
	os.MkdirAll(d, 0o755)
	return d
}

// sessionInfo holds metadata about a session for the index page.
type sessionInfo struct {
	ProjectDir     string    `json:"projectDir"`
	ProjectPath    string    `json:"projectPath"`
	SessionID      string    `json:"sessionId"`
	Slug           string    `json:"slug,omitempty"`
	CustomTitle    string    `json:"customTitle,omitempty"`
	FirstMsg       string    `json:"firstMsg,omitempty"`
	LastActive     time.Time `json:"lastActive"`
	LineCount      int       `json:"lineCount"`
	FileSize       int64     `json:"fileSize"`       // for cache invalidation
	HasPlanContent bool      `json:"hasPlan,omitempty"` // session was launched with a plan
}

// relatedSession is a linked session shown in the UI.
type relatedSession struct {
	SessionID string
	Title     string
	Relation  string // "plan" or "implementation"
}

// Title returns the display title for the session.
func (si sessionInfo) Title() string {
	if si.CustomTitle != "" {
		return si.CustomTitle
	}
	return si.Slug
}

// DisplayName returns what to show as the bold name on the index page.
func (si sessionInfo) DisplayName() string {
	title := si.Title()
	dir := si.ProjectPath
	if title != "" {
		return title + " (" + dir + ")"
	}
	return dir
}

// metaCache caches session metadata both in-memory and on disk.
var metaCache struct {
	sync.Mutex
	mem  map[string]sessionInfo // key: file path
	disk map[string]sessionInfo // loaded from disk cache at startup
}

func init() {
	metaCache.mem = make(map[string]sessionInfo)
}

// cacheFilePath returns the disk cache file path.
func cacheFilePath() string {
	return filepath.Join(cacheDir(), "meta.json")
}

// loadDiskCache reads the disk cache into metaCache.disk.
func loadDiskCache() {
	metaCache.Lock()
	defer metaCache.Unlock()
	if metaCache.disk != nil {
		return // already loaded
	}
	metaCache.disk = make(map[string]sessionInfo)
	data, err := os.ReadFile(cacheFilePath())
	if err != nil {
		return
	}
	var entries []sessionInfo
	if json.Unmarshal(data, &entries) != nil {
		return
	}
	for _, si := range entries {
		key := filepath.Join(claudeDir(), "projects", si.ProjectDir, si.SessionID+".jsonl")
		metaCache.disk[key] = si
	}
}

// saveDiskCache writes the in-memory cache to disk.
func saveDiskCache() {
	metaCache.Lock()
	var entries []sessionInfo
	for _, si := range metaCache.mem {
		entries = append(entries, si)
	}
	metaCache.Unlock()

	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	os.WriteFile(cacheFilePath(), data, 0o644)
}

// getCachedMeta returns cached metadata if the file hasn't changed.
func getCachedMeta(path string, fi os.FileInfo) (sessionInfo, bool) {
	metaCache.Lock()
	defer metaCache.Unlock()

	// Check in-memory first
	if si, ok := metaCache.mem[path]; ok {
		if si.FileSize == fi.Size() && si.LastActive.Equal(fi.ModTime()) {
			return si, true
		}
	}
	// Check disk cache
	if si, ok := metaCache.disk[path]; ok {
		if si.FileSize == fi.Size() && si.LastActive.Equal(fi.ModTime()) {
			metaCache.mem[path] = si // promote to memory
			return si, true
		}
	}
	return sessionInfo{}, false
}

// putCachedMeta stores metadata in the in-memory cache.
func putCachedMeta(path string, si sessionInfo) {
	metaCache.Lock()
	metaCache.mem[path] = si
	metaCache.Unlock()
}

// discoverSessions finds all session JSONL files across all projects.
func discoverSessions() ([]sessionInfo, error) {
	loadDiskCache()

	projectsDir := filepath.Join(claudeDir(), "projects")
	var sessions []sessionInfo
	dirty := false

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, err
	}
	for _, projEntry := range entries {
		if !projEntry.IsDir() {
			continue
		}
		projName := projEntry.Name()
		projPath := filepath.Join(projectsDir, projName)
		files, err := os.ReadDir(projPath)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			sessID := strings.TrimSuffix(f.Name(), ".jsonl")
			filePath := filepath.Join(projPath, f.Name())

			fi, err := os.Stat(filePath)
			if err != nil {
				continue
			}

			if si, ok := getCachedMeta(filePath, fi); ok {
				sessions = append(sessions, si)
				continue
			}

			// Cache miss — parse the file
			si := sessionInfo{
				ProjectDir: projName,
				SessionID:  sessID,
				LastActive: fi.ModTime(),
				FileSize:   fi.Size(),
			}
			fillSessionMeta(&si, filePath)
			if si.ProjectPath == "" {
				si.ProjectPath = projName
			}
			putCachedMeta(filePath, si)
			dirty = true
			sessions = append(sessions, si)
		}
	}

	if dirty {
		go saveDiskCache()
	}

	slices.SortFunc(sessions, func(a, b sessionInfo) int {
		return b.LastActive.Compare(a.LastActive)
	})

	// Build slug index for resolving related sessions.
	slugIndex.Lock()
	slugIndex.m = make(map[string][]sessionInfo)
	for _, si := range sessions {
		if si.Slug != "" {
			slugIndex.m[si.Slug] = append(slugIndex.m[si.Slug], si)
		}
	}
	slugIndex.Unlock()

	return sessions, nil
}

// slugIndex maps slug -> sessions sharing that slug.
var slugIndex struct {
	sync.Mutex
	m map[string][]sessionInfo
}

// findRelated returns related sessions for the given session, using the slug.
func findRelated(si sessionInfo) []relatedSession {
	if si.Slug == "" {
		return nil
	}
	slugIndex.Lock()
	siblings := slugIndex.m[si.Slug]
	slugIndex.Unlock()

	var related []relatedSession
	for _, sib := range siblings {
		if sib.SessionID == si.SessionID {
			continue
		}
		rel := relatedSession{
			SessionID: sib.SessionID,
			Title:     cmp.Or(sib.Title(), sib.ProjectPath),
		}
		if sib.HasPlanContent {
			rel.Relation = "implementation"
		} else {
			rel.Relation = "plan"
		}
		related = append(related, rel)
	}
	return related
}

// metaRecord is a lightweight struct for scanning session metadata without
// fully parsing large message content fields.
type metaRecord struct {
	Type        string          `json:"type"`
	CWD         string          `json:"cwd"`
	Slug        string          `json:"slug"`
	CustomTitle string          `json:"customTitle"`
	PlanContent string          `json:"planContent"`
	Message     json.RawMessage `json:"message"`
}

// fillSessionMeta reads a session JSONL to fill in metadata fields.
// LastActive and FileSize must already be set by the caller.
func fillSessionMeta(si *sessionInfo, path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			si.LineCount++
			var rec metaRecord
			if json.Unmarshal(line, &rec) == nil {
				if si.ProjectPath == "" && rec.CWD != "" {
					si.ProjectPath = rec.CWD
				}
				if si.Slug == "" && rec.Slug != "" {
					si.Slug = rec.Slug
				}
				if rec.CustomTitle != "" {
					si.CustomTitle = rec.CustomTitle
				}
				if !si.HasPlanContent && rec.PlanContent != "" {
					si.HasPlanContent = true
				}
				if si.FirstMsg == "" && rec.Type == "user" {
					var msg message
					if json.Unmarshal(rec.Message, &msg) == nil && msg.Role == "user" {
						si.FirstMsg = extractUserText(msg.Content)
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
}

// findSessionFile locates the JSONL file for a given session ID by scanning all projects.
func findSessionFile(sessionID string) (string, bool) {
	if strings.Contains(sessionID, "..") || strings.Contains(sessionID, "/") {
		return "", false
	}
	projectsDir := filepath.Join(claudeDir(), "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return "", false
	}
	for _, projEntry := range entries {
		if !projEntry.IsDir() {
			continue
		}
		path := filepath.Join(projectsDir, projEntry.Name(), sessionID+".jsonl")
		if _, err := os.Stat(path); err == nil {
			return path, true
		}
	}
	return "", false
}

// record is the top-level JSON structure for each line in a session JSONL.
type record struct {
	Type        string    `json:"type"`
	ParentUUID  *string   `json:"parentUuid"`
	UUID        string    `json:"uuid"`
	Timestamp   time.Time `json:"timestamp"`
	SessionID   string    `json:"sessionId"`
	Message     message   `json:"message"`
	CWD         string    `json:"cwd"`
	Slug        string    `json:"slug"`
	CustomTitle string    `json:"customTitle"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`       // tool_use: tool name
	ID       string          `json:"id"`          // tool_use: id
	Input    json.RawMessage `json:"input"`       // tool_use: input
	ToolID   string          `json:"tool_use_id"` // tool_result
	Content  any             `json:"content"`     // tool_result content (string or structured)
}

// extractUserText gets the text from user message content.
func extractUserText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return truncate(s, 200)
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" {
				return truncate(b.Text, 200)
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// loadSession reads all records from a session file.
func loadSession(path string) ([]record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var records []record
	br := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var rec record
			if json.Unmarshal(line, &rec) == nil {
				records = append(records, rec)
			}
		}
		if err != nil {
			break
		}
	}
	return records, nil
}

// isOwner reports whether the request comes from the owner of the system
// tailscaled node (identified at startup).
func isOwner(r *http.Request) bool {
	if ownerID == 0 {
		return false
	}
	lc, err := tsnetSrv.LocalClient()
	if err != nil {
		return false
	}
	whois, err := lc.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		return false
	}
	return whois.UserProfile.ID == ownerID
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !isOwner(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html><html><body><p>This is the Claude Code session sharing server from <b>%s</b>. They can share particular session links with you, but you can't view their index.</p></body></html>`,
			html.EscapeString(os.Getenv("USER")))
		return
	}
	sessions, err := discoverSessions()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	indexTmpl.Execute(w, sessions)
}

func handleSession(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/session/")
	if sessionID == "" || strings.Contains(sessionID, "/") {
		http.NotFound(w, r)
		return
	}

	// Ensure slug index is populated (needed for related sessions).
	discoverSessions()

	filePath, ok := findSessionFile(sessionID)
	if !ok {
		http.NotFound(w, r)
		return
	}

	records, err := loadSession(filePath)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	rendered := renderRecords(records)

	var si sessionInfo
	si.SessionID = sessionID
	for _, rec := range records {
		if si.ProjectPath == "" && rec.CWD != "" {
			si.ProjectPath = rec.CWD
		}
		if si.Slug == "" && rec.Slug != "" {
			si.Slug = rec.Slug
		}
		if rec.CustomTitle != "" {
			si.CustomTitle = rec.CustomTitle
		}
	}

	// Get HasPlanContent from the cached metadata.
	if fi, err := os.Stat(filePath); err == nil {
		if cached, ok := getCachedMeta(filePath, fi); ok {
			si.HasPlanContent = cached.HasPlanContent
		}
	}

	title := si.CustomTitle
	if title == "" {
		title = si.Slug
	}

	// Find start time from first record with a timestamp
	var startTime string
	for _, rec := range records {
		if !rec.Timestamp.IsZero() {
			startTime = rec.Timestamp.Format(time.RFC3339)
			break
		}
	}

	data := struct {
		Title       string
		ProjectPath string
		SessionID   string
		StartTime   string
		Related     []relatedSession
		Messages    []renderedMsg
	}{
		Title:       title,
		ProjectPath: si.ProjectPath,
		SessionID:   sessionID,
		StartTime:   startTime,
		Related:     findRelated(si),
		Messages:    rendered,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	sessionTmpl.Execute(w, data)
}

type renderedMsg struct {
	Role     string // "user", "assistant"
	Content  template.HTML
	Time     string // short display time "15:04:05"
	FullTime string // RFC3339
	ID       string // fragment anchor id "m0", "m1", ...
}

func renderRecords(records []record) []renderedMsg {
	var msgs []renderedMsg
	for _, rec := range records {
		switch rec.Type {
		case "user":
			if rec.Message.Role != "user" {
				continue
			}
			h := renderUserContent(rec.Message.Content)
			if h == "" {
				continue
			}
			msgs = append(msgs, renderedMsg{
				Role:     "user",
				Content:  template.HTML(h),
				Time:     rec.Timestamp.Format("15:04:05"),
				FullTime: rec.Timestamp.Format(time.RFC3339),
			})
		case "assistant":
			h := renderContent(rec.Message.Content)
			if h == "" {
				continue
			}
			msgs = append(msgs, renderedMsg{
				Role:     "assistant",
				Content:  template.HTML(h),
				Time:     rec.Timestamp.Format("15:04:05"),
				FullTime: rec.Timestamp.Format(time.RFC3339),
			})
		}
	}
	// Merge consecutive same-role messages
	var merged []renderedMsg
	for _, m := range msgs {
		if len(merged) > 0 && merged[len(merged)-1].Role == m.Role {
			merged[len(merged)-1].Content += m.Content
		} else {
			merged = append(merged, m)
		}
	}
	// Assign stable IDs
	for i := range merged {
		merged[i].ID = fmt.Sprintf("m%d", i)
	}
	return merged
}

// renderUserContent renders only the human-typed text from a user message,
// skipping tool_result blocks which are internal plumbing.
func renderUserContent(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return ""
		}
		return renderMarkdown(s)
	}

	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}

	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			sb.WriteString(`<div class="text-block">`)
			sb.WriteString(renderMarkdown(b.Text))
			sb.WriteString("</div>\n")
		}
	}
	return sb.String()
}

func renderContent(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return ""
		}
		return renderMarkdown(s)
	}

	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}

	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			sb.WriteString(`<div class="text-block">`)
			sb.WriteString(renderMarkdown(b.Text))
			sb.WriteString("</div>\n")
		case "thinking":
			if strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			sb.WriteString(`<details class="thinking"><summary>Thinking...</summary><div class="thinking-content">`)
			sb.WriteString(renderMarkdown(b.Thinking))
			sb.WriteString("</div></details>\n")
		case "tool_use":
			summary := toolSummary(b.Name, b.Input)
			inputStr := formatJSON(b.Input)
			fmt.Fprintf(&sb, `<details class="tool-use"><summary>%s</summary><pre>%s</pre></details>`+"\n",
				summary, html.EscapeString(inputStr))
		case "tool_result":
			contentStr := formatToolResult(b.Content)
			if contentStr == "" {
				continue
			}
			fmt.Fprintf(&sb, `<details class="tool-result"><summary>Result</summary><pre>%s</pre></details>`+"\n",
				html.EscapeString(contentStr))
		}
	}
	return sb.String()
}

func formatJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func formatToolResult(v any) string {
	switch v := v.(type) {
	case string:
		return truncate(v, 2000)
	case nil:
		return ""
	default:
		b, _ := json.MarshalIndent(v, "", "  ")
		s := string(b)
		return truncate(s, 2000)
	}
}

// toolSummary returns an HTML summary string for a tool call's <summary> line.
func toolSummary(name string, input json.RawMessage) string {
	esc := html.EscapeString
	code := func(s string) string { return "<code>" + esc(s) + "</code>" }

	var m map[string]json.RawMessage
	json.Unmarshal(input, &m)

	str := func(key string) string {
		raw, ok := m[key]
		if !ok {
			return ""
		}
		var s string
		json.Unmarshal(raw, &s)
		return s
	}
	num := func(key string) (int, bool) {
		raw, ok := m[key]
		if !ok {
			return 0, false
		}
		var n int
		if json.Unmarshal(raw, &n) != nil {
			return 0, false
		}
		return n, true
	}

	switch name {
	case "Read":
		fp := str("file_path")
		if fp == "" {
			break
		}
		s := "Tool: Read " + code(fp)
		offset, haveOff := num("offset")
		limit, haveLim := num("limit")
		if haveOff && haveLim {
			s += fmt.Sprintf(" <code>@%d+%d</code>", offset, limit)
		} else if haveOff {
			s += fmt.Sprintf(" <code>@%d</code>", offset)
		} else if haveLim {
			s += fmt.Sprintf(" <code>+%d</code>", limit)
		}
		return s
	case "Write":
		if fp := str("file_path"); fp != "" {
			return "Tool: Write " + code(fp)
		}
	case "Edit":
		if fp := str("file_path"); fp != "" {
			return "Tool: Edit " + code(fp)
		}
	case "Glob":
		s := "Tool: Glob " + code(str("pattern"))
		if p := str("path"); p != "" {
			s += " in " + code(p)
		}
		return s
	case "Grep":
		s := "Tool: Grep " + code(str("pattern"))
		if p := str("path"); p != "" {
			s += " in " + code(p)
		}
		return s
	case "Bash":
		if cmd := str("command"); cmd != "" {
			if desc := str("description"); desc != "" {
				return "Tool: Bash " + esc(desc)
			}
			cmd = truncate(cmd, 80)
			return "Tool: Bash " + code(cmd)
		}
	case "WebFetch":
		if u := str("url"); u != "" {
			return "Tool: WebFetch " + code(truncate(u, 80))
		}
	case "WebSearch":
		if q := str("query"); q != "" {
			return "Tool: WebSearch " + code(q)
		}
	case "Task", "Agent":
		if desc := str("description"); desc != "" {
			return "Tool: " + esc(name) + " " + esc(desc)
		}
	case "TaskCreate":
		if subj := str("subject"); subj != "" {
			return "Tool: TaskCreate " + esc(subj)
		}
	case "TaskUpdate":
		if status := str("status"); status != "" {
			return "Tool: TaskUpdate " + esc(status)
		}
	}
	return "Tool: " + esc(name)
}

var md = goldmark.New(
	goldmark.WithRendererOptions(
		gmhtml.WithUnsafe(), // allow raw HTML passthrough
	),
)

func renderMarkdown(s string) string {
	var buf bytes.Buffer
	if err := md.Convert([]byte(s), &buf); err != nil {
		return "<p>" + html.EscapeString(s) + "</p>\n"
	}
	return buf.String()
}

var indexTmpl = template.Must(template.New("index").Funcs(template.FuncMap{
	"findRelated": findRelated,
}).Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Claude Sessions</title>
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f5f5f5; color: #333; padding: 2rem; max-width: 960px; margin: 0 auto; }
h1 { margin-bottom: 1.5rem; }
.session-list { list-style: none; }
.session-item { background: #fff; border: 1px solid #ddd; border-radius: 8px; padding: 1rem 1.25rem; margin-bottom: 0.75rem; }
.session-item:hover { border-color: #999; }
.session-item a { text-decoration: none; color: inherit; display: block; }
.session-meta { font-size: 0.85rem; color: #888; margin-bottom: 0.25rem; }
.session-preview { font-size: 0.95rem; color: #555; }
.session-name { font-weight: 600; color: #333; }
.related-link { font-size: 0.8rem; color: #0066cc; }
</style>
</head>
<body>
<h1>Claude Sessions</h1>
<ul class="session-list">
{{range .}}
<li class="session-item">
  <a href="/session/{{.SessionID}}">
    <div class="session-meta">
      <span class="session-name">{{.DisplayName}}</span>
      &mdash; {{.LastActive.Format "2006-01-02 15:04"}}
      &mdash; {{.LineCount}} lines
    </div>
    <div class="session-preview">{{.FirstMsg}}</div>
  </a>
  {{range findRelated .}}<a class="related-link" href="/session/{{.SessionID}}">{{.Relation}}: {{.Title}}</a> {{end}}
</li>
{{end}}
</ul>
</body>
</html>
`))

var sessionTmpl = template.Must(template.New("session").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>{{if .Title}}{{.Title}}{{else}}Session{{end}} - {{.ProjectPath}}</title>
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f5f5f5; color: #333; padding: 2rem; max-width: 860px; margin: 0 auto; }
h1 { margin-bottom: 0.25rem; font-size: 1.25rem; }
h1 a { color: inherit; text-decoration: none; }
h1 a:hover { text-decoration: underline; }
.session-meta { font-size: 0.85rem; color: #888; margin-bottom: 1.5rem; }
.message { border-radius: 8px; padding: 1rem 1.25rem; margin-bottom: 1rem; overflow-x: auto; }
.message.user { background: #d1e7ff; border: 1px solid #b0d0f0; }
.message.assistant { background: #fff; border: 1px solid #ddd; }
.message .role { font-size: 0.8rem; font-weight: 600; text-transform: uppercase; color: #666; margin-bottom: 0.5rem; }
.message .time { font-size: 0.75rem; color: #999; float: right; text-decoration: none; }
.message .time:hover { color: #666; }
.message p { margin-bottom: 0.5rem; white-space: pre-wrap; }
.message pre { background: #1e1e1e; color: #d4d4d4; padding: 0.75rem; border-radius: 4px; overflow-x: auto; margin: 0.5rem 0; font-size: 0.85rem; }
.message code { font-family: "SF Mono", Menlo, monospace; }
details { margin: 0.5rem 0; }
details summary { cursor: pointer; font-size: 0.85rem; color: #666; }
details pre { max-height: 400px; overflow-y: auto; }
.thinking { margin-left: 1em; }
.thinking summary { color: #999; font-style: italic; }
.thinking-content { font-size: 0.9rem; color: #555; padding: 0.75rem 1rem; margin: 0.5rem 0; border: 1.5px dashed #ccc; border-radius: 10px; }
.thinking-content p { margin-bottom: 0.5rem; }
.thinking-content pre { background: #1e1e1e; color: #d4d4d4; padding: 0.75rem; border-radius: 4px; overflow-x: auto; margin: 0.5rem 0; font-size: 0.85rem; }
.thinking-content code { font-family: "SF Mono", Menlo, monospace; font-size: 0.85rem; }
.tool-use { margin-left: 1em; }
.tool-use summary { color: #0066cc; }
.tool-result { margin-left: 1em; }
.tool-result summary { color: #339933; }
.text-block { margin-bottom: 0.25rem; }
.text-block p { margin-bottom: 0.5rem; }
.text-block h1, .text-block h2, .text-block h3, .text-block h4 { margin: 0.75rem 0 0.25rem; }
.text-block ul, .text-block ol { margin: 0.25rem 0 0.5rem 1.5rem; }
.text-block code { font-family: "SF Mono", Menlo, monospace; font-size: 0.85rem; background: #e8e8e8; padding: 0.1rem 0.3rem; border-radius: 3px; }
.text-block pre code { background: none; padding: 0; }
.related { font-size: 0.85rem; margin-bottom: 1.5rem; }
.related a { color: #0066cc; margin-right: 1rem; }
</style>
</head>
<body>
<h1><a href="/">&#8592;</a> {{if .Title}}{{.Title}}{{else}}Session{{end}}</h1>
<div class="session-meta">{{.ProjectPath}} &mdash; {{.StartTime}} &mdash; {{.SessionID}}</div>
{{if .Related}}<div class="related">Related: {{range .Related}}<a href="/session/{{.SessionID}}">{{.Relation}}: {{.Title}}</a> {{end}}</div>{{end}}
{{range .Messages}}
<div class="message {{.Role}}" id="{{.ID}}">
  <a class="time" href="#{{.ID}}" title="{{.FullTime}}">{{.Time}}</a>
  <div class="role">{{.Role}}</div>
  {{.Content}}
</div>
{{end}}
{{if .Related}}<div class="related">Related: {{range .Related}}<a href="/session/{{.SessionID}}">{{.Relation}}: {{.Title}}</a> {{end}}</div>{{end}}
</body>
</html>
`))

// jellyfin-mcp is a dependency-free MCP stdio server exposing a read-only slice
// of the Jellyfin API to Odysseus. Configuration via environment:
//
//	JELLYFIN_URL     base URL, e.g. http://host.docker.internal:7219
//	JELLYFIN_API_KEY API key (X-MediaBrowser-Token header)
//
// Protocol: newline-delimited JSON-RPC 2.0 on stdin/stdout (MCP stdio
// transport). Logs go to stderr; stdout carries only protocol frames.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const serverVersion = "1.0.0"
const maxTextChars = 15000

var httpClient = &http.Client{Timeout: 20 * time.Second}

var baseURL = envOr("JELLYFIN_URL", "http://127.0.0.1:7219")
var apiKey = os.Getenv("JELLYFIN_API_KEY")

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimRight(v, "/")
	}
	return fallback
}

// ---------- JSON-RPC plumbing ----------

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

var out = bufio.NewWriter(os.Stdout)

func respond(id json.RawMessage, result any) {
	frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
	out.Write(frame)
	out.WriteByte('\n')
	out.Flush()
}

func respondError(id json.RawMessage, code int, message string) {
	frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "error": map[string]any{"code": code, "message": message}})
	out.Write(frame)
	out.WriteByte('\n')
	out.Flush()
}

func hasID(id json.RawMessage) bool {
	return len(id) != 0 && string(id) != "null"
}

// ---------- Jellyfin HTTP helpers ----------

func apiGet(path string, query map[string]string) (int, []byte, error) {
	u, err := url.Parse(baseURL + path)
	if err != nil {
		return 0, nil, err
	}
	if len(query) > 0 {
		q := u.Query()
		for k, v := range query {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-MediaBrowser-Token", apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// myUserID resolves and caches a default user id so tools work without a UUID
// every call. JELLYFIN_USER selects by name (case-insensitive) or id;
// otherwise the first administrator is used, else the first user. API keys
// are not tied to a user, so /Users/Me is unusable with key auth.
var (
	cachedUserID string
	userIDOnce   sync.Mutex
)

func myUserID() (string, error) {
	userIDOnce.Lock()
	defer userIDOnce.Unlock()
	if cachedUserID != "" {
		return cachedUserID, nil
	}
	want := strings.ToLower(strings.TrimSpace(os.Getenv("JELLYFIN_USER")))
	status, body, err := apiGet("/Users", nil)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("Jellyfin HTTP %d: %s", status, firstChars(string(body), 300))
	}
	var users []struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Policy struct {
			IsAdministrator bool `json:"IsAdministrator"`
		} `json:"Policy"`
	}
	if err := json.Unmarshal(body, &users); err != nil {
		return "", err
	}
	if len(users) == 0 {
		return "", fmt.Errorf("no Jellyfin users found")
	}
	pick := ""
	if want != "" {
		for _, u := range users {
			if strings.ToLower(u.Name) == want || strings.ToLower(u.ID) == want {
				pick = u.ID
				break
			}
		}
		if pick == "" {
			return "", fmt.Errorf("JELLYFIN_USER %q matches no user", want)
		}
	} else {
		for _, u := range users {
			if u.Policy.IsAdministrator {
				pick = u.ID
				break
			}
		}
		if pick == "" {
			pick = users[0].ID
		}
	}
	cachedUserID = pick
	return pick, nil
}

func userID(args map[string]any) (string, map[string]any) {
	if v, _ := args["userId"].(string); v != "" {
		return v, nil
	}
	id, err := myUserID()
	if err != nil {
		return "", errText("user lookup failed: %v", err)
	}
	return id, nil
}

func clip(s string) string {
	if len(s) > maxTextChars {
		return s[:maxTextChars] + fmt.Sprintf("\n...(truncated, %d chars total)", len(s))
	}
	return s
}

func firstChars(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func okText(body []byte) map[string]any {
	return map[string]any{"content": []textContent{{Type: "text", Text: clip(string(body))}}}
}

func errText(format string, args ...any) map[string]any {
	return map[string]any{
		"content": []textContent{{Type: "text", Text: fmt.Sprintf(format, args...)}},
		"isError": true,
	}
}

func requireKey() map[string]any {
	if apiKey == "" {
		return errText("JELLYFIN_API_KEY is not set")
	}
	return nil
}

// projectItems trims a Jellyfin Items result to the fields an agent needs.
func projectItems(body []byte) []byte {
	var envelope struct {
		Items []map[string]any `json:"Items"`
		Total int              `json:"TotalRecordCount"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return body
	}
	keep := []string{"Id", "Name", "Type", "ProductionYear", "Overview", "Genres", "RunTimeTicks", "SeriesName", "SeasonName", "IndexNumber", "Album", "Artists"}
	out := make([]map[string]any, 0, len(envelope.Items))
	for _, it := range envelope.Items {
		row := map[string]any{}
		for _, k := range keep {
			if v, present := it[k]; present {
				if k == "Overview" {
					if s, ok := v.(string); ok {
						row[k] = firstChars(s, 400)
						continue
					}
				}
				row[k] = v
			}
		}
		out = append(out, row)
	}
	compact, err := json.Marshal(map[string]any{"total": envelope.Total, "items": out})
	if err != nil {
		return body
	}
	return compact
}

// ---------- tools ----------

type handler func(args map[string]any) map[string]any

var tools = []toolDef{
	{
		Name:        "search_media",
		Description: "Search the Jellyfin library by title across movies, series, episodes and music. Returns id, name, type, year and a short overview per hit.",
		InputSchema: objSchema(map[string]any{
			"query":  strProp("Title or keyword to search for"),
			"limit":  intProp("Max results 1-50", 1, 50),
			"userId": strProp("Jellyfin user id. Omit to use the key owner's."),
		}, []string{"query"}),
	},
	{
		Name:        "list_libraries",
		Description: "List Jellyfin media libraries (views): id, name, collection type.",
		InputSchema: objSchema(map[string]any{
			"userId": strProp("Jellyfin user id. Omit to use the key owner's."),
		}, nil),
	},
	{
		Name:        "item_details",
		Description: "Full metadata for one Jellyfin item by id (people, studios, streams, paths, overview).",
		InputSchema: objSchema(map[string]any{
			"itemId": strProp("Item UUID"),
			"userId": strProp("Jellyfin user id. Omit to use the key owner's."),
		}, []string{"itemId"}),
	},
	{
		Name:        "latest_media",
		Description: "Recently added items, optionally scoped to one library id. Returns id, name, type, year per item.",
		InputSchema: objSchema(map[string]any{
			"libraryId": strProp("Parent library UUID from list_libraries. Omit for all libraries."),
			"limit":     intProp("Max results 1-50", 1, 50),
			"userId":    strProp("Jellyfin user id. Omit to use the key owner's."),
		}, nil),
	},
}

var handlers = map[string]handler{
	"search_media":   hSearch,
	"list_libraries": hLibraries,
	"item_details":   hItem,
	"latest_media":   hLatest,
}

func strArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func intArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func clampLimit(n int) int {
	if n < 1 {
		return 1
	}
	if n > 50 {
		return 50
	}
	return n
}

func hSearch(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	uid, errMap := userID(args)
	if errMap != nil {
		return errMap
	}
	q := strArg(args, "query")
	if q == "" {
		return errText("query is required")
	}
	status, body, err := apiGet("/Users/"+url.PathEscape(uid)+"/Items", map[string]string{
		"searchTerm":       q,
		"Recursive":        "true",
		"IncludeItemTypes": "Movie,Series,Episode,Audio,MusicAlbum,MusicArtist",
		"Fields":           "Overview,Genres",
		"Limit":            strconv.Itoa(clampLimit(intArg(args, "limit", 20))),
	})
	if err != nil {
		return errText("search failed: %v", err)
	}
	if status >= 400 {
		return errText("Jellyfin HTTP %d: %s", status, clip(string(body)))
	}
	return okText(projectItems(body))
}

func hLibraries(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	uid, errMap := userID(args)
	if errMap != nil {
		return errMap
	}
	status, body, err := apiGet("/Users/"+url.PathEscape(uid)+"/Views", nil)
	if err != nil {
		return errText("library list failed: %v", err)
	}
	if status >= 400 {
		return errText("Jellyfin HTTP %d: %s", status, clip(string(body)))
	}
	return okText(projectItems(body))
}

func hItem(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	uid, errMap := userID(args)
	if errMap != nil {
		return errMap
	}
	id := strArg(args, "itemId")
	if id == "" {
		return errText("itemId is required")
	}
	status, body, err := apiGet("/Users/"+url.PathEscape(uid)+"/Items/"+url.PathEscape(id), nil)
	if err != nil {
		return errText("item lookup failed: %v", err)
	}
	if status >= 400 {
		return errText("Jellyfin HTTP %d: %s", status, clip(string(body)))
	}
	return okText(body)
}

func hLatest(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	uid, errMap := userID(args)
	if errMap != nil {
		return errMap
	}
	q := map[string]string{
		"Limit":            strconv.Itoa(clampLimit(intArg(args, "limit", 10))),
		"Fields":           "Overview,Genres",
		"IncludeItemTypes": "Movie,Series,Episode,Audio,MusicAlbum",
	}
	if lib := strArg(args, "libraryId"); lib != "" {
		q["ParentId"] = lib
	}
	status, body, err := apiGet("/Users/"+url.PathEscape(uid)+"/Items/Latest", q)
	if err != nil {
		return errText("latest lookup failed: %v", err)
	}
	if status >= 400 {
		return errText("Jellyfin HTTP %d: %s", status, clip(string(body)))
	}
	// /Latest returns a bare array, not an Items envelope.
	var arr []map[string]any
	if err := json.Unmarshal(body, &arr); err == nil {
		wrapped, _ := json.Marshal(map[string]any{"Items": arr, "TotalRecordCount": len(arr)})
		return okText(projectItems(wrapped))
	}
	return okText(body)
}

// ---------- schema helpers ----------

func objSchema(props map[string]any, required []string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": min, "maximum": max}
}

// ---------- dispatch ----------

func handle(req request) {
	if !hasID(req.ID) {
		return // notification: nothing to answer
	}
	switch req.Method {
	case "initialize":
		version := "2024-11-05"
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(req.Params, &p); err == nil && p.ProtocolVersion != "" {
			version = p.ProtocolVersion
		}
		respond(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "jellyfin-mcp", "version": serverVersion},
		})
	case "ping":
		respond(req.ID, map[string]any{})
	case "tools/list":
		respond(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			respondError(req.ID, -32602, "invalid params")
			return
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		h, ok := handlers[p.Name]
		if !ok {
			respondError(req.ID, -32602, "unknown tool: "+p.Name)
			return
		}
		respond(req.ID, h(p.Arguments))
	default:
		respondError(req.ID, -32601, "method not found: "+req.Method)
	}
}

func main() {
	logf("jellyfin-mcp %s up (url=%s)", serverVersion, baseURL)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 10<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			logf("dropping undecodable frame: %v", err)
			continue
		}
		handle(req)
	}
	if err := sc.Err(); err != nil {
		logf("stdin error: %v", err)
		os.Exit(1)
	}
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

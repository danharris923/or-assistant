package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

type Config struct {
	APIKey   string `json:"api_key,omitempty"`
	Model    string `json:"model,omitempty"`
	Approval string `json:"approval,omitempty"` // "ask" (default) or "auto"
}

// Item is one entry in the visible transcript.
type Item struct {
	Kind   string          `json:"kind"` // user, assistant, tool, error, meta
	Text   string          `json:"text,omitempty"`
	Name   string          `json:"name,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
	Status string          `json:"status,omitempty"` // tool: pending_approval, running, done, error, denied, cancelled
	Output string          `json:"output,omitempty"`
	Model  string          `json:"model,omitempty"`
	Cost   float64         `json:"cost,omitempty"`
	Tokens int             `json:"tokens,omitempty"`
}

type Event struct {
	Seq       int64   `json:"seq"`
	Type      string  `json:"type"` // item_add, item_update, delta, run, reset
	Index     int     `json:"index"`
	Item      *Item   `json:"item,omitempty"`
	Text      string  `json:"text,omitempty"`
	Running   bool    `json:"running"`
	TotalCost float64 `json:"total_cost"`
}

type App struct {
	mu        sync.Mutex
	cfg       Config
	items     []Item
	msgs      []Message
	seq       int64
	subs      map[chan Event]struct{}
	running   bool
	cancel    context.CancelFunc
	approvals map[int]chan bool
	totalCost float64
	models    modelCache
	client    *http.Client
}

func newApp() (*App, error) {
	a := &App{
		subs:      map[chan Event]struct{}{},
		approvals: map[int]chan bool{},
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 180 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		}},
	}
	b, err := os.ReadFile(a.configPath())
	if err == nil {
		if err := json.Unmarshal(b, &a.cfg); err != nil {
			return nil, fmt.Errorf("reading %s: %w", a.configPath(), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if a.cfg.Approval != "auto" {
		a.cfg.Approval = "ask"
	}
	return a, nil
}

func (a *App) configPath() string { return filepath.Join(configDir(), "config.json") }

// saveConfigLocked writes the config; caller holds a.mu.
func (a *App) saveConfigLocked() error {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	return writeFileAtomic(a.configPath(), b, 0o600)
}

// apiKeyLocked prefers the environment so a key never has to touch disk.
func (a *App) apiKeyLocked() (key, source string) {
	if k := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")); k != "" {
		return k, "environment"
	}
	if a.cfg.APIKey != "" {
		return a.cfg.APIKey, "settings"
	}
	return "", ""
}

func keyHint(k string) string {
	if len(k) < 12 {
		return "set"
	}
	return k[:8] + "…" + k[len(k)-4:]
}

// ---- transcript + event fan-out ----

func (a *App) emitLocked(ev Event) {
	a.seq++
	ev.Seq, ev.Running, ev.TotalCost = a.seq, a.running, a.totalCost
	for ch := range a.subs {
		select {
		case ch <- ev:
		default: // slow client: drop it; the page reconnects and reloads state
			delete(a.subs, ch)
			close(ch)
		}
	}
}

func (a *App) addItem(it Item) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.items = append(a.items, it)
	i := len(a.items) - 1
	cp := a.items[i]
	a.emitLocked(Event{Type: "item_add", Index: i, Item: &cp})
	return i
}

func (a *App) updateItem(i int, f func(*Item)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(&a.items[i])
	cp := a.items[i]
	a.emitLocked(Event{Type: "item_update", Index: i, Item: &cp})
}

func (a *App) appendText(i int, s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.items[i].Text += s
	a.emitLocked(Event{Type: "delta", Index: i, Text: s})
}

// ---- HTTP ----

func (a *App) routes(port int, token string, shutdown func()) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("POST /internal/shutdown", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		if a.cancel != nil {
			a.cancel()
		}
		a.mu.Unlock()
		io.WriteString(w, "bye")
		shutdown()
	})
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("GET /api/events", a.handleEvents)
	mux.HandleFunc("GET /api/models", a.handleModels)
	mux.HandleFunc("GET /api/balance", a.handleBalance)
	mux.HandleFunc("POST /api/settings", a.handleSettings)
	mux.HandleFunc("POST /api/key", a.handleKey)
	mux.HandleFunc("POST /api/chat", a.handleChat)
	mux.HandleFunc("POST /api/approve", a.handleApprove)
	mux.HandleFunc("POST /api/stop", a.handleStop)
	mux.HandleFunc("POST /api/new", a.handleNew)
	return guard(port, token, mux)
}

func constEq(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// guard closes the hole Slax's helper leaves open: only a client that knows the
// per-launch secret (handed to the browser through the launch URL, then kept in
// an HttpOnly cookie) can use the API, and cross-site requests are refused.
func guard(port int, token string, next http.Handler) http.Handler {
	hosts := map[string]bool{fmt.Sprintf("127.0.0.1:%d", port): true, fmt.Sprintf("localhost:%d", port): true}
	cookie := "ora_" + strconv.Itoa(port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if !hosts[r.Host] { // blocks DNS-rebinding attacks
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/" && r.URL.Query().Has("k") {
			if !constEq(r.URL.Query().Get("k"), token) {
				http.Error(w, "invalid or expired link; reopen the assistant from its launcher", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther) // drops the secret from the address bar
			return
		}
		ok := constEq(r.Header.Get("X-Auth-Token"), token)
		if c, err := r.Cookie(cookie); err == nil && constEq(c.Value, token) {
			ok = true
		}
		if !ok {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "This page only opens from the OR Assistant launcher.\n")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return errors.New("expected application/json")
	}
	return json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(v)
}

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	a.mu.Lock()
	defer a.mu.Unlock()
	key, src := a.apiKeyLocked()
	items := a.items
	if items == nil {
		items = []Item{}
	}
	writeJSON(w, 200, map[string]any{
		"seq": a.seq, "items": items, "running": a.running, "total_cost": a.totalCost,
		"model": a.cfg.Model, "approval": a.cfg.Approval,
		"has_key": key != "", "key_hint": map[bool]string{true: keyHint(key), false: ""}[key != ""], "key_source": src,
		"user": currentUser(), "host": host, "home": homeDir(), "root": isRoot(), "version": version,
	})
}

func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ch := make(chan Event, 1024)
	a.mu.Lock()
	a.subs[ch] = struct{}{}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if _, ok := a.subs[ch]; ok {
			delete(a.subs, ch)
		}
		a.mu.Unlock()
	}()
	io.WriteString(w, ": connected\n\n")
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			io.WriteString(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (a *App) handleModels(w http.ResponseWriter, r *http.Request) {
	list, err := a.models.get(r.Context(), a.client)
	if err != nil {
		jsonErr(w, 502, "Could not load models from OpenRouter: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"models": list})
}

func (a *App) handleBalance(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	key, _ := a.apiKeyLocked()
	a.mu.Unlock()
	if key == "" {
		jsonErr(w, 400, "No API key set")
		return
	}
	info, status, err := a.keyInfo(r.Context(), key)
	if err != nil {
		jsonErr(w, status, err.Error())
		return
	}
	writeJSON(w, 200, info)
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    *string `json:"model"`
		Approval *string `json:"approval"`
	}
	if err := decode(r, &req); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if req.Model != nil {
		a.cfg.Model = strings.TrimSpace(*req.Model)
	}
	if req.Approval != nil {
		if *req.Approval != "ask" && *req.Approval != "auto" {
			jsonErr(w, 400, "approval must be ask or auto")
			return
		}
		a.cfg.Approval = *req.Approval
	}
	if err := a.saveConfigLocked(); err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"model": a.cfg.Model, "approval": a.cfg.Approval})
}

func (a *App) handleKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := decode(r, &req); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	key := strings.TrimSpace(req.Key)
	if key != "" {
		if _, status, err := a.keyInfo(r.Context(), key); err != nil {
			jsonErr(w, status, err.Error())
			return
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.APIKey = key
	if err := a.saveConfigLocked(); err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	k, src := a.apiKeyLocked()
	writeJSON(w, 200, map[string]any{"has_key": k != "", "key_hint": keyHint(k), "key_source": src})
}

func (a *App) handleChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := decode(r, &req); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		jsonErr(w, 400, "Message is empty")
		return
	}
	a.mu.Lock()
	key, _ := a.apiKeyLocked()
	switch {
	case a.running:
		a.mu.Unlock()
		jsonErr(w, 409, "Still working on the previous message")
		return
	case key == "":
		a.mu.Unlock()
		jsonErr(w, 400, "Add your OpenRouter API key in Settings first")
		return
	case a.cfg.Model == "":
		a.mu.Unlock()
		jsonErr(w, 400, "Choose a model first")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.running, a.cancel = true, cancel
	model := a.cfg.Model
	a.emitLocked(Event{Type: "run"})
	a.mu.Unlock()
	go a.run(ctx, model, text)
	writeJSON(w, 202, map[string]bool{"ok": true})
}

func (a *App) finishRun() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
	a.running, a.cancel = false, nil
	a.approvals = map[int]chan bool{}
	a.emitLocked(Event{Type: "run"})
}

func (a *App) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Index int  `json:"index"`
		Allow bool `json:"allow"`
	}
	if err := decode(r, &req); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	a.mu.Lock()
	ch, ok := a.approvals[req.Index]
	delete(a.approvals, req.Index)
	a.mu.Unlock()
	if !ok {
		jsonErr(w, 404, "That action is no longer waiting for approval")
		return
	}
	ch <- req.Allow
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) handleStop(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) handleNew(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		jsonErr(w, 409, "Stop the current task first")
		return
	}
	a.items, a.msgs, a.totalCost = nil, nil, 0
	a.emitLocked(Event{Type: "reset"})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

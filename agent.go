package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const maxSteps = 60

func osName() string {
	if f, err := os.Open("/etc/os-release"); err == nil {
		defer f.Close()
		s := bufio.NewScanner(f)
		for s.Scan() {
			if v, ok := strings.CutPrefix(s.Text(), "PRETTY_NAME="); ok {
				return strings.Trim(v, `"`)
			}
		}
	}
	return runtime.GOOS
}

func systemPrompt() string {
	root := "You do NOT have root. If something needs root, give the user the exact command to run themselves."
	if isRoot() {
		root = "You are running as root, so be careful: confirm before destructive changes."
	}
	return fmt.Sprintf(`You are OR Assistant, a helpful assistant running on the user's own computer (%s).
You can act on this computer with tools. Commands run as the user %q (home: %s). %s
Current date: %s. Commands run with /bin/sh.

Guidelines:
- If you can answer directly, just answer. Use tools when the task needs facts from this machine or changes to it.
- Commands must be non-interactive: no editors or pagers, pass -y/--yes flags. Anything still running at the timeout is killed.
- Prefer read_file, list_dir and edit_file over shell equivalents for files. Read a file before editing it.
- The user may deny an action. If they do, don't retry it; ask what they'd prefer.
- Be concise. Format answers with Markdown.`,
		osName(), currentUser(), homeDir(), root, time.Now().Format("Monday, 2 January 2006"))
}

// run handles one user message: model turn, tool calls, repeat until the model answers.
func (a *App) run(ctx context.Context, model, text string) {
	defer a.finishRun()
	a.addItem(Item{Kind: "user", Text: text})
	a.mu.Lock()
	if len(a.msgs) == 0 {
		a.msgs = append(a.msgs, Message{Role: "system", Content: systemPrompt()})
	}
	a.msgs = append(a.msgs, Message{Role: "user", Content: text})
	a.mu.Unlock()

	var total tokenUsage
	for step := 0; ; step++ {
		if step == maxSteps {
			a.addItem(Item{Kind: "error", Text: fmt.Sprintf("Stopped after %d steps. Send another message to let it continue.", maxSteps)})
			break
		}
		msg, u, err := a.complete(ctx, model)
		total.Cost += u.Cost
		total.Tokens += u.Tokens
		if err != nil {
			if msg.Content != "" { // keep partial text so the conversation stays coherent
				a.mu.Lock()
				a.msgs = append(a.msgs, Message{Role: "assistant", Content: msg.Content})
				a.mu.Unlock()
			}
			if ctx.Err() != nil {
				a.addItem(Item{Kind: "error", Text: "Stopped."})
			} else {
				a.addItem(Item{Kind: "error", Text: err.Error()})
			}
			break
		}
		a.mu.Lock()
		a.msgs = append(a.msgs, msg)
		a.mu.Unlock()
		if len(msg.ToolCalls) == 0 {
			break
		}
		// Every tool call must get a result, even when stopped, or the next request is invalid.
		for _, tc := range msg.ToolCalls {
			out := a.runTool(ctx, tc)
			a.mu.Lock()
			a.msgs = append(a.msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: out})
			a.mu.Unlock()
		}
		if ctx.Err() != nil {
			a.addItem(Item{Kind: "error", Text: "Stopped."})
			break
		}
	}
	a.mu.Lock()
	a.totalCost += total.Cost
	a.mu.Unlock()
	a.addItem(Item{Kind: "meta", Model: model, Cost: total.Cost, Tokens: total.Tokens})
}

var needsApproval = map[string]bool{"run_command": true, "write_file": true, "edit_file": true}

func (a *App) runTool(ctx context.Context, tc ToolCall) string {
	args := strings.TrimSpace(tc.Function.Arguments)
	if args == "" {
		args = "{}"
	}
	name := tc.Function.Name
	if !json.Valid([]byte(args)) {
		raw, _ := json.Marshal(map[string]string{"raw": args})
		a.addItem(Item{Kind: "tool", Name: name, Args: raw, Status: "error", Output: "Arguments were not valid JSON."})
		return "Error: your tool arguments were not valid JSON. Try again."
	}
	idx := a.addItem(Item{Kind: "tool", Name: name, Args: json.RawMessage(args), Status: "running"})
	setDone := func(status, out string) {
		a.updateItem(idx, func(it *Item) { it.Status, it.Output = status, truncateMiddle(out, 20000) })
	}
	if ctx.Err() != nil {
		setDone("cancelled", "")
		return "Cancelled by the user."
	}
	if _, known := toolFuncs[name]; !known {
		setDone("error", "Unknown tool.")
		return "Error: unknown tool " + name
	}
	a.mu.Lock()
	ask := a.cfg.Approval != "auto" && needsApproval[name]
	var ch chan bool
	if ask {
		ch = make(chan bool, 1)
		a.approvals[idx] = ch
	}
	a.mu.Unlock()
	if ask {
		a.updateItem(idx, func(it *Item) { it.Status = "pending_approval" })
		select {
		case ok := <-ch:
			if !ok {
				setDone("denied", "")
				return "The user denied this action. Do not retry it; ask what they would like instead."
			}
		case <-ctx.Done():
			a.mu.Lock()
			delete(a.approvals, idx)
			a.mu.Unlock()
			setDone("cancelled", "")
			return "Cancelled by the user."
		}
		a.updateItem(idx, func(it *Item) { it.Status = "running" })
	}
	out, err := toolFuncs[name](ctx, json.RawMessage(args))
	if err != nil {
		if out != "" {
			out += "\n"
		}
		out += "Error: " + err.Error()
		setDone("error", out)
		return out
	}
	setDone("done", out)
	return truncateMiddle(out, 40000)
}

// ---- tools ----

var toolDefs = []map[string]any{
	tool("run_command", "Run a shell command (/bin/sh -c) on the user's computer and return its exit code and combined stdout/stderr. Non-interactive only.",
		map[string]any{
			"command":     prop("string", "The shell command to run."),
			"workdir":     prop("string", "Working directory (default: home)."),
			"timeout_sec": prop("integer", "Kill the command after this many seconds (default 120, max 900)."),
		}, "command"),
	tool("read_file", "Read a text file. Returns the requested lines.",
		map[string]any{
			"path":   prop("string", "File path (absolute, ~/..., or relative to home)."),
			"offset": prop("integer", "First line to return, 1-based (default 1)."),
			"limit":  prop("integer", "Maximum number of lines (default 2000)."),
		}, "path"),
	tool("write_file", "Create or overwrite a file with the given content. Creates parent folders.",
		map[string]any{
			"path":    prop("string", "File path."),
			"content": prop("string", "Full new file content."),
		}, "path", "content"),
	tool("edit_file", "Replace an exact piece of text in a file. old_text must match exactly once unless replace_all is true.",
		map[string]any{
			"path":        prop("string", "File path."),
			"old_text":    prop("string", "Exact text to find."),
			"new_text":    prop("string", "Replacement text."),
			"replace_all": prop("boolean", "Replace every occurrence."),
		}, "path", "old_text", "new_text"),
	tool("list_dir", "List a folder's entries with type and size.",
		map[string]any{"path": prop("string", "Folder path (default: home).")}),
}

func tool(name, desc string, props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": desc,
		"parameters": map[string]any{"type": "object", "properties": props, "required": required},
	}}
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

type toolFunc func(ctx context.Context, args json.RawMessage) (string, error)

var toolFuncs = map[string]toolFunc{
	"run_command": runCommand,
	"read_file":   readFile,
	"write_file":  writeFile,
	"edit_file":   editFile,
	"list_dir":    listDir,
}

func resolvePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return homeDir()
	}
	if p == "~" {
		return homeDir()
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		p = filepath.Join(homeDir(), rest)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(homeDir(), p)
	}
	return filepath.Clean(p)
}

// limitBuffer keeps the first n bytes of command output and counts the rest.
type limitBuffer struct {
	buf     bytes.Buffer
	n       int
	dropped int
}

func (l *limitBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.buf.Len(); room > 0 {
		if len(p) <= room {
			l.buf.Write(p)
		} else {
			l.buf.Write(p[:room])
			l.dropped += len(p) - room
		}
	} else {
		l.dropped += len(p)
	}
	return len(p), nil
}

func truncateMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head, tail := max*2/5, max*3/5
	return strings.ToValidUTF8(s[:head], "") + fmt.Sprintf("\n… [%d characters omitted] …\n", len(s)-head-tail) + strings.ToValidUTF8(s[len(s)-tail:], "")
}

func runCommand(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Command    string `json:"command"`
		Workdir    string `json:"workdir"`
		TimeoutSec int    `json:"timeout_sec"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", errors.New("command is empty")
	}
	t := a.TimeoutSec
	if t <= 0 {
		t = 120
	}
	t = min(t, 900)
	cctx, cancel := context.WithTimeout(ctx, time.Duration(t)*time.Second)
	defer cancel()
	c := exec.CommandContext(cctx, shellPath, "-c", a.Command)
	c.Dir = resolvePath(a.Workdir)
	c.Env = append(os.Environ(), "TERM=dumb", "PAGER=cat", "GIT_PAGER=cat", "SYSTEMD_PAGER=", "DEBIAN_FRONTEND=noninteractive", "NO_COLOR=1")
	processGroup(c)
	c.WaitDelay = 3 * time.Second
	out := &limitBuffer{n: 4 << 20}
	c.Stdout, c.Stderr = out, out
	err := c.Run()
	code := 0
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case cctx.Err() == nil:
		return "", err // could not start
	}
	var sb strings.Builder
	switch {
	case ctx.Err() != nil:
		sb.WriteString("[stopped by the user]\n")
	case cctx.Err() != nil:
		fmt.Fprintf(&sb, "[timed out after %ds and was killed]\n", t)
	}
	fmt.Fprintf(&sb, "exit code: %d\n", code)
	o := out.buf.String()
	if out.dropped > 0 {
		o += fmt.Sprintf("\n… [%d more bytes of output dropped]", out.dropped)
	}
	if strings.TrimSpace(o) == "" {
		o = "(no output)"
	}
	sb.WriteString(truncateMiddle(o, 30000))
	return sb.String(), nil
}

func readFile(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	p := resolvePath(a.Path)
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s is a folder; use list_dir", p)
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := readUpTo(f, 8<<20)
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return fmt.Sprintf("%s is a binary file (%d bytes). Use run_command with file, xxd or similar to inspect it.", p, st.Size()), nil
	}
	lines := strings.Split(string(data), "\n")
	start := max(a.Offset, 1) - 1
	limit := a.Limit
	if limit <= 0 {
		limit = 2000
	}
	if start >= len(lines) {
		return fmt.Sprintf("[%s has %d lines; offset %d is past the end]", p, len(lines), a.Offset), nil
	}
	end := min(start+limit, len(lines))
	body := truncateMiddle(strings.Join(lines[start:end], "\n"), 100000)
	note := ""
	if int64(len(data)) < st.Size() {
		note = fmt.Sprintf(" (only the first %d bytes of %d were read)", len(data), st.Size())
	}
	return fmt.Sprintf("[%s: lines %d-%d of %d%s]\n%s", p, start+1, end, len(lines), note, body), nil
}

func readUpTo(f *os.File, n int64) ([]byte, error) { return io.ReadAll(io.LimitReader(f, n)) }

func fileMode(p string) os.FileMode {
	if st, err := os.Stat(p); err == nil {
		return st.Mode().Perm()
	}
	return 0o644
}

func writeFile(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	p := resolvePath(a.Path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(a.Content), fileMode(p)); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(a.Content), p), nil
}

func editFile(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Path       string `json:"path"`
		OldText    string `json:"old_text"`
		NewText    string `json:"new_text"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	if a.OldText == "" {
		return "", errors.New("old_text is empty")
	}
	p := resolvePath(a.Path)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	s := string(b)
	n := strings.Count(s, a.OldText)
	switch {
	case n == 0:
		return "", errors.New("old_text was not found in the file; read it again and copy the text exactly")
	case n > 1 && !a.ReplaceAll:
		return "", fmt.Errorf("old_text matches %d places; include more surrounding text or set replace_all", n)
	}
	if a.ReplaceAll {
		s = strings.ReplaceAll(s, a.OldText, a.NewText)
	} else {
		s = strings.Replace(s, a.OldText, a.NewText, 1)
	}
	if err := os.WriteFile(p, []byte(s), fileMode(p)); err != nil {
		return "", err
	}
	return fmt.Sprintf("Replaced %d occurrence(s) in %s", map[bool]int{true: n, false: 1}[a.ReplaceAll], p), nil
}

func listDir(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	p := resolvePath(a.Path)
	entries, err := os.ReadDir(p)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%d entries)\n", p, len(entries))
	for i, e := range entries {
		if i == 1000 {
			fmt.Fprintf(&sb, "… %d more\n", len(entries)-1000)
			break
		}
		switch info, err := e.Info(); {
		case err != nil:
			fmt.Fprintf(&sb, "?  %s\n", e.Name())
		case e.Type()&os.ModeSymlink != 0:
			t, _ := os.Readlink(filepath.Join(p, e.Name()))
			fmt.Fprintf(&sb, "l  %s -> %s\n", e.Name(), t)
		case e.IsDir():
			fmt.Fprintf(&sb, "d  %s/\n", e.Name())
		default:
			fmt.Fprintf(&sb, "f  %s (%d bytes)\n", e.Name(), info.Size())
		}
	}
	return sb.String(), nil
}

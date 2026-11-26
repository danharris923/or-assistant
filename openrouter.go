package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// orBase can point at any OpenAI-compatible API (e.g. a local Ollama) via OPENROUTER_BASE_URL.
var orBase = func() string {
	if u := strings.TrimRight(os.Getenv("OPENROUTER_BASE_URL"), "/"); u != "" {
		return u
	}
	return "https://openrouter.ai/api/v1"
}()

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type tokenUsage struct {
	Cost   float64
	Tokens int
}

func orRequest(ctx context.Context, method, path, key string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, orBase+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Title", "OR Assistant")
	return req, nil
}

// apiError turns an OpenRouter error body into a readable message.
func apiError(status int, body []byte) error {
	var e struct {
		Error struct {
			Message  string `json:"message"`
			Metadata struct {
				Raw          any    `json:"raw"`
				ProviderName string `json:"provider_name"`
			} `json:"metadata"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
		if raw, ok := e.Error.Metadata.Raw.(string); ok && raw != "" && len(raw) < 600 {
			msg += " (" + raw + ")"
		}
	}
	switch status {
	case 401:
		msg = "OpenRouter rejected the API key: " + msg
	case 402:
		msg = "Out of OpenRouter credits or over the key's limit: " + msg
	case 429:
		msg = "Rate limited: " + msg
	}
	if len(msg) > 1000 {
		msg = msg[:1000] + "…"
	}
	return fmt.Errorf("%s", msg)
}

// keyInfo validates a key and returns its spend and limit.
func (a *App) keyInfo(ctx context.Context, key string) (map[string]any, int, error) {
	req, err := orRequest(ctx, "GET", "/key", key, nil)
	if err != nil {
		return nil, 500, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, 502, fmt.Errorf("could not reach OpenRouter: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		status := 502
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			status = 400
		}
		return nil, status, apiError(resp.StatusCode, b)
	}
	var d struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, 502, err
	}
	out := map[string]any{}
	for _, k := range []string{"label", "usage", "limit", "limit_remaining", "is_free_tier"} {
		out[k] = d.Data[k]
	}
	return out, 200, nil
}

// ---- model list ----

type ModelInfo struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Context int     `json:"context"`
	Tools   bool    `json:"tools"`
	In      float64 `json:"in"`  // USD per million input tokens
	Out     float64 `json:"out"` // USD per million output tokens
	Created int64   `json:"created"`
}

type modelCache struct {
	mu      sync.Mutex
	list    []ModelInfo
	byID    map[string]ModelInfo
	fetched time.Time
}

func (m *modelCache) get(ctx context.Context, client *http.Client) ([]ModelInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.list != nil && time.Since(m.fetched) < time.Hour {
		return m.list, nil
	}
	req, err := orRequest(ctx, "GET", "/models", "", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		if m.list != nil {
			return m.list, nil
		}
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return nil, apiError(resp.StatusCode, b)
	}
	var d struct {
		Data []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Created int64  `json:"created"`
			Context int    `json:"context_length"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			Params []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	list := make([]ModelInfo, 0, len(d.Data))
	byID := map[string]ModelInfo{}
	for _, x := range d.Data {
		if strings.HasSuffix(x.ID, ":batch") { // batch endpoints are not for interactive chat
			continue
		}
		in, _ := strconv.ParseFloat(x.Pricing.Prompt, 64)
		out, _ := strconv.ParseFloat(x.Pricing.Completion, 64)
		mi := ModelInfo{ID: x.ID, Name: x.Name, Context: x.Context, In: in * 1e6, Out: out * 1e6, Created: x.Created}
		for _, p := range x.Params {
			if p == "tools" {
				mi.Tools = true
			}
		}
		list = append(list, mi)
		byID[mi.ID] = mi
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Created > list[j].Created })
	m.list, m.byID, m.fetched = list, byID, time.Now()
	return list, nil
}

func (m *modelCache) supportsTools(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	mi, ok := m.byID[id]
	return !ok || mi.Tools // unknown model: offer tools and let the API decide
}

// ---- streaming chat completion ----

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		Total int     `json:"total_tokens"`
		Cost  float64 `json:"cost"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// complete streams one model turn. Text is pushed to the page as it arrives;
// the finished assistant message (text + tool calls) is returned.
func (a *App) complete(ctx context.Context, model string) (Message, tokenUsage, error) {
	var u tokenUsage
	a.mu.Lock()
	msgs := append([]Message(nil), a.msgs...)
	key, _ := a.apiKeyLocked()
	a.mu.Unlock()

	body := map[string]any{
		"model": model, "messages": msgs, "stream": true,
		"usage": map[string]bool{"include": true},
	}
	if a.models.supportsTools(model) {
		body["tools"] = toolDefs
	}
	b, _ := json.Marshal(body)
	req, err := orRequest(ctx, "POST", "/chat/completions", key, b)
	if err != nil {
		return Message{}, u, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return Message{}, u, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return Message{}, u, apiError(resp.StatusCode, eb)
	}

	msg := Message{Role: "assistant"}
	var text strings.Builder
	textIdx := -1
	calls := map[int]*ToolCall{}
	var order []int
	rd := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, rerr := rd.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if p, ok := strings.CutPrefix(line, "data: "); ok {
			if p == "[DONE]" {
				break
			}
			var ch streamChunk
			if json.Unmarshal([]byte(p), &ch) == nil {
				if ch.Error != nil {
					msg.Content = text.String()
					return msg, u, errors.New(ch.Error.Message)
				}
				if ch.Usage != nil {
					u.Cost, u.Tokens = ch.Usage.Cost, ch.Usage.Total
				}
				for _, c := range ch.Choices {
					if c.Delta.Content != "" {
						if textIdx < 0 {
							textIdx = a.addItem(Item{Kind: "assistant"})
						}
						a.appendText(textIdx, c.Delta.Content)
						text.WriteString(c.Delta.Content)
					}
					for _, d := range c.Delta.ToolCalls {
						tc := calls[d.Index]
						if tc == nil {
							tc = &ToolCall{Type: "function"}
							calls[d.Index] = tc
							order = append(order, d.Index)
						}
						if d.ID != "" {
							tc.ID = d.ID
						}
						if tc.Function.Name == "" {
							tc.Function.Name = d.Function.Name
						}
						tc.Function.Arguments += d.Function.Arguments
					}
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			msg.Content = text.String()
			return msg, u, rerr
		}
	}
	msg.Content = text.String()
	for n, i := range order {
		tc := calls[i]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), n)
		}
		msg.ToolCalls = append(msg.ToolCalls, *tc)
	}
	return msg, u, nil
}

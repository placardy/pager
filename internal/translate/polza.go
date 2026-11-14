// Package translate переводит заголовки между русским и английским.
package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Translator переводит пачку заголовков с языка from на to и заодно
// помечает политику. Ответ — ровно столько результатов, сколько пришло
// заголовков, в том же порядке.
type Translator interface {
	Translate(ctx context.Context, from, to string, texts []string) ([]Result, error)
}

type Result struct {
	Text string
	// Заголовок про политику: такие на сайте не показываем.
	Political bool
}

// Polza переводит через chat completions у Polza.ai (OpenAI-совместимый API).
// Справочник: ~/content/video_factory/docs/polza-api.md.
type Polza struct {
	Key     string
	BaseURL string // https://polza.ai/api
	Model   string
	Client  *http.Client
}

func NewPolza(key, baseURL, model string) *Polza {
	return &Polza{
		Key:     key,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		Client:  &http.Client{Timeout: 2 * time.Minute},
	}
}

var langNames = map[string]string{"ru": "Russian", "en": "English"}

func systemPrompt(from, to string) string {
	return fmt.Sprintf(`You translate news headlines from %s to %s for a headline aggregator read by bloggers looking for content ideas.
Input: a JSON array of headlines. Output: a JSON object {"items": [{"t": "...", "p": false}, ...]} with exactly one item per input headline, in the same order.
"t": the translation.
"p": true if the headline is about politics: governments, politicians, elections, parties, laws and regulation, sanctions, wars and military, protests, geopolitics, international relations, political scandals. Business, gadgets, hacks, celebrities, culture, history, fashion, art, science and technology are not politics unless the story is about government action or a political conflict.
Translate each headline as a native %s news editor would write it: short, natural, same tone. Keep names, product names, numbers and tickers accurate; use the common %s spelling of names when one exists. Write every word in %s except proper names and brands; never leave %s words in the output. Translate idioms and headline jargon by meaning, not word by word. Do not add quotes, explanations or anything else.`,
		langNames[from], langNames[to], langNames[to], langNames[to], langNames[to], langNames[from])
}

func (p *Polza) Translate(ctx context.Context, from, to string, texts []string) ([]Result, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	input, err := json.Marshal(texts)
	if err != nil {
		return nil, err
	}
	content, err := p.Complete(ctx, systemPrompt(from, to), string(input), 0.2)
	if err != nil {
		return nil, err
	}
	return parseAnswer(content, len(texts))
}

// Complete — один запрос к модели с ответом в JSON: system задаёт задачу,
// user — данные. Возвращает текст ответа как есть.
func (p *Polza) Complete(ctx context.Context, system, user string, temperature float64) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": p.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature":     temperature,
		"response_format": map[string]string{"type": "json_object"},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+p.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("polza: http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("polza: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("polza: empty response: %s", truncate(string(raw), 300))
	}
	return out.Choices[0].Message.Content, nil
}

// StripFences убирает обёртку ```json … ```, которую модели иногда ставят
// вокруг JSON, даже когда их просили ответить чистым JSON.
func StripFences(content string) string {
	s := strings.TrimSpace(content)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// parseAnswer достаёт переводы и пометки политики из ответа модели. Пометка
// стоит у каждого заголовка, а не списком номеров: номера модель путает.
// Модели иногда оборачивают JSON в ```json или отвечают голым массивом —
// принимаем и так (тогда без пометок).
func parseAnswer(content string, want int) ([]Result, error) {
	s := StripFences(content)

	type item struct {
		T string `json:"t"`
		P bool   `json:"p"`
	}
	var items []item
	var obj struct {
		Items []item   `json:"items"`
		T     []string `json:"t"`
	}
	var arr []string
	switch {
	case json.Unmarshal([]byte(s), &obj) == nil && obj.Items != nil:
		items = obj.Items
	case obj.T != nil:
		arr = obj.T
	case json.Unmarshal([]byte(s), &arr) == nil:
	default:
		return nil, fmt.Errorf("polza: answer is not JSON: %s", truncate(content, 200))
	}
	for _, t := range arr {
		items = append(items, item{T: t})
	}
	if len(items) != want {
		return nil, fmt.Errorf("polza: got %d translations for %d headlines", len(items), want)
	}
	res := make([]Result, len(items))
	for i, it := range items {
		res[i] = Result{Text: strings.TrimSpace(it.T), Political: it.P}
		if res[i].Text == "" {
			return nil, fmt.Errorf("polza: empty translation #%d", i)
		}
	}
	return res, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

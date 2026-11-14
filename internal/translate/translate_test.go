package translate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestParseAnswer(t *testing.T) {
	cases := []struct {
		name, in string
		want     int
		ok       bool
	}{
		{"object", `{"t":["a","b"]}`, 2, true},
		{"items", `{"items":[{"t":"a","p":true},{"t":"b"}]}`, 2, true},
		{"items wrong count", `{"items":[{"t":"a"}]}`, 2, false},
		{"bare array", `["a","b"]`, 2, true},
		{"fenced", "```json\n{\"t\":[\"a\",\"b\"]}\n```", 2, true},
		{"wrong count", `{"t":["a"]}`, 2, false},
		{"empty item", `{"t":["a"," "]}`, 2, false},
		{"prose", `Sure! Here you go`, 2, false},
	}
	for _, c := range cases {
		got, err := parseAnswer(c.in, c.want)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v", c.name, err)
		}
		if c.ok && len(got) != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestPolzaTranslate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("bad request %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var req struct {
			Model    string                     `json:"model"`
			Messages []struct{ Content string } `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var titles []string
		json.Unmarshal([]byte(req.Messages[1].Content), &titles)
		var out []map[string]any
		for i, s := range titles {
			out = append(out, map[string]any{"t": "RU:" + s, "p": i == 1})
		}
		content, _ := json.Marshal(map[string]any{"items": out})
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": string(content)}}},
		})
	}))
	defer srv.Close()

	p := NewPolza("k", srv.URL, "m")
	got, err := p.Translate(context.Background(), "en", "ru", []string{"One", "Two"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Text != "RU:One" || got[1].Text != "RU:Two" || got[0].Political || !got[1].Political {
		t.Fatalf("got %+v", got)
	}
}

func TestCacheSaveLoadPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "tr.json")
	c, err := OpenCache(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now.Add(-30 * 24 * time.Hour) }
	c.Put("ru", "stale", Result{Text: "старое"})
	c.now = func() time.Time { return now }
	c.Put("ru", "fresh", Result{Text: "свежее", Political: true})
	if err := c.Save(14 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}

	c2, err := OpenCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := c2.Get("ru", "fresh"); !ok || got.Text != "свежее" || !got.Political {
		t.Errorf("fresh = %+v %v", got, ok)
	}
	if _, ok := c2.Get("ru", "stale"); ok {
		t.Error("stale entry survived prune")
	}
	if _, ok := c2.Get("en", "fresh"); ok {
		t.Error("direction must be part of the key")
	}
}

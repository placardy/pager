package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/placardy/pager/internal/digest"
	"github.com/placardy/pager/internal/feeds"
	"github.com/placardy/pager/internal/ideas"
	"github.com/placardy/pager/internal/translate"
)

type oneFeed struct{}

func (oneFeed) Fetch(_ context.Context, url string, _ int) ([]feeds.Item, bool, error) {
	return []feeds.Item{{Title: "Rust <2.0>", URL: "https://hn.example/1"}}, false, nil
}

type upper struct{}

func (upper) Translate(_ context.Context, _, to string, texts []string) ([]translate.Result, error) {
	out := make([]translate.Result, len(texts))
	for i, s := range texts {
		out[i].Text = "Раст " + s
	}
	return out, nil
}

func newDigest(t *testing.T, refreshed bool) *digest.Digest {
	d := digest.New(digest.Options{
		Sections: []feeds.Section{{ID: "tech", Name: map[string]string{"ru": "Технологии", "en": "Tech"},
			Sources: []feeds.Source{{Name: "HN", Lang: "en", Site: "https://hn.example", Feed: "hn"}}}},
		Fetcher:    oneFeed{},
		Translator: upper{},
	})
	if refreshed {
		d.Refresh(context.Background())
	}
	return d
}

func get(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestPages(t *testing.T) {
	h := Handler(newDigest(t, true), nil)

	ru := get(h, "/ru")
	if ru.Code != 200 {
		t.Fatal(ru.Code)
	}
	body := ru.Body.String()
	for _, want := range []string{
		`<html lang="ru">`,
		`Технологии`,
		`title="Rust &lt;2.0&gt;">Раст Rust &lt;2.0&gt;</a>`,
		`<a href="?t=tech" aria-current="page">Технологии</a>`,
		`<a href="?t=tech&amp;s=0">HN</a>`,
		`href="/en?t=tech"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/ru: missing %q\n%s", want, body)
		}
	}

	en := get(h, "/en").Body.String()
	if !strings.Contains(en, `>Rust &lt;2.0&gt;</a>`) || strings.Contains(en, "Раст") {
		t.Errorf("/en must show the original:\n%s", en)
	}
	if !strings.Contains(get(h, "/static/style.css").Body.String(), "--paper") {
		t.Error("style.css not served")
	}
}

func TestLoadingPage(t *testing.T) {
	h := Handler(newDigest(t, false), nil)
	w := get(h, "/ru")
	if !strings.Contains(w.Body.String(), "Собираем заголовки") || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("loading page: %s", w.Body.String())
	}
	if get(h, "/healthz").Code != http.StatusServiceUnavailable {
		t.Error("healthz must fail before the first snapshot")
	}
}

func TestRootRedirect(t *testing.T) {
	h := Handler(newDigest(t, false), nil)
	cases := []struct {
		hdr  []string
		want string
	}{
		{[]string{"Accept-Language", "en-US,en;q=0.9,ru;q=0.8"}, "/en"},
		{[]string{"Accept-Language", "de-DE,ru;q=0.9"}, "/ru"},
		{nil, "/ru"},
		{[]string{"Accept-Language", "ru", "Cookie", "lang=en"}, "/en"},
	}
	for _, c := range cases {
		w := get(h, "/", c.hdr...)
		if w.Code != http.StatusFound || w.Header().Get("Location") != c.want {
			t.Errorf("%v → %d %s, want %s", c.hdr, w.Code, w.Header().Get("Location"), c.want)
		}
	}
}

func TestAgo(t *testing.T) {
	if got := Ago("ru", 90*time.Second); got != "1 мин назад" {
		t.Error(got)
	}
	if got := Ago("en", 3*time.Hour); got != "3 h ago" {
		t.Error(got)
	}
}

func TestAge(t *testing.T) {
	cases := []struct {
		lang string
		d    time.Duration
		want string
	}{
		{"ru", -time.Minute, "сейчас"},
		{"ru", 35 * time.Minute, "35 мин"},
		{"en", 5 * time.Hour, "5h"},
		{"ru", 47 * time.Hour, "47 ч"},
		{"ru", 72 * time.Hour, "3 дн"},
	}
	for _, c := range cases {
		if got := Age(c.lang, c.d); got != c.want {
			t.Errorf("Age(%s, %s) = %q, want %q", c.lang, c.d, got, c.want)
		}
	}
}

func TestTopicAndSiteFilter(t *testing.T) {
	d := digest.New(digest.Options{
		Sections: []feeds.Section{
			{ID: "fashion", Name: map[string]string{"ru": "Мода", "en": "Fashion"}, Sources: []feeds.Source{
				{Name: "Vogue", Lang: "en", Feed: "a"}, {Name: "Buro", Lang: "ru", Feed: "b"}}},
			{ID: "tech", Name: map[string]string{"ru": "Технологии", "en": "Tech"}, Sources: []feeds.Source{
				{Name: "HN", Lang: "en", Feed: "c"}}},
		},
		Fetcher: oneFeed{},
	})
	d.Refresh(context.Background())
	h := Handler(d, nil)

	// По умолчанию — первая тематика, все её сайты.
	body := get(h, "/ru").Body.String()
	if !strings.Contains(body, "<h3><a href=\"\">Vogue</a>") || !strings.Contains(body, "<h3><a href=\"\">Buro</a>") || strings.Contains(body, "<h3><a href=\"\">HN</a>") {
		t.Errorf("default topic:\n%s", body)
	}
	// Один сайт.
	body = get(h, "/ru?t=fashion&s=1").Body.String()
	if strings.Contains(body, "<h3><a href=\"\">Vogue</a>") || !strings.Contains(body, `<a href="?t=fashion&amp;s=1" aria-current="page">Buro</a>`) {
		t.Errorf("one site:\n%s", body)
	}
	// Мусор в адресе — первая тематика, все сайты.
	body = get(h, "/ru?t=nope&s=99").Body.String()
	if !strings.Contains(body, `<a href="?t=fashion" aria-current="page">Все</a>`) {
		t.Errorf("bad query:\n%s", body)
	}
}

type ideasLLM struct{}

func (ideasLLM) Complete(context.Context, string, string, float64) (string, error) {
	return `{"ideas":[{"ru":{"topic":"Раст вышел","why":"Все обсуждают","angles":["Обзор","Реакция","Мем"]},
		"en":{"topic":"Rust is out","why":"Everyone talks","angles":["Review","Reaction","Meme"]},"refs":[1]}]}`, nil
}

func TestIdeasTab(t *testing.T) {
	d := newDigest(t, true)
	g := ideas.New(ideasLLM{}, "", time.Hour)
	h := Handler(d, g)

	// Пока идей нет — открываемся на первой тематике, вкладка уже есть.
	body := get(h, "/ru").Body.String()
	if !strings.Contains(body, `<a href="?t=trends">Тренды</a>`) || !strings.Contains(body, `<a href="?t=tech" aria-current="page">`) {
		t.Errorf("before ideas:\n%s", body)
	}
	if !strings.Contains(get(h, "/ru?t=trends").Body.String(), "Тренды появятся") {
		t.Error("pending text missing")
	}

	g.MaybeGenerate(context.Background(), d.Snapshot())
	body = get(h, "/ru").Body.String()
	for _, want := range []string{
		`<a href="?t=trends" aria-current="page">Тренды</a>`,
		`<a href="?t=trends&amp;s=tech">Технологии</a>`,
		`<p class="idea-section">Технологии</p>`,
		`<h2>Раст вышел</h2>`,
		`<li>Реакция</li>`,
		`<a href="https://hn.example/1"><span class="ref-source">HN</span> Раст Rust &lt;2.0&gt;</a>`,
		`href="/en?t=trends"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ideas page: missing %q\n%s", want, body)
		}
	}
	// Один раздел: он выделен, подписи раздела над темой нет.
	body = get(h, "/ru?t=trends&s=tech").Body.String()
	if !strings.Contains(body, `<a href="?t=trends&amp;s=tech" aria-current="page">Технологии</a>`) || strings.Contains(body, "idea-section") {
		t.Errorf("section filter:\n%s", body)
	}
	if !strings.Contains(get(h, "/en").Body.String(), "<h2>Rust is out</h2>") {
		t.Error("english ideas missing")
	}
}

func TestTheme(t *testing.T) {
	h := Handler(newDigest(t, true), nil)

	// Без куки — обе ссылки, CSS прячет лишнюю; адрес возврата экранирован.
	body := get(h, "/ru?t=tech&s=0").Body.String()
	if strings.Contains(body, "data-theme") || !strings.Contains(body, `href="/theme?to=light&back=%2fru%3ft%3dtech%26s%3d0"`) || !strings.Contains(body, "to-dark") {
		t.Errorf("auto theme:\n%s", body)
	}
	body = get(h, "/ru", "Cookie", "theme=light").Body.String()
	if !strings.Contains(body, `data-theme="light"`) || strings.Contains(body, "to-light") || !strings.Contains(body, "to-dark") {
		t.Errorf("light theme:\n%s", body)
	}

	w := get(h, "/theme?to=dark&back=/en?t=tech")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/en?t=tech" || !strings.Contains(w.Header().Get("Set-Cookie"), "theme=dark") {
		t.Errorf("set theme: %d %v", w.Code, w.Header())
	}
	for _, back := range []string{"https://evil.example", "//evil.example", "/\\evil.example", ""} {
		if loc := get(h, "/theme?to=dark&back="+back).Header().Get("Location"); loc != "/" {
			t.Errorf("back=%q redirected to %q", back, loc)
		}
	}
}

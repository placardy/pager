package digest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/placardy/pager/internal/feeds"
	"github.com/placardy/pager/internal/translate"
)

func init() { retryDelay = 0 }

type feedResult struct {
	items []feeds.Item
	nm    bool
	err   error
}

type fakeFetcher map[string]feedResult

func (f fakeFetcher) Fetch(_ context.Context, url string, limit int) ([]feeds.Item, bool, error) {
	r := f[url]
	if len(r.items) > limit {
		r.items = r.items[:limit]
	}
	return r.items, r.nm, r.err
}

// fakeTr переводит приставкой «RU:»/«EN:» и считает политикой всё со словом
// Election.
type fakeTr struct {
	mu    sync.Mutex
	calls int
	fail  bool
}

func (t *fakeTr) Translate(_ context.Context, from, to string, texts []string) ([]translate.Result, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	if t.fail {
		return nil, errors.New("boom")
	}
	out := make([]translate.Result, len(texts))
	for i, s := range texts {
		out[i] = translate.Result{Text: strings.ToUpper(to) + ":" + s, Political: strings.Contains(s, "Election")}
	}
	return out, nil
}

var sections = []feeds.Section{{
	ID:   "tech",
	Name: map[string]string{"ru": "Технологии", "en": "Tech"},
	Sources: []feeds.Source{
		{Name: "HN", Lang: "en", Feed: "hn"},
		{Name: "Хабр", Lang: "ru", Feed: "habr"},
		{Name: "Dead", Lang: "en", Feed: "dead"},
	},
}}

func TestRefreshTranslatesOnceAndKeepsStaleOnError(t *testing.T) {
	ff := fakeFetcher{
		"hn":   {items: []feeds.Item{{Title: "Rust 2.0", URL: "https://hn/1"}}},
		"habr": {items: []feeds.Item{{Title: "Привет", URL: "https://habr/1"}}},
		"dead": {err: errors.New("timeout")},
	}
	tr := &fakeTr{}
	d := New(Options{Sections: sections, Fetcher: ff, Translator: tr, PerSource: 5})
	d.Refresh(context.Background())

	snap := d.Snapshot()
	hn := snap.Sections[0].Sources[0].Items[0]
	if hn.Title["en"] != "Rust 2.0" || hn.Title["ru"] != "RU:Rust 2.0" || !hn.Translated["ru"] {
		t.Errorf("hn headline: %+v", hn)
	}
	habr := snap.Sections[0].Sources[1].Items[0]
	if habr.Title["en"] != "EN:Привет" {
		t.Errorf("habr headline: %+v", habr)
	}
	if !snap.Sections[0].Sources[2].Down {
		t.Error("dead feed with nothing to show must be Down")
	}
	if tr.calls != 2 {
		t.Errorf("want one call per direction, got %d", tr.calls)
	}

	// Второй проход: HN отдаёт 304, Хабр падает — заголовки остаются,
	// переводчик больше не зовём.
	ff["hn"] = feedResult{nm: true}
	ff["habr"] = feedResult{err: errors.New("503")}
	d.Refresh(context.Background())
	snap = d.Snapshot()
	if len(snap.Sections[0].Sources[0].Items) != 1 || len(snap.Sections[0].Sources[1].Items) != 1 {
		t.Fatal("stale items lost")
	}
	if snap.Sections[0].Sources[1].Down {
		t.Error("source with stale items must not be Down")
	}
	if tr.calls != 2 {
		t.Errorf("cached titles were translated again: %d calls", tr.calls)
	}
}

func TestRefreshHidesPoliticsAndKeepsPerSource(t *testing.T) {
	var items []feeds.Item
	for i := 0; i < 8; i++ {
		title := fmt.Sprintf("Story %d", i)
		if i%2 == 0 {
			title = fmt.Sprintf("Election news %d", i)
		}
		items = append(items, feeds.Item{Title: title, URL: fmt.Sprintf("https://hn/%d", i)})
	}
	d := New(Options{Sections: sections, Fetcher: fakeFetcher{"hn": {items: items}}, Translator: &fakeTr{}, PerSource: 3})
	d.Refresh(context.Background())
	got := d.Snapshot().Sections[0].Sources[0].Items
	var titles []string
	for _, h := range got {
		titles = append(titles, h.Title["en"])
	}
	if strings.Join(titles, "|") != "Story 1|Story 3|Story 5" {
		t.Errorf("got %v", titles)
	}
}

func TestRefreshShowsUncheckedWhenTranslatorIsDown(t *testing.T) {
	ff := fakeFetcher{"hn": {items: []feeds.Item{{Title: "Rust 2.0", URL: "https://hn/1"}}}}
	d := New(Options{Sections: sections, Fetcher: ff, Translator: &fakeTr{fail: true}})
	d.Refresh(context.Background())
	h := d.Snapshot().Sections[0].Sources[0].Items[0]
	if h.Title["ru"] != "Rust 2.0" || h.Translated["ru"] {
		t.Errorf("want original on failure: %+v", h)
	}
}

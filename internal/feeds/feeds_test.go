package feeds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/placardy/pager"
)

func TestDefaultSourcesAreValid(t *testing.T) {
	secs, err := ParseSources(pager.DefaultSources)
	if err != nil {
		t.Fatal(err)
	}
	if len(secs) == 0 || len(secs[0].Sources) == 0 {
		t.Fatal("empty default config")
	}
}

func TestParseSourcesRejectsBadLang(t *testing.T) {
	_, err := ParseSources([]byte(`{"sections":[{"id":"x","name":{"ru":"X","en":"X"},
		"sources":[{"name":"A","lang":"de","feed":"https://a.example/rss"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "lang") {
		t.Fatalf("want lang error, got %v", err)
	}
}

const rss = `<?xml version="1.0"?>
<rss version="2.0"><channel><title>T</title><link>https://site.example/</link>
<item><title>Old &amp;amp; boring</title><link>/old</link><pubDate>Mon, 10 Nov 2026 10:00:00 +0000</pubDate></item>
<item><title><![CDATA[<b>New</b>   headline]]></title><link>https://site.example/new</link><pubDate>Thu, 13 Nov 2026 10:00:00 +0000</pubDate></item>
<item><title>New headline</title><link>https://site.example/dup</link></item>
<item><title></title><link>https://site.example/empty</link></item>
<item><title>Script</title><link>javascript:alert(1)</link></item>
<item><title>Middle</title><link>https://site.example/mid</link><pubDate>Wed, 12 Nov 2026 10:00:00 +0000</pubDate></item>
</channel></rss>`

func TestFetchCleansSortsAndUsesConditionalGet(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(rss))
	}))
	defer srv.Close()

	f := NewFetcher("test")
	items, nm, err := f.Fetch(context.Background(), srv.URL, 2)
	if err != nil || nm {
		t.Fatalf("err=%v nm=%v", err, nm)
	}
	want := []Item{
		{Title: "New headline", URL: "https://site.example/new"},
		{Title: "Middle", URL: "https://site.example/mid"},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d items: %+v", len(items), items)
	}
	for i := range want {
		if items[i].Title != want[i].Title || items[i].URL != want[i].URL {
			t.Errorf("item %d = %+v, want %+v", i, items[i], want[i])
		}
	}

	all, _, _ := NewFetcher("test").Fetch(context.Background(), srv.URL, 10)
	last := all[len(all)-1]
	if last.Title != "Old & boring" || last.URL != "https://site.example/old" {
		t.Errorf("relative link / double escaping: %+v", last)
	}

	_, nm, err = f.Fetch(context.Background(), srv.URL, 2)
	if err != nil || !nm {
		t.Fatalf("second fetch: err=%v nm=%v, want not modified", err, nm)
	}
}

func TestFetchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	if _, _, err := NewFetcher("test").Fetch(context.Background(), srv.URL, 5); err == nil {
		t.Fatal("want error on 403")
	}
}

package feeds

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"
)

type Item struct {
	Title     string
	URL       string
	Published time.Time // нулевое, если фид дату не дал
}

// Fetcher качает фиды с условными запросами: помнит ETag и Last-Modified
// каждого фида и на 304 возвращает notModified, а не тянет его заново.
type Fetcher struct {
	Client    *http.Client
	UserAgent string

	mu    sync.Mutex
	cache map[string]validators
}

type validators struct {
	etag, lastModified string
}

const maxFeedSize = 8 << 20

func NewFetcher(userAgent string) *Fetcher {
	// Некоторые сайты за анти-DDoS отвечают на TLS по 10–20 секунд, а
	// стандартный лимит рукопожатия — 10.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSHandshakeTimeout = 25 * time.Second
	return &Fetcher{
		Client:    &http.Client{Timeout: 45 * time.Second, Transport: tr},
		UserAgent: userAgent,
		cache:     map[string]validators{},
	}
}

// Fetch возвращает до limit свежих записей фида, новые первыми.
func (f *Fetcher) Fetch(ctx context.Context, feedURL string, limit int) (items []Item, notModified bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", f.UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, text/xml;q=0.8, */*;q=0.5")
	f.mu.Lock()
	v := f.cache[feedURL]
	f.mu.Unlock()
	if v.etag != "" {
		req.Header.Set("If-None-Match", v.etag)
	}
	if v.lastModified != "" {
		req.Header.Set("If-Modified-Since", v.lastModified)
	}

	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("%s: http %d", feedURL, resp.StatusCode)
	}
	feed, err := gofeed.NewParser().Parse(io.LimitReader(resp.Body, maxFeedSize))
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", feedURL, err)
	}
	items = Extract(feed, feedURL, limit)

	// Валидаторы запоминаем только после успешного разбора: иначе битый ответ
	// застрял бы навсегда за 304.
	f.mu.Lock()
	f.cache[feedURL] = validators{resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")}
	f.mu.Unlock()
	return items, false, nil
}

// Extract вытаскивает из разобранного фида заголовки и ссылки: чистит
// разметку, делает ссылки абсолютными, убирает дубли, сортирует по дате.
func Extract(feed *gofeed.Feed, feedURL string, limit int) []Item {
	base, _ := url.Parse(feedURL)
	if l, err := url.Parse(feed.Link); err == nil && l.IsAbs() {
		base = l
	}
	seen := map[string]bool{}
	var items []Item
	for _, it := range feed.Items {
		title := CleanTitle(it.Title)
		link := strings.TrimSpace(it.Link)
		if link == "" && len(it.Links) > 0 {
			link = strings.TrimSpace(it.Links[0])
		}
		if title == "" || link == "" {
			continue
		}
		u, err := url.Parse(link)
		if err != nil {
			continue
		}
		if base != nil {
			u = base.ResolveReference(u)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			continue
		}
		link = u.String()
		if seen[link] || seen[title] {
			continue
		}
		seen[link], seen[title] = true, true

		item := Item{Title: title, URL: link}
		if it.PublishedParsed != nil {
			item.Published = *it.PublishedParsed
		} else if it.UpdatedParsed != nil {
			item.Published = *it.UpdatedParsed
		}
		items = append(items, item)
	}
	// Записи без даты остаются на своих местах относительно друг друга и
	// уходят в конец: порядок фида для них — лучшее, что есть.
	sort.SliceStable(items, func(i, j int) bool { return items[i].Published.After(items[j].Published) })
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

// CleanTitle убирает из заголовка теги, сущности и лишние пробелы:
// фиды бывают с <b> и &amp;amp; прямо в title.
func CleanTitle(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>' && inTag:
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	s = b.String()
	for i := 0; i < 2 && strings.Contains(s, "&"); i++ {
		s = html.UnescapeString(s)
	}
	return strings.Join(strings.Fields(s), " ")
}

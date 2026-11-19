// Package digest собирает сводку: качает все фиды, переводит новые заголовки
// и держит готовый снимок, который отдаёт сайт.
package digest

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/placardy/pager/internal/feeds"
	"github.com/placardy/pager/internal/translate"
)

// Headline — заголовок на обоих языках. Если перевода ещё нет, на втором
// языке стоит оригинал, а Translated[lang] = false.
type Headline struct {
	URL        string
	Lang       string // язык оригинала
	Title      map[string]string
	Translated map[string]bool
	Published  time.Time
}

type Source struct {
	feeds.Source
	Items []Headline
	// Фид не отвечает, а показать нечего. Если есть что показать с прошлого
	// раза — показываем без пометки.
	Down bool
}

type Section struct {
	ID      string
	Name    map[string]string
	Sources []Source
}

type Snapshot struct {
	Updated  time.Time
	Sections []Section
}

type fetcher interface {
	Fetch(ctx context.Context, feedURL string, limit int) ([]feeds.Item, bool, error)
}

type Digest struct {
	sections  []feeds.Section
	fetcher   fetcher
	tr        translate.Translator // nil — без перевода
	cache     *translate.Cache
	perSource int
	after     func(*Snapshot)
	lastPath  string

	// Последние удачные записи каждого фида: на 304 и на ошибке берём их.
	last map[string][]feeds.Item
	// В last есть новое с прошлой записи на диск.
	lastDirty bool
	// Снимок поднят из файла: первый проход покажет свежие заголовки сразу,
	// не дожидаясь переводов, как после запуска с нуля.
	restored bool

	snap atomic.Pointer[Snapshot]
	now  func() time.Time
}

type Options struct {
	Sections   []feeds.Section
	Fetcher    fetcher
	Translator translate.Translator
	Cache      *translate.Cache
	PerSource  int
	// AfterRefresh зовётся после каждого прохода со свежим снимком.
	AfterRefresh func(*Snapshot)
	// LastPath — файл с последними заголовками каждого фида. После рестарта
	// сайт сразу показывает их, а не «Собираем заголовки» на пару минут,
	// пока медленные фиды не ответят. Пусто — только в памяти.
	LastPath string
}

// saved — содержимое LastPath.
type saved struct {
	Saved time.Time
	Feeds map[string][]feeds.Item
}

func New(o Options) *Digest {
	if o.Cache == nil {
		o.Cache, _ = translate.OpenCache("")
	}
	if o.PerSource <= 0 {
		o.PerSource = 10
	}
	d := &Digest{
		sections:  o.Sections,
		fetcher:   o.Fetcher,
		tr:        o.Translator,
		cache:     o.Cache,
		perSource: o.PerSource,
		after:     o.AfterRefresh,
		lastPath:  o.LastPath,
		last:      map[string][]feeds.Item{},
		now:       time.Now,
	}
	d.loadLast()
	return d
}

// lastMaxAge — заголовки старше этого после рестарта не показываем: сайт,
// простоявший неделю, не должен выдавать недельные новости за свежие.
const lastMaxAge = 24 * time.Hour

// loadLast поднимает заголовки прошлого запуска и сразу собирает из них
// снимок. Время снимка — когда их сохранили, чтобы «обновлено …» не врало.
// Фиды, которых больше нет в sources.json, отбрасываются.
func (d *Digest) loadLast() {
	if d.lastPath == "" {
		return
	}
	data, err := os.ReadFile(d.lastPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("last headlines: %v", err)
		}
		return
	}
	var s saved
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("last headlines: %s: %v", d.lastPath, err)
		return
	}
	if age := d.now().Sub(s.Saved); s.Saved.IsZero() || age > lastMaxAge {
		log.Printf("last headlines: skipped, saved %s ago", age.Round(time.Minute))
		return
	}
	ok := map[string]bool{}
	for _, feed := range d.feedURLs() {
		if items, found := s.Feeds[feed]; found {
			d.last[feed] = items
			ok[feed] = true
		}
	}
	if len(ok) == 0 {
		return
	}
	snap := d.build(ok)
	snap.Updated = s.Saved
	d.snap.Store(snap)
	d.restored = true
}

func (d *Digest) feedURLs() []string {
	var urls []string
	for _, sec := range d.sections {
		for _, s := range sec.Sources {
			urls = append(urls, s.Feed)
		}
	}
	return urls
}

// saveLast пишет last на диск, если с прошлого раза пришло новое. Пишутся
// только фиды из текущего sources.json — удалённые не копятся в файле.
func (d *Digest) saveLast() error {
	if d.lastPath == "" || !d.lastDirty {
		return nil
	}
	cur := map[string][]feeds.Item{}
	for _, feed := range d.feedURLs() {
		if items, ok := d.last[feed]; ok {
			cur[feed] = items
		}
	}
	// Время — как у снимка, чтобы после рестарта «обновлено …» совпадало.
	at := d.now()
	if snap := d.snap.Load(); snap != nil {
		at = snap.Updated
	}
	data, err := json.Marshal(saved{Saved: at, Feeds: cur})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.lastPath), 0o755); err != nil {
		return err
	}
	tmp := d.lastPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, d.lastPath); err != nil {
		return err
	}
	d.lastDirty = false
	return nil
}

// Snapshot — текущая сводка или nil, пока первая не собрана.
func (d *Digest) Snapshot() *Snapshot { return d.snap.Load() }

// Run собирает сводку сразу и дальше раз в interval, пока жив ctx.
func (d *Digest) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		d.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

const (
	fetchParallel     = 8
	batchSize         = 40
	translateParallel = 4
	cacheMaxAge       = 14 * 24 * time.Hour
)

var retryDelay = 3 * time.Second

// Refresh — один проход: фиды → переводы → новый снимок. Refresh не
// вызывается параллельно сам с собой (его зовёт только Run и тесты).
func (d *Digest) Refresh(ctx context.Context) {
	start := d.now()
	fresh := d.fetchAll(ctx)
	if d.tr == nil || d.snap.Load() == nil || d.restored {
		// Первая сводка после запуска — сразу, не дожидаясь переводов: и с
		// нуля, и поверх снимка из файла (переводы после долгого простоя —
		// это минуты).
		d.snap.Store(d.build(fresh))
		d.restored = false
	}
	d.translateMissing(ctx)
	d.snap.Store(d.build(fresh))
	if err := d.cache.Save(cacheMaxAge); err != nil {
		log.Printf("translations cache: %v", err)
	}
	if err := d.saveLast(); err != nil {
		log.Printf("last headlines: %v", err)
	}
	log.Printf("refresh: %d sources, %d translations cached, %s",
		len(fresh), d.cache.Len(), d.now().Sub(start).Round(time.Millisecond))
	if d.after != nil {
		d.after(d.snap.Load())
	}
}

// fetchAll обновляет d.last и возвращает, какие фиды ответили (200 или 304).
func (d *Digest) fetchAll(ctx context.Context) map[string]bool {
	type result struct {
		feed        string
		items       []feeds.Item
		notModified bool
		err         error
	}
	var srcs []feeds.Source
	for _, sec := range d.sections {
		srcs = append(srcs, sec.Sources...)
	}
	results := make(chan result, len(srcs))
	sem := make(chan struct{}, fetchParallel)
	var wg sync.WaitGroup
	for _, s := range srcs {
		wg.Add(1)
		go func(s feeds.Source) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// Берём с запасом: часть заголовков отсеется как политика.
			limit := d.perSource * 2
			items, nm, err := d.fetcher.Fetch(ctx, s.Feed, limit)
			if err != nil && ctx.Err() == nil {
				// Сеть иногда моргает (таймаут TLS и т. п.) — второй шанс сразу,
				// чтобы не ждать следующего прохода.
				time.Sleep(retryDelay)
				items, nm, err = d.fetcher.Fetch(ctx, s.Feed, limit)
			}
			results <- result{s.Feed, items, nm, err}
		}(s)
	}
	wg.Wait()
	close(results)

	ok := map[string]bool{}
	for r := range results {
		switch {
		case r.err != nil:
			log.Printf("fetch: %v", r.err)
		case r.notModified:
			ok[r.feed] = true
		default:
			ok[r.feed] = true
			d.last[r.feed] = r.items
			d.lastDirty = true
		}
	}
	return ok
}

// translateMissing переводит заголовки, которых ещё нет в кэше. Ошибка не
// страшна: заголовок переведётся на следующем проходе.
func (d *Digest) translateMissing(ctx context.Context) {
	if d.tr == nil {
		return
	}
	// from → уникальные заголовки без перевода
	todo := map[string][]string{}
	queued := map[string]bool{}
	for _, sec := range d.sections {
		for _, s := range sec.Sources {
			to := feeds.Other(s.Lang)
			for _, it := range d.last[s.Feed] {
				if _, ok := d.cache.Get(to, it.Title); ok || queued[it.Title] {
					continue
				}
				queued[it.Title] = true
				todo[s.Lang] = append(todo[s.Lang], it.Title)
			}
		}
	}
	// Пачки идут параллельно: после рестарта их пара десятков, по очереди
	// это минуты.
	sem := make(chan struct{}, translateParallel)
	var wg sync.WaitGroup
	for from, titles := range todo {
		to := feeds.Other(from)
		for i := 0; i < len(titles); i += batchSize {
			batch := titles[i:min(i+batchSize, len(titles))]
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				out, err := d.tr.Translate(ctx, from, to, batch)
				if err != nil {
					log.Printf("translate %s→%s (%d): %v", from, to, len(batch), err)
					return
				}
				for j, r := range out {
					d.cache.Put(to, batch[j], r)
				}
			}()
		}
	}
	wg.Wait()
}

func (d *Digest) build(fresh map[string]bool) *Snapshot {
	snap := &Snapshot{Updated: d.now()}
	for _, sec := range d.sections {
		out := Section{ID: sec.ID, Name: sec.Name}
		for _, s := range sec.Sources {
			src := Source{Source: s}
			// Без политики. С переводчиком ещё не проверенные заголовки ждут
			// перевода (он же проверка), кроме случая, когда иначе источник
			// пуст — например, Polza лежит с самого запуска.
			var unchecked []Headline
			for _, it := range d.last[s.Feed] {
				if len(src.Items) == d.perSource {
					break
				}
				h := Headline{
					URL:        it.URL,
					Lang:       s.Lang,
					Title:      map[string]string{s.Lang: it.Title},
					Translated: map[string]bool{},
					Published:  it.Published,
				}
				to := feeds.Other(s.Lang)
				r, ok := d.cache.Get(to, it.Title)
				switch {
				case ok && r.Political:
					continue
				case ok:
					h.Title[to], h.Translated[to] = r.Text, true
				default:
					h.Title[to] = it.Title
					if d.tr != nil {
						unchecked = append(unchecked, h)
						continue
					}
				}
				src.Items = append(src.Items, h)
			}
			if len(src.Items) == 0 {
				src.Items = unchecked[:min(len(unchecked), d.perSource)]
			}
			src.Down = len(src.Items) == 0 && !fresh[s.Feed]
			out.Sources = append(out.Sources, src)
		}
		snap.Sections = append(snap.Sections, out)
	}
	return snap
}

// Package ideas собирает «Тренды»: раз в несколько часов отдаёт модели
// заголовки каждого раздела (политика уже отсеяна) и получает до 3 тем для
// роликов — почему тема сейчас и какие углы взять. Разделы идут отдельными
// запросами, параллельно: у каждого раздела свои темы, их можно фильтровать.
package ideas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/placardy/pager/internal/digest"
	"github.com/placardy/pager/internal/feeds"
	"github.com/placardy/pager/internal/translate"
)

// Text — одна и та же мысль на обоих языках сайта.
type Text map[string]string

type Idea struct {
	Section string              `json:"section"` // id раздела из sources.json
	Topic   Text                `json:"topic"`
	Why     Text                `json:"why"`
	Angles  map[string][]string `json:"angles"`
	Refs    []Ref               `json:"refs"`
}

// Ref — заголовок, из которого выросла тема.
type Ref struct {
	Source Text   `json:"source"`
	Title  Text   `json:"title"`
	URL    string `json:"url"`
}

// version растёт, когда меняется формат ideas.json: старый файл не читается,
// идеи собираются заново.
const version = 4

type Set struct {
	Version   int       `json:"version"`
	Generated time.Time `json:"generated"`
	// По разделам в порядке sources.json, внутри раздела — лучшая первой.
	Ideas []Idea `json:"ideas"`
}

type Completer interface {
	Complete(ctx context.Context, system, user string, temperature float64) (string, error)
}

type Generator struct {
	llm   Completer
	path  string // пусто — только в памяти
	every time.Duration
	now   func() time.Time

	running sync.Mutex
	cur     atomic.Pointer[Set]
}

// New читает прошлые идеи с диска, чтобы рестарт не стоил нового запроса.
func New(llm Completer, path string, every time.Duration) *Generator {
	g := &Generator{llm: llm, path: path, every: every, now: time.Now}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			var s Set
			if json.Unmarshal(data, &s) == nil && s.Version == version && len(s.Ideas) > 0 {
				g.cur.Store(&s)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Printf("ideas: %v", err)
		}
	}
	return g
}

// Current — последние идеи или nil, пока их нет.
func (g *Generator) Current() *Set { return g.cur.Load() }

// MaybeGenerate собирает новые идеи, если старые старше every. Зовётся после
// каждого обхода фидов; повторный вызов, пока идёт генерация, ничего не делает.
func (g *Generator) MaybeGenerate(ctx context.Context, snap *digest.Snapshot) {
	if cur := g.cur.Load(); cur != nil && g.now().Sub(cur.Generated) < g.every {
		return
	}
	if !g.running.TryLock() {
		return
	}
	defer g.running.Unlock()

	start := g.now()
	set, err := g.generate(ctx, snap, g.cur.Load())
	if err != nil {
		log.Printf("ideas: %v", err)
		return
	}
	g.cur.Store(set)
	if err := g.save(set); err != nil {
		log.Printf("ideas: save: %v", err)
	}
	log.Printf("ideas: %d topics in %s", len(set.Ideas), g.now().Sub(start).Round(time.Second))
}

type headline struct {
	source feeds.Source
	h      digest.Headline
}

const system = `You help Russian- and English-speaking content creators find topics. They make all kinds of content: short videos, YouTube, posts, newsletters, scripts they sell. Do not assume a format.
Input: numbered headlines from %s outlets, each as "N. Source: headline".
Pick up to 3 topics from this section that people will want to hear about today, best first. Prefer topics covered by several outlets: these are today's trends. A strong single story is fine too. The topic must belong to the section: for fashion it is about clothes, brands, designers, style; not health or diets. Skip politics, war, crime, deaths and disasters entirely. Skip niche B2B and developer news. If fewer than 3 topics are worth it, return fewer.
For each topic write, in both Russian and English:
- "topic": a short, concrete title (up to 8 words) naming the actual thing, not a category.
- "why": one sentence of plain fact: what happened and why people care. No hype.
- "angles": 3 content ideas, one short line each: what the piece itself is about. For example an opinion to argue, a question the piece answers, a comparison, a story to tell. Each line is the substance of the content, not how to promote it: no engagement tactics (no asking the audience, polls, votes, comments, giveaways, gifts, letters, contests). Do not start lines with labels like "Question:" or "Comparison:". Do not name formats or platforms (no "Reels", "video", "challenge", "POV"), the ideas fit any format.
Also give "refs": the numbers of 1–4 headlines the topic is based on.
Write like a human editor talking to a colleague: plain, specific, calm. Avoid marketing and creator jargon: no "hook", "хук", "вирусный", "залетит", "без хайпа", "разбор", "must-have", "game-changer", no exclamation marks, no emoji.
Russian must be natural Russian, not a translation of the English. Keep brand and people names as they are commonly written in each language.
Output a JSON object: {"ideas": [{"ru": {"topic": "", "why": "", "angles": ["", "", ""]}, "en": {"topic": "", "why": "", "angles": ["", "", ""]}, "refs": [1, 2]}]}`

// generate собирает темы всех разделов параллельно. Раздел, по которому
// запрос не удался, сохраняет темы из old; не удались все — ошибка.
func (g *Generator) generate(ctx context.Context, snap *digest.Snapshot, old *Set) (*Set, error) {
	if snap == nil {
		return nil, errors.New("no snapshot yet")
	}
	type result struct {
		ideas []Idea
		err   error
	}
	results := make([]result, len(snap.Sections))
	var wg sync.WaitGroup
	for i, sec := range snap.Sections {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i].ideas, results[i].err = g.section(ctx, sec)
		}()
	}
	wg.Wait()

	set := &Set{Version: version, Generated: g.now()}
	failed := 0
	for i, sec := range snap.Sections {
		if err := results[i].err; err != nil {
			failed++
			log.Printf("ideas: %s: %v", sec.ID, err)
			if old != nil {
				for _, it := range old.Ideas {
					if it.Section == sec.ID {
						set.Ideas = append(set.Ideas, it)
					}
				}
			}
			continue
		}
		set.Ideas = append(set.Ideas, results[i].ideas...)
	}
	if failed == len(snap.Sections) {
		return nil, errors.New("no section got ideas")
	}
	return set, nil
}

func (g *Generator) section(ctx context.Context, sec digest.Section) ([]Idea, error) {
	var all []headline
	var b strings.Builder
	for _, src := range sec.Sources {
		for _, h := range src.Items {
			all = append(all, headline{src.Source, h})
			// Модели даём английский текст: перевод есть у всех заголовков,
			// а один язык на входе — меньше путаницы.
			fmt.Fprintf(&b, "%d. %s: %s\n", len(all), src.NameIn(feeds.EN), h.Title[feeds.EN])
		}
	}
	if len(all) == 0 {
		return nil, errors.New("no headlines")
	}
	content, err := g.llm.Complete(ctx, fmt.Sprintf(system, sec.Name[feeds.EN]), b.String(), 0.7)
	if err != nil {
		return nil, err
	}
	return parse(content, all, sec.ID)
}

func parse(content string, all []headline, section string) ([]Idea, error) {
	type lang struct {
		Topic  string   `json:"topic"`
		Why    string   `json:"why"`
		Angles []string `json:"angles"`
	}
	var out struct {
		Ideas []struct {
			RU   lang  `json:"ru"`
			EN   lang  `json:"en"`
			Refs []int `json:"refs"`
		} `json:"ideas"`
	}
	if err := json.Unmarshal([]byte(translate.StripFences(content)), &out); err != nil {
		return nil, fmt.Errorf("answer is not JSON: %w", err)
	}
	var ideas []Idea
	for _, it := range out.Ideas {
		if it.RU.Topic == "" || it.EN.Topic == "" {
			continue
		}
		idea := Idea{
			Section: section,
			Topic:   Text{feeds.RU: it.RU.Topic, feeds.EN: it.EN.Topic},
			Why:     Text{feeds.RU: it.RU.Why, feeds.EN: it.EN.Why},
			Angles:  map[string][]string{feeds.RU: it.RU.Angles, feeds.EN: it.EN.Angles},
		}
		seen := map[int]bool{}
		for _, n := range it.Refs {
			// Номера в промпте с единицы; выдуманные номера пропускаем.
			if n < 1 || n > len(all) || seen[n] {
				continue
			}
			seen[n] = true
			h := all[n-1]
			idea.Refs = append(idea.Refs, Ref{
				Source: Text{feeds.RU: h.source.NameIn(feeds.RU), feeds.EN: h.source.NameIn(feeds.EN)},
				Title:  Text{feeds.RU: h.h.Title[feeds.RU], feeds.EN: h.h.Title[feeds.EN]},
				URL:    h.h.URL,
			})
		}
		ideas = append(ideas, idea)
	}
	if len(ideas) == 0 {
		return nil, errors.New("no ideas in answer")
	}
	return ideas, nil
}

func (g *Generator) save(set *Set) error {
	if g.path == "" {
		return nil
	}
	data, err := json.Marshal(set)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(g.path), 0o755); err != nil {
		return err
	}
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, g.path)
}

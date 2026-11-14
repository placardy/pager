package ideas

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/placardy/pager/internal/digest"
	"github.com/placardy/pager/internal/feeds"
)

// fakeLLM отвечает answer; на запрос, в системном промпте которого есть fail,
// возвращает ошибку. Разделы зовут его параллельно.
type fakeLLM struct {
	mu     sync.Mutex
	calls  int
	answer string
	fail   string
	gotIn  []string
}

func (f *fakeLLM) Complete(_ context.Context, system, user string, _ float64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotIn = append(f.gotIn, user)
	if f.fail != "" && strings.Contains(system, f.fail) {
		return "", errors.New("boom")
	}
	return f.answer, nil
}

func section(id, name string, items ...digest.Headline) digest.Section {
	return digest.Section{
		ID:   id,
		Name: map[string]string{"ru": name, "en": name},
		Sources: []digest.Source{{
			Source: feeds.Source{Name: "Vogue", Lang: "en"},
			Items:  items,
		}},
	}
}

func snap() *digest.Snapshot {
	return &digest.Snapshot{Sections: []digest.Section{section("fashion", "Fashion",
		digest.Headline{URL: "https://v/1", Lang: "en", Title: map[string]string{"en": "Rosalía wears white", "ru": "Розалия в белом"}},
		digest.Headline{URL: "https://v/2", Lang: "en", Title: map[string]string{"en": "Ferrari SS27", "ru": "Ferrari весна-лето 27"}},
	)}}
}

const answer = "```json\n" + `{"ideas":[
 {"ru":{"topic":"Белое на Latin Grammy","why":"Все обсуждают","angles":["a","b","c"]},
  "en":{"topic":"White at the Latin Grammys","why":"Everyone talks","angles":["a","b","c"]},
  "refs":[1, 1, 99]},
 {"ru":{"topic":""},"en":{"topic":"skip me"}}
]}` + "\n```"

func TestGenerateParsesAndPersists(t *testing.T) {
	llm := &fakeLLM{answer: answer}
	path := filepath.Join(t.TempDir(), "ideas.json")
	g := New(llm, path, 8*time.Hour)
	g.MaybeGenerate(context.Background(), snap())

	if len(llm.gotIn) != 1 || !strings.Contains(llm.gotIn[0], "1. Vogue: Rosalía wears white") {
		t.Errorf("prompt input: %q", llm.gotIn)
	}
	set := g.Current()
	if set == nil || len(set.Ideas) != 1 {
		t.Fatalf("got %+v", set)
	}
	idea := set.Ideas[0]
	if idea.Section != "fashion" || idea.Topic["ru"] != "Белое на Latin Grammy" || len(idea.Refs) != 1 || idea.Refs[0].URL != "https://v/1" || idea.Refs[0].Title["ru"] != "Розалия в белом" {
		t.Errorf("idea: %+v", idea)
	}

	// Свежие идеи не пересобираются, а после рестарта читаются с диска.
	g.MaybeGenerate(context.Background(), snap())
	if llm.calls != 1 {
		t.Errorf("regenerated too early: %d calls", llm.calls)
	}
	g2 := New(llm, path, 8*time.Hour)
	if g2.Current() == nil || g2.Current().Ideas[0].Topic["en"] != "White at the Latin Grammys" {
		t.Error("ideas not loaded from disk")
	}
	g2.MaybeGenerate(context.Background(), snap())
	if llm.calls != 1 {
		t.Errorf("restart regenerated fresh ideas: %d calls", llm.calls)
	}
}

func TestGenerateKeepsOldOnBadAnswer(t *testing.T) {
	llm := &fakeLLM{answer: answer}
	g := New(llm, "", time.Hour)
	g.MaybeGenerate(context.Background(), snap())
	g.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	llm.answer = "sorry"
	g.MaybeGenerate(context.Background(), snap())
	if g.Current() == nil || len(g.Current().Ideas) != 1 {
		t.Error("old ideas lost after a bad answer")
	}
}

// Упал один раздел — у него остаются прошлые темы, у остальных новые.
func TestGenerateKeepsOldSectionOnError(t *testing.T) {
	h := digest.Headline{URL: "https://v/1", Lang: "en", Title: map[string]string{"en": "A", "ru": "А"}}
	two := &digest.Snapshot{Sections: []digest.Section{section("fashion", "Fashion", h), section("tech", "Tech", h)}}

	llm := &fakeLLM{answer: answer}
	g := New(llm, "", time.Hour)
	g.MaybeGenerate(context.Background(), two)
	if n := len(g.Current().Ideas); n != 2 {
		t.Fatalf("want one idea per section, got %d", n)
	}

	g.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	llm.answer = strings.Replace(answer, "Белое на Latin Grammy", "Новое", 1)
	llm.fail = "from Tech outlets"
	g.MaybeGenerate(context.Background(), two)
	got := map[string]string{}
	for _, it := range g.Current().Ideas {
		got[it.Section] = it.Topic["ru"]
	}
	if got["fashion"] != "Новое" || got["tech"] != "Белое на Latin Grammy" {
		t.Errorf("got %v", got)
	}
}

// Файл старого формата (без разделов) не читается — идеи соберутся заново.
func TestOldFormatIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ideas.json")
	g := New(&fakeLLM{}, path, time.Hour)
	if err := g.save(&Set{Generated: time.Now(), Ideas: []Idea{{Topic: Text{"ru": "x"}}}}); err != nil {
		t.Fatal(err)
	}
	if New(&fakeLLM{}, path, time.Hour).Current() != nil {
		t.Error("old-format ideas loaded")
	}
}

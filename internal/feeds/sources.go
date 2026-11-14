// Package feeds — список источников и загрузка RSS/Atom.
package feeds

import (
	"encoding/json"
	"fmt"
	"net/url"
)

// Языки сайта. Заголовок каждого источника есть на обоих: на своём — как
// в фиде, на другом — переводом.
const (
	RU = "ru"
	EN = "en"
)

var Langs = []string{RU, EN}

// Other — язык, на который переводят заголовки с lang.
func Other(lang string) string {
	if lang == RU {
		return EN
	}
	return RU
}

type Source struct {
	Name string `json:"name"`
	// Название для английской версии, если отличается (Коммерсантъ → Kommersant).
	NameEN string `json:"name_en,omitempty"`
	Lang   string `json:"lang"`
	Site   string `json:"site"`
	Feed   string `json:"feed"`
}

func (s Source) NameIn(lang string) string {
	if lang == EN && s.NameEN != "" {
		return s.NameEN
	}
	return s.Name
}

type Section struct {
	ID      string            `json:"id"`
	Name    map[string]string `json:"name"`
	Sources []Source          `json:"sources"`
}

// ParseSources читает sources.json и проверяет его целиком: ошибка в конфиге
// должна ронять запуск, а не всплывать пустой колонкой на сайте.
func ParseSources(data []byte) ([]Section, error) {
	var cfg struct {
		Sections []Section `json:"sections"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("sources: %w", err)
	}
	if len(cfg.Sections) == 0 {
		return nil, fmt.Errorf("sources: no sections")
	}
	seen := map[string]bool{}
	for _, sec := range cfg.Sections {
		if sec.ID == "" {
			return nil, fmt.Errorf("sources: section without id")
		}
		for _, l := range Langs {
			if sec.Name[l] == "" {
				return nil, fmt.Errorf("sources: section %q has no %s name", sec.ID, l)
			}
		}
		for _, s := range sec.Sources {
			if s.Name == "" {
				return nil, fmt.Errorf("sources: %s: source without name", sec.ID)
			}
			if s.Lang != RU && s.Lang != EN {
				return nil, fmt.Errorf("sources: %s: lang must be ru or en, got %q", s.Name, s.Lang)
			}
			if u, err := url.Parse(s.Feed); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return nil, fmt.Errorf("sources: %s: bad feed url %q", s.Name, s.Feed)
			}
			if seen[s.Feed] {
				return nil, fmt.Errorf("sources: feed %s listed twice", s.Feed)
			}
			seen[s.Feed] = true
		}
	}
	return cfg.Sections, nil
}

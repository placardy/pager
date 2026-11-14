// Package web отдаёт сводку одной HTML-страницей на русском (/ru) и
// английском (/en).
package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/placardy/pager/internal/digest"
	"github.com/placardy/pager/internal/feeds"
	"github.com/placardy/pager/internal/ideas"
)

//go:embed page.html static
var files embed.FS

var page = template.Must(template.New("page.html").Funcs(template.FuncMap{
	"cssVersion": func() string { return cssVersion },
}).ParseFS(files, "page.html"))

// cssVersion — хэш стилей в ссылке на них: браузер кэширует style.css на
// сутки, а после правки сразу берёт новый.
var cssVersion = func() string {
	data, _ := files.ReadFile("static/style.css")
	return fmt.Sprintf("%x", sha256.Sum256(data))[:10]
}()

type strs struct {
	Title, Description, Updated, Down, Loading, Switch, Sources, All string
	Ideas, IdeasNote, IdeasPending                                   string
	ToLight, ToDark, Angles                                          string
}

var text = map[string]strs{
	feeds.RU: {
		Title:        "Pager",
		Description:  "Заголовки русских и английских СМИ на одной странице, английские — в переводе.",
		Updated:      "обновлено %s",
		Down:         "фид не отвечает",
		Loading:      "Собираем заголовки…",
		Switch:       "English",
		Sources:      "Источники",
		All:          "Все",
		Ideas:        "Тренды",
		IdeasNote:    "темы из свежих заголовков, собраны %s",
		IdeasPending: "Тренды появятся через пару минут",
		ToLight:      "Светлая тема",
		ToDark:       "Тёмная тема",
		Angles:       "Идеи для контента",
	},
	feeds.EN: {
		Title:        "Pager",
		Description:  "Russian and English headlines on one page, the Russian ones translated.",
		Updated:      "updated %s",
		Down:         "feed is not responding",
		Loading:      "Collecting headlines…",
		Switch:       "Русский",
		Sources:      "Sources",
		All:          "All",
		Ideas:        "Trends",
		IdeasNote:    "topics from fresh headlines, picked %s",
		IdeasPending: "Trends will appear in a couple of minutes",
		ToLight:      "Light theme",
		ToDark:       "Dark theme",
		Angles:       "Content ideas",
	},
}

type Server struct {
	Digest *digest.Digest
	Ideas  *ideas.Generator // nil — без вкладки идей (нет ключа Polza)
	now    func() time.Time
}

func Handler(d *digest.Digest, g *ideas.Generator) http.Handler {
	s := &Server{Digest: d, Ideas: g, now: time.Now}
	static, _ := fs.Sub(files, "static")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.root)
	mux.HandleFunc("GET /ru", s.page(feeds.RU))
	mux.HandleFunc("GET /en", s.page(feeds.EN))
	mux.HandleFunc("GET /theme", setTheme)
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(24*time.Hour, http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", s.health)
	return mux
}

// root отправляет на язык из куки (последний выбранный) или из браузера.
func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	lang := ""
	if c, err := r.Cookie("lang"); err == nil && (c.Value == feeds.RU || c.Value == feeds.EN) {
		lang = c.Value
	} else {
		lang = PreferredLang(r.Header.Get("Accept-Language"))
	}
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Cookie, Accept-Language")
	http.Redirect(w, r, "/"+lang, http.StatusFound)
}

// PreferredLang — первый ru или en из Accept-Language по порядку, иначе ru.
func PreferredLang(header string) string {
	for _, part := range strings.Split(header, ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		switch {
		case tag == "ru" || strings.HasPrefix(tag, "ru-"):
			return feeds.RU
		case tag == "en" || strings.HasPrefix(tag, "en-"):
			return feeds.EN
		}
	}
	return feeds.RU
}

func (s *Server) page(lang string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name: "lang", Value: lang, Path: "/", MaxAge: 365 * 24 * 3600,
			SameSite: http.SameSiteLaxMode,
		})
		v := s.view(lang, s.Digest.Snapshot(), r.URL.Query().Get("t"), r.URL.Query().Get("s"))
		if c, err := r.Cookie("theme"); err == nil && (c.Value == "light" || c.Value == "dark") {
			v.Theme = c.Value
		}
		v.Back = r.URL.RequestURI()
		var buf bytes.Buffer
		if err := page.Execute(&buf, v); err != nil {
			log.Printf("render: %v", err)
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Vary", "Accept-Encoding, Cookie")
		if v.Loading {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			// Страница зависит от куки темы: после переключения браузер не
			// должен показать копию из кэша.
			w.Header().Set("Cache-Control", "private, no-cache")
		}
		// Страница ~60 КБ текста, сжатие на телефоне заметно. Edge-Caddy сам
		// не сжимает — это забота проекта.
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
			gz.Write(buf.Bytes())
			gz.Close()
			return
		}
		w.Write(buf.Bytes())
	}
}

// setTheme запоминает тему в куке и возвращает на страницу, с которой
// пришли. Без JS: переключатель — обычная ссылка. back — только путь на этом
// же сайте, иначе это был бы открытый редирект.
func setTheme(w http.ResponseWriter, r *http.Request) {
	switch to := r.URL.Query().Get("to"); to {
	case "light", "dark":
		http.SetCookie(w, &http.Cookie{
			Name: "theme", Value: to, Path: "/", MaxAge: 365 * 24 * 3600,
			SameSite: http.SameSiteLaxMode,
		})
	}
	back := r.URL.Query().Get("back")
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") || strings.HasPrefix(back, "/\\") {
		back = "/"
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if s.Digest.Snapshot() == nil {
		http.Error(w, "warming up", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

type pageView struct {
	Lang, Other string
	T           strs
	Updated     string
	Loading     bool
	// Тема из куки: light, dark или пусто — как в системе.
	Theme string
	// Текущий адрес — куда вернуться после переключения темы.
	Back string
	// Выбор в адресе (?t=fashion&s=2) — его сохраняет переключатель языка.
	Query string
	// Тематики во вкладках шапки.
	Topics []link
	// Сайты выбранной тематики, первым — «Все».
	Sites []link
	// Что показываем: все сайты тематики или один выбранный.
	Shown []sourceView

	// Вкладка «Тренды»: темы для роликов, вместо сайтов — фильтр по разделам.
	IdeasTab     bool
	IdeaList     []ideaView
	IdeasNote    string
	IdeasPending bool
}

type ideaView struct {
	// Раздел темы — подпись над ней, когда показаны все разделы.
	Section    string
	Topic, Why string
	Angles     []string
	Refs       []refView
}

type refView struct {
	Source, Title, URL string
}

const ideasID = "trends"

type link struct {
	Name, URL string
	Active    bool
}

type sourceView struct {
	Name, Site string
	// Язык оригинала, если он не совпадает с языком страницы: у такого
	// источника заголовки — перевод.
	Foreign string
	Down    bool
	Items   []itemView
}

type itemView struct {
	Title, URL string
	// Язык текста заголовка: отличается от языка страницы, если перевода нет.
	TextLang string
	// Оригинал — во всплывающей подсказке у переведённого заголовка.
	Original string
	// Возраст заголовка, «35 мин» / «5 ч»; пусто, если фид не дал дату.
	Age string
	// Вышел меньше часа назад. Пока не выделяется (см. style.css).
	Fresh bool
}

// view собирает страницу одной тематики: t — её id (по умолчанию первая),
// s — номер сайта в ней (пусто или неверный номер — все сайты).
func (s *Server) view(lang string, snap *digest.Snapshot, t, site string) pageView {
	v := pageView{Lang: lang, Other: feeds.Other(lang), T: text[lang]}
	if snap == nil || len(snap.Sections) == 0 {
		v.Loading = true
		return v
	}
	v.Updated = fmt.Sprintf(v.T.Updated, Ago(lang, s.now().Sub(snap.Updated)))

	// Открываем на идеях, если они есть: это главное, зачем блогер пришёл.
	var set *ideas.Set
	if s.Ideas != nil {
		set = s.Ideas.Current()
		if t == "" && set != nil {
			t = ideasID
		}
		v.Topics = append(v.Topics, link{Name: v.T.Ideas, URL: query(ideasID, ""), Active: t == ideasID})
	}
	if t == ideasID && s.Ideas != nil {
		s.trends(&v, lang, snap, set, site)
		return v
	}

	cur := snap.Sections[0]
	for _, sec := range snap.Sections {
		if sec.ID == t {
			cur = sec
		}
	}
	for _, sec := range snap.Sections {
		v.Topics = append(v.Topics, link{Name: sec.Name[lang], URL: query(sec.ID, ""), Active: sec.ID == cur.ID})
	}
	pick := -1
	if n, err := strconv.Atoi(site); err == nil && n >= 0 && n < len(cur.Sources) {
		pick = n
	}
	v.Query = query(cur.ID, site)
	if pick < 0 {
		v.Query = query(cur.ID, "")
	}
	v.Sites = append(v.Sites, link{Name: v.T.All, URL: query(cur.ID, ""), Active: pick < 0})
	for i, src := range cur.Sources {
		v.Sites = append(v.Sites, link{Name: src.NameIn(lang), URL: query(cur.ID, strconv.Itoa(i)), Active: i == pick})
		if pick < 0 || i == pick {
			v.Shown = append(v.Shown, s.source(lang, src))
		}
	}
	return v
}

// trends — вкладка «Тренды». Под ней разделы, как сайты под тематикой:
// «Все» — лучшая тема каждого раздела, раздел — все его темы. sec — id
// раздела из адреса (?t=trends&s=film).
func (s *Server) trends(v *pageView, lang string, snap *digest.Snapshot, set *ideas.Set, sec string) {
	v.IdeasTab = true
	for _, x := range snap.Sections {
		v.Topics = append(v.Topics, link{Name: x.Name[lang], URL: query(x.ID, "")})
	}
	if set == nil {
		v.Query = query(ideasID, "")
		v.IdeasPending = true
		return
	}
	has := map[string]bool{}
	for _, it := range set.Ideas {
		has[it.Section] = true
	}
	names := map[string]string{}
	for _, x := range snap.Sections {
		if has[x.ID] {
			names[x.ID] = x.Name[lang]
		}
	}
	if names[sec] == "" {
		sec = ""
	}
	v.Query = query(ideasID, sec)
	v.IdeasNote = fmt.Sprintf(v.T.IdeasNote, Ago(lang, s.now().Sub(set.Generated)))
	v.Sites = append(v.Sites, link{Name: v.T.All, URL: query(ideasID, ""), Active: sec == ""})
	for _, x := range snap.Sections {
		if names[x.ID] != "" {
			v.Sites = append(v.Sites, link{Name: names[x.ID], URL: query(ideasID, x.ID), Active: x.ID == sec})
		}
	}

	shown := map[string]bool{}
	for _, it := range set.Ideas {
		if names[it.Section] == "" || (sec != "" && it.Section != sec) || (sec == "" && shown[it.Section]) {
			continue
		}
		shown[it.Section] = true
		iv := ideaView{Topic: it.Topic[lang], Why: it.Why[lang], Angles: it.Angles[lang]}
		if sec == "" {
			iv.Section = names[it.Section]
		}
		for _, r := range it.Refs {
			iv.Refs = append(iv.Refs, refView{Source: r.Source[lang], Title: r.Title[lang], URL: r.URL})
		}
		v.IdeaList = append(v.IdeaList, iv)
	}
}

func query(topic, site string) string {
	q := "?t=" + url.QueryEscape(topic)
	if site != "" {
		q += "&s=" + url.QueryEscape(site)
	}
	return q
}

func (s *Server) source(lang string, src digest.Source) sourceView {
	srv := sourceView{Name: src.NameIn(lang), Site: src.Site, Down: src.Down}
	if src.Lang != lang {
		srv.Foreign = src.Lang
	}
	for _, h := range src.Items {
		iv := itemView{Title: h.Title[lang], URL: h.URL, TextLang: lang}
		if !h.Published.IsZero() {
			age := s.now().Sub(h.Published)
			iv.Age, iv.Fresh = Age(lang, age), age < time.Hour
		}
		if h.Lang != lang {
			if h.Translated[lang] {
				iv.Original = h.Title[h.Lang]
			} else {
				iv.TextLang = h.Lang
			}
		}
		srv.Items = append(srv.Items, iv)
	}
	return srv
}

// Age — короткий возраст заголовка: «35 мин», «5 ч», «2 дн» / «35m», «5h»,
// «2d». Будущие даты (часы у фида спешат) считаются как «сейчас».
func Age(lang string, d time.Duration) string {
	ru := lang == feeds.RU
	m := int(d.Minutes())
	switch {
	case m < 1 && ru:
		return "сейчас"
	case m < 1:
		return "now"
	case m < 60 && ru:
		return fmt.Sprintf("%d мин", m)
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m < 48*60 && ru:
		return fmt.Sprintf("%d ч", m/60)
	case m < 48*60:
		return fmt.Sprintf("%dh", m/60)
	case ru:
		return fmt.Sprintf("%d дн", m/60/24)
	default:
		return fmt.Sprintf("%dd", m/60/24)
	}
}

// Ago — «5 мин назад» / «5 min ago». Сокращения избавляют от склонений.
func Ago(lang string, d time.Duration) string {
	m := int(d.Minutes())
	ru := lang == feeds.RU
	switch {
	case m < 1 && ru:
		return "только что"
	case m < 1:
		return "just now"
	case m < 60 && ru:
		return fmt.Sprintf("%d мин назад", m)
	case m < 60:
		return fmt.Sprintf("%d min ago", m)
	case ru:
		return fmt.Sprintf("%d ч назад", m/60)
	default:
		return fmt.Sprintf("%d h ago", m/60)
	}
}

func cacheFor(d time.Duration, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(d.Seconds())))
		h.ServeHTTP(w, r)
	})
}

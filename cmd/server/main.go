// Сервер Pager: раз в REFRESH собирает фиды, переводит заголовки и отдаёт
// страницу.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/placardy/pager"
	"github.com/placardy/pager/internal/digest"
	"github.com/placardy/pager/internal/feeds"
	"github.com/placardy/pager/internal/ideas"
	"github.com/placardy/pager/internal/translate"
	"github.com/placardy/pager/internal/web"
)

func main() {
	addr := env("ADDR", ":8080")
	refresh := envDuration("REFRESH", 15*time.Minute)
	perSource := envInt("PER_SOURCE", 10)

	data := pager.DefaultSources
	if path := os.Getenv("SOURCES"); path != "" {
		var err error
		if data, err = os.ReadFile(path); err != nil {
			log.Fatal(err)
		}
	}
	sections, err := feeds.ParseSources(data)
	if err != nil {
		log.Fatal(err)
	}

	cache, err := translate.OpenCache(env("CACHE_PATH", "data/translations.json"))
	if err != nil {
		log.Fatalf("translations cache: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var tr translate.Translator
	var gen *ideas.Generator
	after := func(*digest.Snapshot) {}
	if key := os.Getenv("POLZA_AI_API_KEY"); key != "" {
		base := env("POLZA_BASE_URL", "https://polza.ai/api")
		model := env("TRANSLATE_MODEL", "anthropic/claude-haiku-5.5")
		trLLM := translate.NewPolza(key, base, model)
		tr = trLLM
		// Тренды на Haiku: сбор ~0,6 ₽ против ~9,5 ₽ у Sonnet (оценка по
		// ценам Polza и размеру запроса). Раз в 12 часов от прошлого сбора,
		// не по часам.
		ideasModel := env("IDEAS_MODEL", "anthropic/claude-haiku-5.5")
		ideasLLM := translate.NewPolza(key, base, ideasModel)
		gen = ideas.New(ideasLLM, env("IDEAS_PATH", "data/ideas.json"), envDuration("IDEAS_EVERY", 12*time.Hour))
		logged := 0.0
		after = func(s *digest.Snapshot) {
			// Траты — в лог, только когда выросли: иначе строка каждые 15 минут.
			if spent := trLLM.Spent() + ideasLLM.Spent(); spent != logged {
				logged = spent
				log.Printf("polza: spent since start %.2f ₽ (translate %.2f, trends %.2f)",
					spent, trLLM.Spent(), ideasLLM.Spent())
			}
			go gen.MaybeGenerate(ctx, s)
		}
		log.Printf("translator: polza %s, ideas: %s", model, ideasModel)
	} else {
		log.Printf("translator and ideas: off (POLZA_AI_API_KEY is empty), headlines stay in original")
	}

	d := digest.New(digest.Options{
		Sections:     sections,
		Fetcher:      feeds.NewFetcher("Pager/1.0 (RSS headline reader)"),
		Translator:   tr,
		Cache:        cache,
		PerSource:    perSource,
		AfterRefresh: after,
		LastPath:     env("LAST_PATH", "data/last.json"),
	})
	go d.Run(ctx, refresh)

	srv := &http.Server{
		Addr:              addr,
		Handler:           web.Handler(d, gen),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("listening on %s, refresh every %s", addr, refresh)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return def
}

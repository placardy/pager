package translate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Cache хранит готовые переводы, чтобы каждый заголовок оплачивался один раз,
// в том числе после рестарта. Живёт в одном JSON-файле: на экране всего
// пара сотен заголовков, база тут не нужна.
type Cache struct {
	path string
	now  func() time.Time

	mu    sync.Mutex
	m     map[string]*entry
	dirty bool
}

type entry struct {
	Text      string `json:"t"`
	Political bool   `json:"p,omitempty"`
	// Когда перевод последний раз понадобился, unix-секунды. По нему чистим.
	Used int64 `json:"u"`
}

// OpenCache читает кэш с диска. Пустой path — кэш только в памяти.
func OpenCache(path string) (*Cache, error) {
	c := &Cache{path: path, now: time.Now, m: map[string]*entry{}}
	if path == "" {
		return c, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &c.m); err != nil {
		return nil, err
	}
	return c, nil
}

func key(to, text string) string { return to + "\x00" + text }

func (c *Cache) Get(to, text string) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key(to, text)]
	if !ok {
		return Result{}, false
	}
	if now := c.now().Unix(); now-e.Used > 3600 {
		e.Used = now
		c.dirty = true
	}
	return Result{Text: e.Text, Political: e.Political}, true
}

func (c *Cache) Put(to, text string, r Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key(to, text)] = &entry{Text: r.Text, Political: r.Political, Used: c.now().Unix()}
	c.dirty = true
}

func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// Save выкидывает переводы, которые не были нужны дольше maxAge, и пишет
// файл атомарно через временный. Без изменений ничего не делает.
func (c *Cache) Save(maxAge time.Duration) error {
	c.mu.Lock()
	cutoff := c.now().Add(-maxAge).Unix()
	for k, e := range c.m {
		if e.Used < cutoff {
			delete(c.m, k)
			c.dirty = true
		}
	}
	if !c.dirty || c.path == "" {
		c.mu.Unlock()
		return nil
	}
	data, err := json.Marshal(c.m)
	c.dirty = false
	c.mu.Unlock()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".translations-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}

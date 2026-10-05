package ignatius

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// CacheConfig is the global [cache] table (SPEC 11.2). Off unless Enabled.
type CacheConfig struct {
	Enabled    bool `toml:"enabled"`
	TTLMS      int  `toml:"ttl_ms"`
	MaxEntries int  `toml:"max_entries"`
}

// Cache is a bounded in-memory LRU with TTL, keyed per question.
type Cache struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	now   func() time.Time
	ll    *list.List
	items map[string]*list.Element
}

type cacheEntry struct {
	key     string
	ans     Answer
	expires time.Time
}

func NewCache(cfg CacheConfig, now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	ttl, max := time.Duration(cfg.TTLMS)*time.Millisecond, cfg.MaxEntries
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if max <= 0 {
		max = 10000
	}
	return &Cache{ttl: ttl, max: max, now: now, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *Cache) get(key string) (Answer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return Answer{}, false
	}
	e := el.Value.(*cacheEntry)
	if !c.now().Before(e.expires) {
		c.ll.Remove(el)
		delete(c.items, key)
		return Answer{}, false
	}
	c.ll.MoveToFront(el)
	return cloneAnswer(e.ans), true
}

func (c *Cache) put(key string, a Answer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp := c.now().Add(c.ttl)
	if el, ok := c.items[key]; ok {
		e := el.Value.(*cacheEntry)
		e.ans, e.expires = cloneAnswer(a), exp
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(&cacheEntry{key: key, ans: cloneAnswer(a), expires: exp})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.items, last.Value.(*cacheEntry).key)
	}
}

func cloneAnswer(a Answer) Answer {
	cp := func(p *float64) *float64 {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	a.Noul, a.Score, a.Confidence = cp(a.Noul), cp(a.Score), cp(a.Confidence)
	if a.Probabilities != nil {
		m := make(map[string]float64, len(a.Probabilities))
		for k, v := range a.Probabilities {
			m[k] = v
		}
		a.Probabilities = m
	}
	if a.Legend != nil {
		m := make(map[string]string, len(a.Legend))
		for k, v := range a.Legend {
			m[k] = v
		}
		a.Legend = m
	}
	a.Contributors = append([]string(nil), a.Contributors...)
	return a
}

// questionKey hashes everything that determines one question's answer from one
// model: the alias, its upstream model, the state, and the question content
// (not its id). The state itself is not retained, only the hash.
func questionKey(alias, upstream string, state any, imagesHash string, q Question) (string, bool) {
	b, err := json.Marshal(struct {
		Alias, Model string
		State        any
		Images       string // a digest of the images: the same text with a different picture is a different question
		Type         string
		Instructions any
		Criteria     any
	}{alias, upstream, state, imagesHash, q.Type, q.Instructions, q.Criteria})
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), true
}

// hashImages is a digest of an ordered list of images, "" for none.
func hashImages(images []string) string {
	if len(images) == 0 {
		return ""
	}
	h := sha256.New()
	for _, img := range images {
		sum := sha256.Sum256([]byte(img)) // length-delimited by construction: fixed-size digests
		h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Cached serves repeated questions from a Cache. Cached questions are answered
// without a call; the rest go to Inner as a smaller request and are merged.
type Cached struct {
	Inner         Backend
	C             *Cache
	Alias         string
	UpstreamModel string
	Priced        bool // report cost 0 (not nil) when nothing had to be called
}

func (c *Cached) Call(ctx context.Context, req Request) (WireResponse, error) {
	hits := map[string]Answer{}
	keys := map[string]string{}
	miss := Request{State: req.State, Images: req.Images, Questions: map[string]Question{}}
	imagesHash := hashImages(req.Images) // once per call, not once per question: images are large
	for id, q := range req.Questions {
		key, ok := questionKey(c.Alias, c.UpstreamModel, req.State, imagesHash, q)
		if ok {
			if a, hit := c.C.get(key); hit {
				hits[id] = a
				continue
			}
			keys[id] = key
		}
		miss.Questions[id] = q
	}
	if len(miss.Questions) == 0 {
		w := WireResponse{Model: "cache", Answers: hits, Cached: len(hits)}
		if c.Priced {
			zero := 0.0
			w.CostUSD = &zero
		}
		return w, nil
	}
	w, err := c.Inner.Call(ctx, miss)
	if err != nil {
		return w, err
	}
	for id := range miss.Questions {
		if a, ok := w.Answers[id]; ok {
			if key, ok := keys[id]; ok {
				c.C.put(key, a)
			}
		}
	}
	if len(hits) > 0 {
		merged := make(map[string]Answer, len(w.Answers)+len(hits))
		for id, a := range w.Answers {
			merged[id] = a
		}
		for id, a := range hits {
			merged[id] = a
		}
		w.Answers, w.Cached = merged, len(hits)
	}
	return w, nil
}

func (c *Cached) Probe(ctx context.Context) string { return probeOf(ctx, c.Inner) }

package tui

// Rendering caches for the live screens. A mouse wheel or trackpad swipe
// reaches a screen as a storm of arrow keys (hundreds a second with
// momentum in iTerm2), and Bubble Tea calls View after every one of them.
// Without caches each frame re-measured every row, so a swipe queued
// seconds of rendering while the keys kept coming. Now a frame is drawn
// again only when something it shows changed, and the costly parts (the
// board's column layout over every row, each drawn row, the dashboard's
// body) are reused across frames.
//
// Models are values copied on every Update, so each cache sits behind a
// pointer all copies of a model share. Every entry is keyed by everything
// it depends on, data generation included, so a copy in another state
// never reads an entry that is not its own.

import (
	"sync"
	"sync/atomic"
	"time"
)

var dataGen atomic.Int64

// nextGen stamps new screen data. Stamps are unique across models, so two
// copies of one model that received different data never share a key.
func nextGen() int64 { return dataGen.Add(1) }

// clockKey is the part of a key that tracks the clock: ages ("3m") and
// "updated 2s ago" change with it, at most once a second.
func clockKey(now func() time.Time) int64 { return now().Unix() }

// errText is err's message for a key; "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// frameCache holds the last frame drawn and its key.
type frameCache[K comparable] struct {
	mu    sync.Mutex
	key   K
	frame string
	ok    bool
}

// get returns the frame for key, drawing it on a miss. A nil cache always
// draws.
func (c *frameCache[K]) get(key K, draw func() string) string {
	if c == nil {
		return draw()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ok || c.key != key {
		c.frame, c.key, c.ok = draw(), key, true
	}
	return c.frame
}

// partCache holds values derived from one state of the data (one key):
// a different key empties it.
type partCache[K, P comparable, V any] struct {
	mu    sync.Mutex
	key   K
	parts map[P]V
}

// get returns the part p of key, building it on a miss. A nil cache
// always builds it.
func (c *partCache[K, P, V]) get(key K, p P, build func() V) V {
	if c == nil {
		return build()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.parts == nil || c.key != key {
		c.key, c.parts = key, map[P]V{}
	}
	v, ok := c.parts[p]
	if !ok {
		v = build()
		c.parts[p] = v
	}
	return v
}

func (c *prbCache) frameFor(k prbFrameKey, draw func() string) string {
	if c == nil {
		return draw()
	}
	return c.frame.get(k, draw)
}

func (c *prbCache) layoutFor(k prbRowsKey, build func() prbLayout) prbLayout {
	if c == nil {
		return build()
	}
	return c.layout.get(k, struct{}{}, build)
}

func (c *prbCache) row(k prbRowsKey, r prbRowKey, draw func() string) string {
	if c == nil {
		return draw()
	}
	return c.rows.get(k, r, draw)
}

func (c *dashCache) frameFor(k dashFrameKey, draw func() string) string {
	if c == nil {
		return draw()
	}
	return c.frame.get(k, draw)
}

func (c *dashCache) bodyFor(k dashBodyKey, draw func() dashBody) dashBody {
	if c == nil {
		return draw()
	}
	return c.body.get(k, struct{}{}, draw)
}

// summaryFor is the board's summary line for k at the spinner's frame anim.
func (c *prbCache) summaryFor(k prbRowsKey, anim int, draw func() string) string {
	if c == nil {
		return draw()
	}
	return c.summary.get(k, anim, draw)
}

func (c *dashCache) headerFor(k dashHeaderKey, draw func() []string) []string {
	if c == nil {
		return draw()
	}
	return c.header.get(k, struct{}{}, draw)
}

func (c *prbCache) naturalFor(k prbRowsKey, build func() prbNatural) prbNatural {
	if c == nil {
		return build()
	}
	return c.natural.get(k, struct{}{}, build)
}

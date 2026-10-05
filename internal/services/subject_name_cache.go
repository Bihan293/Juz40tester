package services

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// subjectNamesTTL: how long the cached subject list is served (A5).
const subjectNamesTTL = 60 * time.Second

// subjectNameCache is a thread-safe in-memory cache of subject id -> name,
// refreshed from the full subject list at most once per TTL. A subject
// missing from a fresh-enough list forces one reload (a subject added a
// moment ago is not reported as unknown for a whole TTL).
type subjectNameCache struct {
	ttl  time.Duration
	load func(context.Context) ([]models.Subject, error)

	mu     sync.Mutex
	names  map[int64]string
	loaded time.Time
}

func newSubjectNameCache(ttl time.Duration, load func(context.Context) ([]models.Subject, error)) *subjectNameCache {
	return &subjectNameCache{ttl: ttl, load: load}
}

func (c *subjectNameCache) name(ctx context.Context, id int64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fresh := c.names != nil && time.Since(c.loaded) < c.ttl
	if fresh {
		if n, ok := c.names[id]; ok {
			return n, nil
		}
	}
	if err := c.reloadLocked(ctx); err != nil {
		return "", err
	}
	n, ok := c.names[id]
	if !ok {
		return "", fmt.Errorf("subject %d not found", id)
	}
	return n, nil
}

func (c *subjectNameCache) reloadLocked(ctx context.Context) error {
	list, err := c.load(ctx)
	if err != nil {
		return err
	}
	names := make(map[int64]string, len(list))
	for _, s := range list {
		names[s.ID] = s.Name
	}
	c.names, c.loaded = names, time.Now()
	return nil
}

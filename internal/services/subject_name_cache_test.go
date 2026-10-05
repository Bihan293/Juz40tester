package services

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
)

func TestSubjectNameCacheLoadsOncePerTTL(t *testing.T) {
	var mu sync.Mutex
	loads := 0
	list := []models.Subject{{ID: 1, Name: "Математика"}}
	c := newSubjectNameCache(time.Hour, func(context.Context) ([]models.Subject, error) {
		mu.Lock()
		defer mu.Unlock()
		loads++
		return append([]models.Subject(nil), list...), nil
	})
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n, err := c.name(ctx, 1); err != nil || n != "Математика" {
				t.Errorf("name: %q %v", n, err)
			}
		}()
	}
	wg.Wait()
	if loads != 1 {
		t.Fatalf("loads = %d, want 1", loads)
	}
	// An unknown id forces one reload and then finds a new subject.
	mu.Lock()
	list = append(list, models.Subject{ID: 2, Name: "Русский язык"})
	mu.Unlock()
	if n, err := c.name(ctx, 2); err != nil || n != "Русский язык" || loads != 2 {
		t.Fatalf("new subject: %q %v loads=%d", n, err, loads)
	}
	if _, err := c.name(ctx, 3); err == nil {
		t.Fatal("missing subject must be an error")
	}
}

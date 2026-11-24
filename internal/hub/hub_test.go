package hub

import (
	"sync"
	"testing"
	"time"
)

func TestFanOut(t *testing.T) {
	h := New()
	var subs []*Sub
	for i := 0; i < 100; i++ {
		subs = append(subs, h.Subscribe(0))
	}
	h.Publish(Frame{V: 1, Data: []byte("x")})
	for _, s := range subs {
		select {
		case f := <-s.C:
			if f.V != 1 {
				t.Fatal("wrong frame")
			}
		case <-time.After(time.Second):
			t.Fatal("timeout")
		}
	}
	if h.Count() != 100 {
		t.Fatal("count")
	}
}

func TestSlowSubscriberDropped(t *testing.T) {
	h := New()
	slow := h.Subscribe(0)
	fast := h.Subscribe(0)
	go func() {
		for range fast.C {
		}
	}()
	for i := 0; i < 100; i++ {
		h.Publish(Frame{V: int64(i)})
		time.Sleep(200 * time.Microsecond) // let the fast consumer keep up; slow never reads
	}
	if !slow.Dropped() {
		t.Fatal("slow subscriber should be dropped")
	}
	if h.Count() != 1 {
		t.Fatalf("count=%d", h.Count())
	}
	h.Unsubscribe(slow) // must be a harmless no-op
}

func TestMaxAndClose(t *testing.T) {
	h := New()
	if h.Subscribe(1) == nil {
		t.Fatal("first should succeed")
	}
	if h.Subscribe(1) != nil {
		t.Fatal("max not enforced")
	}
	h.Close()
	if h.Subscribe(0) != nil {
		t.Fatal("closed hub accepted subscriber")
	}
}

func TestConcurrent(t *testing.T) {
	h := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := h.Subscribe(0)
			if s == nil {
				return
			}
			for j := 0; j < 5; j++ {
				select {
				case <-s.C:
				case <-time.After(10 * time.Millisecond):
				}
			}
			h.Unsubscribe(s)
		}()
	}
	for i := 0; i < 200; i++ {
		h.Publish(Frame{V: int64(i)})
	}
	wg.Wait()
}

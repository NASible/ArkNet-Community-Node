package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEngineValidationCapacity(t *testing.T) {
	e := New(time.Hour, nil)
	for _, input := range []struct {
		id   string
		size int64
		hash string
	}{{"bad", 1, ""}, {id, -1, ""}, {id, 1, "BAD"}, {id, 1, strings.Repeat("A", 64)}} {
		if _, err := e.Add(input.id, input.size, input.hash); !errors.Is(err, ErrInvalid) {
			t.Fatal(input, err)
		}
	}
	for i := 0; i < 4096; i++ {
		if _, err := e.Add(fmt.Sprintf("media_%064x", i), 1, ""); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := e.Add(id, 1, ""); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := e.Add(fmt.Sprintf("media_%064x", 0), 1, ""); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentSnapshotsAndReports(t *testing.T) {
	e := New(time.Hour, nil)
	e.Add(id, 100, "")
	var wg sync.WaitGroup
	for worker := 0; worker < 20; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				e.Add(id, 100, "")
				e.Action(id, "pause")
				e.Action(id, "resume")
				e.ReportProgress(id, int64(i))
				e.ReportCompletion(id)
				e.ReportVerificationFailure(id)
				e.Action(id, "cancel")
				s, err := e.Get(id)
				if err != nil || !s.Mock || s.CompletedBytes < 0 || s.CompletedBytes > s.TotalBytes || (s.State == Completed && s.CompletedBytes != s.TotalBytes) {
					t.Errorf("incoherent snapshot: %+v %v", s, err)
				}
				e.List()
			}
		}()
	}
	wg.Wait()
}
func TestElapsedNotPollCountAndReportRules(t *testing.T) {
	now := time.Unix(0, 0)
	e := New(10*time.Second, func() time.Time { return now })
	e.Add(id, 100, "")
	for i := 0; i < 100; i++ {
		s, _ := e.Get(id)
		if s.CompletedBytes != 0 {
			t.Fatal(s)
		}
	}
	e.ReportProgress(id, 100)
	s, _ := e.Get(id)
	if s.State != Downloading {
		t.Fatal(s)
	}
	e.Action(id, "pause")
	if _, err := e.ReportProgress(id, 100); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
	s, err := e.ReportCompletion(id)
	if err != nil || s.State != Completed {
		t.Fatal(s, err)
	}
	e = New(10*time.Second, func() time.Time { return now })
	e.Add(id, 100, "")
	now = now.Add(11 * time.Second)
	s, _ = e.Get(id)
	if s.State != Completed || s.CompletedBytes != 100 {
		t.Fatal(s)
	}
}

package engine

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

const id = "media_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestAddElapsedPauseResumeCancel(t *testing.T) {
	now := time.Unix(100, 0)
	e := New(10*time.Second, func() time.Time { return now })
	s, err := e.Add(id, math.MaxInt64, "")
	if err != nil || s.State != Downloading || s.CompletedBytes != 0 || !s.Mock {
		t.Fatal(s, err)
	}
	now = now.Add(5 * time.Second)
	s, err = e.Get(id)
	if err != nil || s.CompletedBytes != math.MaxInt64/2 {
		t.Fatal(s, err)
	}
	s, err = e.Action(id, "pause")
	if err != nil || s.State != Paused {
		t.Fatal(s, err)
	}
	now = now.Add(time.Hour)
	repeated, _ := e.Add(id, math.MaxInt64, "")
	if repeated != s {
		t.Fatal(repeated, s)
	}
	if _, err = e.Add(id, 1, ""); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err = e.Action(id, "pause"); err != nil {
		t.Fatal(err)
	}
	s, err = e.Action(id, "resume")
	if err != nil || s.State != Downloading {
		t.Fatal(s, err)
	}
	now = now.Add(5 * time.Second)
	s, _ = e.Get(id)
	if s.State != Completed || s.CompletedBytes != math.MaxInt64 {
		t.Fatal(s)
	}
	if _, err = e.Action(id, "resume"); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
	second := "media_" + strings.Repeat("b", 64)
	e.Add(second, 50, "")
	s, err = e.Action(second, "cancel")
	if err != nil || s.State != Cancelled {
		t.Fatal(s, err)
	}
	s2, err := e.Action(second, "cancel")
	if err != nil || s2 != s {
		t.Fatal(s2, err)
	}
	if _, err = e.Action(second, "pause"); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
	list := e.List()
	if len(list) != 2 || list[0].ObjectID != id || list[1].ObjectID != second {
		t.Fatal(list)
	}
	if _, err = e.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

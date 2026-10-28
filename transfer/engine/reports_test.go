package engine

import (
	"errors"
	"testing"
	"time"
)

func TestWorkerReports(t *testing.T) {
	now := time.Unix(0, 0)
	e := New(time.Second, func() time.Time { return now })
	var reporter Reporter = e
	e.Add(id, 100, "")
	s, err := reporter.ReportProgress(id, 40)
	if err != nil || s.CompletedBytes != 40 || s.State != Downloading {
		t.Fatal(s, err)
	}
	for _, n := range []int64{-1, 39, 101} {
		if _, err = reporter.ReportProgress(id, n); !errors.Is(err, ErrInvalid) {
			t.Fatal(n, err)
		}
	}
	s, err = reporter.ReportVerificationFailure(id)
	if err != nil || s.State != VerificationFailed || s.CompletedBytes != 40 || s.ErrorCode != "verification_failed" || !s.Mock {
		t.Fatal(s, err)
	}
	again, err := reporter.ReportVerificationFailure(id)
	if err != nil || again != s {
		t.Fatal(again, err)
	}
	if _, err = reporter.ReportCompletion(id); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
	if _, err = reporter.ReportProgress(id, 40); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
	for _, action := range []string{"pause", "resume", "cancel"} {
		if _, err = e.Action(id, action); !errors.Is(err, ErrTransition) {
			t.Fatal(err)
		}
	}
	e = New(time.Second, func() time.Time { return now })
	e.Add(id, 0, "")
	s, _ = e.Get(id)
	if s.State != Downloading {
		t.Fatal(s)
	}
	s, err = e.ReportCompletion(id)
	if err != nil || s.State != Completed || s.CompletedBytes != 0 {
		t.Fatal(s, err)
	}
	again, err = e.ReportCompletion(id)
	if err != nil || again != s {
		t.Fatal(again, err)
	}
	if _, err = e.ReportVerificationFailure(id); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
	if _, err = e.ReportCompletion("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	e = New(time.Second, func() time.Time { return now })
	e.Add(id, 0, "")
	now = now.Add(time.Second)
	s, _ = e.Get(id)
	if s.State != Completed {
		t.Fatal(s)
	}
	e = New(time.Second, func() time.Time { return now })
	e.Add(id, 10, "")
	e.Action(id, "cancel")
	if _, err = e.ReportCompletion(id); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
	if _, err = e.ReportVerificationFailure(id); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
}

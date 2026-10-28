package transfer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
		want error
	}{
		{401, `{"error":"unauthorized"}`, ErrUnauthorized}, {404, `{"error":"not_found"}`, ErrNotFound}, {409, `{"error":"conflict"}`, ErrConflict}, {409, `{"error":"invalid_transition"}`, ErrInvalidTransition}, {400, `{"error":"invalid_request"}`, ErrInvalidRequest}, {500, `{"error":"private"}`, ErrUnavailable}, {200, `{"error":"unauthorized"}`, ErrInvalidResponse}, {401, `{"error":"not_found"}`, ErrInvalidResponse}, {404, `{"error":"not_found","secret":"private"}`, ErrInvalidResponse}, {400, `not json private`, ErrInvalidResponse}, {403, `{"error":"unauthorized"}`, ErrInvalidResponse},
	} {
		t.Run(fmt.Sprint(tc.code, tc.body), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testToken {
					t.Error("wrong-token test did not send supplied token")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			got, err := New(s.URL, testToken).Get(context.Background(), testID)
			if !errors.Is(err, tc.want) || got != (Status{}) {
				t.Fatalf("got %#v %v want %v", got, err, tc.want)
			}
		})
	}
}

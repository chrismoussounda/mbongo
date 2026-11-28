package mtn

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDefaultSenderPutsJSON(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- r.Method + " " + r.Header.Get("Content-Type") + " " + string(body)
	}))
	defer srv.Close()

	status, err := put(&http.Client{Timeout: time.Second})(context.Background(), srv.URL, []byte(`{"status":"SUCCESSFUL"}`))
	if err != nil || status != http.StatusOK {
		t.Fatal(status, err)
	}
	if want := `PUT application/json {"status":"SUCCESSFUL"}`; <-got != want {
		t.Fatalf("want %s", want)
	}
}

package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestRunServesThenStopsCleanly(t *testing.T) {
	addr := freeAddr(t)
	env := map[string]string{"MBONGO_ADDR": addr}
	var logs safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, func(k string) string { return env[k] }, &logs) }()

	url := "http://" + addr + "/healthz"
	var res *http.Response
	for range 50 {
		var err error
		if res, err = http.Get(url); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if res == nil {
		t.Fatal("server never answered")
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
	if !strings.Contains(logs.String(), "stopped") {
		t.Errorf("logs = %s", logs.String())
	}
}

func TestRunFailsWhenTheAddressIsTaken(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	env := map[string]string{"MBONGO_ADDR": l.Addr().String()}

	err = run(context.Background(), func(k string) string { return env[k] }, &safeBuffer{})
	if err == nil {
		t.Fatal("run returned nil on a taken address")
	}
}

func TestAddress(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, ":8080"},
		{map[string]string{"PORT": "10000"}, ":10000"},
		{map[string]string{"PORT": "10000", "MBONGO_ADDR": "127.0.0.1:9"}, "127.0.0.1:9"},
	}
	for _, c := range cases {
		if got := address(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("address(%v) = %q, want %q", c.env, got, c.want)
		}
	}
}

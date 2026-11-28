package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
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

// waitUp waits for the server to answer, failing the test after a few seconds.
func waitUp(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := http.Get(base + "/healthz")
		if err == nil {
			_ = res.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never answered: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunServesThenStopsCleanly(t *testing.T) {
	addr := freeAddr(t)
	env := map[string]string{"MBONGO_ADDR": addr, "MBONGO_DB": filepath.Join(t.TempDir(), "m.db")}
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
	env := map[string]string{"MBONGO_ADDR": l.Addr().String(), "MBONGO_DB": filepath.Join(t.TempDir(), "m.db")}

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

func TestRunFailsWhenTheDatabaseCannotOpen(t *testing.T) {
	env := map[string]string{"MBONGO_ADDR": freeAddr(t), "MBONGO_DB": filepath.Join(t.TempDir(), "missing", "m.db")}
	if err := run(context.Background(), func(k string) string { return env[k] }, &safeBuffer{}); err == nil {
		t.Fatal("run returned nil with an unusable database path")
	}
}

// An open phone page holds an event stream; stopping must not wait for it.
func TestRunStopsWithAPhonePageOpen(t *testing.T) {
	addr := freeAddr(t)
	env := map[string]string{"MBONGO_ADDR": addr, "MBONGO_DB": filepath.Join(t.TempDir(), "m.db"),
		"MBONGO_ADMIN_TOKEN": "secret"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var logs safeBuffer
	go func() { done <- run(ctx, func(k string) string { return env[k] }, &logs) }()

	base := "http://" + addr
	admin := func(path, body string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
	}
	waitUp(t, base)
	admin("/_mbongo/customers", `{"operator":"mtn","phone":"242061234567","currency":"XAF"}`)
	open, _ := http.NewRequest(http.MethodGet, base+"/_mbongo/phone/mtn/242061234567", nil)
	phone, err := http.DefaultTransport.RoundTrip(open)
	if err != nil {
		t.Fatal(err)
	}
	_ = phone.Body.Close()
	req, _ := http.NewRequest(http.MethodGet, base+phone.Header.Get("Location")+"/events", nil)
	stream, err := http.DefaultClient.Do(req)
	if err != nil || stream.StatusCode != http.StatusOK {
		t.Fatalf("events: %v %v", err, stream)
	}
	defer func() { _ = stream.Body.Close() }()

	// Shutdown gives up on the open page after 10 s with an error; a connection the client
	// opened and never used can hold it up to 5 s, which is fine.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("run waited for the open page; logs:\n%s", logs.String())
	}
}

func TestAutoCustomersSettings(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		fail string
	}{
		{map[string]string{"MBONGO_AUTO_CUSTOMERS": "yes"}, "MBONGO_AUTO_CUSTOMERS must be true or false"},
		{map[string]string{"MBONGO_AUTO_CUSTOMERS": "true", "MBONGO_AUTO_BALANCE": "lots"}, "MBONGO_AUTO_BALANCE must be"},
		{map[string]string{"MBONGO_AUTO_CUSTOMERS": "false", "MBONGO_AUTO_BALANCE": "lots"}, ""},
	} {
		c.env["MBONGO_ADDR"], c.env["MBONGO_DB"] = freeAddr(t), filepath.Join(t.TempDir(), "m.db")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- run(ctx, func(k string) string { return c.env[k] }, &safeBuffer{}) }()
		if c.fail == "" {
			waitUp(t, "http://"+c.env["MBONGO_ADDR"])
			cancel()
		}
		err := <-done
		cancel()
		if c.fail == "" && err != nil || c.fail != "" && (err == nil || !strings.Contains(err.Error(), c.fail)) {
			t.Errorf("%v: %v", c.env, err)
		}
	}
}

func TestAutoCustomersReachTheAPI(t *testing.T) {
	addr := freeAddr(t)
	env := map[string]string{"MBONGO_ADDR": addr, "MBONGO_DB": filepath.Join(t.TempDir(), "m.db"),
		"MBONGO_ADMIN_TOKEN": "secret", "MBONGO_AUTO_CUSTOMERS": "true", "MBONGO_AUTO_BALANCE": "2500"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, func(k string) string { return env[k] }, &safeBuffer{}) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run returned %v", err)
		}
	}()
	base := "http://" + addr
	waitUp(t, base)
	post := func(path, body string, headers map[string]string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	app := post("/_mbongo/airtel/apps", `{"name":"Shop"}`, map[string]string{"Authorization": "Bearer secret"})
	tok := post("/auth/oauth2/token", `{"client_id":"`+app["clientId"].(string)+`","client_secret":"`+
		app["clientSecret"].(string)+`","grant_type":"client_credentials"}`, nil)
	paid := post("/merchant/v1/payments/", `{"reference":"r","subscriber":{"country":"CG","currency":"XAF","msisdn":"055000009"},
		"transaction":{"amount":1000,"country":"CG","currency":"XAF","id":"t1"}}`, map[string]string{
		"Authorization": "Bearer " + tok["access_token"].(string), "X-Country": "CG", "X-Currency": "XAF"})
	if status, _ := paid["status"].(map[string]any); status["success"] != true {
		t.Fatalf("collection from a new number: %v", paid)
	}
}

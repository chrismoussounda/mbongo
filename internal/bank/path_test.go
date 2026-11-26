package bank_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/clock"
)

func TestOpenTakesAnyFileName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my data #%20")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Skipf("this file system refuses the name: %v", err)
	}
	path := filepath.Join(dir, "books 1.db")
	b, err := bank.Open(context.Background(), path, clock.NewManual(start))
	if err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not at %s: %v", path, err)
	}
}

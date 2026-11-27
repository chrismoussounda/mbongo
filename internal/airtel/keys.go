package airtel

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
)

const (
	keyBits = 1024
	keyLife = 30 * 24 * time.Hour
	// validLayout is the docs' "12-08-2024 21:29:26".
	validLayout = "02-01-2006 15:04:05"
)

type rsaKey struct {
	ID        int64
	Private   *rsa.PrivateKey
	ValidUpto time.Time
}

// currentKey returns the live key, making a new one when the last has expired.
func (s *Service) currentKey(ctx context.Context) (rsaKey, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	now := s.books.Clock().Now()
	k, err := s.store.lastKey(ctx)
	if err == nil && now.Before(k.ValidUpto) {
		return k, nil
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return k, err
	}
	private, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return k, err
	}
	return s.store.addKey(ctx, private, now.Add(keyLife))
}

func (s *Service) encryptionKeys(w http.ResponseWriter, r *http.Request, _ App) {
	k, err := s.currentKey(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	der, err := x509.MarshalPKIXPublicKey(&k.Private.PublicKey)
	if err != nil {
		s.internal(w, err)
		return
	}
	ok(w, "DP02010001001", "Success", map[string]any{
		"key_id": k.ID, "key": base64.StdEncoding.EncodeToString(der), "valid_upto": k.ValidUpto.UTC().Format(validLayout),
	})
}

// checkPIN answers false after writing the refusal: ROUTER116 when the value can't be
// decrypted with the live key, ROUTER115 when it decrypts to another PIN.
func (s *Service) checkPIN(w http.ResponseWriter, r *http.Request, app App, encrypted string) bool {
	if encrypted == "" {
		refuse(w, http.StatusBadRequest, "ROUTER003", "Missing header/body params")
		return false
	}
	k, err := s.currentKey(r.Context())
	if err != nil {
		s.internal(w, err)
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		refuse(w, http.StatusOK, "ROUTER116", "Incorrect Encrypted Pin")
		return false
	}
	pin, err := rsa.DecryptPKCS1v15(nil, k.Private, raw) //nolint:staticcheck // SA1019: Airtel's scheme, not ours to choose
	if err != nil {
		refuse(w, http.StatusOK, "ROUTER116", "Incorrect Encrypted Pin")
		return false
	}
	if string(pin) != app.PIN {
		refuse(w, http.StatusOK, "ROUTER115", "Incorrect Pin")
		return false
	}
	return true
}

func (st *store) lastKey(ctx context.Context) (rsaKey, error) {
	var (
		k     rsaKey
		der   []byte
		until string
	)
	err := st.db.QueryRowContext(ctx, "SELECT key_id, private_der, valid_upto FROM airtel_keys ORDER BY key_id DESC LIMIT 1").
		Scan(&k.ID, &der, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	if err != nil {
		return k, err
	}
	if k.Private, err = x509.ParsePKCS1PrivateKey(der); err != nil {
		return k, err
	}
	k.ValidUpto, err = time.Parse(bank.TimeLayout, until)
	return k, err
}

func (st *store) addKey(ctx context.Context, private *rsa.PrivateKey, until time.Time) (rsaKey, error) {
	res, err := st.db.ExecContext(ctx, "INSERT INTO airtel_keys (private_der, valid_upto) VALUES (?, ?)",
		x509.MarshalPKCS1PrivateKey(private), until.UTC().Format(bank.TimeLayout))
	if err != nil {
		return rsaKey{}, err
	}
	id, err := res.LastInsertId()
	return rsaKey{ID: id, Private: private, ValidUpto: until}, err
}

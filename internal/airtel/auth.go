package airtel

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// issueToken accepts a request without client_secret, as Airtel does in practice; a wrong
// secret is refused.
func (s *Service) issueToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientID     string  `json:"client_id"`
		ClientSecret *string `json:"client_secret"`
		GrantType    string  `json:"grant_type"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil ||
		in.GrantType != "client_credentials" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	app, err := s.store.app(r.Context(), in.ClientID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.internal(w, err)
		return
	}
	if err != nil || (in.ClientSecret != nil &&
		subtle.ConstantTimeCompare([]byte(app.ClientSecret), []byte(*in.ClientSecret)) != 1) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	token, err := s.store.addToken(r.Context(), app.ClientID, s.books.Clock().Now().Add(tokenTTL))
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"access_token": token, "expires_in": strconv.Itoa(int(tokenTTL.Seconds())), "token_type": "bearer",
	})
}

// authorized checks the token, then the country and currency headers every call carries.
func (s *Service) authorized(next func(http.ResponseWriter, *http.Request, App)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		clientID, err := s.store.tokenOwner(r.Context(), token, s.books.Clock().Now())
		if !found || errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "invalid_token", "error_description": "The access token is invalid or expired"})
			return
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		switch {
		case r.Header.Get("X-Country") == "" || r.Header.Get("X-Currency") == "":
			refuse(w, http.StatusBadRequest, "ROUTER003", "Missing header/body params")
			return
		case r.Header.Get("X-Country") != country:
			refuse(w, http.StatusBadRequest, "ROUTER006", "Invalid Country")
			return
		case r.Header.Get("X-Currency") != currency:
			refuse(w, http.StatusBadRequest, "ROUTER112", "Invalid Currency")
			return
		}
		app, err := s.store.app(r.Context(), clientID)
		if err != nil {
			s.internal(w, err)
			return
		}
		next(w, r, app)
	}
}

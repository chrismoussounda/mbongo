package mtn

import (
	"net/http"
	"strings"
)

func (s *Service) issueToken(product string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Ocp-Apim-Subscription-Key")
		if p, err := s.store.subscriptionProduct(r.Context(), key); err != nil || p != product {
			apimDenied(w)
			return
		}
		id, secret, ok := r.BasicAuth()
		u, err := s.store.apiUser(r.Context(), id)
		if !ok || err != nil || u.Subscription != key || u.APIKey == "" || u.APIKey != secret {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "login_failed"})
			return
		}
		token, err := s.store.addToken(r.Context(), u.ID, product, s.books.Clock().Now().Add(tokenTTL))
		if err != nil {
			s.internal(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": token, "token_type": "access_token", "expires_in": int(tokenTTL.Seconds()),
		})
	}
}

func (s *Service) authorized(product string, next func(http.ResponseWriter, *http.Request, apiUser)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Ocp-Apim-Subscription-Key")
		if p, err := s.store.subscriptionProduct(r.Context(), key); err != nil || p != product {
			apimDenied(w)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			writeError(w, http.StatusUnauthorized, "INVALID_TOKEN", "Bearer token is missing.")
			return
		}
		user, tokenProduct, expires, err := s.store.token(r.Context(), token)
		if err != nil || tokenProduct != product || !s.books.Clock().Now().Before(expires) {
			writeError(w, http.StatusUnauthorized, "INVALID_TOKEN", "Access token is invalid or expired.")
			return
		}
		u, err := s.store.apiUser(r.Context(), user)
		if err != nil {
			s.internal(w, err)
			return
		}
		if u.Subscription != key {
			apimDenied(w)
			return
		}
		if r.Header.Get("X-Target-Environment") != u.Environment {
			wrongEnvironment(w)
			return
		}
		next(w, r, u)
	}
}

// apimDenied copies the gateway's answer to a missing or wrong subscription key.
func apimDenied(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]any{"statusCode": 401,
		"message": "Access denied due to invalid subscription key. Make sure to provide a valid key for an active subscription."})
}

func wrongEnvironment(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, "NOT_ALLOWED_TARGET_ENVIRONMENT", "Access to target environment is forbidden.")
}

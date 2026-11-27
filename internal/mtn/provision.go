package mtn

import (
	"encoding/json"
	"errors"
	"net/http"
)

func (s *Service) createAPIUser(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Ocp-Apim-Subscription-Key")
	if _, err := s.store.subscriptionProduct(r.Context(), key); err != nil {
		apimDenied(w)
		return
	}
	id := r.Header.Get("X-Reference-Id")
	if !isUUID(id) {
		writeError(w, http.StatusBadRequest, "INVALID_REFERENCE_ID", "X-Reference-Id must be a UUID version 4.")
		return
	}
	var body struct {
		ProviderCallbackHost string `json:"providerCallbackHost"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ProviderCallbackHost == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "providerCallbackHost is required.")
		return
	}
	if _, err := s.store.apiUser(r.Context(), id); err == nil {
		writeError(w, http.StatusConflict, "RESOURCE_ALREADY_EXIST", "Duplicated reference id. Creation of resource failed.")
		return
	}
	if err := s.store.addAPIUser(r.Context(), apiUser{ID: id, Subscription: key,
		CallbackHost: body.ProviderCallbackHost, Environment: Sandbox}); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Service) sandboxUser(w http.ResponseWriter, r *http.Request) (apiUser, bool) {
	key := r.Header.Get("Ocp-Apim-Subscription-Key")
	if _, err := s.store.subscriptionProduct(r.Context(), key); err != nil {
		apimDenied(w)
		return apiUser{}, false
	}
	u, err := s.store.apiUser(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) || (err == nil && (u.Subscription != key || u.Environment != Sandbox)) {
		writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "Requested resource was not found.")
		return apiUser{}, false
	}
	if err != nil {
		s.internal(w, err)
		return apiUser{}, false
	}
	return u, true
}

func (s *Service) getAPIUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.sandboxUser(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"providerCallbackHost": u.CallbackHost, "targetEnvironment": u.Environment})
}

func (s *Service) createAPIKey(w http.ResponseWriter, r *http.Request) {
	u, ok := s.sandboxUser(w, r)
	if !ok {
		return
	}
	key := randomHex()
	if err := s.store.setAPIKey(r.Context(), u.ID, key); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"apiKey": key})
}

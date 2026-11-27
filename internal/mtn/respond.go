package mtn

import (
	"encoding/json"
	"net/http"
	"regexp"
)

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

func (s *Service) internal(w http.ResponseWriter, err error) {
	s.logger.Error("mtn", "err", err)
	writeError(w, http.StatusInternalServerError, "INTERNAL_PROCESSING_ERROR", "An internal error occurred while processing.")
}

var uuidV4 = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool { return uuidV4.MatchString(s) }

func newUUID() string {
	h := []byte(randomHex())
	h[12] = '4'
	h[16] = "89ab"[h[16]%4]
	s := string(h)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

package airtel

import (
	"encoding/json"
	"net/http"
	"strconv"
)

const resultOK = "ESB000010"

type status struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	ResultCode   string `json:"result_code"`
	ResponseCode string `json:"response_code,omitempty"`
	Success      bool   `json:"success"`
}

type envelope struct {
	Data   any    `json:"data"`
	Status status `json:"status"`
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func ok(w http.ResponseWriter, responseCode, message string, data any) {
	writeJSON(w, http.StatusOK, envelope{Data: data, Status: status{Code: "200", Message: message,
		ResultCode: resultOK, ResponseCode: responseCode, Success: true}})
}

// fail is a refusal of the payment itself, as opposed to a broken request.
func fail(w http.ResponseWriter, responseCode, message string, data any) {
	writeJSON(w, http.StatusOK, envelope{Data: data, Status: status{Code: "200", Message: message,
		ResultCode: responseCode, ResponseCode: responseCode, Success: false}})
}

func refuse(w http.ResponseWriter, httpStatus int, code, message string) {
	writeJSON(w, httpStatus, envelope{Data: map[string]any{}, Status: status{Code: strconv.Itoa(httpStatus),
		Message: message, ResultCode: code, ResponseCode: code, Success: false}})
}

func (s *Service) internal(w http.ResponseWriter, err error) {
	s.logger.Error("airtel", "err", err)
	refuse(w, http.StatusInternalServerError, "ESB000001", "Internal error")
}

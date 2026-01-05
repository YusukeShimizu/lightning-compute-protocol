package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func decodeJSONRequest(c *gin.Context, dst any) ([]byte, bool) {
	enc := strings.TrimSpace(c.GetHeader("Content-Encoding"))
	if enc != "" && !strings.EqualFold(enc, contentEncodingIdentity) {
		writeOpenAIError(
			c,
			http.StatusUnsupportedMediaType,
			"invalid_request_error",
			fmt.Sprintf("unsupported Content-Encoding: %q (only %q is supported)", enc, contentEncodingIdentity),
		)
		return nil, false
	}

	body, readErr := readRequestBodyBytes(c)
	if readErr != nil {
		if isRequestBodyTooLarge(readErr) {
			writeOpenAIError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", readErr.Error())
			return nil, false
		}
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", readErr.Error())
		return nil, false
	}

	if err := json.Unmarshal(body, dst); err != nil {
		msg := strings.TrimPrefix(err.Error(), "json: ")
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", msg)
		return nil, false
	}

	return body, true
}

func validateModelField(c *gin.Context, model string) bool {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return false
	}
	if trimmed != model {
		writeOpenAIError(
			c,
			http.StatusBadRequest,
			"invalid_request_error",
			"model must not have leading/trailing whitespace",
		)
		return false
	}
	return true
}

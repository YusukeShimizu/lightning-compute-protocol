package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bruwbird/lcp/apps/openai-serve/internal/openai"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"github.com/gin-gonic/gin"
)

func (s *Server) handleResponses(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		writeOpenAIError(c, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	started := time.Now()

	reqBytes, req, ok := decodeAndValidateResponsesRequest(c)
	if !ok {
		return
	}

	streaming := req.Stream != nil && *req.Stream
	model := strings.TrimSpace(req.Model)
	s.handleOpenAIPassthrough(
		c,
		started,
		lcpdv1.LCPTaskKind_LCP_TASK_KIND_OPENAI_RESPONSES_V1,
		model,
		reqBytes,
		streaming,
		s.buildLCPOpenAIResponsesV1Task,
		"response",
	)
}

func decodeAndValidateResponsesRequest(c *gin.Context) ([]byte, openai.ResponsesRequest, bool) {
	enc := strings.TrimSpace(c.GetHeader("Content-Encoding"))
	if enc != "" && !strings.EqualFold(enc, contentEncodingIdentity) {
		writeOpenAIError(
			c,
			http.StatusUnsupportedMediaType,
			"invalid_request_error",
			fmt.Sprintf("unsupported Content-Encoding: %q (only %q is supported)", enc, contentEncodingIdentity),
		)
		return nil, openai.ResponsesRequest{}, false
	}

	body, readErr := readRequestBodyBytes(c)
	if readErr != nil {
		if isRequestBodyTooLarge(readErr) {
			writeOpenAIError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", readErr.Error())
			return nil, openai.ResponsesRequest{}, false
		}
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", readErr.Error())
		return nil, openai.ResponsesRequest{}, false
	}

	var parsed openai.ResponsesRequest
	if err := json.Unmarshal(body, &parsed); err != nil {
		msg := strings.TrimPrefix(err.Error(), "json: ")
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", msg)
		return nil, openai.ResponsesRequest{}, false
	}

	model := parsed.Model
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, openai.ResponsesRequest{}, false
	}
	if trimmedModel != model {
		writeOpenAIError(
			c,
			http.StatusBadRequest,
			"invalid_request_error",
			"model must not have leading/trailing whitespace",
		)
		return nil, openai.ResponsesRequest{}, false
	}

	input := bytes.TrimSpace(parsed.Input)
	if len(input) == 0 || bytes.Equal(input, []byte("null")) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "input is required")
		return nil, openai.ResponsesRequest{}, false
	}

	return body, parsed, true
}

func (s *Server) buildLCPOpenAIResponsesV1Task(
	c *gin.Context,
	model string,
	reqBytes []byte,
) (*lcpdv1.Task, bool) {
	task, err := buildLCPOpenAIResponsesV1Task(model, reqBytes)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, false
	}
	return task, true
}

func buildLCPOpenAIResponsesV1Task(model string, requestJSON []byte) (*lcpdv1.Task, error) {
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("model is required")
	}
	if len(requestJSON) == 0 {
		return nil, errors.New("request body is required")
	}
	return &lcpdv1.Task{
		Spec: &lcpdv1.Task_OpenaiResponsesV1{
			OpenaiResponsesV1: &lcpdv1.OpenAIResponsesV1TaskSpec{
				RequestJson: requestJSON,
				Params: &lcpdv1.OpenAIResponsesV1Params{
					Model: model,
				},
			},
		},
	}, nil
}

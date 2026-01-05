package httpapi

import (
	"bytes"
	"errors"
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
	var parsed openai.ResponsesRequest
	body, ok := decodeJSONRequest(c, &parsed)
	if !ok {
		return nil, openai.ResponsesRequest{}, false
	}
	if !validateModelField(c, parsed.Model) {
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

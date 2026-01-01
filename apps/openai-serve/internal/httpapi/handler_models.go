package httpapi

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/bruwbird/lcp/apps/openai-serve/internal/openai"
	"github.com/gin-gonic/gin"
)

func (s *Server) handleModels(c *gin.Context) {
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		writeOpenAIError(c, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), s.cfg.TimeoutQuote)
	defer cancel()

	models, err := s.discoverModels(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "models discovery failed", "err", err)
		writeOpenAIError(c, httpStatusFromGRPC(err), "server_error", "failed to list models")
		return
	}

	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	created := time.Now().Unix()
	resp := openai.ModelsResponse{
		Object: "list",
		Data:   make([]openai.ModelInfo, 0, len(ids)),
	}
	for _, id := range ids {
		resp.Data = append(resp.Data, openai.ModelInfo{
			ID:      id,
			Object:  "model",
			Created: created,
			OwnedBy: "lcp",
		})
	}
	writeJSON(c, http.StatusOK, resp)
}

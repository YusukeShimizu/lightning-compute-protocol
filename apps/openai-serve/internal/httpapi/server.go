package httpapi

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/bruwbird/lcp/apps/openai-serve/internal/config"
	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"github.com/gin-gonic/gin"
)

type Server struct {
	cfg  config.Config
	lcpd lcpdv1.LCPDServiceClient
	log  *slog.Logger

	router *gin.Engine
}

func New(cfg config.Config, lcpdClient lcpdv1.LCPDServiceClient, logger *slog.Logger) (*Server, error) {
	if lcpdClient == nil {
		return nil, lcpdClientRequiredError{}
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	s := &Server{
		cfg:  cfg,
		lcpd: lcpdClient,
		log:  logger,
	}
	s.router = s.newRouter()
	return s, nil
}

type lcpdClientRequiredError struct{}

func (lcpdClientRequiredError) Error() string { return "lcpd gRPC client is required" }

func (s *Server) Handler() http.Handler {
	return s.router
}

func (s *Server) newRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(s.requestLoggerMiddleware())

	r.GET("/healthz", s.handleHealthz)
	r.HEAD("/healthz", s.handleHealthz)

	v1 := r.Group("/v1")
	v1.Use(s.authMiddleware())
	v1.GET("/models", s.handleModels)
	v1.HEAD("/models", s.handleModels)
	v1.POST("/chat/completions", s.handleChatCompletions)
	v1.POST("/responses", s.handleResponses)

	return r
}

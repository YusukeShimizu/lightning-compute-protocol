package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) authMiddleware() gin.HandlerFunc {
	if len(s.cfg.APIKeys) == 0 {
		return func(c *gin.Context) {
			c.Next()
		}
	}

	return func(c *gin.Context) {
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			c.Header("WWW-Authenticate", `Bearer realm="openai-serve"`)
			writeOpenAIError(c, http.StatusUnauthorized, "invalid_request_error", "missing bearer token")
			c.Abort()
			return
		}
		if _, tokenOK := s.cfg.APIKeys[token]; !tokenOK {
			c.Header("WWW-Authenticate", `Bearer realm="openai-serve"`)
			writeOpenAIError(c, http.StatusUnauthorized, "invalid_request_error", "invalid api key")
			c.Abort()
			return
		}
		c.Next()
	}
}

func bearerToken(headerValue string) (string, bool) {
	v := strings.TrimSpace(headerValue)
	if v == "" {
		return "", false
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(strings.ToLower(v), strings.ToLower(prefix)) {
		return "", false
	}

	token := strings.TrimSpace(v[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

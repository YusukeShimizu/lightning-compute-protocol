package httpapi

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

const maxRequestBodyBytes int64 = 1 << 20

func readRequestBodyBytes(c *gin.Context) ([]byte, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	b, err := io.ReadAll(c.Request.Body)
	if err != nil {
		if isRequestBodyTooLarge(err) {
			return nil, requestBodyTooLargeError{}
		}
		return nil, err
	}
	return b, nil
}

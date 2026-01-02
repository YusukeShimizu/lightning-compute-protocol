package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const maxRequestBodyBytes int64 = 1 << 20

func decodeJSONBody(c *gin.Context, dst any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)

	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		if isRequestBodyTooLarge(err) {
			return requestBodyTooLargeError{}
		}
		msg := strings.TrimPrefix(err.Error(), "json: ")
		return errors.New(msg)
	}

	var extra any
	if err := dec.Decode(&extra); err == nil {
		return errors.New("unexpected extra JSON content")
	} else if !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing JSON content")
	}

	return nil
}

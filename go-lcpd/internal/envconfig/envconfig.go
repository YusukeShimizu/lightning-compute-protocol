package envconfig

import (
	"os"
	"strconv"
	"strings"
)

func Uint64(name string, def uint64) uint64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}

func Int64(name string, def int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}

func Int(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 0)
	if err != nil {
		return def
	}
	return int(v)
}

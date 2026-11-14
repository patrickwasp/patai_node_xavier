package api

import (
	"strconv"

	"patai/console/internal/auth"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func hashOf(secret string) string { return auth.HashSecret(secret) }

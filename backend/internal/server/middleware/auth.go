package middleware

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

type ctxKey struct{}

func SetAPIKey(c *gin.Context, key *service.APIKey) {
	c.Set("mailbox_api_key", key)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxKey{}, key))
}

func GetAPIKeyFromContext(c *gin.Context) (*service.APIKey, bool) {
	v, ok := c.Get("mailbox_api_key")
	if !ok {
		return nil, false
	}
	key, ok := v.(*service.APIKey)
	return key, ok
}

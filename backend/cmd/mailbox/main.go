package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

func main() {
	cfg := config.LoadFromEnv()
	log.Printf("mailbox spool=%s api-key=%s", cfg.Mailbox.SpoolDir, maskKey(cfg.APIKey))

	mh := handler.NewMailboxHandler(cfg)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	v1 := r.Group("/v1")
	v1.Use(func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(h, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")) != cfg.APIKey {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{"message": "Invalid API key", "type": "authentication_error", "code": "invalid_api_key"},
			})
			return
		}
		middleware.SetAPIKey(c, &service.APIKey{ID: 1, Key: cfg.APIKey, Group: service.Group{Platform: service.PlatformMailbox}})
		c.Next()
	})
	v1.GET("/models", func(c *gin.Context) {
		models := make([]gin.H, 0, len(handler.MailboxModelIDs))
		for _, id := range handler.MailboxModelIDs {
			models = append(models, gin.H{
				"id": id, "type": "model", "display_name": id,
				"created_at": time.Unix(1704067200, 0).UTC().Format(time.RFC3339),
			})
		}
		c.JSON(http.StatusOK, gin.H{"data": models})
	})
	v1.POST("/chat/completions", mh.ChatCompletions)
	v1.POST("/responses", mh.Responses)

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	log.Printf("mailbox mini listening on %s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatal(err)
	}
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "****" + k[len(k)-4:]
}

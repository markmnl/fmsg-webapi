package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	"github.com/markmnl/fmsg-webapi/db"
	"github.com/markmnl/fmsg-webapi/handlers"
	"github.com/markmnl/fmsg-webapi/middleware"
)

func main() {
	// Load .env file if present (ignore error when absent).
	_ = godotenv.Load()

	// Required configuration.
	dataDir := mustEnv("FMSG_DATA_DIR")
	jwtSecret := mustEnv("FMSG_API_JWT_SECRET")
	jwtKey := parseSecret(jwtSecret)

	// Optional configuration with defaults.
	port := envOrDefault("FMSG_API_PORT", "8000")
	idURL := envOrDefault("FMSG_ID_URL", "http://127.0.0.1:8080")
	rateLimit := envOrDefaultInt("FMSG_API_RATE_LIMIT", 10)
	rateBurst := envOrDefaultInt("FMSG_API_RATE_BURST", 20)
	maxDataSize := int64(envOrDefaultInt("FMSG_API_MAX_DATA_SIZE", 10)) * 1024 * 1024
	maxAttachSize := int64(envOrDefaultInt("FMSG_API_MAX_ATTACH_SIZE", 10)) * 1024 * 1024
	maxMsgSize := int64(envOrDefaultInt("FMSG_API_MAX_MSG_SIZE", 20)) * 1024 * 1024

	// Connect to PostgreSQL (uses standard PG* environment variables).
	ctx := context.Background()
	database, err := db.New(ctx, "")
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer database.Close()
	log.Println("connected to PostgreSQL")

	// Initialise JWT middleware.
	jwtMiddleware, err := middleware.SetupJWT(jwtKey, idURL)
	if err != nil {
		log.Fatalf("failed to initialise JWT middleware: %v", err)
	}

	// Create Gin router.
	router := gin.Default()

	// Global rate limiter.
	router.Use(middleware.NewRateLimiter(ctx, float64(rateLimit), rateBurst))

	// Instantiate handlers.
	msgHandler := handlers.NewMessageHandler(database, dataDir, maxDataSize, maxMsgSize)
	attHandler := handlers.NewAttachmentHandler(database, dataDir, maxAttachSize, maxMsgSize)

	// Register routes under /fmsg, all protected by JWT.
	fmsg := router.Group("/fmsg")
	fmsg.Use(jwtMiddleware.MiddlewareFunc())
	{
		fmsg.GET("/wait", msgHandler.Wait)
		fmsg.GET("", msgHandler.List)
		fmsg.POST("", msgHandler.Create)
		fmsg.GET("/:id", msgHandler.Get)
		fmsg.PUT("/:id", msgHandler.Update)
		fmsg.DELETE("/:id", msgHandler.Delete)
		fmsg.POST("/:id/send", msgHandler.Send)
		fmsg.POST("/:id/add-to", msgHandler.AddRecipients)
		fmsg.GET("/:id/data", msgHandler.DownloadData)

		fmsg.POST("/:id/attach", attHandler.Upload)
		fmsg.GET("/:id/attach/:filename", attHandler.Download)
		fmsg.DELETE("/:id/attach/:filename", attHandler.DeleteAttachment)
	}

	log.Printf("fmsg-webapi starting on :%s", port)
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      65 * time.Second, // must exceed /wait max timeout (60s)
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MB
	}
	if err = srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

// mustEnv returns the value of an environment variable or exits if it is unset.
func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

// envOrDefault returns the environment variable value or defaultValue when unset.
func envOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

// envOrDefaultInt returns the environment variable as an int or defaultValue when unset.
// Fatally exits if the value is set but not a valid integer.
func envOrDefaultInt(key string, defaultValue int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Fatalf("environment variable %s must be an integer: %v", key, err)
		}
		return n
	}
	return defaultValue
}

// parseSecret returns the HMAC key bytes for the given secret string.
// If s begins with "base64:" the remainder is base64-decoded; otherwise the
// raw string bytes are used.
func parseSecret(s string) []byte {
	const prefix = "base64:"
	if strings.HasPrefix(s, prefix) {
		b, err := base64.StdEncoding.DecodeString(s[len(prefix):])
		if err != nil {
			log.Fatalf("FMSG_API_JWT_SECRET has base64: prefix but is not valid base64: %v", err)
		}
		return b
	}
	return []byte(s)
}

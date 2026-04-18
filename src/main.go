package main

import (
	"context"
	"encoding/base64"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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
	tlsCert := mustEnv("FMSG_TLS_CERT")
	tlsKey := mustEnv("FMSG_TLS_KEY")

	// Optional configuration with defaults.
	idURL := envOrDefault("FMSG_ID_URL", "http://127.0.0.1:8080")
	acmeDir := envOrDefault("FMSG_ACME_DIR", "/var/www/letsencrypt")

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

	// Instantiate handlers.
	msgHandler := handlers.NewMessageHandler(database, dataDir)
	attHandler := handlers.NewAttachmentHandler(database, dataDir)

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

	// Start HTTP server on port 80 for ACME challenges and HTTPS redirect.
	httpRouter := gin.New()
	httpRouter.Use(gin.Recovery())
	httpRouter.Static("/.well-known/acme-challenge", filepath.Join(acmeDir, ".well-known", "acme-challenge"))
	httpRouter.NoRoute(func(c *gin.Context) {
		target := "https://" + c.Request.Host + c.Request.RequestURI
		c.Redirect(http.StatusMovedPermanently, target)
	})
	go func() {
		if err := http.ListenAndServe(":80", httpRouter); err != nil {
			log.Fatalf("HTTP :80 server error: %v", err)
		}
	}()
	log.Println("listening on :80 (ACME + HTTPS redirect)")

	log.Println("fmsg-webapi starting on :443")
	if err = router.RunTLS(":443", tlsCert, tlsKey); err != nil {
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

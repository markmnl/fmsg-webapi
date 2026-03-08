package main

import (
	"context"
	"log"
	"os"

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

	// Optional configuration with defaults.
	port := envOrDefault("FMSG_API_PORT", "8000")
	idURL := envOrDefault("FMSG_ID_URL", "http://127.0.0.1:8080")

	// Connect to PostgreSQL (uses standard PG* environment variables).
	ctx := context.Background()
	database, err := db.New(ctx, "")
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer database.Close()
	log.Println("connected to PostgreSQL")

	// Initialise JWT middleware.
	jwtMiddleware, err := middleware.SetupJWT(jwtSecret, idURL)
	if err != nil {
		log.Fatalf("failed to initialise JWT middleware: %v", err)
	}

	// Create Gin router.
	router := gin.Default()

	// Instantiate handlers.
	msgHandler := handlers.NewMessageHandler(database, dataDir)
	attHandler := handlers.NewAttachmentHandler(database, dataDir)

	// Register routes under /api/v1, all protected by JWT.
	v1 := router.Group("/api/v1")
	v1.Use(jwtMiddleware.MiddlewareFunc())
	{
		v1.POST("/messages", msgHandler.Create)
		v1.GET("/messages/:id", msgHandler.Get)
		v1.PUT("/messages/:id", msgHandler.Update)
		v1.DELETE("/messages/:id", msgHandler.Delete)
		v1.POST("/messages/:id/send", msgHandler.Send)

		v1.POST("/messages/:id/attachments", attHandler.Upload)
		v1.GET("/messages/:id/attachments/:filename", attHandler.Download)
		v1.DELETE("/messages/:id/attachments/:filename", attHandler.DeleteAttachment)
	}

	log.Printf("fmsg-webapi starting on :%s", port)
	if err = router.Run(":" + port); err != nil {
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

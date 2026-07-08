package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	"github.com/markmnl/fmsg-webapi/internal/apiauth"
	"github.com/markmnl/fmsg-webapi/internal/db"
	"github.com/markmnl/fmsg-webapi/internal/handlers"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

func main() {
	// Load .env file if present (ignore error when absent).
	_ = godotenv.Load()

	if len(os.Args) > 1 && os.Args[1] == "api-key" {
		if err := runAPIKeyCLI(context.Background(), os.Args[2:]); err != nil {
			log.Fatalf("api-key: %v", err)
		}
		return
	}

	// Required configuration.
	dataDir := mustEnv("FMSG_DATA_DIR")

	// JWT configuration. EdDSA provider JWTs and first-party Ed25519 API
	// tokens can be enabled independently.
	jwksURL := os.Getenv("FMSG_JWT_JWKS_URL")
	jwtIssuer := os.Getenv("FMSG_JWT_ISSUER")
	jwtAudience := os.Getenv("FMSG_JWT_AUDIENCE")
	jwtAddressClaim := os.Getenv("FMSG_JWT_ADDRESS_CLAIM")
	apiTokenPrivate := os.Getenv("FMSG_API_TOKEN_ED25519_PRIVATE_KEY")
	apiTokenIssuer := envOrDefault("FMSG_API_TOKEN_ISSUER", apiauth.DefaultTokenIssuer)
	apiTokenAudience := envOrDefault("FMSG_API_TOKEN_AUDIENCE", apiauth.DefaultTokenAudience)
	apiTokenTTL := envOrDefaultDuration("FMSG_API_TOKEN_TTL", apiauth.DefaultTokenTTL)

	// TLS configuration (optional — omit both to run plain HTTP).
	tlsCert := os.Getenv("FMSG_TLS_CERT")
	tlsKey := os.Getenv("FMSG_TLS_KEY")
	tlsEnabled := tlsCert != "" && tlsKey != ""
	if (tlsCert != "") != (tlsKey != "") {
		log.Fatal("FMSG_TLS_CERT and FMSG_TLS_KEY must both be set or both be empty")
	}

	// Optional configuration with defaults.
	idURL := envOrDefault("FMSG_ID_URL", "http://127.0.0.1:8080")
	maxDataSize := int64(envOrDefaultInt("FMSG_API_MAX_DATA_SIZE", 10)) * 1024 * 1024
	maxAttachSize := int64(envOrDefaultInt("FMSG_API_MAX_ATTACH_SIZE", 10)) * 1024 * 1024
	maxMsgSize := int64(envOrDefaultInt("FMSG_API_MAX_MSG_SIZE", 20)) * 1024 * 1024
	shortTextSize := envOrDefaultInt("FMSG_API_SHORT_TEXT_SIZE", 768)

	// CORS: comma-separated list of allowed browser origins, e.g.
	// "https://app.example.com,https://www.example.com". Empty disables CORS.
	corsOrigins := parseCSV(os.Getenv("FMSG_CORS_ORIGINS"))

	// Web Push (VAPID) configuration. Push is enabled only when all three
	// VAPID values are set; otherwise the subscribe routes are not registered
	// and no pushes are sent. Generate the key pair once with
	// `npx web-push generate-vapid-keys`.
	vapidPublic := os.Getenv("FMSG_VAPID_PUBLIC_KEY")
	vapidPrivate := os.Getenv("FMSG_VAPID_PRIVATE_KEY")
	vapidSubject := os.Getenv("FMSG_VAPID_SUBJECT")
	pushIconURL := envOrDefault("FMSG_PUSH_ICON_URL", "/icon-192.png")
	pushEnabled := vapidPublic != "" && vapidPrivate != "" && vapidSubject != ""

	// Connect to PostgreSQL (uses standard PG* environment variables).
	ctx := context.Background()
	database, err := db.New(ctx, "")
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer database.Close()
	log.Println("connected to PostgreSQL")

	apiStore := apiauth.NewStore(database)
	var tokenIssuer *apiauth.TokenIssuer
	if apiTokenPrivate != "" {
		privateKey, err := apiauth.ParseEd25519PrivateKey(apiTokenPrivate)
		if err != nil {
			log.Fatalf("failed to parse FMSG_API_TOKEN_ED25519_PRIVATE_KEY: %v", err)
		}
		tokenIssuer = apiauth.NewTokenIssuer(privateKey, apiTokenIssuer, apiTokenAudience, apiTokenTTL)
		log.Printf("API token auth enabled (issuer=%s, audience=%s, ttl=%s)", tokenIssuer.Issuer(), tokenIssuer.Audience(), tokenIssuer.TTL())
	} else {
		log.Println("API token auth disabled (FMSG_API_TOKEN_ED25519_PRIVATE_KEY not set)")
	}

	// Initialise authentication middleware.
	jwtCfg, err := buildJWTConfig(ctx, jwksURL, jwtIssuer, jwtAudience, jwtAddressClaim, idURL, tokenIssuer, apiStore)
	if err != nil {
		log.Fatalf("failed to configure auth: %v", err)
	}
	jwtMiddleware, err := middleware.New(jwtCfg)
	if err != nil {
		log.Fatalf("failed to initialise auth middleware: %v", err)
	}

	// The WebSocket endpoint authenticates outside the Gin middleware chain,
	// so it needs a directly callable verifier built from the same config.
	jwtVerifier, err := middleware.NewVerifier(jwtCfg)
	if err != nil {
		log.Fatalf("failed to initialise JWT verifier: %v", err)
	}

	// Create Gin router.
	router := gin.Default()
	if trustedProxies := parseCSV(os.Getenv("FMSG_TRUSTED_PROXIES")); len(trustedProxies) > 0 {
		if err := router.SetTrustedProxies(trustedProxies); err != nil {
			log.Fatalf("invalid FMSG_TRUSTED_PROXIES: %v", err)
		}
	} else if err := router.SetTrustedProxies(nil); err != nil {
		log.Fatalf("failed to disable trusted proxies: %v", err)
	}

	// CORS must run before authentication so that browser preflight (OPTIONS)
	// requests, which do not carry the Authorization header, are answered
	// directly instead of being rejected by the JWT middleware.
	if len(corsOrigins) > 0 {
		corsCfg := middleware.DefaultCORSConfig()
		corsCfg.AllowedOrigins = corsOrigins
		router.Use(middleware.NewCORS(corsCfg))
		log.Printf("CORS enabled for origins: %s", strings.Join(corsOrigins, ", "))
	}

	// Global rate limiting is handled by nftables at the host level.

	// Instantiate handlers.
	msgHandler := handlers.NewMessageHandler(database, dataDir, maxDataSize, maxMsgSize, shortTextSize, apiStore)
	attHandler := handlers.NewAttachmentHandler(database, dataDir, maxAttachSize, maxMsgSize)

	// Web Push handler: stores subscriptions and delivers VAPID pushes for
	// new-message events. Only instantiated when VAPID is configured.
	var pushHandler *handlers.PushHandler
	if pushEnabled {
		pushHandler = handlers.NewPushHandler(database, msgHandler, vapidPublic, vapidPrivate, vapidSubject, pushIconURL)
		log.Println("web push enabled")
	} else {
		log.Println("web push disabled (FMSG_VAPID_* not set)")
	}

	// WebSocket hub: a single dedicated PostgreSQL listener fans out
	// new-message events to every connected client, so the number of clients
	// does not consume connection-pool capacity.
	hub := handlers.NewHub(msgHandler)
	if pushHandler != nil {
		// The hub also dispatches a Web Push for every new_msg notification.
		hub.SetPushNotifier(pushHandler.NotifyNewMsg)
	}
	go hub.Run(context.Background())
	wsHandler := handlers.NewWSHandler(jwtVerifier, hub, corsOrigins)

	if tokenIssuer != nil {
		tokenHandler := handlers.NewTokenHandler(apiStore, tokenIssuer, idURL)
		router.POST("/fmsg/token", tokenHandler.Exchange)
	}

	// Register routes under /fmsg, all protected by JWT.
	fmsg := router.Group("/fmsg")
	fmsg.Use(jwtMiddleware)
	{
		if tokenIssuer != nil {
			subAccountHandler := handlers.NewSubAccountHandler(apiStore, idURL)
			fmsg.GET("/sub-accounts", subAccountHandler.List)
			fmsg.POST("/sub-accounts", subAccountHandler.Create)
			fmsg.GET("/sub-accounts/:agent", subAccountHandler.Get)
			fmsg.PATCH("/sub-accounts/:agent", subAccountHandler.UpdateCIDRs)
			fmsg.POST("/sub-accounts/:agent/rotate-key", subAccountHandler.RotateKey)
			fmsg.DELETE("/sub-accounts/:agent", subAccountHandler.Delete)
		}

		fmsg.GET("", msgHandler.List)
		fmsg.GET("/sent", msgHandler.Sent)
		fmsg.POST("", msgHandler.Create)
		fmsg.GET("/:id", msgHandler.Get)
		fmsg.PUT("/:id", msgHandler.Update)
		fmsg.DELETE("/:id", msgHandler.Delete)
		fmsg.POST("/:id/send", msgHandler.Send)
		fmsg.POST("/:id/read", msgHandler.MarkRead)
		fmsg.POST("/:id/add-to", msgHandler.AddRecipients)
		fmsg.GET("/:id/data", msgHandler.DownloadData)

		fmsg.POST("/:id/attach", attHandler.Upload)
		fmsg.GET("/:id/attach/:filename", attHandler.Download)
		fmsg.DELETE("/:id/attach/:filename", attHandler.DeleteAttachment)

		if pushHandler != nil {
			fmsg.POST("/push/subscribe", pushHandler.Subscribe)
			fmsg.DELETE("/push/subscribe", pushHandler.Unsubscribe)
		}
	}

	// The WebSocket endpoint is registered outside the JWT-protected group:
	// browsers cannot set an Authorization header on a WebSocket, so the
	// handler authenticates itself via the access_token query parameter or
	// an Authorization header.
	router.GET("/fmsg/ws", wsHandler.Connect)

	srv := &http.Server{
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      65 * time.Second, // generous, to allow large file transfers
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MB
	}

	if tlsEnabled {
		port := envOrDefault("FMSG_API_PORT", "443")
		srv.Addr = ":" + port
		log.Printf("fmsg-webapi starting on :%s (HTTPS)", port)
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		if err = srv.ListenAndServeTLS(tlsCert, tlsKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	} else {
		port := envOrDefault("FMSG_API_PORT", "8000")
		srv.Addr = ":" + port
		log.Printf("fmsg-webapi starting on :%s (plain HTTP)", port)
		if err = srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
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

// parseCSV splits a comma-separated string into trimmed, non-empty values.
func parseCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
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

func envOrDefaultDuration(key string, defaultValue time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("environment variable %s must be a Go duration such as 12h: %v", key, err)
		}
		return d
	}
	return defaultValue
}

// buildJWTConfig assembles a middleware.Config from environment-derived inputs.
func buildJWTConfig(ctx context.Context, jwksURL, issuer, audience, addressClaim, idURL string, tokenIssuer *apiauth.TokenIssuer, apiStore *apiauth.Store) (middleware.Config, error) {
	cfg := middleware.Config{
		Issuer:       issuer,
		Audience:     audience,
		AddressClaim: addressClaim,
		IDURL:        idURL,
	}

	if jwksURL != "" {
		if issuer == "" || addressClaim == "" {
			return cfg, errors.New("FMSG_JWT_ISSUER and FMSG_JWT_ADDRESS_CLAIM are required when FMSG_JWT_JWKS_URL is set")
		}
		k, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
		if err != nil {
			return cfg, err
		}
		cfg.JWKS = k.Keyfunc
		log.Printf("EdDSA auth enabled (issuer=%s, jwks=%s, audience=%q, address_claim=%s)", issuer, jwksURL, audience, addressClaim)
	} else {
		log.Println("EdDSA auth disabled (FMSG_JWT_JWKS_URL not set)")
	}

	if tokenIssuer != nil {
		cfg.APIPublicKey = tokenIssuer.PublicKey()
		cfg.APIIssuer = tokenIssuer.Issuer()
		cfg.APIAudience = tokenIssuer.Audience()
		cfg.APIKeys = apiStore
	}

	if cfg.JWKS == nil && len(cfg.APIPublicKey) == 0 {
		return cfg, errors.New("either FMSG_JWT_JWKS_URL or FMSG_API_TOKEN_ED25519_PRIVATE_KEY must be set")
	}
	return cfg, nil
}

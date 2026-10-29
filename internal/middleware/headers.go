package middleware

import "github.com/gin-gonic/gin"

// SecurityHeaders sets response headers that every API response should
// carry. nosniff stops a browser from treating a downloaded attachment or
// message body as HTML or script whatever its bytes look like.
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Next()
	}
}

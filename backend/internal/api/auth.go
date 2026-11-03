package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"example.com/go-workerpool-http/internal/auth"
	"example.com/go-workerpool-http/internal/store"
)

type authPrincipalContextKey struct{}

type registerInput struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginInput struct {
	Login    string `json:"login"`
	Username string `json:"username"` // accepted for simple browser clients
	Password string `json:"password"`
}

type refreshInput struct {
	RefreshToken string `json:"refresh_token"`
}

func registerAuthRoutes(router *gin.Engine, service *auth.Service) {
	router.POST("/auth/register", func(c *gin.Context) {
		if service == nil {
			writeAuthDisabled(c)
			return
		}
		var input registerInput
		if err := c.ShouldBindJSON(&input); err != nil {
			writeAuthError(c, http.StatusBadRequest, err)
			return
		}
		tokens, err := service.Register(c.Request.Context(), input.Username, input.Email, input.Password)
		if err != nil {
			writeAuthServiceError(c, err)
			return
		}
		c.JSON(http.StatusCreated, tokens)
	})

	router.POST("/auth/login", func(c *gin.Context) {
		if service == nil {
			writeAuthDisabled(c)
			return
		}
		var input loginInput
		if err := c.ShouldBindJSON(&input); err != nil {
			writeAuthError(c, http.StatusBadRequest, err)
			return
		}
		login := input.Login
		if strings.TrimSpace(login) == "" {
			login = input.Username
		}
		tokens, err := service.Login(c.Request.Context(), login, input.Password, c.Request.UserAgent(), c.ClientIP())
		if err != nil {
			writeAuthServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, tokens)
	})

	router.POST("/auth/refresh", func(c *gin.Context) {
		if service == nil {
			writeAuthDisabled(c)
			return
		}
		var input refreshInput
		if err := c.ShouldBindJSON(&input); err != nil {
			writeAuthError(c, http.StatusBadRequest, err)
			return
		}
		tokens, err := service.Refresh(c.Request.Context(), input.RefreshToken, c.Request.UserAgent(), c.ClientIP())
		if err != nil {
			writeAuthServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, tokens)
	})

	router.POST("/auth/logout", func(c *gin.Context) {
		if service == nil {
			writeAuthDisabled(c)
			return
		}
		var input refreshInput
		if err := c.ShouldBindJSON(&input); err != nil {
			writeAuthError(c, http.StatusBadRequest, err)
			return
		}
		if err := service.Logout(c.Request.Context(), input.RefreshToken); err != nil {
			writeAuthServiceError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	if service != nil {
		router.GET("/auth/me", requireAuthenticated(service), func(c *gin.Context) {
			principal, ok := principalFromRequest(c.Request)
			if !ok {
				writeAuthError(c, http.StatusUnauthorized, auth.ErrInvalidToken)
				return
			}
			user, err := service.User(c.Request.Context(), principal)
			if err != nil {
				writeAuthServiceError(c, err)
				return
			}
			user.PasswordHash = ""
			c.JSON(http.StatusOK, user)
		})
	}
}

func requireAuthenticated(service *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			writeAuthDisabled(c)
			c.Abort()
			return
		}
		token := bearerToken(c.GetHeader("Authorization"))
		principal, err := service.AuthenticateAccessToken(token)
		if err != nil {
			writeAuthError(c, http.StatusUnauthorized, err)
			c.Abort()
			return
		}
		ctx := context.WithValue(c.Request.Context(), authPrincipalContextKey{}, principal)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func requireAccess(apiKey string, service *auth.Service) gin.HandlerFunc {
	if service == nil {
		return requireAPIKey(apiKey)
	}
	return func(c *gin.Context) {
		providedAPIKey := strings.TrimSpace(c.GetHeader("X-API-Key"))
		if providedAPIKey == "" {
			providedAPIKey = strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		}
		if apiKey != "" && subtleConstantCompare(providedAPIKey, apiKey) {
			c.Next()
			return
		}
		principal, err := service.AuthenticateAccessToken(bearerToken(c.GetHeader("Authorization")))
		if err != nil {
			c.Header("WWW-Authenticate", `Bearer realm="go-ai-studio"`)
			writeAuthError(c, http.StatusUnauthorized, err)
			c.Abort()
			return
		}
		ctx := context.WithValue(c.Request.Context(), authPrincipalContextKey{}, principal)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func principalFromRequest(r *http.Request) (auth.Principal, bool) {
	principal, ok := r.Context().Value(authPrincipalContextKey{}).(auth.Principal)
	return principal, ok
}

func bearerToken(value string) string {
	parts := strings.Fields(value)
	if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return parts[1]
	}
	return ""
}

func ownerIDForRequest(c *gin.Context, service *auth.Service, persistenceEnabled bool) (string, error) {
	if principal, ok := principalFromRequest(c.Request); ok {
		return principal.UserID, nil
	}
	if service != nil && persistenceEnabled {
		return "", errors.New("authentication is required when MySQL persistence is enabled")
	}
	return optionalClientID(c.Request, persistenceEnabled)
}

func writeAuthDisabled(c *gin.Context) {
	writeAuthError(c, http.StatusNotImplemented, errors.New("user authentication is disabled; configure MYSQL_DSN and AUTH_JWT_SECRET"))
}

func writeAuthServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrUserExists):
		writeAuthError(c, http.StatusConflict, err)
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrInvalidToken), errors.Is(err, auth.ErrUserInactive),
		errors.Is(err, store.ErrAuthSessionNotFound), errors.Is(err, store.ErrUserNotFound):
		writeAuthError(c, http.StatusUnauthorized, err)
	case errors.Is(err, auth.ErrInvalidInput):
		writeAuthError(c, http.StatusBadRequest, err)
	default:
		writeAuthError(c, http.StatusInternalServerError, err)
	}
}

func writeAuthError(c *gin.Context, status int, err error) {
	c.JSON(status, errorResponse{Error: err.Error(), RequestID: requestID(c.Request)})
}

func subtleConstantCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var result byte
	for i := range a {
		result |= a[i] ^ b[i]
	}
	return result == 0
}

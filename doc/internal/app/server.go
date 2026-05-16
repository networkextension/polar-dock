package app

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"context"
)

type Server struct {
	db          *sql.DB
	redis       *redis.Client
	router      *gin.Engine
	addr        string
	redisPrefix string
}

func NewServer(cfg Config) (*Server, error) {
	db, err := sql.Open("postgres", cfg.PostgresDSN)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPass,
		DB:       cfg.RedisDB,
	})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		_ = db.Close()
		return nil, err
	}

	prefix := cfg.RedisPrefix
	if prefix == "" {
		prefix = DefaultRedisPrefix
	}

	s := &Server{
		db:          db,
		redis:       rdb,
		addr:        cfg.Addr,
		redisPrefix: prefix,
	}

	s.router = gin.Default()
	s.router.Use(corsMiddleware())
	s.registerRoutes()

	return s, nil
}

func (s *Server) Run() error {
	return s.router.Run(s.addr)
}

func (s *Server) Close() error {
	if s.redis != nil {
		_ = s.redis.Close()
	}
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *Server) registerRoutes() {
	api := s.router.Group("/api")
	{
		// Auth
		api.POST("/register", s.handleRegister)
		api.POST("/login", s.handleLogin)
		api.POST("/logout", s.AuthMiddleware(), s.handleLogout)
		api.GET("/me", s.AuthMiddleware(), s.handleMe)

		// PacketTunnel endpoints (authenticated users)
		pt := api.Group("/packtunnel", s.AuthMiddleware())
		{
			pt.GET("/profiles/active", s.handlePacketTunnelActiveProfile)
			pt.GET("/rules", s.handlePacketTunnelRules)
		}

		// Proxy configs
		configs := api.Group("/proxy-configs", s.AuthMiddleware())
		{
			configs.GET("", s.handleProxyConfigList)
			configs.POST("", s.handleProxyConfigCreate)
			configs.GET("/active", s.handleProxyConfigGetActive)
			configs.GET("/:id", s.handleProxyConfigGet)
			configs.PUT("/:id", s.handleProxyConfigUpdate)
			configs.DELETE("/:id", s.handleProxyConfigDelete)
			configs.POST("/:id/activate", s.handleProxyConfigActivate)
		}
	}
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type,Authorization,X-Device-Type,X-Device-Id")
		c.Header("Access-Control-Allow-Credentials", "true")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func jsonError(c *gin.Context, status int, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"error": msg})
}

func jsonOK(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, data)
}

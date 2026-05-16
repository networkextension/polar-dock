package app

import "time"

const (
	SessionCookieName  = "session_id"
	DefaultRedisPrefix = "latch"
	SessionDuration    = 7 * 24 * time.Hour
	DefaultAddr        = ":8080"
)

type Config struct {
	Addr        string
	PostgresDSN string
	RedisAddr   string
	RedisPass   string
	RedisDB     int
	RedisPrefix string
}

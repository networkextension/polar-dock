package app

import "time"

type User struct {
	ID        string
	Username  string
	Email     string
	Password  string
	Role      string
	CreatedAt time.Time
}

type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ProxyConfig struct {
	ID        int64     `json:"id"`
	UserID    string    `json:"user_id"`
	Name      string    `json:"name"`
	Content   string    `json:"content"`
	IsActive  bool      `json:"is_active"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) sessionKey(id string) string {
	return fmt.Sprintf("%s:session:%s", s.redisPrefix, id)
}

func (s *Server) createSession(user *User) (string, error) {
	id := generateID()
	sess := &Session{
		ID:        id,
		UserID:    user.ID,
		Username:  user.Username,
		Role:      user.Role,
		ExpiresAt: time.Now().Add(SessionDuration),
	}
	payload, err := json.Marshal(sess)
	if err != nil {
		return "", err
	}
	return id, s.redis.Set(context.Background(), s.sessionKey(id), payload, SessionDuration).Err()
}

func (s *Server) getSession(id string) *Session {
	payload, err := s.redis.Get(context.Background(), s.sessionKey(id)).Bytes()
	if err != nil {
		return nil
	}
	var sess Session
	if err := json.Unmarshal(payload, &sess); err != nil {
		_ = s.deleteSession(id)
		return nil
	}
	if time.Now().After(sess.ExpiresAt) {
		_ = s.deleteSession(id)
		return nil
	}
	return &sess
}

func (s *Server) deleteSession(id string) error {
	return s.redis.Del(context.Background(), s.sessionKey(id)).Err()
}

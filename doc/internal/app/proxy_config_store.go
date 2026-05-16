package app

import (
	"database/sql"
	"errors"
	"time"
)

func (s *Server) listProxyConfigs(userID string) ([]*ProxyConfig, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, name, content, is_active, version, created_at, updated_at
		FROM proxy_configs
		WHERE user_id = $1
		ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []*ProxyConfig
	for rows.Next() {
		c := &ProxyConfig{}
		if err := rows.Scan(&c.ID, &c.UserID, &c.Name, &c.Content,
			&c.IsActive, &c.Version, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		configs = append(configs, c)
	}
	return configs, rows.Err()
}

func (s *Server) getProxyConfig(id int64, userID string) (*ProxyConfig, error) {
	c := &ProxyConfig{}
	err := s.db.QueryRow(`
		SELECT id, user_id, name, content, is_active, version, created_at, updated_at
		FROM proxy_configs
		WHERE id = $1 AND user_id = $2`, id, userID).
		Scan(&c.ID, &c.UserID, &c.Name, &c.Content,
			&c.IsActive, &c.Version, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (s *Server) getActiveProxyConfig(userID string) (*ProxyConfig, error) {
	c := &ProxyConfig{}
	err := s.db.QueryRow(`
		SELECT id, user_id, name, content, is_active, version, created_at, updated_at
		FROM proxy_configs
		WHERE user_id = $1 AND is_active = true
		LIMIT 1`, userID).
		Scan(&c.ID, &c.UserID, &c.Name, &c.Content,
			&c.IsActive, &c.Version, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (s *Server) createProxyConfig(c *ProxyConfig) error {
	now := time.Now()
	return s.db.QueryRow(`
		INSERT INTO proxy_configs (user_id, name, content, is_active, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, $5, $5)
		RETURNING id, created_at, updated_at`,
		c.UserID, c.Name, c.Content, c.IsActive, now).
		Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt)
}

func (s *Server) updateProxyConfig(c *ProxyConfig) error {
	now := time.Now()
	result, err := s.db.Exec(`
		UPDATE proxy_configs
		SET name = $1, content = $2, version = version + 1, updated_at = $3
		WHERE id = $4 AND user_id = $5`,
		c.Name, c.Content, now, c.ID, c.UserID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return errors.New("not found")
	}
	c.UpdatedAt = now
	return nil
}

func (s *Server) deleteProxyConfig(id int64, userID string) error {
	result, err := s.db.Exec(`
		DELETE FROM proxy_configs WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return errors.New("not found")
	}
	return nil
}

// activateProxyConfig sets the given config as active and deactivates all others for the user.
func (s *Server) activateProxyConfig(id int64, userID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		UPDATE proxy_configs SET is_active = false WHERE user_id = $1`, userID)
	if err != nil {
		return err
	}

	result, err := tx.Exec(`
		UPDATE proxy_configs SET is_active = true, updated_at = $1
		WHERE id = $2 AND user_id = $3`, time.Now(), id, userID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return errors.New("not found")
	}

	return tx.Commit()
}

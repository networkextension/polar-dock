package app

import (
	"database/sql"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var errEmailExists = errors.New("email already exists")

func hashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

func checkPassword(plain, hashed string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hashed), []byte(plain)) == nil
}

func (s *Server) createUser(u *User) error {
	_, err := s.db.Exec(`
		INSERT INTO users (id, username, email, password, role, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		u.ID, u.Username, u.Email, u.Password, u.Role, u.CreatedAt,
	)
	if err != nil && isUniqueViolation(err) {
		return errEmailExists
	}
	return err
}

func (s *Server) getUserByEmail(email string) (*User, error) {
	return s.scanUser(s.db.QueryRow(`
		SELECT id, username, email, password, role, created_at
		FROM users WHERE email = $1`, email))
}

func (s *Server) getUserByID(id string) (*User, error) {
	return s.scanUser(s.db.QueryRow(`
		SELECT id, username, email, password, role, created_at
		FROM users WHERE id = $1`, id))
}

func (s *Server) scanUser(row *sql.Row) (*User, error) {
	u := &User{}
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.Password, &u.Role, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return contains(err.Error(), "unique") || contains(err.Error(), "duplicate")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ensure time pkg used
var _ = time.Now

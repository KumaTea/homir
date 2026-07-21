package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/KumaTea/homir/internal/config"
)

type Authenticator struct {
	username string
	hash     string
	mu       sync.Mutex
	sessions map[string]time.Time
}

func NewAuth(settings config.AdminSettings, bootstrapPassword string) (*Authenticator, error) {
	hash := settings.PasswordHash
	if hash == "" && bootstrapPassword != "" {
		var err error
		hash, err = HashPassword(bootstrapPassword)
		if err != nil {
			return nil, err
		}
	}
	if hash == "" {
		return nil, nil
	}
	username := settings.Username
	if username == "" {
		username = "admin"
	}
	if _, _, err := parseHash(hash); err != nil {
		return nil, fmt.Errorf("invalid admin password_hash: %w", err)
	}
	return &Authenticator{username: username, hash: hash, sessions: make(map[string]time.Time)}, nil
}

func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return "$argon2id$v=19$m=65536,t=3,p=2$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash), nil
}

func (a *Authenticator) Login(username, password string) (string, bool) {
	if subtle.ConstantTimeCompare([]byte(username), []byte(a.username)) != 1 || !verify(a.hash, password) {
		return "", false
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return "", false
	}
	value := base64.RawURLEncoding.EncodeToString(token)
	a.mu.Lock()
	a.sessions[value] = time.Now().Add(12 * time.Hour)
	a.mu.Unlock()
	return value, true
}

func (a *Authenticator) Valid(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	expiry, ok := a.sessions[token]
	if !ok || time.Now().After(expiry) {
		delete(a.sessions, token)
		return false
	}
	return true
}

func verify(encoded, password string) bool {
	salt, expected, err := parseHash(encoded)
	if err != nil {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func parseHash(encoded string) ([]byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=2" {
		return nil, nil, fmt.Errorf("expected Argon2id PHC format")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, err
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, err
	}
	return salt, hash, nil
}

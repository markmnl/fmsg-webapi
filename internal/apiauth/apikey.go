package apiauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	KeyPrefix = "fmsgk"
)

var (
	ErrInvalidAPIKey = errors.New("invalid api key")
)

type APIKey struct {
	ID     string
	Secret string
	Value  string
}

func GenerateAPIKey() (APIKey, error) {
	id, err := randomURLToken(12)
	if err != nil {
		return APIKey{}, err
	}
	secret, err := randomURLToken(32)
	if err != nil {
		return APIKey{}, err
	}
	value := fmt.Sprintf("%s_%s_%s", KeyPrefix, id, secret)
	return APIKey{ID: id, Secret: secret, Value: value}, nil
}

func ParseAPIKey(value string) (APIKey, error) {
	parts := strings.Split(value, "_")
	if len(parts) != 3 || parts[0] != KeyPrefix || parts[1] == "" || parts[2] == "" {
		return APIKey{}, ErrInvalidAPIKey
	}
	return APIKey{ID: parts[1], Secret: parts[2], Value: value}, nil
}

func HashAPIKey(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	out := make([]byte, len(sum))
	copy(out, sum[:])
	return out
}

func APIKeyHashMatches(value string, hash []byte) bool {
	got := HashAPIKey(value)
	return subtle.ConstantTimeCompare(got, hash) == 1
}

func randomURLToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}

package apiauth

import (
	"errors"
	"net/netip"
	"strings"
	"unicode"
)

var ErrInvalidAgent = errors.New("invalid agent")

func ValidateAgent(agent string) error {
	if agent == "" || len(agent) > 64 || strings.Contains(agent, "_") || strings.Contains(agent, "@") {
		return ErrInvalidAgent
	}
	for _, r := range agent {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.' {
			continue
		}
		return ErrInvalidAgent
	}
	return nil
}

func DeriveSubAccountAddr(ownerAddr, agent string) (string, error) {
	if err := ValidateAgent(agent); err != nil {
		return "", err
	}
	if !strings.HasPrefix(ownerAddr, "@") {
		return "", ErrInvalidAgent
	}
	parts := strings.SplitN(ownerAddr[1:], "@", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ErrInvalidAgent
	}
	return "@" + parts[0] + "_" + agent + "@" + parts[1], nil
}

func ValidateCIDRs(cidrs []string) error {
	if len(cidrs) == 0 {
		return errors.New("at least one CIDR is required")
	}
	for _, raw := range cidrs {
		if _, err := netip.ParsePrefix(strings.TrimSpace(raw)); err != nil {
			return err
		}
	}
	return nil
}

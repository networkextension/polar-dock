package dock

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// loadASCConfigPrivateKey decrypts the stored p8 cipher (when the server
// has an IOSDIST_RESOURCE_KEY) and parses it into an ECDSA key. The .p8
// Apple emits is a PEM block of type "PRIVATE KEY" wrapping a PKCS#8
// structure that holds the EC P-256 key.
//
// Extracted from iosdist_asc.go (moved to polar-iosdist) — kept in dock
// because iosdist_agent_handlers.go (also still in dock, depends on
// PolarAgentAuthMiddleware) needs it to forward signing creds to the
// polar-agent worker.
func (s *Server) loadASCConfigPrivateKey(cfg *IOSASCConfig) (*ecdsa.PrivateKey, []byte, error) {
	if cfg == nil {
		return nil, nil, errors.New("asc config nil")
	}
	pemBytes := []byte(cfg.p8Cipher)
	if cfg.P8Encrypted {
		plain, err := s.decryptIOSDistSecret(cfg.p8Cipher)
		if err != nil {
			return nil, nil, fmt.Errorf("decrypt p8: %w", err)
		}
		pemBytes = []byte(plain)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, nil, errors.New("p8: not PEM-encoded")
	}
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if k2, e2 := x509.ParseECPrivateKey(block.Bytes); e2 == nil {
			return k2, pemBytes, nil
		}
		return nil, nil, fmt.Errorf("p8 parse: %w", err)
	}
	ec, ok := priv.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("p8 key is not ECDSA")
	}
	return ec, pemBytes, nil
}

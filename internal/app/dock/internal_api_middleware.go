package dock

// internal_api_middleware.go — Phase 1-A: HMAC verification for the
// Dock /internal/v1/* surface that plugin services call. Wire protocol
// is documented in doc/arch/internal-api-v1.md.
//
// Per request, the plugin sends three headers:
//   X-Polar-Plugin-Name:      <name>
//   X-Polar-Plugin-Timestamp: <unix-seconds-decimal>
//   X-Polar-Plugin-Sig:       <hex hmac-sha256>
//
// Signature is over the canonical string:
//   <METHOD>\n<PATH_WITH_QUERY>\n<TIMESTAMP>\n<HEX_SHA256_OF_BODY>
// Plugin key is the plaintext token whose sha256 is stored in
// plugin_modules.plugin_key_hash. Middleware re-derives the expected
// HMAC from the stored hash? — NO, that won't work: HMAC needs the
// plaintext. So the plugin signs with its plaintext; the middleware
// must also hold the plaintext to verify. Since we only store the
// hash, we instead derive the key as the *hash itself* (treating the
// hash hex as the secret bytes). That keeps Dock's stored-secret
// posture: the row in DB cannot be replayed as the plaintext, only the
// hash, and the hash is what both sides actually use as the HMAC key.
// Plugin-side: PLUGIN_INTERNAL_KEY == hashPluginToken(plaintext_seen_at_creation).
// Doc-side: read plugin_modules.plugin_key_hash → use as []byte.
//
// This deviates from the wire doc (which implies the plaintext is the
// secret). Fix: update doc/arch/internal-api-v1.md to make the
// "secret = sha256(plaintext)" relationship explicit and provide the
// plugin a way to compute it from the plaintext shown at creation.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	hdrPluginName      = "X-Polar-Plugin-Name"
	hdrPluginTimestamp = "X-Polar-Plugin-Timestamp"
	hdrPluginSig       = "X-Polar-Plugin-Sig"
	// pluginSigReplayWindowSecs — accept ±300s skew between plugin
	// clock and Dock clock. Plugins are expected to run NTP.
	pluginSigReplayWindowSecs = 300
	// pluginBodyMaxBytes — guard against pathological payloads. Phase
	// 1-A only mounts /internal/v1/ping (empty body), so this is just
	// a forward defense for 1-B's /llm/chat-completion etc.
	pluginBodyMaxBytes = 1 << 20 // 1 MiB
)

// hmacPluginAuth gates /internal/v1/* routes. Sets c.Set("plugin_name", ...)
// on success so downstream handlers can attribute requests.
func (s *Server) hmacPluginAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		name := strings.TrimSpace(c.GetHeader(hdrPluginName))
		tsStr := strings.TrimSpace(c.GetHeader(hdrPluginTimestamp))
		sigHex := strings.TrimSpace(c.GetHeader(hdrPluginSig))
		if name == "" || tsStr == "" || sigHex == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "plugin auth: missing headers"})
			c.Abort()
			return
		}
		tsUnix, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "plugin auth: bad timestamp"})
			c.Abort()
			return
		}
		now := time.Now().Unix()
		if tsUnix > now+pluginSigReplayWindowSecs || tsUnix < now-pluginSigReplayWindowSecs {
			c.JSON(http.StatusRequestTimeout, gin.H{"error": "plugin auth: timestamp skew"})
			c.Abort()
			return
		}

		row, keyHash, err := s.getPluginModuleByName(name)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin auth: lookup failed"})
			c.Abort()
			return
		}
		if row == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "plugin auth: unknown plugin"})
			c.Abort()
			return
		}
		if !row.Enabled {
			c.JSON(http.StatusForbidden, gin.H{"error": "plugin auth: plugin disabled"})
			c.Abort()
			return
		}

		// Read body with size cap. We re-attach it so the handler can
		// still read it. Without this, c.ShouldBindJSON would see EOF.
		var bodyBytes []byte
		if c.Request.Body != nil {
			lr := io.LimitReader(c.Request.Body, pluginBodyMaxBytes+1)
			b, err := io.ReadAll(lr)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "plugin auth: body read failed"})
				c.Abort()
				return
			}
			if len(b) > pluginBodyMaxBytes {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "plugin auth: body too large"})
				c.Abort()
				return
			}
			bodyBytes = b
			c.Request.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
		}

		expected := computePluginSig(keyHash,
			c.Request.Method,
			c.Request.URL.RequestURI(),
			tsStr,
			bodyBytes)

		got, err := hex.DecodeString(sigHex)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "plugin auth: bad signature encoding"})
			c.Abort()
			return
		}
		if !hmac.Equal(got, expected) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "plugin auth: signature mismatch"})
			c.Abort()
			return
		}
		c.Set("plugin_name", row.Name)
		c.Next()
	}
}

// computePluginSig builds the canonical string + returns the HMAC-SHA256
// raw bytes. Exported as lower-case (package-internal) but exported for
// tests in the same package.
//
// keyHash: the stored plugin_modules.plugin_key_hash — used directly as
//          the HMAC key (the "secret" both sides agreed on).
// method:  HTTP method, uppercased by caller.
// path:    URI including query string (RequestURI()).
// tsStr:   the same timestamp string the client sent.
// body:    raw request body (may be nil/empty).
func computePluginSig(keyHash, method, path, tsStr string, body []byte) []byte {
	bodySum := sha256.Sum256(body) // works on nil/empty body too
	canonical := strings.ToUpper(method) + "\n" + path + "\n" + tsStr + "\n" + hex.EncodeToString(bodySum[:])
	m := hmac.New(sha256.New, []byte(keyHash))
	m.Write([]byte(canonical))
	return m.Sum(nil)
}

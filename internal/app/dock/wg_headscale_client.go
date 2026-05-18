package dock

// Phase 11 C3 — Polar dock calls embedded Headscale's gRPC over its
// unix socket to mint matching PreAuthKeys when admins create
// wg-mac tokens. Net effect: one admin click in /wg-tokens.html
// produces both:
//   - polar_wg_<32 hex>  → wg-mac native client
//   - tskey-<...>         → stock Tailscale client
//
// Distributed together in the plaintext-once modal. Either client
// (or both) can join the same mesh with the operator-chosen role.
//
// Failure mode: if headscale is disabled OR the unix socket isn't
// ready OR gRPC fails, mintTailscaleAuthKey returns ("", err) and
// the caller proceeds with wg-mac-only. Polar's wg_tokens row is
// the source of truth either way.

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	v1 "github.com/juanfont/headscale/gen/go/headscale/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// headscaleClient is a lazy-dialed wrapper around the embedded
// headscale's gRPC unix socket. The same conn is reused across
// calls (gRPC is multiplexed) — no need to redial per request.
type headscaleClient struct {
	mu     sync.Mutex
	conn   *grpc.ClientConn
	client v1.HeadscaleServiceClient
}

var headscaleClientSingleton = &headscaleClient{}

// connect lazy-dials the unix socket. Caller holds c.mu.
func (c *headscaleClient) connect(socketPath string) error {
	if c.client != nil {
		return nil
	}
	if strings.TrimSpace(socketPath) == "" {
		return fmt.Errorf("headscale socket path empty (server boot didn't stash one)")
	}
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: 2 * time.Second}
			return d.DialContext(ctx, "unix", strings.TrimPrefix(addr, "unix://"))
		}),
	)
	if err != nil {
		return fmt.Errorf("grpc.NewClient unix://%s: %w", socketPath, err)
	}
	c.conn = conn
	c.client = v1.NewHeadscaleServiceClient(conn)
	return nil
}

// ensureUser looks up a headscale User by name; creates it if missing.
// Returns the numeric user ID needed by CreatePreAuthKey.
func (c *headscaleClient) ensureUser(ctx context.Context, name string) (uint64, error) {
	list, err := c.client.ListUsers(ctx, &v1.ListUsersRequest{Name: name})
	if err != nil {
		return 0, fmt.Errorf("ListUsers(%q): %w", name, err)
	}
	for _, u := range list.GetUsers() {
		if u.GetName() == name {
			return u.GetId(), nil
		}
	}
	resp, err := c.client.CreateUser(ctx, &v1.CreateUserRequest{Name: name})
	if err != nil {
		return 0, fmt.Errorf("CreateUser(%q): %w", name, err)
	}
	return resp.GetUser().GetId(), nil
}

// mintTailscaleAuthKey is the C3 entry point called by the wg-tokens
// admin handler. Returns the plaintext tskey-... string for the
// operator to copy into `tailscale up --authkey=…`.
//
// Empty userName falls back to "polar"; allows device tokens that
// aren't tied to a specific hub to still get a key.
//
// role: "hub" → AclTags=["tag:hub"]; "device" → no tags.
// ttlDays <= 0 → non-expiring (Tailscale clients then never need
// rotation, matching Polar's wg-mac default for ttl=0).
func (s *Server) mintTailscaleAuthKey(ctx context.Context, userName, role string, reusable, ephemeral bool, ttlDays int) (string, error) {
	if s.headscaleSocketPath == "" {
		// Headscale wasn't enabled at boot. C3 path is a silent no-op.
		return "", nil
	}
	c := headscaleClientSingleton
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connect(s.headscaleSocketPath); err != nil {
		return "", err
	}

	name := strings.TrimSpace(userName)
	if name == "" {
		name = "polar"
	}
	uid, err := c.ensureUser(ctx, name)
	if err != nil {
		return "", err
	}

	req := &v1.CreatePreAuthKeyRequest{
		User:      uid,
		Reusable:  reusable,
		Ephemeral: ephemeral,
	}
	if ttlDays > 0 {
		exp := time.Now().UTC().Add(time.Duration(ttlDays) * 24 * time.Hour)
		req.Expiration = timestamppb.New(exp)
	}
	if strings.EqualFold(strings.TrimSpace(role), "hub") {
		req.AclTags = []string{"tag:hub"}
	}

	resp, err := c.client.CreatePreAuthKey(ctx, req)
	if err != nil {
		return "", fmt.Errorf("CreatePreAuthKey: %w", err)
	}
	return resp.GetPreAuthKey().GetKey(), nil
}

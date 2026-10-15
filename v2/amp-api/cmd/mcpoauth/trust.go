package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	dexapi "github.com/dexidp/dex/api/v2"
)

// crossClientScopePrefix is Dex's cross-client trust scope. A client that
// asks for "audience:server:client_id:<peer>" gets <peer> added to its
// tokens' aud claim — but only if <peer>'s TrustedPeers lists the client.
// Dex re-checks that trust at every token issuance, refreshes included.
const crossClientScopePrefix = "audience:server:client_id:"

// trustManager keeps the MCP audience client in Dex and its TrustedPeers
// list in step with the dynamically registered MCP OAuth clients.
//
// Why this is a read-modify-write loop instead of a single call: Dex's
// gRPC UpdateClient replaces TrustedPeers wholesale, and amp-mcpoauth runs
// more than one replica, so two concurrent registrations on different pods
// can each overwrite the other's addition. ensureTrusted therefore re-reads
// after writing and retries until its client is present, and re-checks once
// more shortly afterwards to catch an overwrite that landed after its own
// verification. /authorize also calls ensureTrusted for every login, so a
// lost update is repaired the next time that client signs in.
type trustManager struct {
	dex        dexapi.DexClient
	audienceID string

	mu sync.Mutex // serialises read-modify-write within this replica

	// recheckDelay is how long to wait before the post-write re-verify.
	// Zero disables it (tests).
	recheckDelay time.Duration
	maxAttempts  int
}

func newTrustManager(dex dexapi.DexClient, audienceID string) *trustManager {
	return &trustManager{dex: dex, audienceID: audienceID, recheckDelay: 3 * time.Second, maxAttempts: 8}
}

// audienceScope is the scope a client must request to get audienceID in aud.
func (t *trustManager) audienceScope() string {
	return crossClientScopePrefix + t.audienceID
}

// ensureAudienceClient creates the audience client in Dex if it doesn't
// exist. It is a confidential client with a random secret nobody holds and
// no redirect URIs, so it can't be used to log in — it exists only to be
// the "aud" of MCP tokens and to hold the TrustedPeers list. It must live in
// Dex storage (not staticClients) because Dex refuses to update static
// clients through the API.
func (t *trustManager) ensureAudienceClient(ctx context.Context) error {
	got, err := t.dex.GetClient(ctx, &dexapi.GetClientReq{Id: t.audienceID})
	if err == nil && got.GetClient() != nil {
		return nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	resp, err := t.dex.CreateClient(ctx, &dexapi.CreateClientReq{Client: &dexapi.Client{
		Id:     t.audienceID,
		Secret: hex.EncodeToString(secret),
		Public: false,
		Name:   "AMP MCP resource (token audience)",
	}})
	if err != nil {
		return fmt.Errorf("create audience client %q: %w", t.audienceID, err)
	}
	if resp.GetAlreadyExists() {
		return nil // another replica won the race
	}
	slog.Info("created MCP audience client in dex", "client_id", t.audienceID)
	return nil
}

// runEnsureAudienceClient retries ensureAudienceClient until it succeeds or
// ctx ends, so a Dex that comes up after the broker doesn't wedge startup.
func (t *trustManager) runEnsureAudienceClient(ctx context.Context) {
	backoff := time.Second
	for {
		err := t.ensureAudienceClient(ctx)
		if err == nil {
			return
		}
		slog.Warn("ensure MCP audience client failed, retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// isTrusted reports whether clientID is in the audience client's TrustedPeers.
func (t *trustManager) isTrusted(ctx context.Context, clientID string) (bool, error) {
	got, err := t.dex.GetClient(ctx, &dexapi.GetClientReq{Id: t.audienceID})
	if err != nil {
		return false, err
	}
	return slices.Contains(got.GetClient().GetTrustedPeers(), clientID), nil
}

// ensureTrusted adds clientID to the audience client's TrustedPeers and
// verifies it stuck. See the type comment for why it loops.
func (t *trustManager) ensureTrusted(ctx context.Context, clientID string) error {
	if err := t.ensureTrustedOnce(ctx, clientID); err != nil {
		return err
	}
	if t.recheckDelay > 0 {
		go func() {
			time.Sleep(t.recheckDelay)
			rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := t.ensureTrustedOnce(rctx, clientID); err != nil {
				slog.Error("re-verify of MCP client trust failed", "client_id", clientID, "err", err)
			}
		}()
	}
	return nil
}

func (t *trustManager) ensureTrustedOnce(ctx context.Context, clientID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	for attempt := 1; attempt <= t.maxAttempts; attempt++ {
		got, err := t.dex.GetClient(ctx, &dexapi.GetClientReq{Id: t.audienceID})
		if err != nil {
			if attempt == 1 {
				// Audience client may not exist yet (fresh install, Dex just
				// came up); try to create it once inline.
				if cerr := t.ensureAudienceClient(ctx); cerr == nil {
					continue
				}
			}
			return fmt.Errorf("get audience client: %w", err)
		}
		peers := got.GetClient().GetTrustedPeers()
		if slices.Contains(peers, clientID) {
			return nil
		}
		next := append(slices.Clone(peers), clientID)
		resp, err := t.dex.UpdateClient(ctx, &dexapi.UpdateClientReq{Id: t.audienceID, TrustedPeers: next})
		if err != nil {
			return fmt.Errorf("update audience client trusted peers: %w", err)
		}
		if resp.GetNotFound() {
			return fmt.Errorf("audience client %q not found in dex", t.audienceID)
		}
		// Loop to re-read: confirms the write, and repairs it if another
		// replica's concurrent write replaced ours.
		if attempt > 1 {
			time.Sleep(time.Duration(attempt*25) * time.Millisecond)
		}
	}
	return fmt.Errorf("client %q still not in %q trusted peers after %d attempts", clientID, t.audienceID, t.maxAttempts)
}

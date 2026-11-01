// Package relayctl is the client side of the remote control relay's internal
// control API (epic 165, #2387): what a process that is not the relay needs to
// tell the relay replica holding a session that the session ended.
//
// The relay runs as its own service, amp-relay (cmd/relay). amp-api serves no
// relay routes and doesn't link the relay (internal/relay); it only ends
// sessions in the database (an org admin turning remote control off for the
// org) and then calls POST /internal/relay/ctl/end on the holder's internal
// listener, with the relay cluster token, so the holder closes the streams at
// once.
//
// The relay cluster token (#2392) is the relay's own: RELAY_CLUSTER_TOKEN
// (the Helm chart's Secret amp-relay-cluster-token, mounted in amp-relay and
// amp-api), else a cluster_settings row of its own (relay_token). It is never
// amp-api's cluster token, so a relay that leaks it exposes only the relay's
// internal listener, and amp-api's actors refuse it.
// If no relay replica is running, there's nobody to tell: a session can't be
// live without one. If the call fails, the holder's own sweep (every 5 s)
// finds the session ended in the database and closes it anyway.
package relayctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/simstech/amp-api/internal/domain"
	"github.com/simstech/amp-api/internal/repository"
)

const (
	// InternalPrefix is where the relay's internal listener serves forwarded
	// requests and control calls.
	InternalPrefix = "/internal/relay"
	// TokenEnv names the relay cluster token's environment variable.
	TokenEnv = "RELAY_CLUSTER_TOKEN"
	// MinTokenLen is the shortest relay cluster token accepted from TokenEnv.
	MinTokenLen = 32
	// TokenHeader carries the cluster token on every internal call.
	TokenHeader = "X-AMP-Cluster-Token"
	// CallTimeout bounds one control call.
	CallTimeout = 3 * time.Second
	// DefaultTTL is how long after its last heartbeat a replica counts as
	// gone (cluster.Options' default).
	DefaultTTL = 10 * time.Second
)

// ConfiguredToken is the relay cluster token from the environment (TokenEnv),
// or "" when it isn't set. A token shorter than MinTokenLen is an error.
func ConfiguredToken() (string, error) {
	tok := strings.TrimSpace(os.Getenv(TokenEnv))
	if tok != "" && len(tok) < MinTokenLen {
		return "", fmt.Errorf("%s is too short: use at least %d random characters", TokenEnv, MinTokenLen)
	}
	return tok, nil
}

// Token is the relay cluster token: ConfiguredToken, else the relay's own
// cluster_settings row (repository.RelayClusterToken), created on first use.
// It is never amp-api's cluster token.
func Token(ctx context.Context, repo *repository.Repo) (string, error) {
	tok, err := ConfiguredToken()
	if err != nil || tok != "" {
		return tok, err
	}
	return repo.RelayClusterToken(ctx)
}

// EndNote names a session that ended, and why.
type EndNote struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Notes groups ended sessions by the replica holding them, leaving out the
// ones held by skip (the caller itself) and those with no holder.
func Notes(ended []domain.RemoteSession, skip string) map[string][]EndNote {
	out := map[string][]EndNote{}
	for i := range ended {
		h := ended[i].HolderNode
		if h == nil || *h == "" || *h == skip {
			continue
		}
		reason := domain.RemoteEndHarnessEnded
		if ended[i].EndReason != nil {
			reason = *ended[i].EndReason
		}
		out[*h] = append(out[*h], EndNote{ID: ended[i].ID, Reason: reason})
	}
	return out
}

// Call makes a control call (path under /internal/relay/ctl/) to the replica
// at addr with the relay cluster token; out, if set, receives the JSON reply.
func Call(ctx context.Context, client *http.Client, token, addr, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+InternalPrefix+"/ctl/"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set(TokenHeader, token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("replica %s: %s %s: %d", addr, method, path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// TellEnded tells the replica node that these sessions it holds ended. A
// replica that's gone (no heartbeat within ttl) holds nothing and isn't
// called; a failed call is logged and left to the holder's own sweep.
func TellEnded(ctx context.Context, repo *repository.Repo, client *http.Client, token string, ttl time.Duration, node string, notes []EndNote) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	addr, alive, err := repo.NodeAddr(ctx, node, ttl)
	if err != nil || !alive || addr == "" {
		return
	}
	if err := Call(ctx, client, token, addr, http.MethodPost, "end", map[string]any{"sessions": notes}, nil); err != nil {
		slog.Warn("telling the relay a remote session ended failed; its sweep will catch it", "holder", node, "err", err)
	}
}

// EndNotifier is amp-api's side of ending sessions: Notify is set as
// service.Service.OnRemoteEnded. It never blocks the caller.
type EndNotifier struct {
	repo   *repository.Repo
	client *http.Client
	ttl    time.Duration

	mu    sync.Mutex
	token string
}

// NewEndNotifier tells relay replicas (found through repo's api_nodes) about
// ended sessions. ttl is the cluster heartbeat TTL (0: DefaultTTL).
func NewEndNotifier(repo *repository.Repo, ttl time.Duration) *EndNotifier {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	d := &net.Dialer{Timeout: CallTimeout}
	return &EndNotifier{repo: repo, ttl: ttl, client: &http.Client{Transport: &http.Transport{DialContext: d.DialContext}}}
}

func (n *EndNotifier) clusterToken(ctx context.Context) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.token != "" {
		return n.token, nil
	}
	tok, err := Token(ctx, n.repo)
	if err != nil {
		return "", err
	}
	n.token = tok
	return tok, nil
}

// Notify sends the ended sessions to their holders in the background.
func (n *EndNotifier) Notify(ended []domain.RemoteSession) {
	byNode := Notes(ended, "")
	if len(byNode) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*CallTimeout)
		defer cancel()
		tok, err := n.clusterToken(ctx)
		if err != nil {
			slog.Warn("reading the relay cluster token failed; the relay's sweep will close ended sessions", "err", err)
			return
		}
		var wg sync.WaitGroup
		for node, notes := range byNode {
			wg.Add(1)
			go func(node string, notes []EndNote) {
				defer wg.Done()
				TellEnded(ctx, n.repo, n.client, tok, n.ttl, node, notes)
			}(node, notes)
		}
		wg.Wait()
	}()
}

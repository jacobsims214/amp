// Package dexclient wraps Dex's gRPC admin API (password CRUD, dynamic
// client registration) behind mTLS. Shared by amp-authadmin (password CRUD
// for the Users admin page) and amp-mcp-oauth (dynamic client registration
// for MCP OAuth clients).
package dexclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	dexapi "github.com/dexidp/dex/api/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Config controls how the client connects to Dex's gRPC listener.
type Config struct {
	Addr       string // host:port, e.g. dex-grpc.amp.svc.cluster.local:5557
	CACertFile string // CA that signed Dex's gRPC server cert (mTLS)
	CertFile   string // this service's client cert
	KeyFile    string // this service's client key
	Insecure   bool   // dev-only: skip TLS entirely (plaintext grpc)
}

// ConfigFromEnv reads DEX_GRPC_ADDR / DEX_GRPC_CA_CERT / DEX_GRPC_CLIENT_CERT /
// DEX_GRPC_CLIENT_KEY / DEX_GRPC_INSECURE.
func ConfigFromEnv() Config {
	return Config{
		Addr:       os.Getenv("DEX_GRPC_ADDR"),
		CACertFile: os.Getenv("DEX_GRPC_CA_CERT"),
		CertFile:   os.Getenv("DEX_GRPC_CLIENT_CERT"),
		KeyFile:    os.Getenv("DEX_GRPC_CLIENT_KEY"),
		Insecure:   os.Getenv("DEX_GRPC_INSECURE") == "true",
	}
}

// New dials Dex's gRPC API and returns a ready-to-use client.
func New(cfg Config) (dexapi.DexClient, *grpc.ClientConn, error) {
	if cfg.Addr == "" {
		return nil, nil, fmt.Errorf("dexclient: DEX_GRPC_ADDR is required")
	}

	var creds credentials.TransportCredentials
	if cfg.Insecure {
		creds = insecure.NewCredentials()
	} else {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("load client cert/key: %w", err)
		}
		caPEM, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, nil, fmt.Errorf("failed to parse CA cert %s", cfg.CACertFile)
		}
		creds = credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
		})
	}

	conn, err := grpc.NewClient(cfg.Addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, nil, fmt.Errorf("dial dex grpc at %s: %w", cfg.Addr, err)
	}
	return dexapi.NewDexClient(conn), conn, nil
}

// Ping is a cheap way to confirm connectivity/auth on startup.
func Ping(ctx context.Context, client dexapi.DexClient) error {
	_, err := client.ListPasswords(ctx, &dexapi.ListPasswordReq{})
	return err
}

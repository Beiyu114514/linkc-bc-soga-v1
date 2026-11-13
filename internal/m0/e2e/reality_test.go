package e2e

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/engine"
	"golang.org/x/crypto/curve25519"
)

func TestVLESSRealityVision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// Camouflage target: a real TLS server for the REALITY serverName.
	certFile, keyFile, _ := makeCert(t, "www.example.com")
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	target, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(io.Discard, c); c.Close() }()
		}
	}()

	// REALITY key pair.
	var priv [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		t.Fatal(err)
	}
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	privB64 := base64.RawURLEncoding.EncodeToString(priv[:])
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	shortID := "0123456789abcdef"

	nodePort := freePort(t)
	e, err := engine.Start(ctx, engine.NodeConfig{
		Listen: "0.0.0.0", Port: nodePort, StreamType: "tcp",
		TLSType: "reality", Flow: "xtls-rprx-vision",
		ServerNames:  []string{"www.example.com"},
		PrivateKey:   privB64,
		ShortIDs:     []string{shortID},
		Dest:         target.Addr().String(),
		AllowPrivate: true,
	}, []engine.User{{ID: 1, UUID: testUUID, SpeedLimit: 4}})
	if err != nil {
		t.Fatalf("start reality node: %v", err)
	}
	defer e.Close()
	time.Sleep(200 * time.Millisecond)

	socksPort := freePort(t)
	client, err := engine.StartClient(ctx, engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: nodePort, UUID: testUUID,
		Flow:    "xtls-rprx-vision",
		Reality: true, PublicKey: pubB64, ShortID: shortID,
		ServerName: "www.example.com", Fingerprint: "chrome",
	})
	if err != nil {
		t.Fatalf("start reality client: %v", err)
	}
	defer client.Close()
	time.Sleep(200 * time.Millisecond)

	payload := make([]byte, 512*1024)
	targetHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer targetHTTP.Close()
	start := time.Now()
	resp, err := socksHTTPClient(t, socksPort).Get(targetHTTP.URL)
	if err != nil {
		t.Fatalf("http over vless+reality: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)
	if len(body) != len(payload) {
		t.Fatalf("body len = %d", len(body))
	}
	if elapsed < 400*time.Millisecond {
		t.Fatalf("reality/vision rate limit bypassed: 512 KiB in %v", elapsed)
	}
	t.Logf("reality+vision 512 KiB in %v with 4 Mbps limit", elapsed)
	_ = fmt.Sprintf("%v", target.Addr())
}

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const consoleTokenTTL = 90 * time.Second

type consoleTarget struct {
	network string
	addr    string
	domain  string
	expires time.Time
}

// consoleTokens maps short-lived random tokens to VNC endpoints so the
// browser never learns the real host:port and every console session is
// tied to an authenticated API call.
type consoleTokens struct {
	mu sync.Mutex
	m  map[string]consoleTarget
}

func newConsoleTokens() *consoleTokens { return &consoleTokens{m: map[string]consoleTarget{}} }

func (c *consoleTokens) issue(network, addr, domain string) string {
	tok := randomHex(24)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, t := range c.m {
		if now.After(t.expires) {
			delete(c.m, k)
		}
	}
	c.m[tok] = consoleTarget{network: network, addr: addr, domain: domain, expires: now.Add(consoleTokenTTL)}
	return tok
}

func (c *consoleTokens) lookup(tok string) (consoleTarget, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[tok]
	if !ok || time.Now().After(t.expires) {
		delete(c.m, tok)
		return consoleTarget{}, false
	}
	return t, true
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	Subprotocols:    []string{"binary"},
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	},
}

// handleVNCWebSocket is a websockify-compatible endpoint: it accepts a
// WebSocket connection and pipes binary frames to/from the VNC TCP (or unix)
// socket of the target domain.
func (s *Server) handleVNCWebSocket(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(r) {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	tok := r.PathValue("token")
	target, ok := s.tokens.lookup(tok)
	if !ok {
		http.Error(w, "invalid or expired console token", http.StatusForbidden)
		return
	}

	backend, err := net.DialTimeout(target.network, target.addr, 5*time.Second)
	if err != nil {
		log.Printf("console %s: cannot connect to %s %s: %v", target.domain, target.network, target.addr, err)
		http.Error(w, "cannot connect to VNC server: "+err.Error(), http.StatusBadGateway)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		backend.Close()
		log.Printf("console %s: websocket upgrade failed: %v", target.domain, err)
		return
	}
	log.Printf("console %s: %s -> %s", target.domain, r.RemoteAddr, target.addr)
	defer log.Printf("console %s: closed (%s)", target.domain, r.RemoteAddr)

	done := make(chan struct{}, 2)

	// browser -> VNC
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage && mt != websocket.TextMessage {
				continue
			}
			if _, err := backend.Write(data); err != nil {
				return
			}
		}
	}()

	// VNC -> browser
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := backend.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					log.Printf("console %s: backend read: %v", target.domain, err)
				}
				return
			}
		}
	}()

	<-done
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	backend.Close()
	ws.Close()
	<-done
}

// writeWebsockifyToken creates a token file for an external websockify
// started with `--token-plugin TokenFile --token-source <dir>`.
func (s *Server) writeWebsockifyToken(tok, addr string) error {
	if !strings.Contains(addr, ":") {
		return fmt.Errorf("external websockify only supports TCP targets")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	path := filepath.Join(s.cfg.WebsockifyTokenDir, tok)
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%s: %s:%s\n", tok, host, port)), 0o640); err != nil {
		return err
	}
	time.AfterFunc(consoleTokenTTL, func() { _ = os.Remove(path) })
	return nil
}

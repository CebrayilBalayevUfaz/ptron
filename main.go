// Puppetron – a small web UI for KVM/QEMU virtual machines managed by libvirt.
//
// It shells out to `virsh` for all hypervisor operations, serves an embedded
// HTML/JS frontend, and provides a websockify-compatible WebSocket→VNC proxy so
// that noVNC can attach to guest consoles from the browser.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Config holds every runtime option. Each flag has an environment-variable
// fallback (PUPPETRON_<NAME>) so the binary is easy to drive from systemd.
type Config struct {
	Listen             string
	ConnectURI         string
	VirshBin           string
	VirtInstallBin     string
	NoVNCDir           string
	User               string
	Password           string
	TLSCert            string
	TLSKey             string
	WebsockifyURL      string // if set, use an external websockify instead of the built-in proxy
	WebsockifyTokenDir string // token directory for websockify --token-plugin TokenFile
	ISODir             string
	EnableCreate       bool
	AllowUndefine      bool
	AllowXMLEdit       bool
	SessionTTL         time.Duration
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv("PUPPETRON_" + key); ok {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv("PUPPETRON_" + key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func loadConfig() *Config {
	cfg := &Config{}
	flag.StringVar(&cfg.Listen, "listen", envOr("LISTEN", ":8080"), "address to listen on")
	flag.StringVar(&cfg.ConnectURI, "connect", envOr("CONNECT", "qemu:///system"), "libvirt connection URI")
	flag.StringVar(&cfg.VirshBin, "virsh", envOr("VIRSH", "virsh"), "path to the virsh binary")
	flag.StringVar(&cfg.VirtInstallBin, "virt-install", envOr("VIRT_INSTALL", "virt-install"), "path to the virt-install binary (used for VM creation)")
	flag.StringVar(&cfg.NoVNCDir, "novnc", envOr("NOVNC", ""), "directory containing noVNC (auto-detected if empty)")
	flag.StringVar(&cfg.User, "user", envOr("USER", "admin"), "web UI username")
	flag.StringVar(&cfg.Password, "password", envOr("PASSWORD", ""), "web UI password (random one is generated and logged if empty)")
	flag.StringVar(&cfg.TLSCert, "tls-cert", envOr("TLS_CERT", ""), "TLS certificate file (enables HTTPS)")
	flag.StringVar(&cfg.TLSKey, "tls-key", envOr("TLS_KEY", ""), "TLS private key file")
	flag.StringVar(&cfg.WebsockifyURL, "websockify-url", envOr("WEBSOCKIFY_URL", ""), "external websockify base URL, e.g. wss://host:6080/websockify (built-in proxy is used if empty)")
	flag.StringVar(&cfg.WebsockifyTokenDir, "websockify-token-dir", envOr("WEBSOCKIFY_TOKEN_DIR", ""), "token directory for external websockify (--token-plugin TokenFile)")
	flag.StringVar(&cfg.ISODir, "iso-dir", envOr("ISO_DIR", ""), "directory with ISO images offered when creating VMs")
	flag.BoolVar(&cfg.EnableCreate, "enable-create", envBool("ENABLE_CREATE", true), "allow creating VMs with virt-install")
	flag.BoolVar(&cfg.AllowUndefine, "allow-undefine", envBool("ALLOW_UNDEFINE", true), "allow deleting (undefining) VMs")
	flag.BoolVar(&cfg.AllowXMLEdit, "allow-xml-edit", envBool("ALLOW_XML_EDIT", true), "allow editing domain XML from the UI")
	flag.DurationVar(&cfg.SessionTTL, "session-ttl", 12*time.Hour, "login session lifetime")
	flag.Parse()
	return cfg
}

// detectNoVNC looks for a noVNC checkout in common locations.
func detectNoVNC(explicit string) string {
	candidates := []string{explicit}
	if explicit == "" {
		exe, _ := os.Executable()
		candidates = []string{
			"./novnc",
			filepath.Join(filepath.Dir(exe), "novnc"),
			"/usr/share/novnc",
			"/usr/share/webapps/novnc",
			"/usr/local/share/novnc",
			"/opt/novnc",
		}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(filepath.Join(c, "core", "rfb.js")); err == nil && !st.IsDir() {
			abs, err := filepath.Abs(c)
			if err == nil {
				return abs
			}
			return c
		}
	}
	return ""
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("puppetron: ")
	cfg := loadConfig()

	if cfg.Password == "" {
		cfg.Password = randomHex(8)
		log.Printf("no password configured; generated one-time password for user %q: %s", cfg.User, cfg.Password)
		log.Printf("set PUPPETRON_PASSWORD (or -password) to make it permanent")
	}

	if _, err := exec.LookPath(cfg.VirshBin); err != nil {
		log.Printf("WARNING: virsh not found at %q – install libvirt-clients. (%v)", cfg.VirshBin, err)
	}
	if cfg.EnableCreate {
		if _, err := exec.LookPath(cfg.VirtInstallBin); err != nil {
			log.Printf("virt-install not found; VM creation disabled (%v)", err)
			cfg.EnableCreate = false
		}
	}

	cfg.NoVNCDir = detectNoVNC(cfg.NoVNCDir)
	if cfg.NoVNCDir == "" {
		log.Printf("WARNING: noVNC not found. Install the 'novnc' package or run: git clone --depth 1 https://github.com/novnc/noVNC novnc")
	} else {
		log.Printf("serving noVNC from %s", cfg.NoVNCDir)
	}
	if cfg.WebsockifyURL != "" {
		if cfg.WebsockifyTokenDir == "" {
			log.Fatalf("-websockify-token-dir is required when -websockify-url is set")
		}
		if err := os.MkdirAll(cfg.WebsockifyTokenDir, 0o750); err != nil {
			log.Fatalf("cannot create token dir: %v", err)
		}
		log.Printf("using external websockify at %s (tokens in %s)", cfg.WebsockifyURL, cfg.WebsockifyTokenDir)
	} else {
		log.Printf("using built-in websockify-compatible proxy at /ws/vnc/{token}")
	}

	virsh := NewVirsh(cfg.VirshBin, cfg.ConnectURI)
	srv := NewServer(cfg, virsh)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		var err error
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			log.Printf("listening on https://%s", cfg.Listen)
			err = httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			log.Printf("listening on http://%s", cfg.Listen)
			err = httpSrv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Printf("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	srv.Close()
	fmt.Fprintln(os.Stderr, strings.TrimSpace("bye"))
}

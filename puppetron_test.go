package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const sampleDominfo = `Id:             3
Name:           web01
UUID:           6695eb01-f6a4-8304-79aa-97f2502e193f
OS Type:        hvm
State:          running
CPU(s):         2
CPU time:       1234.5s
Max memory:     4194304 KiB
Used memory:    4194304 KiB
Persistent:     yes
Autostart:      enable
Managed save:   no
Security model: apparmor
Security DOI:   0
Security label: libvirt-6695eb01-f6a4-8304-79aa-97f2502e193f (enforcing)
`

const sampleXML = `<domain type='kvm' id='3'>
  <name>web01</name>
  <uuid>6695eb01-f6a4-8304-79aa-97f2502e193f</uuid>
  <title>Web server</title>
  <memory unit='KiB'>4194304</memory>
  <vcpu placement='static'>2</vcpu>
  <os firmware='efi'>
    <type arch='x86_64' machine='pc-q35-8.2'>hvm</type>
    <loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE.fd</loader>
  </os>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/web01.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
    <interface type='network'>
      <mac address='52:54:00:12:34:56'/>
      <source network='default' portid='x' bridge='virbr0'/>
      <target dev='vnet2'/>
      <model type='virtio'/>
    </interface>
    <graphics type='vnc' port='5901' autoport='yes' listen='127.0.0.1' passwd='s3cret'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
  </devices>
</domain>
`

func TestParseDominfoAndXML(t *testing.T) {
	kv := parseKV(sampleDominfo)
	d := &Domain{Name: "web01", ID: -1, State: kv["State"], StateKey: normalizeState(kv["State"]),
		VCPUs: int(firstInt(kv["CPU(s)"])), MaxMemKiB: firstInt(kv["Max memory"]), Autostart: yes(kv["Autostart"]), Persistent: yes(kv["Persistent"])}
	if d.StateKey != "running" || d.VCPUs != 2 || d.MaxMemKiB != 4194304 || !d.Autostart || !d.Persistent {
		t.Fatalf("bad dominfo parse: %+v", d)
	}
	if normalizeState("shut off") != "shutoff" || normalizeState("in shutdown") != "inshutdown" {
		t.Fatal("state normalization")
	}
	v := NewVirsh("virsh", "test:///default")
	v.applyXML(d, sampleXML)
	if d.Firmware != "uefi" || d.Arch != "x86_64" || d.Title != "Web server" {
		t.Fatalf("xml basics: %+v", d)
	}
	if len(d.Disks) != 2 || d.Disks[0].Source != "/var/lib/libvirt/images/web01.qcow2" || d.Disks[0].Format != "qcow2" || d.Disks[1].Device != "cdrom" {
		t.Fatalf("disks: %+v", d.Disks)
	}
	if len(d.NICs) != 1 || d.NICs[0].Source != "default" || d.NICs[0].MAC != "52:54:00:12:34:56" {
		t.Fatalf("nics: %+v", d.NICs)
	}
	if d.ConsoleType != "vnc" || len(d.Graphics) != 1 || d.Graphics[0].Port != 5901 {
		t.Fatalf("graphics: %+v", d.Graphics)
	}
	network, addr, pw, err := d.VNCTarget()
	if err != nil || network != "tcp" || addr != "127.0.0.1:5901" || pw != "s3cret" {
		t.Fatalf("vnc target: %s %s %s %v", network, addr, pw, err)
	}
}

func TestVNCTargetEdgeCases(t *testing.T) {
	d := &Domain{Graphics: []Graphics{{Type: "vnc", Port: -1, Listen: "0.0.0.0"}}, ConsoleType: "vnc"}
	if _, _, _, err := d.VNCTarget(); err == nil {
		t.Fatal("expected error for unallocated port")
	}
	d = &Domain{Graphics: []Graphics{{Type: "vnc", Port: 5900, Listen: "0.0.0.0"}}, ConsoleType: "vnc"}
	if _, addr, _, _ := d.VNCTarget(); addr != "127.0.0.1:5900" {
		t.Fatalf("wildcard listen should map to loopback, got %s", addr)
	}
	d = &Domain{Graphics: []Graphics{{Type: "vnc", Port: -1, Socket: "/run/vm.sock"}}, ConsoleType: "vnc"}
	if n, addr, _, err := d.VNCTarget(); err != nil || n != "unix" || addr != "/run/vm.sock" {
		t.Fatalf("unix socket: %s %s %v", n, addr, err)
	}
	d = &Domain{Graphics: []Graphics{{Type: "spice", Port: 5900}}, ConsoleType: "spice"}
	if _, _, _, err := d.VNCTarget(); err == nil || !strings.Contains(err.Error(), "spice") {
		t.Fatalf("spice should be rejected: %v", err)
	}
}

func TestParseTable(t *testing.T) {
	out := ` Name      State    Autostart   Persistent
--------------------------------------------
 default   active   yes         yes
 isolated  inactive no          yes

`
	rows := parseTable(out)
	if len(rows) != 2 || rows[0][0] != "default" || rows[1][1] != "inactive" {
		t.Fatalf("rows: %v", rows)
	}
	ifaddr := ` Name       MAC address          Protocol     Address
-------------------------------------------------------------------------------
 vnet2      52:54:00:12:34:56    ipv4         192.168.122.50/24
 -          -                    ipv6         fd00::1/64
`
	rows = parseTable(ifaddr)
	if len(rows) != 2 || rows[0][3] != "192.168.122.50/24" {
		t.Fatalf("ifaddr rows: %v", rows)
	}
}

func TestValidation(t *testing.T) {
	for _, ok := range []string{"vm1", "web-01", "a.b_c+d", "X"} {
		if !validName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-rf", "a b", "x;y", "../etc", "a/b", strings.Repeat("a", 200)} {
		if validName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
	_, err := (&Virsh{}).DomainAction(context.Background(), "ok", "rm")
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("unknown action must be rejected: %v", err)
	}
	_, _, err = (&Virsh{}).CreateVM(context.Background(), "virt-install", CreateRequest{Name: "x", MemoryMiB: 512, VCPUs: 1, ISO: "--evil"})
	if err == nil {
		t.Fatal("option-like argument must be rejected")
	}
}

func TestDefineNameMismatch(t *testing.T) {
	v := NewVirsh("virsh", "test:///default")
	if _, err := v.Define(context.Background(), "other", sampleXML); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("expected name mismatch error, got %v", err)
	}
	if _, err := v.Define(context.Background(), "web01", "<domain"); err == nil {
		t.Fatal("invalid xml must fail")
	}
}

// newTestServer builds a Server with auth configured and a VNC-like echo
// backend so the WebSocket proxy can be exercised without libvirt.
func newTestServer(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	cfg := &Config{User: "admin", Password: "pw", SessionTTL: time.Hour, ConnectURI: "test:///default"}
	s := NewServer(cfg, NewVirsh("/nonexistent/virsh", cfg.ConnectURI))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte("RFB 003.008\n"))
				_, _ = io.Copy(c, c) // echo
			}(c)
		}
	}()
	return s, ts, ln.Addr().String()
}

func login(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("POST", ts.URL+"/api/login", strings.NewReader(`{"username":"admin","password":"pw"}`))
	req.Header.Set("X-Requested-With", "puppetron")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("login status %d", res.StatusCode)
	}
	return client
}

func TestAuthFlow(t *testing.T) {
	_, ts, _ := newTestServer(t)

	res, _ := http.Get(ts.URL + "/api/vms")
	if res.StatusCode != 401 {
		t.Fatalf("unauthenticated API should be 401, got %d", res.StatusCode)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, _ = client.Get(ts.URL + "/")
	if res.StatusCode != 302 || !strings.HasPrefix(res.Header.Get("Location"), "/login") {
		t.Fatalf("unauthenticated page should redirect to /login, got %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	res, _ = http.Post(ts.URL+"/api/login", "application/json", strings.NewReader(`{"username":"admin","password":"wrong"}`))
	if res.StatusCode != 401 {
		t.Fatalf("bad password should be 401, got %d", res.StatusCode)
	}

	c := login(t, ts)
	res, _ = c.Get(ts.URL + "/api/me")
	if res.StatusCode != 200 {
		t.Fatalf("me: %d", res.StatusCode)
	}
	// Missing CSRF header on a mutating call → 403
	req, _ := http.NewRequest("POST", ts.URL+"/api/vms/x/action", strings.NewReader(`{"action":"start"}`))
	res, _ = c.Do(req)
	if res.StatusCode != 403 {
		t.Fatalf("missing X-Requested-With should be 403, got %d", res.StatusCode)
	}
	// Login page must be reachable and static files served
	res, _ = http.Get(ts.URL + "/login")
	if res.StatusCode != 200 {
		t.Fatalf("login page: %d", res.StatusCode)
	}
	res, _ = http.Get(ts.URL + "/static/app.js")
	if res.StatusCode != 200 {
		t.Fatalf("static: %d", res.StatusCode)
	}
	// virsh missing → API returns an error payload, not a crash
	res, _ = c.Get(ts.URL + "/api/vms")
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode < 400 || body["error"] == nil {
		t.Fatalf("expected virsh error, got %d %v", res.StatusCode, body)
	}
}

func TestWebSocketProxy(t *testing.T) {
	s, ts, backend := newTestServer(t)
	c := login(t, ts)
	tok := s.tokens.issue("tcp", backend, "echo-vm")

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/vnc/" + tok
	u, _ := url.Parse(ts.URL)
	hdr := http.Header{}
	for _, ck := range c.Jar.Cookies(u) {
		hdr.Add("Cookie", ck.String())
	}
	hdr.Set("Origin", ts.URL)
	dialer := websocket.Dialer{Subprotocols: []string{"binary"}}
	ws, res, err := dialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, res)
	}
	defer ws.Close()
	if ws.Subprotocol() != "binary" {
		t.Fatalf("subprotocol %q", ws.Subprotocol())
	}
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, msg, err := ws.ReadMessage()
	if err != nil || string(msg) != "RFB 003.008\n" {
		t.Fatalf("greeting: %q %v", msg, err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("hello vnc")); err != nil {
		t.Fatal(err)
	}
	_, msg, err = ws.ReadMessage()
	if err != nil || string(msg) != "hello vnc" {
		t.Fatalf("echo: %q %v", msg, err)
	}

	// bad token → 403, no cookie → 401, bad origin → rejected
	_, res, err = dialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws/vnc/nope", hdr)
	if err == nil || res == nil || res.StatusCode != 403 {
		t.Fatalf("bad token should be 403: %v %v", err, res)
	}
	_, res, err = dialer.Dial(wsURL, http.Header{"Origin": {ts.URL}})
	if err == nil || res == nil || res.StatusCode != 401 {
		t.Fatalf("no cookie should be 401: %v %v", err, res)
	}
	hdr2 := hdr.Clone()
	hdr2.Set("Origin", "http://evil.example")
	if _, _, err = dialer.Dial(wsURL, hdr2); err == nil {
		t.Fatal("foreign origin must be rejected")
	}
}

func TestConsoleTokenExpiry(t *testing.T) {
	ct := newConsoleTokens()
	tok := ct.issue("tcp", "127.0.0.1:5900", "x")
	if _, ok := ct.lookup(tok); !ok {
		t.Fatal("fresh token must be valid")
	}
	ct.mu.Lock()
	e := ct.m[tok]
	e.expires = time.Now().Add(-time.Second)
	ct.m[tok] = e
	ct.mu.Unlock()
	if _, ok := ct.lookup(tok); ok {
		t.Fatal("expired token must be rejected")
	}
}

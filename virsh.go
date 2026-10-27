package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// domainNameRe restricts names to a safe subset. Leading '-' is rejected so a
// name can never be interpreted as a virsh option.
var domainNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+-]{0,127}$`)

func validName(s string) bool { return domainNameRe.MatchString(s) }

// VirshError carries virsh's stderr so the UI can show a meaningful message.
type VirshError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *VirshError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return msg
}

func (e *VirshError) Unwrap() error { return e.Err }

// Virsh wraps the virsh CLI.
type Virsh struct {
	bin string
	uri string

	statsMu sync.Mutex
	prev    map[string]statsSample

	listMu    sync.Mutex
	listCache []Domain
	listAt    time.Time
}

type statsSample struct {
	at     time.Time
	values map[string]float64
}

func NewVirsh(bin, uri string) *Virsh {
	return &Virsh{bin: bin, uri: uri, prev: map[string]statsSample{}}
}

const defaultTimeout = 30 * time.Second

func (v *Virsh) run(ctx context.Context, args ...string) (string, error) {
	return v.runBin(ctx, defaultTimeout, v.bin, append([]string{"-c", v.uri}, args...)...)
}

func (v *Virsh) runBin(ctx context.Context, timeout time.Duration, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	// Force English output so our parsers are stable regardless of host locale.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "LANGUAGE=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("command timed out after %s", timeout)
		}
		return stdout.String(), &VirshError{Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.String(), nil
}

// ---------------------------------------------------------------------------
// Domains
// ---------------------------------------------------------------------------

type Graphics struct {
	Type     string `json:"type"`               // vnc, spice, ...
	Port     int    `json:"port"`               // -1 when not allocated
	TLSPort  int    `json:"tls_port,omitempty"` // spice
	Listen   string `json:"listen"`
	Socket   string `json:"socket,omitempty"`
	Password string `json:"-"` // never sent in listings
}

type Disk struct {
	Device string `json:"device"` // disk, cdrom
	Type   string `json:"type"`   // file, block, volume, network
	Source string `json:"source"`
	Target string `json:"target"`
	Bus    string `json:"bus"`
	Format string `json:"format"`
}

type NIC struct {
	Type   string `json:"type"` // network, bridge, direct, user
	MAC    string `json:"mac"`
	Source string `json:"source"`
	Model  string `json:"model"`
}

type IPAddr struct {
	Iface    string `json:"iface"`
	MAC      string `json:"mac"`
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
}

type Domain struct {
	Name         string     `json:"name"`
	ID           int        `json:"id"`
	UUID         string     `json:"uuid"`
	Title        string     `json:"title,omitempty"`
	Description  string     `json:"description,omitempty"`
	State        string     `json:"state"`     // human string from virsh, e.g. "shut off"
	StateKey     string     `json:"state_key"` // normalized: running, shutoff, paused, ...
	VCPUs        int        `json:"vcpus"`
	MaxMemKiB    int64      `json:"max_mem_kib"`
	UsedMemKiB   int64      `json:"used_mem_kib"`
	Persistent   bool       `json:"persistent"`
	Autostart    bool       `json:"autostart"`
	ManagedSave  bool       `json:"managed_save"`
	OSType       string     `json:"os_type"`
	Arch         string     `json:"arch"`
	Machine      string     `json:"machine"`
	Firmware     string     `json:"firmware"`
	CPUTime      string     `json:"cpu_time"`
	Graphics     []Graphics `json:"graphics"`
	Disks        []Disk     `json:"disks"`
	NICs         []NIC      `json:"nics"`
	IPs          []IPAddr   `json:"ips"`
	ConsoleType  string     `json:"console_type"` // "vnc" if a VNC console can be attached, otherwise "spice"/"none"
	HasSnapshots bool       `json:"has_snapshots"`
	Error        string     `json:"error,omitempty"`
}

var stateKeys = map[string]string{
	"running":     "running",
	"idle":        "idle",
	"paused":      "paused",
	"in shutdown": "inshutdown",
	"shut off":    "shutoff",
	"crashed":     "crashed",
	"pmsuspended": "pmsuspended",
	"blocked":     "blocked",
}

func normalizeState(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if k, ok := stateKeys[s]; ok {
		return k
	}
	return strings.ReplaceAll(s, " ", "")
}

// parseKV parses "Key:   value" lines as produced by dominfo, nodeinfo, etc.
func parseKV(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(val)
	}
	return m
}

func firstInt(s string) int64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(f[0], 10, 64)
	return n
}

func yes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "yes" || s == "enable" || s == "enabled" || s == "true"
}

// ListNames returns all defined/running domain names.
func (v *Virsh) ListNames(ctx context.Context) ([]string, error) {
	out, err := v.run(ctx, "list", "--all", "--name")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			names = append(names, l)
		}
	}
	sort.Strings(names)
	return names, nil
}

// ListDomains returns details for every domain. Results are cached for a
// short time because the UI polls frequently.
func (v *Virsh) ListDomains(ctx context.Context) ([]Domain, error) {
	v.listMu.Lock()
	if time.Since(v.listAt) < 2*time.Second && v.listCache != nil {
		c := v.listCache
		v.listMu.Unlock()
		return c, nil
	}
	v.listMu.Unlock()

	names, err := v.ListNames(ctx)
	if err != nil {
		return nil, err
	}
	doms := make([]Domain, len(names))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d, err := v.GetDomain(ctx, n, false)
			if err != nil {
				d = &Domain{Name: n, ID: -1, State: "unknown", StateKey: "unknown", Error: err.Error()}
			}
			doms[i] = *d
		}(i, n)
	}
	wg.Wait()

	v.listMu.Lock()
	v.listCache = doms
	v.listAt = time.Now()
	v.listMu.Unlock()
	return doms, nil
}

func (v *Virsh) invalidateList() {
	v.listMu.Lock()
	v.listAt = time.Time{}
	v.listMu.Unlock()
}

// GetDomain gathers dominfo + dumpxml (+ IP addresses when running).
func (v *Virsh) GetDomain(ctx context.Context, name string, withSnapshots bool) (*Domain, error) {
	if !validName(name) {
		return nil, errors.New("invalid domain name")
	}
	info, err := v.run(ctx, "dominfo", name)
	if err != nil {
		return nil, err
	}
	kv := parseKV(info)
	d := &Domain{
		Name:        name,
		ID:          -1,
		UUID:        kv["UUID"],
		State:       kv["State"],
		StateKey:    normalizeState(kv["State"]),
		VCPUs:       int(firstInt(kv["CPU(s)"])),
		MaxMemKiB:   firstInt(kv["Max memory"]),
		UsedMemKiB:  firstInt(kv["Used memory"]),
		Persistent:  yes(kv["Persistent"]),
		Autostart:   yes(kv["Autostart"]),
		ManagedSave: yes(kv["Managed save"]),
		OSType:      kv["OS Type"],
		CPUTime:     kv["CPU time"],
		ConsoleType: "none",
		Graphics:    []Graphics{},
		Disks:       []Disk{},
		NICs:        []NIC{},
		IPs:         []IPAddr{},
	}
	if id, err := strconv.Atoi(kv["Id"]); err == nil {
		d.ID = id
	}
	if n, ok := kv["Name"]; ok && n != "" {
		d.Name = n
	}

	xmlOut, err := v.run(ctx, "dumpxml", "--security-info", name)
	if err == nil {
		v.applyXML(d, xmlOut)
	} else {
		d.Error = "dumpxml: " + err.Error()
	}

	if d.StateKey == "running" || d.StateKey == "paused" || d.StateKey == "idle" || d.StateKey == "blocked" {
		if ips, err := v.DomIfAddr(ctx, name); err == nil {
			d.IPs = ips
		}
	}
	if withSnapshots {
		if out, err := v.run(ctx, "snapshot-list", "--name", name); err == nil {
			d.HasSnapshots = strings.TrimSpace(out) != ""
		}
	}
	return d, nil
}

// domainXML mirrors the parts of the libvirt domain XML we care about.
type domainXML struct {
	Name        string `xml:"name"`
	UUID        string `xml:"uuid"`
	Title       string `xml:"title"`
	Description string `xml:"description"`
	OS          struct {
		Type struct {
			Arch    string `xml:"arch,attr"`
			Machine string `xml:"machine,attr"`
			Value   string `xml:",chardata"`
		} `xml:"type"`
		Firmware string `xml:"firmware,attr"`
		Loader   struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"loader"`
	} `xml:"os"`
	Devices struct {
		Disks []struct {
			Device string `xml:"device,attr"`
			Type   string `xml:"type,attr"`
			Driver struct {
				Type string `xml:"type,attr"`
			} `xml:"driver"`
			Source struct {
				File   string `xml:"file,attr"`
				Dev    string `xml:"dev,attr"`
				Pool   string `xml:"pool,attr"`
				Volume string `xml:"volume,attr"`
				Name   string `xml:"name,attr"`
				Proto  string `xml:"protocol,attr"`
			} `xml:"source"`
			Target struct {
				Dev string `xml:"dev,attr"`
				Bus string `xml:"bus,attr"`
			} `xml:"target"`
		} `xml:"disk"`
		Interfaces []struct {
			Type string `xml:"type,attr"`
			MAC  struct {
				Address string `xml:"address,attr"`
			} `xml:"mac"`
			Source struct {
				Network string `xml:"network,attr"`
				Bridge  string `xml:"bridge,attr"`
				Dev     string `xml:"dev,attr"`
			} `xml:"source"`
			Model struct {
				Type string `xml:"type,attr"`
			} `xml:"model"`
		} `xml:"interface"`
		Graphics []struct {
			Type     string `xml:"type,attr"`
			Port     string `xml:"port,attr"`
			TLSPort  string `xml:"tlsPort,attr"`
			AutoPort string `xml:"autoport,attr"`
			Listen   string `xml:"listen,attr"`
			Socket   string `xml:"socket,attr"`
			Passwd   string `xml:"passwd,attr"`
			Listens  []struct {
				Type    string `xml:"type,attr"`
				Address string `xml:"address,attr"`
				Socket  string `xml:"socket,attr"`
			} `xml:"listen"`
		} `xml:"graphics"`
	} `xml:"devices"`
}

func (v *Virsh) applyXML(d *Domain, raw string) {
	var x domainXML
	if err := xml.Unmarshal([]byte(raw), &x); err != nil {
		d.Error = "xml: " + err.Error()
		return
	}
	d.Title = x.Title
	d.Description = x.Description
	d.Arch = x.OS.Type.Arch
	d.Machine = x.OS.Type.Machine
	switch {
	case x.OS.Firmware == "efi", strings.Contains(strings.ToLower(x.OS.Loader.Value), "ovmf"), strings.Contains(strings.ToLower(x.OS.Loader.Value), "aavmf"):
		d.Firmware = "uefi"
	default:
		d.Firmware = "bios"
	}
	for _, dk := range x.Devices.Disks {
		src := dk.Source.File
		if src == "" {
			src = dk.Source.Dev
		}
		if src == "" && dk.Source.Volume != "" {
			src = dk.Source.Pool + "/" + dk.Source.Volume
		}
		if src == "" && dk.Source.Name != "" {
			src = dk.Source.Proto + ":" + dk.Source.Name
		}
		d.Disks = append(d.Disks, Disk{
			Device: dk.Device, Type: dk.Type, Source: src,
			Target: dk.Target.Dev, Bus: dk.Target.Bus, Format: dk.Driver.Type,
		})
	}
	for _, in := range x.Devices.Interfaces {
		src := in.Source.Network
		if src == "" {
			src = in.Source.Bridge
		}
		if src == "" {
			src = in.Source.Dev
		}
		d.NICs = append(d.NICs, NIC{Type: in.Type, MAC: in.MAC.Address, Source: src, Model: in.Model.Type})
	}
	for _, g := range x.Devices.Graphics {
		gi := Graphics{Type: g.Type, Port: -1, TLSPort: -1, Listen: g.Listen, Socket: g.Socket, Password: g.Passwd}
		if p, err := strconv.Atoi(g.Port); err == nil {
			gi.Port = p
		}
		if p, err := strconv.Atoi(g.TLSPort); err == nil {
			gi.TLSPort = p
		}
		for _, l := range g.Listens {
			switch l.Type {
			case "address":
				if l.Address != "" {
					gi.Listen = l.Address
				}
			case "socket":
				if l.Socket != "" {
					gi.Socket = l.Socket
				}
			}
		}
		d.Graphics = append(d.Graphics, gi)
		if g.Type == "vnc" {
			d.ConsoleType = "vnc"
		} else if d.ConsoleType == "none" {
			d.ConsoleType = g.Type
		}
	}
}

// VNCTarget returns the network/address the WebSocket proxy should dial to
// reach the domain's VNC server.
func (d *Domain) VNCTarget() (network, addr, password string, err error) {
	for _, g := range d.Graphics {
		if g.Type != "vnc" {
			continue
		}
		if g.Socket != "" {
			return "unix", g.Socket, g.Password, nil
		}
		if g.Port <= 0 {
			return "", "", "", errors.New("VNC port not allocated – is the VM running?")
		}
		host := g.Listen
		if host == "" || host == "0.0.0.0" || host == "::" || host == "*" {
			host = "127.0.0.1"
		}
		return "tcp", net.JoinHostPort(host, strconv.Itoa(g.Port)), g.Password, nil
	}
	if d.ConsoleType != "none" && d.ConsoleType != "vnc" {
		return "", "", "", fmt.Errorf("domain uses %s graphics; only VNC consoles are supported by noVNC", d.ConsoleType)
	}
	return "", "", "", errors.New("domain has no VNC graphics device")
}

// DomIfAddr returns guest IP addresses (from DHCP leases, falling back to the
// guest agent and ARP table).
func (v *Virsh) DomIfAddr(ctx context.Context, name string) ([]IPAddr, error) {
	var result []IPAddr
	seen := map[string]bool{}
	for _, source := range []string{"lease", "agent", "arp"} {
		out, err := v.run(ctx, "domifaddr", "--source", source, name)
		if err != nil {
			continue
		}
		for _, row := range parseTable(out) {
			if len(row) < 4 {
				continue
			}
			ip := IPAddr{Iface: row[0], MAC: row[1], Protocol: row[2], Address: row[3]}
			if ip.Iface == "-" && len(result) > 0 { // continuation rows for the same iface
				ip.Iface = result[len(result)-1].Iface
				ip.MAC = result[len(result)-1].MAC
			}
			if strings.HasPrefix(ip.Address, "127.") || strings.HasPrefix(ip.Address, "::1") || strings.HasPrefix(ip.Address, "fe80:") {
				continue
			}
			if seen[ip.Address] {
				continue
			}
			seen[ip.Address] = true
			result = append(result, ip)
		}
		if source == "lease" && len(result) > 0 {
			// Prefer leases; still try agent for additional addresses but don't fail.
		}
	}
	if result == nil {
		result = []IPAddr{}
	}
	return result, nil
}

// parseTable parses virsh's table output: a header line, a dashed line, then
// whitespace-separated rows.
func parseTable(out string) [][]string {
	var rows [][]string
	lines := strings.Split(out, "\n")
	start := 0
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t != "" && strings.Trim(t, "-") == "" {
			start = i + 1
			break
		}
	}
	for _, l := range lines[start:] {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		rows = append(rows, f)
	}
	return rows
}

// Action maps a UI action to virsh arguments.
var domainActions = map[string][]string{
	"start":         {"start"},
	"shutdown":      {"shutdown"},
	"reboot":        {"reboot"},
	"reset":         {"reset"},
	"destroy":       {"destroy"},
	"suspend":       {"suspend"},
	"resume":        {"resume"},
	"managedsave":   {"managedsave"},
	"autostart-on":  {"autostart"},
	"autostart-off": {"autostart", "--disable"},
}

func (v *Virsh) DomainAction(ctx context.Context, name, action string) (string, error) {
	if !validName(name) {
		return "", errors.New("invalid domain name")
	}
	spec, ok := domainActions[action]
	if !ok {
		return "", fmt.Errorf("unknown action %q", action)
	}
	args := []string{spec[0], name}
	args = append(args, spec[1:]...)
	defer v.invalidateList()
	out, err := v.run(ctx, args...)
	return strings.TrimSpace(out), err
}

func (v *Virsh) Undefine(ctx context.Context, name string, removeStorage bool) (string, error) {
	if !validName(name) {
		return "", errors.New("invalid domain name")
	}
	defer v.invalidateList()
	args := []string{"undefine", name, "--managed-save", "--snapshots-metadata", "--nvram"}
	if removeStorage {
		args = append(args, "--remove-all-storage")
	}
	out, err := v.run(ctx, args...)
	if err != nil && strings.Contains(err.Error(), "nvram") {
		// Older libvirt without --nvram support
		args2 := []string{"undefine", name, "--managed-save", "--snapshots-metadata"}
		if removeStorage {
			args2 = append(args2, "--remove-all-storage")
		}
		out, err = v.run(ctx, args2...)
	}
	return strings.TrimSpace(out), err
}

func (v *Virsh) DumpXML(ctx context.Context, name string, inactive bool) (string, error) {
	if !validName(name) {
		return "", errors.New("invalid domain name")
	}
	args := []string{"dumpxml", name}
	if inactive {
		args = append(args, "--inactive")
	}
	return v.run(ctx, args...)
}

// Define writes the XML to a temp file and runs `virsh define`.
func (v *Virsh) Define(ctx context.Context, expectName, xmlText string) (string, error) {
	var x domainXML
	if err := xml.Unmarshal([]byte(xmlText), &x); err != nil {
		return "", fmt.Errorf("invalid XML: %w", err)
	}
	if expectName != "" && x.Name != expectName {
		return "", fmt.Errorf("XML <name> is %q, expected %q", x.Name, expectName)
	}
	if !validName(x.Name) {
		return "", errors.New("invalid domain name in XML")
	}
	f, err := os.CreateTemp("", "puppetron-*.xml")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(xmlText); err != nil {
		f.Close()
		return "", err
	}
	f.Close()
	defer v.invalidateList()
	out, err := v.run(ctx, "define", f.Name())
	return strings.TrimSpace(out), err
}

// ---------------------------------------------------------------------------
// Snapshots
// ---------------------------------------------------------------------------

type Snapshot struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	State       string `json:"state"`
	Parent      string `json:"parent"`
	Current     bool   `json:"current"`
	Location    string `json:"location"`
	Created     string `json:"created"`
	Children    int    `json:"children"`
}

var snapshotNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._ +-]{0,127}$`)

func (v *Virsh) ListSnapshots(ctx context.Context, name string) ([]Snapshot, error) {
	if !validName(name) {
		return nil, errors.New("invalid domain name")
	}
	out, err := v.run(ctx, "snapshot-list", "--name", name)
	if err != nil {
		return nil, err
	}
	snaps := []Snapshot{}
	for _, l := range strings.Split(out, "\n") {
		sn := strings.TrimSpace(l)
		if sn == "" {
			continue
		}
		s := Snapshot{Name: sn}
		if info, err := v.run(ctx, "snapshot-info", name, "--snapshotname", sn); err == nil {
			kv := parseKV(info)
			s.State = kv["State"]
			s.Parent = kv["Parent"]
			if s.Parent == "-" {
				s.Parent = ""
			}
			s.Current = yes(kv["Current"])
			s.Location = kv["Location"]
			s.Children, _ = strconv.Atoi(kv["Children"])
		}
		// creation time + description come from the XML
		if sx, err := v.run(ctx, "snapshot-dumpxml", name, "--snapshotname", sn); err == nil {
			var meta struct {
				Description  string `xml:"description"`
				CreationTime int64  `xml:"creationTime"`
			}
			if xml.Unmarshal([]byte(sx), &meta) == nil {
				s.Description = meta.Description
				if meta.CreationTime > 0 {
					s.Created = time.Unix(meta.CreationTime, 0).UTC().Format(time.RFC3339)
				}
			}
		}
		snaps = append(snaps, s)
	}
	return snaps, nil
}

func (v *Virsh) CreateSnapshot(ctx context.Context, name, snap, desc string) (string, error) {
	if !validName(name) {
		return "", errors.New("invalid domain name")
	}
	args := []string{"snapshot-create-as", name}
	if snap != "" {
		if !snapshotNameRe.MatchString(snap) {
			return "", errors.New("invalid snapshot name")
		}
		args = append(args, "--name", snap)
	}
	if desc != "" {
		args = append(args, "--description", desc)
	}
	out, err := v.runBin(ctx, 10*time.Minute, v.bin, append([]string{"-c", v.uri}, args...)...)
	return strings.TrimSpace(out), err
}

func (v *Virsh) RevertSnapshot(ctx context.Context, name, snap string) (string, error) {
	if !validName(name) || !snapshotNameRe.MatchString(snap) {
		return "", errors.New("invalid name")
	}
	defer v.invalidateList()
	out, err := v.runBin(ctx, 10*time.Minute, v.bin, "-c", v.uri, "snapshot-revert", name, "--snapshotname", snap, "--force")
	return strings.TrimSpace(out), err
}

func (v *Virsh) DeleteSnapshot(ctx context.Context, name, snap string) (string, error) {
	if !validName(name) || !snapshotNameRe.MatchString(snap) {
		return "", errors.New("invalid name")
	}
	out, err := v.runBin(ctx, 10*time.Minute, v.bin, "-c", v.uri, "snapshot-delete", name, "--snapshotname", snap)
	return strings.TrimSpace(out), err
}

// ---------------------------------------------------------------------------
// Statistics
// ---------------------------------------------------------------------------

type DomStats struct {
	Name       string             `json:"name"`
	At         time.Time          `json:"at"`
	CPUPercent float64            `json:"cpu_percent"` // 0..100 relative to the VM's vCPUs
	VCPUs      int                `json:"vcpus"`
	MemKiB     int64              `json:"mem_kib"`     // balloon.current
	MemMaxKiB  int64              `json:"mem_max_kib"` // balloon.maximum
	MemRSSKiB  int64              `json:"mem_rss_kib"`
	MemUsedKiB int64              `json:"mem_used_kib"` // from guest agent if available, else rss
	Blocks     []BlockStat        `json:"blocks"`
	Nets       []NetStat          `json:"nets"`
	Raw        map[string]float64 `json:"-"`
}

type BlockStat struct {
	Name       string  `json:"name"`
	Path       string  `json:"path"`
	ReadBytes  float64 `json:"rd_bytes"`
	WriteBytes float64 `json:"wr_bytes"`
	ReadBps    float64 `json:"rd_bps"`
	WriteBps   float64 `json:"wr_bps"`
	Capacity   float64 `json:"capacity"`
	Allocation float64 `json:"allocation"`
	Physical   float64 `json:"physical"`
}

type NetStat struct {
	Name    string  `json:"name"`
	RxBytes float64 `json:"rx_bytes"`
	TxBytes float64 `json:"tx_bytes"`
	RxBps   float64 `json:"rx_bps"`
	TxBps   float64 `json:"tx_bps"`
}

func (v *Virsh) Stats(ctx context.Context, name string) (*DomStats, error) {
	if !validName(name) {
		return nil, errors.New("invalid domain name")
	}
	out, err := v.run(ctx, "domstats", "--cpu-total", "--balloon", "--vcpu", "--block", "--interface", name)
	if err != nil {
		return nil, err
	}
	vals := map[string]float64{}
	strs := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		k, val, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		strs[k] = val
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			vals[k] = f
		}
	}
	now := time.Now()
	st := &DomStats{Name: name, At: now, Raw: vals, Blocks: []BlockStat{}, Nets: []NetStat{}}
	st.VCPUs = int(vals["vcpu.current"])
	if st.VCPUs == 0 {
		st.VCPUs = int(vals["vcpu.maximum"])
	}
	st.MemKiB = int64(vals["balloon.current"])
	st.MemMaxKiB = int64(vals["balloon.maximum"])
	st.MemRSSKiB = int64(vals["balloon.rss"])
	if avail, ok := vals["balloon.available"]; ok {
		if unused, ok := vals["balloon.unused"]; ok {
			st.MemUsedKiB = int64(avail - unused)
		}
	}
	if st.MemUsedKiB == 0 {
		st.MemUsedKiB = st.MemRSSKiB
	}

	v.statsMu.Lock()
	prev, hasPrev := v.prev[name]
	v.prev[name] = statsSample{at: now, values: vals}
	v.statsMu.Unlock()

	rate := func(key string) float64 {
		if !hasPrev {
			return 0
		}
		dt := now.Sub(prev.at).Seconds()
		if dt <= 0 {
			return 0
		}
		p, ok := prev.values[key]
		if !ok {
			return 0
		}
		d := vals[key] - p
		if d < 0 {
			return 0
		}
		return d / dt
	}

	if hasPrev && st.VCPUs > 0 {
		ns := rate("cpu.time") // ns of CPU per second of wall clock
		st.CPUPercent = ns / 1e9 * 100 / float64(st.VCPUs)
		if st.CPUPercent > 100 {
			st.CPUPercent = 100
		}
	}

	for i := 0; i < int(vals["block.count"]); i++ {
		p := fmt.Sprintf("block.%d.", i)
		st.Blocks = append(st.Blocks, BlockStat{
			Name:       strs[p+"name"],
			Path:       strs[p+"path"],
			ReadBytes:  vals[p+"rd.bytes"],
			WriteBytes: vals[p+"wr.bytes"],
			ReadBps:    rate(p + "rd.bytes"),
			WriteBps:   rate(p + "wr.bytes"),
			Capacity:   vals[p+"capacity"],
			Allocation: vals[p+"allocation"],
			Physical:   vals[p+"physical"],
		})
	}
	for i := 0; i < int(vals["net.count"]); i++ {
		p := fmt.Sprintf("net.%d.", i)
		st.Nets = append(st.Nets, NetStat{
			Name:    strs[p+"name"],
			RxBytes: vals[p+"rx.bytes"],
			TxBytes: vals[p+"tx.bytes"],
			RxBps:   rate(p + "rx.bytes"),
			TxBps:   rate(p + "tx.bytes"),
		})
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// Host
// ---------------------------------------------------------------------------

type HostInfo struct {
	Hostname      string  `json:"hostname"`
	URI           string  `json:"uri"`
	CPUModel      string  `json:"cpu_model"`
	CPUs          int     `json:"cpus"`
	CPUMHz        int     `json:"cpu_mhz"`
	MemTotalKiB   int64   `json:"mem_total_kib"`
	MemFreeKiB    int64   `json:"mem_free_kib"` // free + buffers + cached
	CPUUsage      float64 `json:"cpu_usage"`    // percent
	LibvirtVer    string  `json:"libvirt_version"`
	HypervisorVer string  `json:"hypervisor_version"`
	Error         string  `json:"error,omitempty"`
}

func (v *Virsh) Host(ctx context.Context) *HostInfo {
	h := &HostInfo{URI: v.uri}
	if out, err := v.run(ctx, "hostname"); err == nil {
		h.Hostname = strings.TrimSpace(out)
	} else {
		h.Error = err.Error()
		return h
	}
	if out, err := v.run(ctx, "nodeinfo"); err == nil {
		kv := parseKV(out)
		h.CPUModel = kv["CPU model"]
		h.CPUs = int(firstInt(kv["CPU(s)"]))
		h.CPUMHz = int(firstInt(kv["CPU frequency"]))
		h.MemTotalKiB = firstInt(kv["Memory size"])
	}
	if out, err := v.run(ctx, "nodememstats"); err == nil {
		kv := parseKV(out)
		h.MemFreeKiB = firstInt(kv["free"]) + firstInt(kv["buffers"]) + firstInt(kv["cached"])
	}
	if out, err := v.run(ctx, "nodecpustats", "--percent"); err == nil {
		kv := parseKV(out)
		u := strings.TrimSuffix(strings.TrimSpace(kv["usage"]), "%")
		h.CPUUsage, _ = strconv.ParseFloat(u, 64)
	}
	if out, err := v.run(ctx, "version"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			l = strings.TrimSpace(l)
			switch {
			case strings.HasPrefix(l, "Using library:"):
				h.LibvirtVer = strings.TrimSpace(strings.TrimPrefix(l, "Using library:"))
			case strings.HasPrefix(l, "Running hypervisor:"):
				h.HypervisorVer = strings.TrimSpace(strings.TrimPrefix(l, "Running hypervisor:"))
			}
		}
	}
	return h
}

// ---------------------------------------------------------------------------
// Networks, storage
// ---------------------------------------------------------------------------

type Network struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Autostart  bool   `json:"autostart"`
	Persistent bool   `json:"persistent"`
	Bridge     string `json:"bridge"`
}

func (v *Virsh) Networks(ctx context.Context) ([]Network, error) {
	out, err := v.run(ctx, "net-list", "--all")
	if err != nil {
		return nil, err
	}
	nets := []Network{}
	for _, r := range parseTable(out) {
		if len(r) < 2 {
			continue
		}
		n := Network{Name: r[0], State: r[1]}
		if len(r) > 2 {
			n.Autostart = yes(r[2])
		}
		if len(r) > 3 {
			n.Persistent = yes(r[3])
		}
		if validName(n.Name) {
			if b, err := v.run(ctx, "net-info", n.Name); err == nil {
				n.Bridge = parseKV(b)["Bridge"]
			}
		}
		nets = append(nets, n)
	}
	return nets, nil
}

type Pool struct {
	Name          string `json:"name"`
	State         string `json:"state"`
	Autostart     bool   `json:"autostart"`
	Path          string `json:"path"`
	CapacityB     int64  `json:"capacity_bytes"`
	AllocationB   int64  `json:"allocation_bytes"`
	AvailableB    int64  `json:"available_bytes"`
	ParseWarnings string `json:"-"`
}

func (v *Virsh) Pools(ctx context.Context) ([]Pool, error) {
	out, err := v.run(ctx, "pool-list", "--all")
	if err != nil {
		return nil, err
	}
	pools := []Pool{}
	for _, r := range parseTable(out) {
		if len(r) < 2 {
			continue
		}
		p := Pool{Name: r[0], State: r[1]}
		if len(r) > 2 {
			p.Autostart = yes(r[2])
		}
		if validName(p.Name) {
			if info, err := v.run(ctx, "pool-info", "--bytes", p.Name); err == nil {
				kv := parseKV(info)
				p.CapacityB = firstInt(kv["Capacity"])
				p.AllocationB = firstInt(kv["Allocation"])
				p.AvailableB = firstInt(kv["Available"])
			}
			if px, err := v.run(ctx, "pool-dumpxml", p.Name); err == nil {
				var x struct {
					Target struct {
						Path string `xml:"path"`
					} `xml:"target"`
				}
				if xml.Unmarshal([]byte(px), &x) == nil {
					p.Path = x.Target.Path
				}
			}
		}
		pools = append(pools, p)
	}
	return pools, nil
}

type Volume struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (v *Virsh) Volumes(ctx context.Context, pool string) ([]Volume, error) {
	if !validName(pool) {
		return nil, errors.New("invalid pool name")
	}
	out, err := v.run(ctx, "vol-list", pool)
	if err != nil {
		return nil, err
	}
	vols := []Volume{}
	for _, r := range parseTable(out) {
		if len(r) < 1 {
			continue
		}
		vol := Volume{Name: r[0]}
		if len(r) > 1 {
			vol.Path = strings.Join(r[1:], " ")
		}
		vols = append(vols, vol)
	}
	return vols, nil
}

// ISOs lists ISO images from the configured directory and from all storage pools.
func (v *Virsh) ISOs(ctx context.Context, isoDir string) []string {
	seen := map[string]bool{}
	var isos []string
	add := func(p string) {
		if strings.HasSuffix(strings.ToLower(p), ".iso") && !seen[p] {
			seen[p] = true
			isos = append(isos, p)
		}
	}
	if isoDir != "" {
		if entries, err := os.ReadDir(isoDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					add(filepath.Join(isoDir, e.Name()))
				}
			}
		}
	}
	if pools, err := v.Pools(ctx); err == nil {
		for _, p := range pools {
			if p.State != "active" {
				continue
			}
			if vols, err := v.Volumes(ctx, p.Name); err == nil {
				for _, vol := range vols {
					add(vol.Path)
				}
			}
		}
	}
	sort.Strings(isos)
	if isos == nil {
		isos = []string{}
	}
	return isos
}

// ---------------------------------------------------------------------------
// VM creation (virt-install)
// ---------------------------------------------------------------------------

type CreateRequest struct {
	Name       string `json:"name"`
	MemoryMiB  int    `json:"memory_mib"`
	VCPUs      int    `json:"vcpus"`
	DiskGiB    int    `json:"disk_gib"`
	Pool       string `json:"pool"`      // storage pool for the new disk
	DiskPath   string `json:"disk_path"` // existing disk image to import (alternative to Pool/DiskGiB)
	ISO        string `json:"iso"`
	OSVariant  string `json:"os_variant"`
	Network    string `json:"network"` // libvirt network name
	Bridge     string `json:"bridge"`  // or host bridge
	UEFI       bool   `json:"uefi"`
	DiskFormat string `json:"disk_format"`
	Autostart  bool   `json:"autostart"`
}

var safeArgRe = regexp.MustCompile(`^[A-Za-z0-9._/+:=,@-]+$`)

func (v *Virsh) CreateVM(ctx context.Context, virtInstall string, req CreateRequest) (string, []string, error) {
	if !validName(req.Name) {
		return "", nil, errors.New("invalid VM name")
	}
	if req.MemoryMiB < 64 {
		return "", nil, errors.New("memory must be at least 64 MiB")
	}
	if req.VCPUs < 1 {
		return "", nil, errors.New("vcpus must be at least 1")
	}
	for _, s := range []string{req.Pool, req.DiskPath, req.ISO, req.OSVariant, req.Network, req.Bridge, req.DiskFormat} {
		if s != "" && (!safeArgRe.MatchString(s) || strings.HasPrefix(s, "-")) {
			return "", nil, fmt.Errorf("invalid characters in %q", s)
		}
	}
	args := []string{
		"--connect", v.uri,
		"--name", req.Name,
		"--memory", strconv.Itoa(req.MemoryMiB),
		"--vcpus", strconv.Itoa(req.VCPUs),
		"--graphics", "vnc,listen=127.0.0.1",
		"--noautoconsole",
	}
	osv := req.OSVariant
	if osv == "" {
		osv = "generic"
	}
	args = append(args, "--os-variant", osv)

	format := req.DiskFormat
	if format == "" {
		format = "qcow2"
	}
	switch {
	case req.DiskPath != "":
		args = append(args, "--disk", fmt.Sprintf("path=%s,format=%s,bus=virtio", req.DiskPath, format))
		if req.ISO == "" {
			args = append(args, "--import")
		}
	case req.DiskGiB > 0:
		disk := fmt.Sprintf("size=%d,format=%s,bus=virtio", req.DiskGiB, format)
		if req.Pool != "" {
			disk += ",pool=" + req.Pool
		}
		args = append(args, "--disk", disk)
	default:
		args = append(args, "--disk", "none")
	}
	if req.ISO != "" {
		args = append(args, "--cdrom", req.ISO)
	} else if req.DiskPath == "" {
		args = append(args, "--pxe")
	}
	switch {
	case req.Bridge != "":
		args = append(args, "--network", "bridge="+req.Bridge+",model=virtio")
	case req.Network != "":
		args = append(args, "--network", "network="+req.Network+",model=virtio")
	default:
		args = append(args, "--network", "network=default,model=virtio")
	}
	if req.UEFI {
		args = append(args, "--boot", "uefi")
	}
	if req.Autostart {
		args = append(args, "--autostart")
	}
	defer v.invalidateList()
	out, err := v.runBin(ctx, 5*time.Minute, virtInstall, args...)
	return strings.TrimSpace(out), args, err
}

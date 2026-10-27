# Puppetron

A small, self-contained web UI for **KVM/QEMU virtual machines managed by libvirt**.

* Single Go binary, frontend (HTML/CSS/JS) embedded.
* All hypervisor work is done by shelling out to **`virsh`** (and optionally `virt-install`), so it works with any libvirt version that ships `virsh`.
* Browser consoles via **noVNC**. The binary includes a **websockify-compatible WebSocket→VNC proxy**, so you do not need a separate `websockify` process (but you can use an external one if you want).
* Login with username/password, cookie sessions, CSRF protection, optional TLS.

## Features

| Area | What you can do |
|------|-----------------|
| VM list | live state, vCPU/RAM, IP addresses (DHCP lease / guest agent / ARP), autostart & console tags, filter/search |
| Lifecycle | start, ACPI shutdown, reboot, pause/resume, hard reset, force off, managed save, autostart on/off |
| Console | noVNC in the browser: scaling, fullscreen, Ctrl+Alt+Del, extra keys, clipboard, "type text", power menu, VNC password passthrough |
| Monitoring | per-VM CPU %, memory, disk & network throughput; host CPU/RAM |
| Snapshots | list, create, revert, delete |
| XML | view live/persistent XML, edit and `virsh define` |
| Create | new VMs through `virt-install` (ISO install, import existing disk, PXE; UEFI; pool/network selection) |
| Delete | undefine, optionally with all storage |
| Inventory | networks, storage pools, volumes, ISO images |

## Requirements (on the Linux server)

* libvirt + QEMU/KVM running (`libvirtd` or the modular daemons), `virsh` (`libvirt-clients`).
* Optional: `virt-install` (`virtinst` / `virt-install` package) for creating VMs.
* **noVNC** files on disk. Any of:
  * `apt install novnc` (Debian/Ubuntu → `/usr/share/novnc`)
  * `dnf install novnc` (Fedora/RHEL → `/usr/share/novnc`)
  * `git clone --depth 1 https://github.com/novnc/noVNC novnc` next to the binary
  * or point `-novnc /path/to/noVNC`.
* Go 1.22+ to build.
* The user running Puppetron must be allowed to talk to libvirt (member of the `libvirt` group, or root).
* VMs must have a **VNC** graphics device (the default on most distros is SPICE – see "SPICE vs VNC" below).

## Build

```bash
git clone <this repo> puppetron && cd puppetron
go mod tidy          # fetches github.com/gorilla/websocket
go build -o puppetron .
```

Cross-compile from macOS/Windows for a Linux x86_64 server:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o puppetron-linux-amd64 .
```

## Run

```bash
# noVNC next to the binary (if not installed as a package)
git clone --depth 1 https://github.com/novnc/noVNC novnc

PUPPETRON_USER=admin PUPPETRON_PASSWORD='change-me' ./puppetron -listen :8080
```

Open <http://server:8080>, log in, click a VM, press **Console**.

If no password is set, a random one is generated and printed in the log on startup.

### All options

Every flag can also be set as an environment variable `PUPPETRON_<FLAG_IN_UPPERCASE>` (dashes → underscores).

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `-listen` | `PUPPETRON_LISTEN` | `:8080` | Listen address |
| `-connect` | `PUPPETRON_CONNECT` | `qemu:///system` | libvirt URI |
| `-virsh` | `PUPPETRON_VIRSH` | `virsh` | virsh binary |
| `-virt-install` | `PUPPETRON_VIRT_INSTALL` | `virt-install` | virt-install binary |
| `-novnc` | `PUPPETRON_NOVNC` | auto-detect | noVNC directory (must contain `core/rfb.js`) |
| `-user` | `PUPPETRON_USER` | `admin` | UI username |
| `-password` | `PUPPETRON_PASSWORD` | random | UI password |
| `-tls-cert` / `-tls-key` | `PUPPETRON_TLS_CERT` / `PUPPETRON_TLS_KEY` | | Enable HTTPS |
| `-websockify-url` | `PUPPETRON_WEBSOCKIFY_URL` | | Use an external websockify (see below) |
| `-websockify-token-dir` | `PUPPETRON_WEBSOCKIFY_TOKEN_DIR` | | Token dir for external websockify |
| `-iso-dir` | `PUPPETRON_ISO_DIR` | | Extra directory scanned for `.iso` files |
| `-enable-create` | `PUPPETRON_ENABLE_CREATE` | `true` | Allow VM creation |
| `-allow-undefine` | `PUPPETRON_ALLOW_UNDEFINE` | `true` | Allow deleting VMs |
| `-allow-xml-edit` | `PUPPETRON_ALLOW_XML_EDIT` | `true` | Allow XML editing |
| `-session-ttl` | | `12h` | Login session lifetime |

### systemd

```bash
sudo ./deploy/install.sh        # builds, installs to /opt/puppetron, clones noVNC, installs the unit
sudo systemctl status puppetron
```

Or by hand: see `deploy/puppetron.service` and `deploy/puppetron.env.example`.

## How the console works

```
browser ──HTTPS──▶ puppetron ──virsh dumpxml──▶ libvirt   (finds VNC listen addr / port / password)
browser ──WebSocket /ws/vnc/<token>──▶ puppetron ──TCP──▶ 127.0.0.1:59xx (QEMU VNC)
```

1. The UI calls `POST /api/vms/<name>/console`. Puppetron reads the domain XML (`--security-info`), finds the VNC `listen`/`port`/`socket`/`passwd`, and issues a random **90-second token** bound to that endpoint.
2. noVNC (`/novnc/core/rfb.js`) connects to `wss://<host>/ws/vnc/<token>`. Puppetron validates the session cookie and the token and pipes bytes between the WebSocket and the VNC TCP (or UNIX) socket, exactly like websockify does.
3. The browser never learns the real VNC host/port, and the VNC password (if the domain has one) is passed to noVNC automatically.

### Using an external websockify instead

If you prefer the real `websockify`:

```bash
mkdir -p /var/run/puppetron-tokens
websockify --token-plugin TokenFile --token-source /var/run/puppetron-tokens 0.0.0.0:6080
./puppetron -websockify-url ws://server:6080/websockify -websockify-token-dir /var/run/puppetron-tokens
```

Puppetron then writes a `token: host:port` file per console session and the browser connects to websockify with `?token=`.
(Use `wss://` and `--cert/--key` on websockify when Puppetron runs with TLS, otherwise browsers block the mixed connection.)

### SPICE vs VNC

noVNC only speaks VNC. VMs created by virt-manager often use SPICE. Switch a VM to VNC with:

```bash
virsh edit <vm>   # replace <graphics type='spice' .../> with:
#   <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'>
#     <listen type='address' address='127.0.0.1'/>
#   </graphics>
```

or use the **XML** tab in the UI. VMs created from the UI get `--graphics vnc,listen=127.0.0.1`.

Since Puppetron connects from the same host, keeping VNC bound to `127.0.0.1` is the safest option.

## Security notes

* Everything except `/login` requires a session cookie (`HttpOnly`, `SameSite=Strict`, `Secure` under TLS).
* Mutating API calls additionally require the `X-Requested-With: puppetron` header (CSRF defence).
* WebSocket upgrades check the `Origin` header against the request host.
* `virsh` is executed with argument arrays (no shell). Domain/snapshot/pool names are validated against a strict pattern; `virt-install` parameters are character-whitelisted.
* Run behind TLS (`-tls-cert/-tls-key`) or a reverse proxy (nginx/Caddy). When proxying, forward WebSockets for `/ws/` and pass `X-Forwarded-Proto`.
* Single admin account only. Put it behind your VPN / SSO proxy if you need more.

## API (for scripting)

All JSON; send `X-Requested-With: puppetron` on non-GET.

```
POST   /api/login                      {username,password}
POST   /api/logout
GET    /api/me
GET    /api/capabilities
GET    /api/host
GET    /api/vms
POST   /api/vms                        CreateRequest (see virsh.go)
GET    /api/vms/{name}
DELETE /api/vms/{name}?storage=1
GET    /api/vms/{name}/stats
GET    /api/vms/{name}/xml?inactive=1
PUT    /api/vms/{name}/xml             (raw XML body)
POST   /api/vms/{name}/action          {action: start|shutdown|reboot|reset|destroy|suspend|resume|managedsave|autostart-on|autostart-off}
GET    /api/vms/{name}/snapshots
POST   /api/vms/{name}/snapshots       {name?, description?}
POST   /api/vms/{name}/snapshots/{snap}/revert
DELETE /api/vms/{name}/snapshots/{snap}
POST   /api/vms/{name}/console         → {token, ws_path | ws_url, password, external}
GET    /ws/vnc/{token}                 WebSocket (binary subprotocol)
GET    /api/networks | /api/pools | /api/pools/{pool}/volumes | /api/isos
```

## Project layout

```
main.go      config, startup
server.go    router, middleware, JSON helpers
auth.go      sessions, login, CSRF
api.go       HTTP handlers
virsh.go     virsh/virt-install wrapper + output parsers
vnc.go       console tokens + websockify-compatible proxy
web/         embedded frontend (index, login, console, static/)
deploy/      systemd unit, env example, install script
```

## Troubleshooting

* **"noVNC is not installed"** – install the `novnc` package or clone noVNC and start with `-novnc /path`. The dir must contain `core/rfb.js`.
* **"VNC port not allocated"** – the VM is off, or its graphics device isn't VNC.
* **"cannot connect to VNC server"** – QEMU listens on an address Puppetron can't reach. Check `virsh domdisplay <vm>` and the `<graphics>` element.
* **virsh errors like `authentication failed`** – the service user is not in the `libvirt` group (or polkit denies access). `sudo usermod -aG libvirt <user>`.
* **Console works over HTTP but not through a reverse proxy** – ensure WebSocket upgrade headers are forwarded (`Upgrade`, `Connection`) and timeouts are long.
* Run with `-connect qemu:///session` to manage your own user-session VMs instead of the system ones.

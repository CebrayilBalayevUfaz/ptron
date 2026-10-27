package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Auth endpoints
// ---------------------------------------------------------------------------

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if !s.checkCredentials(req.Username, req.Password) {
		time.Sleep(700 * time.Millisecond) // crude brute-force throttle
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	id := s.sessions.create()
	s.setSessionCookie(w, r, id, int(s.cfg.SessionTTL.Seconds()))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": s.cfg.User})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.revoke(c.Value)
	}
	s.setSessionCookie(w, r, "", -1)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(r) {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": s.cfg.User})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"create":              s.cfg.EnableCreate,
		"undefine":            s.cfg.AllowUndefine,
		"xml_edit":            s.cfg.AllowXMLEdit,
		"novnc":               s.cfg.NoVNCDir != "",
		"external_websockify": s.cfg.WebsockifyURL != "",
		"uri":                 s.cfg.ConnectURI,
	})
}

// ---------------------------------------------------------------------------
// Host
// ---------------------------------------------------------------------------

func (s *Server) handleHost(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.virsh.Host(r.Context()))
}

// ---------------------------------------------------------------------------
// Domains
// ---------------------------------------------------------------------------

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	doms, err := s.virsh.ListDomains(r.Context())
	if err != nil {
		writeVirshError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doms)
}

func (s *Server) handleGetVM(w http.ResponseWriter, r *http.Request) {
	d, err := s.virsh.GetDomain(r.Context(), r.PathValue("name"), true)
	if err != nil {
		writeVirshError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.virsh.Stats(r.Context(), r.PathValue("name"))
	if err != nil {
		writeVirshError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	name := r.PathValue("name")
	out, err := s.virsh.DomainAction(r.Context(), name, req.Action)
	if err != nil {
		writeVirshError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": out})
}

func (s *Server) handleUndefine(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowUndefine {
		writeError(w, http.StatusForbidden, "deleting VMs is disabled by configuration")
		return
	}
	removeStorage := r.URL.Query().Get("storage") == "1" || r.URL.Query().Get("storage") == "true"
	out, err := s.virsh.Undefine(r.Context(), r.PathValue("name"), removeStorage)
	if err != nil {
		writeVirshError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": out})
}

func (s *Server) handleGetXML(w http.ResponseWriter, r *http.Request) {
	inactive := r.URL.Query().Get("inactive") == "1"
	x, err := s.virsh.DumpXML(r.Context(), r.PathValue("name"), inactive)
	if err != nil {
		writeVirshError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, x)
}

func (s *Server) handlePutXML(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowXMLEdit {
		writeError(w, http.StatusForbidden, "XML editing is disabled by configuration")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	out, err := s.virsh.Define(r.Context(), r.PathValue("name"), string(body))
	if err != nil {
		writeVirshError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": out})
}

func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.EnableCreate {
		writeError(w, http.StatusForbidden, "VM creation is disabled (virt-install missing or -enable-create=false)")
		return
	}
	var req CreateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	out, args, err := s.virsh.CreateVM(r.Context(), s.cfg.VirtInstallBin, req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "command": append([]string{s.cfg.VirtInstallBin}, args...), "output": out})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "message": out, "command": append([]string{s.cfg.VirtInstallBin}, args...)})
}

// ---------------------------------------------------------------------------
// Snapshots
// ---------------------------------------------------------------------------

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.virsh.ListSnapshots(r.Context(), r.PathValue("name"))
	if err != nil {
		writeVirshError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snaps)
}

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	out, err := s.virsh.CreateSnapshot(r.Context(), r.PathValue("name"), strings.TrimSpace(req.Name), strings.TrimSpace(req.Description))
	if err != nil {
		writeVirshError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "message": out})
}

func (s *Server) handleRevertSnapshot(w http.ResponseWriter, r *http.Request) {
	out, err := s.virsh.RevertSnapshot(r.Context(), r.PathValue("name"), r.PathValue("snap"))
	if err != nil {
		writeVirshError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": out})
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	out, err := s.virsh.DeleteSnapshot(r.Context(), r.PathValue("name"), r.PathValue("snap"))
	if err != nil {
		writeVirshError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": out})
}

// ---------------------------------------------------------------------------
// Console
// ---------------------------------------------------------------------------

// handleConsoleToken resolves the VNC endpoint of a domain and returns a
// short-lived token plus the WebSocket URL noVNC should connect to.
func (s *Server) handleConsoleToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d, err := s.virsh.GetDomain(r.Context(), name, false)
	if err != nil {
		writeVirshError(w, err)
		return
	}
	network, addr, password, err := d.VNCTarget()
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	tok := s.tokens.issue(network, addr, d.Name)

	resp := map[string]any{
		"token":    tok,
		"domain":   d.Name,
		"state":    d.StateKey,
		"password": password,
		"target":   addr,
	}
	if s.cfg.WebsockifyURL != "" {
		if err := s.writeWebsockifyToken(tok, addr); err != nil {
			writeError(w, http.StatusInternalServerError, "cannot write websockify token: "+err.Error())
			return
		}
		u, err := url.Parse(s.cfg.WebsockifyURL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "bad websockify URL")
			return
		}
		q := u.Query()
		q.Set("token", tok)
		u.RawQuery = q.Encode()
		resp["ws_url"] = u.String()
		resp["external"] = true
	} else {
		// Relative path; the browser completes it with its own scheme/host.
		resp["ws_path"] = "ws/vnc/" + tok
		resp["external"] = false
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Networks / storage
// ---------------------------------------------------------------------------

func (s *Server) handleNetworks(w http.ResponseWriter, r *http.Request) {
	nets, err := s.virsh.Networks(r.Context())
	if err != nil {
		writeVirshError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nets)
}

func (s *Server) handlePools(w http.ResponseWriter, r *http.Request) {
	pools, err := s.virsh.Pools(r.Context())
	if err != nil {
		writeVirshError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pools)
}

func (s *Server) handleVolumes(w http.ResponseWriter, r *http.Request) {
	vols, err := s.virsh.Volumes(r.Context(), r.PathValue("pool"))
	if err != nil {
		writeVirshError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, vols)
}

func (s *Server) handleISOs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.virsh.ISOs(r.Context(), s.cfg.ISODir))
}

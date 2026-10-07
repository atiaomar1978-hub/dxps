package netsim

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

// ---------------------------------------------------------------- NRF (TS 29.510 OAuth2 client credentials)

func (n *Netsim) issue(scope string) string {
	claims, _ := json.Marshal(map[string]any{"scope": scope, "exp": time.Now().Add(time.Hour).Unix(), "iss": "nrf01"})
	p := base64.RawURLEncoding.EncodeToString(claims)
	m := hmac.New(sha256.New, n.tokKey)
	m.Write([]byte(p))
	return "nrf." + p + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (n *Netsim) validToken(r *http.Request, scope string) bool {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return false
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[0] != "nrf" {
		return false
	}
	m := hmac.New(sha256.New, n.tokKey)
	m.Write([]byte(parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, m.Sum(nil)) {
		return false
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Scope string `json:"scope"`
		Exp   int64  `json:"exp"`
	}
	return json.Unmarshal(b, &c) == nil && c.Exp > time.Now().Unix() && strings.Contains(" "+c.Scope+" ", " "+scope+" ")
}

func (n *Netsim) nrf(s *sim) {
	s.mux.HandleFunc("POST /oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "client_credentials" {
			writeJSON(w, 400, map[string]string{"error": "unsupported_grant_type"})
			return
		}
		scope := r.PostForm.Get("scope")
		if scope == "" || r.PostForm.Get("nfInstanceId") == "" {
			writeJSON(w, 400, map[string]string{"error": "invalid_request"})
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": n.issue(scope), "token_type": "Bearer", "expires_in": 3600, "scope": scope})
	})
	s.mux.HandleFunc("GET /nnrf-disc/v1/nf-instances", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"validityPeriod": 3600, "nfInstances": []map[string]any{{
			"nfInstanceId": "1b0b2f3a-0000-4000-8000-00000000d0a1", "nfType": r.URL.Query().Get("target-nf-type"),
			"nfStatus": "REGISTERED", "fqdn": "udr01.5gc.mnc077.mcc416.3gppnetwork.org"}}})
	})
}

// ---------------------------------------------------------------- UDR (TS 29.504 / 29.505 Nudr-dr)

var supiRe = regexp.MustCompile(`^imsi-[0-9]{15}$`)

func (n *Netsim) udr(s *sim) {
	resource := func(w http.ResponseWriter, r *http.Request, validate func(map[string]any) string) {
		if !n.validToken(r, "nudr-dr") {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			problem(w, 401, "UNAUTHORIZED", "missing or invalid NRF access token")
			return
		}
		supi := r.PathValue("supi")
		if !supiRe.MatchString(supi) {
			problem(w, 400, "INVALID_SUPI", "supi must be imsi-<15 digits>")
			return
		}
		t, key := tenantOf(r), r.URL.Path
		switch r.Method {
		case http.MethodGet:
			if v, ok := s.get(t, key); ok {
				w.Header().Set("Content-Type", "application/json")
				w.Write(v)
				return
			}
			problem(w, 404, "DATA_NOT_FOUND", "resource not found")
		case http.MethodDelete:
			if !s.del(t, key) {
				problem(w, 403, "OWNED_BY_OTHER_TENANT", "subscriber belongs to another tenant")
				return
			}
			w.WriteHeader(204)
		case http.MethodPut:
			var m map[string]any
			if err := decode(r, &m); err != nil {
				problem(w, 400, "INVALID_MSG_FORMAT", "invalid JSON")
				return
			}
			if msg := validate(m); msg != "" {
				problem(w, 400, "MANDATORY_IE_INCORRECT", msg)
				return
			}
			b, _ := json.Marshal(m)
			created, conflict := s.put(t, key, b)
			if conflict {
				problem(w, 409, "OWNED_BY_OTHER_TENANT", "subscriber belongs to another tenant")
				return
			}
			if created {
				w.Header().Set("Location", r.URL.Path)
				writeJSON(w, 201, m)
				return
			}
			w.WriteHeader(204)
		case http.MethodPatch:
			if ct := r.Header.Get("Content-Type"); ct != "application/merge-patch+json" {
				problem(w, 415, "UNSUPPORTED_MEDIA_TYPE", "use application/merge-patch+json")
				return
			}
			cur, ok := s.get(t, key)
			if !ok {
				problem(w, 404, "DATA_NOT_FOUND", "resource not found")
				return
			}
			var base, patch map[string]any
			_ = json.Unmarshal(cur, &base)
			if err := decode(r, &patch); err != nil {
				problem(w, 400, "INVALID_MSG_FORMAT", "invalid JSON")
				return
			}
			b, _ := json.Marshal(mergePatch(base, patch))
			s.put(t, key, b)
			w.WriteHeader(204)
		}
	}
	none := func(map[string]any) string { return "" }
	auth := func(m map[string]any) string {
		switch m["authenticationMethod"] {
		case "5G_AKA", "EAP_AKA_PRIME":
		default:
			return "authenticationMethod must be 5G_AKA or EAP_AKA_PRIME"
		}
		for _, k := range []string{"encPermanentKey", "encOpcKey"} {
			if v, _ := m[k].(string); !strings.HasPrefix(v, "hsm://") {
				return k + " must be an HSM reference (raw key material is not accepted)"
			}
		}
		return ""
	}
	am := func(m map[string]any) string {
		if _, ok := m["gpsis"].([]any); !ok {
			return "gpsis required"
		}
		return ""
	}
	base := "/nudr-dr/v2/subscription-data/{supi}"
	for _, p := range []string{"GET", "PUT", "DELETE"} {
		s.mux.HandleFunc(p+" "+base+"/authentication-data/authentication-subscription", func(w http.ResponseWriter, r *http.Request) { resource(w, r, auth) })
		s.mux.HandleFunc(p+" "+base+"/{plmn}/provisioned-data/smf-selection-subscription-data", func(w http.ResponseWriter, r *http.Request) { resource(w, r, none) })
		s.mux.HandleFunc(p+" /nudr-dr/v2/policy-data/ues/{supi}/am-data", func(w http.ResponseWriter, r *http.Request) { resource(w, r, none) })
	}
	for _, p := range []string{"GET", "PUT", "DELETE", "PATCH"} {
		s.mux.HandleFunc(p+" "+base+"/{plmn}/provisioned-data/am-data", func(w http.ResponseWriter, r *http.Request) { resource(w, r, am) })
	}
}

// mergePatch applies RFC 7396 JSON Merge Patch.
func mergePatch(base, patch map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for k, v := range patch {
		if v == nil {
			delete(base, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			bm, _ := base[k].(map[string]any)
			base[k] = mergePatch(bm, pm)
			continue
		}
		base[k] = v
	}
	return base
}

// ---------------------------------------------------------------- IMS provisioning gateway

func (n *Netsim) ims(s *sim) {
	need := map[string]string{"impu": "impi", "mmtel": "impu"}
	h := func(w http.ResponseWriter, r *http.Request) {
		t, supi, kind := tenantOf(r), r.PathValue("supi"), r.PathValue("kind")
		if !supiRe.MatchString(supi) || (kind != "impi" && kind != "impu" && kind != "mmtel") {
			problem(w, 404, "NOT_FOUND", "unknown resource")
			return
		}
		key := "/ims/" + supi + "/" + kind
		if r.Method == http.MethodDelete {
			s.del(t, key)
			w.WriteHeader(204)
			return
		}
		if dep, ok := need[kind]; ok {
			if _, ok := s.get(t, "/ims/"+supi+"/"+dep); !ok {
				problem(w, 409, "PRECONDITION", dep+" must be provisioned first")
				return
			}
		}
		var m map[string]any
		if err := decode(r, &m); err != nil {
			problem(w, 400, "INVALID", "invalid JSON")
			return
		}
		b, _ := json.Marshal(m)
		if _, conflict := s.put(t, key, b); conflict {
			problem(w, 409, "OWNED_BY_OTHER_TENANT", "identity belongs to another tenant")
			return
		}
		writeJSON(w, 200, map[string]any{"result": "OK", "resource": kind, "supi": supi})
	}
	s.mux.HandleFunc("PUT /ims-pg/v1/subscribers/{supi}/{kind}", h)
	s.mux.HandleFunc("DELETE /ims-pg/v1/subscribers/{supi}/{kind}", h)
}

// ---------------------------------------------------------------- RESTCONF (RFC 8040 / 8072) + USP (TR-369)

func (n *Netsim) restconf(s *sim) {
	yangPatch := func(module string, validate func(edit map[string]any) string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Content-Type") != "application/yang-patch+json" {
				problem(w, 415, "UNSUPPORTED_MEDIA_TYPE", "use application/yang-patch+json")
				return
			}
			var m map[string]any
			if err := decode(r, &m); err != nil {
				problem(w, 400, "malformed-message", "invalid JSON")
				return
			}
			yp, _ := m["ietf-yang-patch:yang-patch"].(map[string]any)
			edits, _ := yp["edit"].([]any)
			pid, _ := yp["patch-id"].(string)
			if yp == nil || len(edits) == 0 || pid == "" {
				problem(w, 400, "malformed-message", "ietf-yang-patch:yang-patch with patch-id and edit[] required")
				return
			}
			for _, e := range edits {
				em, _ := e.(map[string]any)
				if msg := validate(em); msg != "" {
					writeJSON(w, 409, map[string]any{"ietf-yang-patch:yang-patch-status": map[string]any{"patch-id": pid,
						"edit-status": map[string]any{"edit": []any{map[string]any{"edit-id": em["edit-id"], "errors": map[string]any{
							"error": []any{map[string]any{"error-type": "application", "error-tag": "invalid-value", "error-message": msg}}}}}}}})
					return
				}
				target, _ := em["target"].(string)
				b, _ := json.Marshal(em["value"])
				s.put(tenantOf(r), module+target, b)
			}
			writeJSON(w, 200, map[string]any{"ietf-yang-patch:yang-patch-status": map[string]any{"patch-id": pid, "ok": []any{nil}}})
		}
	}
	s.mux.HandleFunc("PATCH /restconf/data/ietf-l3vpn-svc:l3vpn-svc", yangPatch("l3vpn", func(e map[string]any) string {
		v, _ := e["value"].(map[string]any)
		sites, _ := v["ietf-l3vpn-svc:site"].([]any)
		if len(sites) == 0 {
			return "site list required"
		}
		site, _ := sites[0].(map[string]any)
		svc, _ := site["service"].(map[string]any)
		qos, _ := svc["qos"].(map[string]any)
		if bw, _ := qos["svc-input-bandwidth"].(float64); bw > 10000 {
			return "svc-input-bandwidth exceeds PE capacity (10000 Mbps)"
		}
		return ""
	}))
	s.mux.HandleFunc("PATCH /restconf/data/bbf-xpon-onu-states:onus", yangPatch("onu", func(e map[string]any) string {
		if e["operation"] != "merge" && e["operation"] != "create" {
			return "unsupported edit operation"
		}
		return ""
	}))
	del := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }
	s.mux.HandleFunc("DELETE /restconf/data/ietf-l3vpn-svc:l3vpn-svc/sites/{site}", del)
	s.mux.HandleFunc("DELETE /restconf/data/bbf-xpon-onu-states:onus/{onu}", del)
	s.mux.HandleFunc("POST /usp/v1/agents/{ep}/set", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		if err := decode(r, &m); err != nil {
			problem(w, 400, "INVALID", "invalid USP message")
			return
		}
		hdr, _ := m["header"].(map[string]any)
		if hdr["msgType"] != "SET" {
			problem(w, 400, "INVALID", "msgType must be SET")
			return
		}
		resp := map[string]any{"header": map[string]any{"msgId": hdr["msgId"], "msgType": "SET_RESP"}}
		if strings.Contains(r.PathValue("ep"), "OFFLINE") {
			resp["body"] = map[string]any{"error": map[string]any{"errCode": 7002, "errMsg": "agent not reachable"}}
		} else {
			resp["body"] = map[string]any{"response": map[string]any{"setResp": map[string]any{"updatedObjResults": []any{
				map[string]any{"requestedPath": "Device.WiFi.SSID.1.", "operStatus": map[string]any{"operSuccess": map[string]any{}}}}}}}
		}
		writeJSON(w, 200, resp)
	})
}

// ---------------------------------------------------------------- SM-DP+ (GSMA SGP.22 ES2+)

func es2(status string, extra map[string]any, scd map[string]any) map[string]any {
	fes := map[string]any{"status": status}
	if scd != nil {
		fes["statusCodeData"] = scd
	}
	out := map[string]any{"header": map[string]any{"functionExecutionStatus": fes}}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

var eidRe = regexp.MustCompile(`^[0-9]{32}$`)

func (n *Netsim) smdp(s *sim) {
	guard := func(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
		if !strings.HasPrefix(r.Header.Get("X-Admin-Protocol"), "gsma/rsp/") {
			problem(w, 400, "INVALID", "X-Admin-Protocol header required")
			return nil, false
		}
		var m map[string]any
		if err := decode(r, &m); err != nil {
			problem(w, 400, "INVALID", "invalid JSON")
			return nil, false
		}
		return m, true
	}
	s.mux.HandleFunc("POST /gsma/rsp2/es2plus/downloadOrder", func(w http.ResponseWriter, r *http.Request) {
		m, ok := guard(w, r)
		if !ok {
			return
		}
		eid, _ := m["eid"].(string)
		if !eidRe.MatchString(eid) || strings.HasSuffix(eid, "998") {
			writeJSON(w, 200, es2("Failed", nil, map[string]any{"subjectCode": "8.1.1", "reasonCode": "3.8", "message": "EID not known / not eligible"}))
			return
		}
		iccid := "8949" + eid[len(eid)-15:]
		s.put(tenantOf(r), "/order/"+iccid, []byte(`{"state":"allocated"}`))
		writeJSON(w, 200, es2("Executed-Success", map[string]any{"iccid": iccid}, nil))
	})
	s.mux.HandleFunc("POST /gsma/rsp2/es2plus/confirmOrder", func(w http.ResponseWriter, r *http.Request) {
		m, ok := guard(w, r)
		if !ok {
			return
		}
		iccid, _ := m["iccid"].(string)
		if _, ok := s.get(tenantOf(r), "/order/"+iccid); !ok {
			writeJSON(w, 200, es2("Failed", nil, map[string]any{"subjectCode": "8.2.1", "reasonCode": "3.9", "message": "ICCID unknown"}))
			return
		}
		s.put(tenantOf(r), "/order/"+iccid, []byte(`{"state":"released"}`))
		writeJSON(w, 200, es2("Executed-Success", map[string]any{"eid": m["eid"], "smdpAddress": "smdp.dxps.local",
			"matchingId": fmt.Sprintf("MID-%s", iccid[len(iccid)-8:])}, nil))
	})
	s.mux.HandleFunc("POST /gsma/rsp2/es2plus/cancelOrder", func(w http.ResponseWriter, r *http.Request) {
		m, ok := guard(w, r)
		if !ok {
			return
		}
		iccid, _ := m["iccid"].(string)
		s.del(tenantOf(r), "/order/"+iccid)
		writeJSON(w, 200, es2("Executed-Success", nil, nil))
	})
}

// ---------------------------------------------------------------- OCS / BSS (TMF666 / TMF637)

func charValue(m map[string]any, name string) string {
	cs, _ := m["characteristic"].([]any)
	for _, c := range cs {
		cm, _ := c.(map[string]any)
		if cm["name"] == name {
			return fmt.Sprint(cm["value"])
		}
	}
	return ""
}

func (n *Netsim) ocs(s *sim) {
	create := func(kind string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rid := r.Header.Get("X-Request-ID")
			if e, ok := s.replay(rid); ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(e.Status)
				w.Write(e.Response)
				return
			}
			var m map[string]any
			if err := decode(r, &m); err != nil {
				problem(w, 400, "INVALID", "invalid JSON")
				return
			}
			msisdn := charValue(m, "msisdn")
			if msisdn == "" {
				writeJSON(w, 400, map[string]any{"code": "MISSING_MSISDN", "reason": "characteristic msisdn required", "@type": "Error"})
				return
			}
			id := kind + "-" + msisdn
			if kind == "prd" {
				id = fmt.Sprintf("prd-%s-%d", msisdn, time.Now().UnixNano()%100000)
			}
			if _, exists := s.get(tenantOf(r), "/"+kind+"/"+msisdn); exists && kind == "acc" {
				writeJSON(w, 409, map[string]any{"code": "ALREADY_EXISTS", "reason": "billing account exists", "@type": "Error"})
				return
			}
			m["id"], m["href"], m["state"] = id, "/tmf-api/"+kind+"/"+id, "active"
			b, _ := json.Marshal(m)
			if _, conflict := s.put(tenantOf(r), "/"+kind+"/"+msisdn, b); conflict {
				writeJSON(w, 409, map[string]any{"code": "OWNED_BY_OTHER_TENANT", "reason": "msisdn belongs to another brand", "@type": "Error"})
				return
			}
			s.remember(rid, 201, m)
			writeJSON(w, 201, m)
		}
	}
	s.mux.HandleFunc("POST /tmf-api/accountManagement/v5/billingAccount", create("acc"))
	s.mux.HandleFunc("POST /tmf-api/productInventory/v5/product", create("prd"))
	s.mux.HandleFunc("DELETE /tmf-api/accountManagement/v5/billingAccount/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.del(tenantOf(r), "/acc/"+strings.TrimPrefix(r.PathValue("id"), "acc-"))
		w.WriteHeader(204)
	})
}

// ---------------------------------------------------------------- NPDB

func (n *Netsim) npdb(s *sim) {
	s.mux.HandleFunc("PUT /npdb/v1/numbers/{msisdn}", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		if err := decode(r, &m); err != nil || m["msisdn"] != r.PathValue("msisdn") {
			problem(w, 400, "INVALID", "body msisdn must match path")
			return
		}
		b, _ := json.Marshal(m)
		s.put("npdb", "/n/"+r.PathValue("msisdn"), b)
		writeJSON(w, 200, map[string]any{"msisdn": m["msisdn"], "routingNumber": m["routingNumber"], "status": "PORTED_IN"})
	})
	s.mux.HandleFunc("DELETE /npdb/v1/numbers/{msisdn}", func(w http.ResponseWriter, r *http.Request) {
		s.del("npdb", "/n/"+r.PathValue("msisdn"))
		w.WriteHeader(204)
	})
}

// ---------------------------------------------------------------- NEF / CAMARA QoD

func (n *Netsim) nef(s *sim) {
	s.mux.HandleFunc("POST /quality-on-demand/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		if err := decode(r, &m); err != nil {
			problem(w, 400, "INVALID_ARGUMENT", "invalid JSON")
			return
		}
		dev, _ := m["device"].(map[string]any)
		phone, _ := dev["phoneNumber"].(string)
		if !strings.HasPrefix(phone, "+") || m["qosProfile"] == nil {
			writeJSON(w, 400, map[string]any{"status": 400, "code": "INVALID_ARGUMENT", "message": "device.phoneNumber and qosProfile required"})
			return
		}
		id := fmt.Sprintf("qod-%d", time.Now().UnixNano())
		m["sessionId"], m["qosStatus"] = id, "AVAILABLE"
		b, _ := json.Marshal(m)
		s.put(tenantOf(r), "/qod/"+strings.TrimPrefix(phone, "+"), b)
		writeJSON(w, 201, m)
	})
	s.mux.HandleFunc("DELETE /quality-on-demand/v1/sessions/msisdn/{msisdn}", func(w http.ResponseWriter, r *http.Request) {
		s.del(tenantOf(r), "/qod/"+r.PathValue("msisdn"))
		w.WriteHeader(204)
	})
}

// ---------------------------------------------------------------- TMF688 webhook sink (MVNO listener)

type SinkEvent struct {
	At        time.Time       `json:"at"`
	Path      string          `json:"path"`
	Tenant    string          `json:"tenant"`
	EventID   string          `json:"eventId"`
	Signature string          `json:"signature"`
	Body      json.RawMessage `json:"body"`
}

func (n *Netsim) sink(s *sim) {
	s.mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := decode(r, &raw); err != nil {
			problem(w, 400, "INVALID", "invalid JSON")
			return
		}
		ev := SinkEvent{At: time.Now(), Path: r.URL.Path, Tenant: r.Header.Get("X-DxPS-Tenant"), EventID: r.Header.Get("X-DxPS-Event-Id"),
			Signature: r.Header.Get("X-DxPS-Signature"), Body: raw}
		b, _ := json.Marshal(ev)
		s.put("sink", fmt.Sprintf("/ev/%d", time.Now().UnixNano()), b)
		w.WriteHeader(204)
	})
}

// SinkEvents returns the webhook deliveries received by the sink (newest last).
func (n *Netsim) SinkEvents() []SinkEvent {
	s := n.byName["hub-sink"]
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SinkEvent
	for k, v := range s.store {
		if strings.HasPrefix(k, "sink|/ev/") {
			var e SinkEvent
			if json.Unmarshal(v, &e) == nil {
				out = append(out, e)
			}
		}
	}
	sortEvents(out)
	return out
}

func sortEvents(es []SinkEvent) {
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && es[j].At.Before(es[j-1].At); j-- {
			es[j], es[j-1] = es[j-1], es[j]
		}
	}
}

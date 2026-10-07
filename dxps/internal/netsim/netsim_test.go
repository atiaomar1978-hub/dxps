package netsim

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"dxps/internal/testenv"
)

const (
	supi   = "imsi-416770000000101"
	tenant = "mvno-alpha"
)

func newNS(t *testing.T) *Netsim {
	t.Helper()
	n := New("localhost", "admin-token")
	for _, s := range n.sims {
		s.latency = 0
	}
	return n
}

type call struct {
	method, path, body, ct, tenant string
	hdr                            map[string]string
}

func do(t *testing.T, n *Netsim, sim string, c call) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	if c.ct == "" {
		c.ct = "application/json"
	}
	req.Header.Set("Content-Type", c.ct)
	if c.tenant == "" {
		c.tenant = tenant
	}
	req.Header.Set("X-DxPS-Tenant", c.tenant)
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	n.Handler(sim).ServeHTTP(rr, req)
	var m map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	return rr, m
}

func token(t *testing.T, n *Netsim, scope string) string {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {scope}, "nfInstanceId": {"dxps-adapter"}}
	rr, m := do(t, n, "nrf", call{method: "POST", path: "/oauth2/token", body: form.Encode(), ct: "application/x-www-form-urlencoded"})
	if rr.Code != 200 {
		t.Fatalf("token: %d %s", rr.Code, rr.Body)
	}
	return "Bearer " + m["access_token"].(string)
}

// TC-SIM-001: NRF issues client_credentials tokens and rejects malformed grant requests; discovery answers.
func TestNRF(t *testing.T) {
	n := newNS(t)
	if tok := token(t, n, "nudr-dr"); !strings.HasPrefix(tok, "Bearer nrf.") {
		t.Fatal(tok)
	}
	for _, body := range []string{"grant_type=password&scope=x&nfInstanceId=a", "grant_type=client_credentials&nfInstanceId=a", "grant_type=client_credentials&scope=x"} {
		if rr, _ := do(t, n, "nrf", call{method: "POST", path: "/oauth2/token", body: body, ct: "application/x-www-form-urlencoded"}); rr.Code != 400 {
			t.Fatalf("%s -> %d", body, rr.Code)
		}
	}
	rr, m := do(t, n, "nrf", call{method: "GET", path: "/nnrf-disc/v1/nf-instances?target-nf-type=UDR"})
	if rr.Code != 200 || m["nfInstances"].([]any)[0].(map[string]any)["nfType"] != "UDR" {
		t.Fatal(rr.Body)
	}
}

// TC-SIM-002: UDR rejects missing, forged, expired-scope and wrong-scope bearer tokens (401).
func TestUDRAuth(t *testing.T) {
	n := newNS(t)
	p := "/nudr-dr/v2/subscription-data/" + supi + "/authentication-data/authentication-subscription"
	other := New("x", "y") // tokens signed by a different NRF key
	bad := []string{"", "Basic abc", "Bearer nrf.a", "Bearer x.y.z", "Bearer nrf.e30.!!", token(t, n, "nnef"), token(t, other, "nudr-dr")}
	for _, a := range bad {
		rr, _ := do(t, n, "udr", call{method: "GET", path: p, hdr: map[string]string{"Authorization": a}})
		if rr.Code != 401 || rr.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("auth %q -> %d", a, rr.Code)
		}
	}
}

// TC-SIM-003: UDR auth-subscription validates AKA method and HSM key references; PUT 201 then 204; GET; DELETE.
func TestUDRAuthSubscription(t *testing.T) {
	n := newNS(t)
	tok := token(t, n, "nudr-dr")
	h := map[string]string{"Authorization": tok}
	p := "/nudr-dr/v2/subscription-data/" + supi + "/authentication-data/authentication-subscription"
	good := `{"authenticationMethod":"5G_AKA","encPermanentKey":"hsm://k/1","encOpcKey":"hsm://k/2"}`
	cases := []struct {
		body string
		code int
	}{
		{`{"authenticationMethod":"MILENAGE","encPermanentKey":"hsm://a","encOpcKey":"hsm://b"}`, 400},
		{`{"authenticationMethod":"5G_AKA","encPermanentKey":"00112233","encOpcKey":"hsm://b"}`, 400},
		{`{not json`, 400},
		{good, 201},
		{good, 204},
	}
	for _, c := range cases {
		if rr, _ := do(t, n, "udr", call{method: "PUT", path: p, body: c.body, hdr: h}); rr.Code != c.code {
			t.Fatalf("%s -> %d %s", c.body, rr.Code, rr.Body)
		}
	}
	if rr, m := do(t, n, "udr", call{method: "GET", path: p, hdr: h}); rr.Code != 200 || m["authenticationMethod"] != "5G_AKA" {
		t.Fatal(rr.Body)
	}
	if rr, _ := do(t, n, "udr", call{method: "GET", path: "/nudr-dr/v2/subscription-data/imsi-12/authentication-data/authentication-subscription", hdr: h}); rr.Code != 400 {
		t.Fatal("invalid supi accepted")
	}
	if rr, _ := do(t, n, "udr", call{method: "DELETE", path: p, hdr: h}); rr.Code != 204 {
		t.Fatal(rr.Code)
	}
	if rr, _ := do(t, n, "udr", call{method: "GET", path: p, hdr: h}); rr.Code != 404 {
		t.Fatal(rr.Code)
	}
}

// TC-SIM-004: shared UDR isolates tenants: another tenant cannot overwrite (409) or delete (403) a subscriber.
func TestUDRTenantIsolation(t *testing.T) {
	n := newNS(t)
	h := map[string]string{"Authorization": token(t, n, "nudr-dr")}
	p := "/nudr-dr/v2/subscription-data/" + supi + "/41677/provisioned-data/am-data"
	body := `{"gpsis":["msisdn-14165550101"]}`
	if rr, _ := do(t, n, "udr", call{method: "PUT", path: p, body: body, hdr: h}); rr.Code != 201 {
		t.Fatal(rr.Code)
	}
	if rr, _ := do(t, n, "udr", call{method: "PUT", path: p, body: body, hdr: h, tenant: "mvno-beta"}); rr.Code != 409 {
		t.Fatal("cross-tenant overwrite", rr.Code)
	}
	if rr, _ := do(t, n, "udr", call{method: "DELETE", path: p, hdr: h, tenant: "mvno-beta"}); rr.Code != 403 {
		t.Fatal("cross-tenant delete", rr.Code)
	}
	if rr, _ := do(t, n, "udr", call{method: "GET", path: p, hdr: h, tenant: "mvno-beta"}); rr.Code != 404 {
		t.Fatal("cross-tenant read", rr.Code)
	}
	if rr, _ := do(t, n, "udr", call{method: "PUT", path: p, body: `{"x":1}`, hdr: h}); rr.Code != 400 {
		t.Fatal("am-data without gpsis", rr.Code)
	}
}

// TC-SIM-005: UDR PATCH applies RFC 7396 merge patch and requires application/merge-patch+json.
func TestUDRMergePatch(t *testing.T) {
	n := newNS(t)
	h := map[string]string{"Authorization": token(t, n, "nudr-dr")}
	p := "/nudr-dr/v2/subscription-data/" + supi + "/41677/provisioned-data/am-data"
	if rr, _ := do(t, n, "udr", call{method: "PATCH", path: p, body: `{}`, ct: "application/merge-patch+json", hdr: h}); rr.Code != 404 {
		t.Fatal("patch on missing", rr.Code)
	}
	do(t, n, "udr", call{method: "PUT", path: p, body: `{"gpsis":["msisdn-1"],"subscribedUeAmbr":{"uplink":"1 Gbps","downlink":"2 Gbps"},"rfsp":1}`, hdr: h})
	if rr, _ := do(t, n, "udr", call{method: "PATCH", path: p, body: `{}`, hdr: h}); rr.Code != 415 {
		t.Fatal("wrong content type", rr.Code)
	}
	if rr, _ := do(t, n, "udr", call{method: "PATCH", path: p, body: `{bad`, ct: "application/merge-patch+json", hdr: h}); rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	patch := `{"subscribedUeAmbr":{"downlink":"500 Mbps"},"rfsp":null}`
	if rr, _ := do(t, n, "udr", call{method: "PATCH", path: p, body: patch, ct: "application/merge-patch+json", hdr: h}); rr.Code != 204 {
		t.Fatal(rr.Code)
	}
	_, m := do(t, n, "udr", call{method: "GET", path: p, hdr: h})
	ambr := m["subscribedUeAmbr"].(map[string]any)
	if ambr["downlink"] != "500 Mbps" || ambr["uplink"] != "1 Gbps" || m["rfsp"] != nil {
		t.Fatal(m)
	}
	if got := mergePatch(nil, map[string]any{"a": 1}); got["a"] != 1 {
		t.Fatal(got)
	}
}

// TC-SIM-006: other UDR resources (SMF selection, policy am-data) accept PUT without extra validation.
func TestUDROtherResources(t *testing.T) {
	n := newNS(t)
	h := map[string]string{"Authorization": token(t, n, "nudr-dr")}
	for _, p := range []string{
		"/nudr-dr/v2/subscription-data/" + supi + "/41677/provisioned-data/smf-selection-subscription-data",
		"/nudr-dr/v2/policy-data/ues/" + supi + "/am-data",
	} {
		if rr, _ := do(t, n, "udr", call{method: "PUT", path: p, body: `{"a":1}`, hdr: h}); rr.Code != 201 {
			t.Fatal(p, rr.Code)
		}
	}
}

// TC-SIM-007: IMS-PG enforces IMPI -> IMPU -> MMTEL ordering (409 PRECONDITION) and tenant ownership.
func TestIMS(t *testing.T) {
	n := newNS(t)
	base := "/ims-pg/v1/subscribers/" + supi + "/"
	if rr, m := do(t, n, "ims-pg", call{method: "PUT", path: base + "impu", body: `{}`}); rr.Code != 409 || m["cause"] != "PRECONDITION" {
		t.Fatal(rr.Code)
	}
	for _, k := range []string{"impi", "impu", "mmtel"} {
		if rr, _ := do(t, n, "ims-pg", call{method: "PUT", path: base + k, body: `{"k":"` + k + `"}`}); rr.Code != 200 {
			t.Fatal(k, rr.Code)
		}
	}
	if rr, _ := do(t, n, "ims-pg", call{method: "PUT", path: base + "impi", body: `{}`, tenant: "mvno-beta"}); rr.Code != 409 {
		t.Fatal("cross-tenant", rr.Code)
	}
	if rr, _ := do(t, n, "ims-pg", call{method: "PUT", path: base + "impi", body: `{x`}); rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	if rr, _ := do(t, n, "ims-pg", call{method: "PUT", path: base + "volte", body: `{}`}); rr.Code != 404 {
		t.Fatal(rr.Code)
	}
	if rr, _ := do(t, n, "ims-pg", call{method: "DELETE", path: base + "mmtel"}); rr.Code != 204 {
		t.Fatal(rr.Code)
	}
}

func yp(bw float64, op string) string {
	b, _ := json.Marshal(map[string]any{"ietf-yang-patch:yang-patch": map[string]any{"patch-id": "p1", "edit": []any{map[string]any{
		"edit-id": "e1", "operation": op, "target": "/site=S1",
		"value": map[string]any{"ietf-l3vpn-svc:site": []any{map[string]any{"site-id": "S1", "service": map[string]any{"qos": map[string]any{"svc-input-bandwidth": bw}}}}}}}}})
	return string(b)
}

// TC-SIM-008: RESTCONF YANG-Patch (RFC 8072): media type, structure, capacity check (409 yang-patch-status).
func TestRESTCONF(t *testing.T) {
	n := newNS(t)
	l3 := "/restconf/data/ietf-l3vpn-svc:l3vpn-svc"
	ct := "application/yang-patch+json"
	if rr, _ := do(t, n, "restconf", call{method: "PATCH", path: l3, body: yp(100, "create")}); rr.Code != 415 {
		t.Fatal(rr.Code)
	}
	if rr, _ := do(t, n, "restconf", call{method: "PATCH", path: l3, body: `{x`, ct: ct}); rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	if rr, _ := do(t, n, "restconf", call{method: "PATCH", path: l3, body: `{"ietf-yang-patch:yang-patch":{"edit":[]}}`, ct: ct}); rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	if rr, m := do(t, n, "restconf", call{method: "PATCH", path: l3, body: yp(100, "create"), ct: ct}); rr.Code != 200 || m["ietf-yang-patch:yang-patch-status"] == nil {
		t.Fatal(rr.Code, rr.Body)
	}
	if rr, _ := do(t, n, "restconf", call{method: "PATCH", path: l3, body: yp(20000, "create"), ct: ct}); rr.Code != 409 || !strings.Contains(rr.Body.String(), "invalid-value") {
		t.Fatal(rr.Code)
	}
	empty := `{"ietf-yang-patch:yang-patch":{"patch-id":"p","edit":[{"edit-id":"e","operation":"create","value":{}}]}}`
	if rr, _ := do(t, n, "restconf", call{method: "PATCH", path: l3, body: empty, ct: ct}); rr.Code != 409 {
		t.Fatal(rr.Code)
	}
	onu := "/restconf/data/bbf-xpon-onu-states:onus"
	if rr, _ := do(t, n, "restconf", call{method: "PATCH", path: onu, body: yp(1, "merge"), ct: ct}); rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	if rr, _ := do(t, n, "restconf", call{method: "PATCH", path: onu, body: yp(1, "remove"), ct: ct}); rr.Code != 409 {
		t.Fatal(rr.Code)
	}
	for _, p := range []string{l3 + "/sites/S1", onu + "/ONU1"} {
		if rr, _ := do(t, n, "restconf", call{method: "DELETE", path: p}); rr.Code != 204 {
			t.Fatal(p, rr.Code)
		}
	}
}

// TC-SIM-009: USP (TR-369) Set returns SET_RESP; offline agents report errCode 7002; bad messages 400.
func TestUSP(t *testing.T) {
	n := newNS(t)
	msg := `{"header":{"msgId":"m1","msgType":"SET"},"body":{}}`
	_, m := do(t, n, "restconf", call{method: "POST", path: "/usp/v1/agents/os::0001-ABC/set", body: msg})
	if m["header"].(map[string]any)["msgType"] != "SET_RESP" || m["body"].(map[string]any)["response"] == nil {
		t.Fatal(m)
	}
	_, m = do(t, n, "restconf", call{method: "POST", path: "/usp/v1/agents/os::OFFLINE-1/set", body: msg})
	if m["body"].(map[string]any)["error"].(map[string]any)["errCode"] != float64(7002) {
		t.Fatal(m)
	}
	for _, b := range []string{`{x`, `{"header":{"msgType":"GET"}}`} {
		if rr, _ := do(t, n, "restconf", call{method: "POST", path: "/usp/v1/agents/a/set", body: b}); rr.Code != 400 {
			t.Fatal(b, rr.Code)
		}
	}
}

// TC-SIM-010: SM-DP+ ES2+ download/confirm/cancel flow, protocol header and business errors in 200 responses.
func TestSMDP(t *testing.T) {
	n := newNS(t)
	h := map[string]string{"X-Admin-Protocol": "gsma/rsp/v2.5.0"}
	eid := "89049032000000000000000000000101"
	status := func(m map[string]any) string {
		return m["header"].(map[string]any)["functionExecutionStatus"].(map[string]any)["status"].(string)
	}
	if rr, _ := do(t, n, "smdp", call{method: "POST", path: "/gsma/rsp2/es2plus/downloadOrder", body: `{"eid":"` + eid + `"}`}); rr.Code != 400 {
		t.Fatal("missing X-Admin-Protocol", rr.Code)
	}
	if rr, _ := do(t, n, "smdp", call{method: "POST", path: "/gsma/rsp2/es2plus/downloadOrder", body: `{x`, hdr: h}); rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	_, m := do(t, n, "smdp", call{method: "POST", path: "/gsma/rsp2/es2plus/downloadOrder", body: `{"eid":"` + eid + `"}`, hdr: h})
	iccid, _ := m["iccid"].(string)
	if status(m) != "Executed-Success" || iccid != "8949"+eid[17:] {
		t.Fatal(m)
	}
	_, m = do(t, n, "smdp", call{method: "POST", path: "/gsma/rsp2/es2plus/confirmOrder", body: `{"iccid":"` + iccid + `","eid":"` + eid + `"}`, hdr: h})
	if status(m) != "Executed-Success" || !strings.HasPrefix(m["matchingId"].(string), "MID-") {
		t.Fatal(m)
	}
	_, m = do(t, n, "smdp", call{method: "POST", path: "/gsma/rsp2/es2plus/confirmOrder", body: `{"iccid":"0000"}`, hdr: h})
	if status(m) != "Failed" {
		t.Fatal(m)
	}
	for _, bad := range []string{"123", "89049032000000000000000000000998"} {
		rr, m := do(t, n, "smdp", call{method: "POST", path: "/gsma/rsp2/es2plus/downloadOrder", body: `{"eid":"` + bad + `"}`, hdr: h})
		if rr.Code != 200 || status(m) != "Failed" {
			t.Fatal(bad, m)
		}
	}
	_, m = do(t, n, "smdp", call{method: "POST", path: "/gsma/rsp2/es2plus/cancelOrder", body: `{"iccid":"` + iccid + `"}`, hdr: h})
	if status(m) != "Executed-Success" {
		t.Fatal(m)
	}
	for _, p := range []string{"confirmOrder", "cancelOrder"} {
		if rr, _ := do(t, n, "smdp", call{method: "POST", path: "/gsma/rsp2/es2plus/" + p, body: `{}`}); rr.Code != 400 {
			t.Fatal(p, rr.Code)
		}
	}
}

// TC-SIM-011: OCS/BSS TMF666/637: create, X-Request-ID replay, ALREADY_EXISTS, missing msisdn, brand isolation.
func TestOCS(t *testing.T) {
	n := newNS(t)
	acc := "/tmf-api/accountManagement/v5/billingAccount"
	body := `{"name":"acct","characteristic":[{"name":"msisdn","value":"14165550101"}]}`
	rr, m := do(t, n, "ocs-bss", call{method: "POST", path: acc, body: body, hdr: map[string]string{"X-Request-ID": "r1"}})
	if rr.Code != 201 || m["id"] != "acc-14165550101" {
		t.Fatal(rr.Code, m)
	}
	if rr, m := do(t, n, "ocs-bss", call{method: "POST", path: acc, body: body, hdr: map[string]string{"X-Request-ID": "r1"}}); rr.Code != 201 || m["id"] != "acc-14165550101" {
		t.Fatal("replay", rr.Code)
	}
	if rr, m := do(t, n, "ocs-bss", call{method: "POST", path: acc, body: body}); rr.Code != 409 || m["code"] != "ALREADY_EXISTS" {
		t.Fatal(rr.Code, m)
	}
	if rr, m := do(t, n, "ocs-bss", call{method: "POST", path: "/tmf-api/productInventory/v5/product", body: body}); rr.Code != 201 || !strings.HasPrefix(m["id"].(string), "prd-14165550101-") {
		t.Fatal(rr.Code, m)
	}
	if rr, m := do(t, n, "ocs-bss", call{method: "POST", path: "/tmf-api/productInventory/v5/product", body: body, tenant: "mvno-beta"}); rr.Code != 409 || m["code"] != "OWNED_BY_OTHER_TENANT" {
		t.Fatal(rr.Code, m)
	}
	if rr, _ := do(t, n, "ocs-bss", call{method: "POST", path: acc, body: `{"characteristic":[]}`}); rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	if rr, _ := do(t, n, "ocs-bss", call{method: "POST", path: acc, body: `{x`}); rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	if rr, _ := do(t, n, "ocs-bss", call{method: "DELETE", path: acc + "/acc-14165550101"}); rr.Code != 204 {
		t.Fatal(rr.Code)
	}
	if rr, _ := do(t, n, "ocs-bss", call{method: "POST", path: acc, body: body}); rr.Code != 201 {
		t.Fatal("recreate after delete", rr.Code)
	}
}

// TC-SIM-012: NPDB port-in requires body msisdn == path msisdn; delete.
func TestNPDB(t *testing.T) {
	n := newNS(t)
	p := "/npdb/v1/numbers/14165550101"
	if rr, m := do(t, n, "npdb", call{method: "PUT", path: p, body: `{"msisdn":"14165550101","routingNumber":"D077"}`}); rr.Code != 200 || m["status"] != "PORTED_IN" {
		t.Fatal(rr.Code)
	}
	if rr, _ := do(t, n, "npdb", call{method: "PUT", path: p, body: `{"msisdn":"1"}`}); rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	if rr, _ := do(t, n, "npdb", call{method: "DELETE", path: p}); rr.Code != 204 {
		t.Fatal(rr.Code)
	}
}

// TC-SIM-013: NEF CAMARA QoD session create requires E.164 phone and qosProfile; delete by msisdn.
func TestNEF(t *testing.T) {
	n := newNS(t)
	p := "/quality-on-demand/v1/sessions"
	rr, m := do(t, n, "nef", call{method: "POST", path: p, body: `{"device":{"phoneNumber":"+14165550101"},"qosProfile":"QOS_L","duration":600}`})
	if rr.Code != 201 || m["qosStatus"] != "AVAILABLE" || !strings.HasPrefix(m["sessionId"].(string), "qod-") {
		t.Fatal(rr.Code, m)
	}
	for _, b := range []string{`{x`, `{"device":{"phoneNumber":"14165550101"},"qosProfile":"Q"}`, `{"device":{"phoneNumber":"+1"}}`} {
		if rr, _ := do(t, n, "nef", call{method: "POST", path: p, body: b}); rr.Code != 400 {
			t.Fatal(b, rr.Code)
		}
	}
	if rr, _ := do(t, n, "nef", call{method: "DELETE", path: p + "/msisdn/14165550101"}); rr.Code != 204 {
		t.Fatal(rr.Code)
	}
}

// TC-SIM-014: webhook sink records TMF688 deliveries with tenant, event id and signature (ordered by time).
func TestSink(t *testing.T) {
	n := newNS(t)
	for i, id := range []string{"e1", "e2"} {
		rr, _ := do(t, n, "hub-sink", call{method: "POST", path: "/hooks/" + tenant, body: `{"eventType":"ServiceOrderStateChangeEvent","i":` + string(rune('0'+i)) + `}`,
			hdr: map[string]string{"X-DxPS-Event-Id": id, "X-DxPS-Signature": "sha256=ab"}})
		if rr.Code != 204 {
			t.Fatal(rr.Code)
		}
		time.Sleep(time.Millisecond)
	}
	if rr, _ := do(t, n, "hub-sink", call{method: "POST", path: "/hooks/x", body: `{x`}); rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	ev := n.SinkEvents()
	if len(ev) != 2 || ev[0].EventID != "e1" || ev[1].EventID != "e2" || ev[0].Tenant != tenant || ev[0].Signature == "" {
		t.Fatal(ev)
	}
	es := []SinkEvent{{At: time.Unix(3, 0)}, {At: time.Unix(1, 0)}, {At: time.Unix(2, 0)}}
	sortEvents(es)
	if es[0].At.Unix() != 1 || es[2].At.Unix() != 3 {
		t.Fatal(es)
	}
}

// TC-SIM-015: fault injection by key suffix: 999 -> 503 + Retry-After, 998 -> 400, 997 -> two 503s then success.
func TestFaultInjection(t *testing.T) {
	n := newNS(t)
	p := "/npdb/v1/numbers/"
	rr, _ := do(t, n, "npdb", call{method: "PUT", path: p + "14165550999", body: `{"msisdn":"14165550999"}`})
	if rr.Code != 503 || rr.Header().Get("Retry-After") == "" {
		t.Fatal(rr.Code)
	}
	if rr, m := do(t, n, "npdb", call{method: "PUT", path: p + "14165550998", body: `{"msisdn":"14165550998"}`}); rr.Code != 400 || m["cause"] != "MANDATORY_IE_INCORRECT" {
		t.Fatal(rr.Code)
	}
	for i, want := range []int{503, 503, 200, 200} {
		if rr, _ := do(t, n, "npdb", call{method: "PUT", path: p + "14165550997", body: `{"msisdn":"14165550997"}`}); rr.Code != want {
			t.Fatalf("flaky attempt %d -> %d", i, rr.Code)
		}
	}
	// NRF and the sink are never faulted.
	if faultFor("/x/12345678999", nil) != "unavailable" || faultFor("/x/1", []byte(`"12345678997"`)) != "flaky" || faultFor("/x/1234", nil) != "" {
		t.Fatal("faultFor")
	}
	st := statsOf(n, "npdb")
	if st.Faults != 4 || st.Calls != 6 || st.ByStatus[503] != 3 {
		t.Fatalf("%+v", st)
	}
}

// TC-SIM-016: configured random error rate makes every call fail with 503 when set to 1.0.
func TestRandomErrors(t *testing.T) {
	n := newNS(t)
	n.byName["nef"].errRate = 1
	if rr, _ := do(t, n, "nef", call{method: "POST", path: "/quality-on-demand/v1/sessions", body: `{}`}); rr.Code != 503 {
		t.Fatal(rr.Code)
	}
}

// TC-SIM-017: request bodies above 256 KiB are rejected with 413 before reaching the simulator.
func TestBodyLimit(t *testing.T) {
	n := newNS(t)
	big := `{"x":"` + strings.Repeat("a", maxBody) + `"}`
	if rr, _ := do(t, n, "nef", call{method: "POST", path: "/quality-on-demand/v1/sessions", body: big}); rr.Code != 413 {
		t.Fatal(rr.Code)
	}
}

// TC-SIM-018: exchange log captures compacted request/response payloads, tenant, status; bounded to 300.
func TestExchangeLog(t *testing.T) {
	n := newNS(t)
	do(t, n, "npdb", call{method: "PUT", path: "/npdb/v1/numbers/1", body: "{ \"msisdn\" : \"1\" }"})
	ex := n.Exchanges(1)
	if len(ex) != 1 || string(ex[0].Request) != `{"msisdn":"1"}` || ex[0].Tenant != tenant || ex[0].Status != 200 || ex[0].Sim != "npdb" {
		t.Fatalf("%+v", ex)
	}
	for i := 0; i < 310; i++ {
		do(t, n, "npdb", call{method: "DELETE", path: "/npdb/v1/numbers/1"})
	}
	if len(n.Exchanges(0)) != 300 || len(n.Exchanges(5)) != 5 {
		t.Fatal(len(n.Exchanges(0)))
	}
	if compactJSON(nil) != nil || !strings.Contains(string(compactJSON([]byte("not json"))), "not json") {
		t.Fatal("compactJSON")
	}
	if c := compactJSON([]byte(`"` + strings.Repeat("a", 3000) + `"`)); len(c) > 2100 {
		t.Fatal(len(c))
	}
	if tenantOf(httptest.NewRequest("GET", "/", nil)) != "unknown" {
		t.Fatal("tenantOf")
	}
	if pct(nil, 0.5) != 0 || pct([]float64{3, 1, 2}, 0.5) != 2 || pct([]float64{3, 1, 2}, 0.99) != 3 {
		t.Fatal("pct")
	}
}

func statsOf(n *Netsim, name string) SimStats {
	for _, s := range n.Stats() {
		if s.Name == name {
			return s
		}
	}
	return SimStats{}
}

// TC-SIM-019: admin API requires the HMAC admin token; validates fault requests; applies latency/error rate.
func TestAdmin(t *testing.T) {
	n := newNS(t)
	h := n.AdminHandler()
	req := func(method, path, tok, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if tok != "" {
			r.Header.Set("X-Netsim-Admin", tok)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	for _, tok := range []string{"", "wrong"} {
		if rr := req("GET", "/stats", tok, ""); rr.Code != 401 {
			t.Fatal(tok, rr.Code)
		}
	}
	if rr := req("GET", "/stats", "admin-token", ""); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"udr"`) {
		t.Fatal(rr.Code)
	}
	if rr := req("GET", "/exchanges", "admin-token", ""); rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	for _, b := range []string{`{x`, `{"sim":"udr","latencyMs":-1}`, `{"sim":"udr","latencyMs":6000}`, `{"sim":"udr","errorRate":2}`, `{"sim":"udr","evil":1}`} {
		if rr := req("POST", "/faults", "admin-token", b); rr.Code != 400 {
			t.Fatal(b, rr.Code)
		}
	}
	if rr := req("POST", "/faults", "admin-token", `{"sim":"nope"}`); rr.Code != 404 {
		t.Fatal(rr.Code)
	}
	if rr := req("POST", "/faults", "admin-token", `{"sim":"udr","latencyMs":7,"errorRate":0.25}`); rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	if st := statsOf(n, "udr"); st.Latency != 7 || st.ErrRate != 0.25 {
		t.Fatalf("%+v", st)
	}
	empty := New("h", "")
	r := httptest.NewRequest("GET", "/stats", nil)
	rr := httptest.NewRecorder()
	empty.AdminHandler().ServeHTTP(rr, r)
	if rr.Code != 401 {
		t.Fatal("empty admin token must deny", rr.Code)
	}
	if AdminToken([]byte("a")) == AdminToken([]byte("b")) || len(AdminToken([]byte("a"))) != 40 {
		t.Fatal("AdminToken")
	}
}

// TC-SIM-020: admin listener refuses non-loopback binds and serves on loopback.
func TestServeAdmin(t *testing.T) {
	for _, a := range []string{"0.0.0.0:0", ":0", "10.0.0.1:0", "bad"} {
		if _, err := ServeAdmin(a, http.NotFoundHandler()); err == nil {
			t.Fatal("bound", a)
		}
	}
	srv, err := ServeAdmin("127.0.0.1:0", http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
}

// TC-SIM-021: simulators serve HTTP/2 over mutual TLS; clients without a certificate are rejected.
func TestServeMTLS(t *testing.T) {
	p := testenv.NewPKI(t)
	n := newNS(t)
	for _, s := range n.sims {
		s.port = 0
	}
	n.sims = n.sims[:1] // nrf only (ephemeral port)
	srv := httptest.NewUnstartedServer(n.Handler("nrf"))
	srv.TLS = p.ServerTLS(t, true)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	ok := &http.Client{Transport: &http.Transport{TLSClientConfig: p.ClientTLS(t, true), ForceAttemptHTTP2: true}}
	resp, err := ok.Get(srv.URL + "/nnrf-disc/v1/nf-instances?target-nf-type=UDR")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.ProtoMajor != 2 {
		t.Fatal(resp.Status, resp.Proto)
	}
	anon := &http.Client{Transport: &http.Transport{TLSClientConfig: p.ClientTLS(t, false)}}
	if _, err := anon.Get(srv.URL + "/"); err == nil {
		t.Fatal("client without certificate accepted")
	}
	if err := n.Serve(p.ServerTLS(t, true), p.ServerTLS(t, false)); err != nil {
		t.Fatal(err)
	}
	n.Close()
	if n.Ports()["nrf"] != 0 {
		t.Fatal(n.Ports())
	}
	bad := &tls.Config{}
	n2 := newNS(t)
	n2.sims = []*sim{{name: "x", port: -1}}
	if err := n2.Serve(bad, bad); err == nil {
		t.Fatal("invalid port accepted")
	}
}

// TC-SIM-022 (security): the exchange log (shown on the dashboard, exported to reports) never carries
// credentials: NRF access tokens, nested secrets and form-encoded secrets are masked; other fields are kept.
func TestExchangeRedaction(t *testing.T) {
	n := newNS(t)
	bearer := token(t, n, "nudr-dr")
	ex := n.Exchanges(1)
	if len(ex) != 1 || ex[0].Path != "/oauth2/token" {
		t.Fatalf("%+v", ex)
	}
	raw := string(ex[0].Response)
	if strings.Contains(raw, strings.TrimPrefix(bearer, "Bearer ")) || !strings.Contains(raw, redacted) || !strings.Contains(raw, `"token_type":"Bearer"`) {
		t.Fatal("token not redacted:", raw)
	}
	nested := string(compactJSON([]byte(`{"a":[{"Client_Secret":"s1","keep":"k"}],"auth":{"password":"p","user":"u"}}`)))
	if strings.Contains(nested, "s1") || strings.Contains(nested, `"p"`) || !strings.Contains(nested, `"keep":"k"`) || !strings.Contains(nested, `"user":"u"`) {
		t.Fatal(nested)
	}
	form := string(compactJSON([]byte("grant_type=client_credentials&client_secret=s3cr3t")))
	if strings.Contains(form, "s3cr3t") || !strings.Contains(form, "grant_type=client_credentials") {
		t.Fatal(form)
	}
	if got := string(compactJSON([]byte("{not json"))); !strings.Contains(got, "{not json") {
		t.Fatal("non-secret text must be kept verbatim", got)
	}
}
